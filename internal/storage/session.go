package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 純 Go SQLite 驅動，不引入 cgo（憲法 1.2）

	"github.com/rexshen5913/oryxos/internal/core"
)

// statusActive 是 Session 尚在進行中的狀態；statusArchived 是已收尾、不再接續
// 對話的狀態。歸檔由 `oryxos chat --new` 觸發（spec #2 定案 2026-08-07）。
//
// 這兩個字面值是落在 db 檔裡的既成事實——除了 status 欄位值，sessions_single_active
// 的部分索引條件也寫死 'active'。改字面值等同改 schema（既有資料列存的是舊字串、
// 索引條件要重建），必須連遷移一起做，所以索引裡刻意不做常數插值，那不是疏漏。
const (
	statusActive   = "active"
	statusArchived = "archived"
)

// timestampLayout 是時間欄位的落庫格式：RFC3339 加固定九位小數。刻意不用
// time.RFC3339Nano——它會裁掉尾端的零，讓字串排序偏離時間排序（無小數的
// "…:01Z" 會排到 "…:01.5Z" 之後）。固定寬度讓 SQL 直接按欄位排序就成立。
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// sessionsSchema 是手寫的建表語句，首次開啟時執行。不引入 ORM 與任何自動遷移
// 機制——SQLite 的 ALTER TABLE 能力有限，表結構演進必須顯式受控；真的需要演進
// 時再引入 goose／golang-migrate 一類的遷移工具（技術方案 §9.2）。
//
// 部分唯一索引把「同一聯合標識同時至多一個 active Session」這條不變式交給
// 資料庫守，而不是靠呼叫端自律。
const sessionsSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	session_id     TEXT PRIMARY KEY,
	profile_name   TEXT NOT NULL,
	channel        TEXT NOT NULL,
	user_id        TEXT NOT NULL,
	messages_json  TEXT NOT NULL,
	status         TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	last_active_at TEXT NOT NULL,
	archived_at    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_single_active
	ON sessions (channel, user_id, profile_name) WHERE status = 'active';
`

// DB 是 Workspace 內那個 SQLite 檔的連線。Session 與兩張審計表同庫（技術方案
// §9.2）：備份或搬遷 Workspace 就是搬一個檔案，日後要以 session_id 還原一場對話
// 的完整執行軌跡也不必跨庫。
//
// 前景與背景**分開兩個連線池**。兩者都限一條連線（SQLite 寫入互斥，單連線讓
// 寫入天然串行化），但不能共用同一條：審計的背景 worker 若佔著唯一那條連線
// 等鎖或等 I/O，前景的 Session Save 會連「試著寫」都做不到，只能排隊等到呼叫端
// 逾期而整輪 rollback——審計就這樣間接把對話弄失敗了，那正是這條旁路不能做的事。
//
// 分池收掉的是「排隊」，收不掉的是「搶鎖」：兩個池最終寫的還是同一個檔案，
// INSERT 之間仍會在 SQLite 的寫鎖上相遇。這是**刻意接受**的語義，界線如下——
// 前景最多等一筆審計 INSERT 執行完（單一語句、次毫秒級），而不是等整批背景工作
// 排完；兩邊 DSN 都帶 busy_timeout=5000，等待是對等且有界的。要再往前一步就得引
// 進前景優先或延後審計那類協調機制，那是為一個量級差三個數量級的等待付出的複雜度
// （憲法 3.2）。這條界線由 TestSessionSaveSurvivesAuditWriteBurst 守著。
type DB struct {
	fg *sql.DB // 前景：Session 讀寫，走使用者對話的關鍵路徑
	bg *sql.DB // 背景：審計寫入，旁路
}

// Open 開啟（必要時建立）path 上的 SQLite 資料庫並建好全部表。呼叫端負責 Close。
func Open(ctx context.Context, path string) (*DB, error) {
	dsn, err := dataSourceName(path)
	if err != nil {
		return nil, err
	}
	fg, err := openPool(dsn)
	if err != nil {
		return nil, fmt.Errorf("開啟 SQLite %s: %w", path, err)
	}
	for _, schema := range []string{sessionsSchema, auditSchema} {
		if _, err := fg.ExecContext(ctx, schema); err != nil {
			return nil, errors.Join(fmt.Errorf("建表（%s）: %w", path, err), fg.Close())
		}
	}
	bg, err := openPool(dsn)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("開啟 SQLite %s（背景）: %w", path, err), fg.Close())
	}
	return &DB{fg: fg, bg: bg}, nil
}

// openPool 開一個限單連線的連線池。SQLite 是單檔資料庫、寫入互斥；限一條連線讓
// 同一個池內的寫入天然串行化。
func openPool(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// busyTimeoutPragma 讓撞上鎖的寫入等一下再試，而不是立刻回 SQLITE_BUSY。
//
// `SetMaxOpenConns(1)` 只串行化**同一進程內**的寫入；同一個 Workspace 開兩個
// oryxos 進程是設想過的情境，而審計把每個 turn 的寫入從一次變成數次，撞鎖的
// 機率跟著上升——沒有它，別人的 Save 會因為我們在寫審計而失敗、整輪對話 rollback。
//
// 設在 **DSN** 而不是開啟後跑一次 `PRAGMA`：pragma 只對當下那條連線生效，
// database/sql 換掉連線（連線被判定壞掉、或 ctx 中斷後驅動丟棄它）後就回到 0。
// 實測確認過這個差異。DSN 形式則每條新連線都會套上。
//
// 不開 WAL：它會在 Workspace 裡多出 -wal 與 -shm 兩個側車檔案，破壞「備份或
// 搬遷 Workspace 就是搬一個檔案」這個既定性質。要開該是連帶重新定義那個性質
// 的一次獨立決定。
const busyTimeoutPragma = "_pragma=busy_timeout(5000)"

// dataSourceName 把檔案路徑組成 modernc.org/sqlite 的 DSN。
func dataSourceName(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析資料庫路徑 %s: %w", path, err)
	}
	return fileDSN(abs), nil
}

// fileDSN 把一個絕對路徑組成帶 pragma 的 `file:` DSN。
//
// 用 url.URL 組而不是字串拼接：路徑含 `?` 或 `#` 時，拼出來的 DSN 會把路徑的
// 後半當成 query，pragma **靜默失效**（連線照開、表照建，只是 busy_timeout
// 回到 0，不報任何錯）——這種失敗方式只能靠正確跳脫來避免。
//
// 路徑要先正規化成 URI 形式再交給 url.URL：Windows 的 `C:\x\y` 直接放進 Path
// 會被整段百分號跳脫成 `C:%5Cx%5Cy`，SQLite 認不得那是路徑分隔；UNC 的
// `\\server\share` 同理。轉成斜線並補上前導斜線後，`C:/x/y` 與 `//server/share`
// 都是合法的 file URI 路徑。
func fileDSN(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows 的 C:/x/y → /C:/x/y
	}
	dsn := url.URL{Scheme: "file", Path: p, RawQuery: busyTimeoutPragma}
	return dsn.String()
}

// Close 關閉兩個連線池。呼叫端要先關掉審計儲存（見 AuditLog.Close），否則
// 背景還沒寫出去的記錄會撞上已關閉的連線。
func (d *DB) Close() error {
	if err := errors.Join(d.fg.Close(), d.bg.Close()); err != nil {
		return fmt.Errorf("關閉 SQLite: %w", err)
	}
	return nil
}

// SessionManager 是 Session 的 SQLite 儲存：依聯合標識取回 active Session，
// 並在每個成功 turn 後持久化對話歷史。實作 core.SessionStore。
type SessionManager struct {
	db *sql.DB
}

// NewSessionManager 以已開啟的 DB 建立 Session 儲存；生命週期由 DB 持有，
// 本型別不負責關閉。
func NewSessionManager(db *DB) *SessionManager {
	return &SessionManager{db: db.fg}
}

// ActiveSession 依（Channel、使用者、Profile）聯合標識取回 active Session；
// 沒有時回傳一個空對話歷史的新 Session——它尚未落庫，第一個成功 turn 的 Save
// 才寫入，開了 chat 卻沒對話不會留下空資料列。
func (m *SessionManager) ActiveSession(ctx context.Context, channel, userID, profileName string) (*core.Session, error) {
	var sessionID, messagesJSON string
	err := m.db.QueryRowContext(ctx,
		`SELECT session_id, messages_json FROM sessions
		 WHERE channel = ? AND user_id = ? AND profile_name = ? AND status = ?`,
		channel, userID, profileName, statusActive).Scan(&sessionID, &messagesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NewSession(channel, userID, profileName), nil
	}
	if err != nil {
		return nil, fmt.Errorf("查詢 active Session（%s／%s／%s）: %w", channel, userID, profileName, err)
	}

	messages, err := decodeMessages(messagesJSON)
	if err != nil {
		return nil, fmt.Errorf("還原 Session %s 的對話歷史: %w", sessionID, err)
	}
	return &core.Session{
		ID:          sessionID,
		Channel:     channel,
		UserID:      userID,
		ProfileName: profileName,
		Messages:    messages,
	}, nil
}

// ArchiveActive 把該（Channel、使用者、Profile）聯合標識的 active Session 標記
// 為 archived 並寫 archived_at。歸檔只改狀態欄位，對話歷史原樣保留供日後查閱。
//
// 沒有 active Session 時什麼也不做、不算錯誤——在全新 Workspace 上執行
// `oryxos chat --new` 就是這個情況，語義等同直接開一場新對話。歸檔後聯合標識
// 上沒有 active 列，ActiveSession 便會開出一場乾淨的新對話。
//
// 定位方式是聯合標識、不是 session_id，只服務「歸檔當前這場」這一個用例。技術
// 方案 §7.2 的 DELETE /api/v1/sessions/{id} 以 ID 定位，落地時要另加一支 ID-based
// 的歸檔——別接到這裡：傳入舊 Session 的 ID 會歸檔到當前那場，改錯目標。兩者共用
// 的是狀態轉移本身（status 轉 archived、蓋 archived_at、對話歷史不動），不是這支
// 函式。
func (m *SessionManager) ArchiveActive(ctx context.Context, channel, userID, profileName string) error {
	now := time.Now().UTC().Format(timestampLayout)
	if _, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, archived_at = ?
		 WHERE channel = ? AND user_id = ? AND profile_name = ? AND status = ?`,
		statusArchived, now, channel, userID, profileName, statusActive); err != nil {
		return fmt.Errorf("歸檔 active Session（%s／%s／%s）: %w", channel, userID, profileName, err)
	}
	return nil
}

// Save 覆寫 session 的對話歷史並更新 last_active_at；Session 首次落庫時建立
// 資料列（status 為 active、created_at 為當下）。整段歷史一次覆寫而非增量
// 追加：對話歷史本就以 JSON 整欄儲存，單次寫入天然原子，不會留半截 tool 序列。
//
// 只寫得進 active 的資料列：`--new` 落地後，同一個 Workspace 開兩個 oryxos 進程
// 就可能讓 A 手上的 Session 被 B 歸檔掉，此時 A 的 Save 若照樣覆寫那個 archived
// 列，訊息會寫成功卻再也不被 ActiveSession 看見——使用者看到的是成功回應，實際是
// 靜默遺失。改為只更新 active 列，並以受影響列數判斷：0 列代表這個 Session 已不
// 再 active，回明確錯誤讓該 turn 失敗、走既有 rollback（同「寧可看到錯誤重試，
// 也不要在不知情下遺失對話」的既定取捨）。
func (m *SessionManager) Save(ctx context.Context, session *core.Session) error {
	messagesJSON, err := encodeMessages(session.Messages)
	if err != nil {
		return fmt.Errorf("序列化 Session %s 的對話歷史: %w", session.ID, err)
	}
	now := time.Now().UTC().Format(timestampLayout)

	// 衝突時只更新對話歷史與最後活躍時間：created_at、status、archived_at
	// 各有自己的寫入時機，不該被每個 turn 的存檔順手蓋掉。
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO sessions
		     (session_id, profile_name, channel, user_id, messages_json, status, created_at, last_active_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		     messages_json  = excluded.messages_json,
		     last_active_at = excluded.last_active_at
		 WHERE status = ?`,
		session.ID, session.ProfileName, session.Channel, session.UserID,
		messagesJSON, statusActive, now, now, statusActive)
	if err != nil {
		return fmt.Errorf("寫入 Session %s: %w", session.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("取得 Session %s 的寫入結果: %w", session.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("Session %s 已不是 active（可能已被另一個 oryxos 進程的 chat --new 歸檔），本輪未寫入", session.ID)
	}
	return nil
}

// persistedMessage 是 messages_json 的落庫形狀。刻意與 core.Message 分開定義：
// 落庫格式是對外承諾（使用者可直接開 db 檔查看、舊資料要能讀回），不該隨
// core 內部欄位改名而漂移。
type persistedMessage struct {
	Role      string              `json:"role"`
	Content   string              `json:"content"`
	Timestamp time.Time           `json:"timestamp"`
	ToolCalls []persistedToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 是 tool 訊息回應的 ToolCall.ID。少了它，恢復的歷史在
	// OpenAI 兼容協議下不合法（tool 訊息必須指回某次呼叫），跨重啟續談
	// 會被 Provider 拒絕——所以一併落庫。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// persistedToolCall 是 tool_calls 的落庫形狀。
type persistedToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// encodeMessages 把對話歷史序列化成 messages_json。
func encodeMessages(messages []core.Message) (string, error) {
	persisted := make([]persistedMessage, 0, len(messages))
	for _, msg := range messages {
		calls := make([]persistedToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			calls = append(calls, persistedToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
		p := persistedMessage{
			Role:       string(msg.Role),
			Content:    msg.Content,
			Timestamp:  msg.Timestamp,
			ToolCallID: msg.ToolCallID,
		}
		if len(calls) > 0 {
			p.ToolCalls = calls
		}
		persisted = append(persisted, p)
	}

	data, err := json.Marshal(persisted)
	if err != nil {
		return "", fmt.Errorf("序列化對話歷史: %w", err)
	}
	return string(data), nil
}

// decodeMessages 把 messages_json 還原成對話歷史。
func decodeMessages(messagesJSON string) ([]core.Message, error) {
	var persisted []persistedMessage
	if err := json.Unmarshal([]byte(messagesJSON), &persisted); err != nil {
		return nil, fmt.Errorf("解析對話歷史: %w", err)
	}

	messages := make([]core.Message, 0, len(persisted))
	for _, p := range persisted {
		calls := make([]core.ToolCall, 0, len(p.ToolCalls))
		for _, call := range p.ToolCalls {
			calls = append(calls, core.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
		msg := core.Message{
			Role:       core.Role(p.Role),
			Content:    p.Content,
			Timestamp:  p.Timestamp,
			ToolCallID: p.ToolCallID,
		}
		if len(calls) > 0 {
			msg.ToolCalls = calls
		}
		messages = append(messages, msg)
	}
	return messages, nil
}
