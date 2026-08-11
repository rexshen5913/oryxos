package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 純 Go SQLite 驅動，不引入 cgo（憲法 1.2）

	"github.com/rexshen5913/oryxos/internal/core"
)

// statusActive 是 Session 尚在進行中的狀態。歸檔（archived）的寫入路徑隨
// `oryxos chat --new` 落地，本切片只讀不寫。
const statusActive = "active"

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

// SessionManager 是 Session 的 SQLite 儲存：依聯合標識取回 active Session，
// 並在每個成功 turn 後持久化對話歷史。實作 core.SessionStore。
type SessionManager struct {
	db *sql.DB
}

// OpenSessionManager 開啟（必要時建立）path 上的 SQLite 資料庫並建表。
// 呼叫端負責 Close。
func OpenSessionManager(ctx context.Context, path string) (*SessionManager, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("開啟 SQLite %s: %w", path, err)
	}
	// SQLite 是單檔資料庫、寫入互斥；限一條連線讓寫入天然串行化，免去
	// SQLITE_BUSY 重試邏輯。核心階段（單機、每個 turn 一次寫）綽綽有餘。
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, sessionsSchema); err != nil {
		return nil, errors.Join(fmt.Errorf("建立 sessions 表（%s）: %w", path, err), db.Close())
	}
	return &SessionManager{db: db}, nil
}

// Close 關閉底層資料庫連線。
func (m *SessionManager) Close() error {
	if err := m.db.Close(); err != nil {
		return fmt.Errorf("關閉 SQLite: %w", err)
	}
	return nil
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

// Save 覆寫 session 的對話歷史並更新 last_active_at；Session 首次落庫時建立
// 資料列（status 為 active、created_at 為當下）。整段歷史一次覆寫而非增量
// 追加：對話歷史本就以 JSON 整欄儲存，單次寫入天然原子，不會留半截 tool 序列。
func (m *SessionManager) Save(ctx context.Context, session *core.Session) error {
	messagesJSON, err := encodeMessages(session.Messages)
	if err != nil {
		return fmt.Errorf("序列化 Session %s 的對話歷史: %w", session.ID, err)
	}
	now := time.Now().UTC().Format(timestampLayout)

	// 衝突時只更新對話歷史與最後活躍時間：created_at、status、archived_at
	// 各有自己的寫入時機，不該被每個 turn 的存檔順手蓋掉。
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO sessions
		     (session_id, profile_name, channel, user_id, messages_json, status, created_at, last_active_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		     messages_json  = excluded.messages_json,
		     last_active_at = excluded.last_active_at`,
		session.ID, session.ProfileName, session.Channel, session.UserID,
		messagesJSON, statusActive, now, now); err != nil {
		return fmt.Errorf("寫入 Session %s: %w", session.ID, err)
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
