package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

const (
	// auditWriteBudget 是單次審計寫入的時間上限。寫入在背景進行、與任何 turn 的
	// ctx 無關，所以要自己給一個界限，免得 db 掛住時 worker 永遠卡在同一筆。
	auditWriteBudget = 5 * time.Second
	// auditQueueSize 是待寫佇列的容量。滿了就丟棄並記日誌——絕不反壓到對話上。
	// 一個 turn 最多產生 2×max_iterations 筆，這個容量夠好幾個 turn 的突發。
	auditQueueSize = 256
	// auditShutdownBudget 是**整個**關閉流程的時間上限。auditWriteBudget 只約束
	// 單筆，db 持續故障時佇列裡每一筆都會各用滿它——最壞是 auditQueueSize×5 秒
	// （約 21 分鐘），使用者結束 CLI 之後要等一場電影才回得到 shell。逾期後
	// worker 只排空不寫，剩下幾筆記一行日誌。
	auditShutdownBudget = 2 * time.Second
)

// auditSchema 是兩張審計表的手寫建表語句，與 sessions 同庫、首次開啟時執行
// （需求文檔第 10 章的欄位）。不引入 ORM 與自動遷移，理由同 sessionsSchema。
//
// 刻意不建索引：核心階段的審計只寫不查（技術方案 §9.2 明載不做查詢介面與報表），
// 對只寫的表建索引是純成本。日後真要以 session_id 還原執行軌跡時再 CREATE INDEX
// ——加索引不必重建表，跟 SQLite 那個能力有限的 ALTER TABLE 不是同一回事。
//
// llm_calls 沒有 error 欄位：需求文檔第 10 章的欄位清單裡就沒有，失敗以 status
// 記錄，訊息由既有的結構化日誌承載。不自行擴充欄位。
//
// cost_micro_usd 是唯一的例外，它由 ticket #49 加入：需求文檔第 10 章的清單裡同樣
// 沒有，但 spec #5 的使用者故事 30-34 明確要求「查得到這個 Agent 花了多少錢」，
// 而 tool_invocations.token_cost 不是它的位置（那個欄位的歸因口徑不成立，見下方
// RecordToolInvocation）。可空是語義的一部分：NULL 代表沒算，不是不用錢。
//
// **加欄位對既有 Workspace 不會自動生效**，CREATE TABLE IF NOT EXISTS 對已存在的表
// 什麼都不做——補欄位由 applyMigrations 負責（見 migrate.go）。
//
// 因此這一行與 migrate.go 的 ADD COLUMN **互為備份**：全新資料庫走這裡一次到位，
// 既有資料庫走那裡補上。突變測試證實拿掉這一行行為不變（遷移會補），差別只在新
// 資料庫要多跑一次 ALTER。保留它是為了讓建表語句本身就是完整的表定義——讀 schema
// 的人不必再翻 migrate.go 才知道這張表長什麼樣。
const auditSchema = `
CREATE TABLE IF NOT EXISTS llm_calls (
	call_id           TEXT PRIMARY KEY,
	session_id        TEXT NOT NULL,
	provider          TEXT NOT NULL,
	model             TEXT NOT NULL,
	prompt_tokens     INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	total_tokens      INTEGER NOT NULL,
	latency_ms        INTEGER NOT NULL,
	status            TEXT NOT NULL,
	started_at        TEXT NOT NULL,
	completed_at      TEXT NOT NULL,
	cost_micro_usd    INTEGER
);
CREATE TABLE IF NOT EXISTS tool_invocations (
	invocation_id TEXT PRIMARY KEY,
	session_id    TEXT NOT NULL,
	profile_name  TEXT NOT NULL,
	tool_name     TEXT NOT NULL,
	parameters    TEXT NOT NULL,
	status        TEXT NOT NULL,
	result        TEXT,
	error         TEXT,
	started_at    TEXT NOT NULL,
	completed_at  TEXT NOT NULL,
	token_cost    INTEGER
);
`

// AuditLog 是審計記錄的 SQLite 儲存，實作 core.AuditStore：每次 LLM 呼叫寫
// llm_calls 一行、每次 Tool 呼叫寫 tool_invocations 一行，以 session_id 關聯。
//
// 審計是**旁路**，要同時守住兩件事：不得讓對話失敗，也不得在系統健康時漏記。
// 同步寫入做不到兩者兼顧——沿用 turn 的 ctx 會讓逾時那筆寫不進去；改用獨立 ctx
// 又會在 db 卡住時燒掉呼叫端的 deadline，害它接著的 SaveSession 逾期而整輪
// rollback；為了避免那件事而在時間緊時跳過寫入，等於在 db 完全健康時故意漏記。
//
// 所以寫入放到背景 worker：呼叫端只把記錄排進佇列（不阻塞、不看它的 ctx），
// 由 worker 用自己的時間預算寫。佇列滿了才丟棄並記日誌。錯誤仍被顯式處理
// （憲法 5.1），處理方式是落結構化日誌而非上拋。
type AuditLog struct {
	db     *sql.DB
	logger *slog.Logger
	seq    atomic.Uint64 // 與時間戳一起組出不重複的主鍵

	queue chan auditJob
	// done 在背景 worker 結束時關閉。用 channel 而不是 sync.WaitGroup：等待
	// worker 結束本身是阻塞路徑，要能被呼叫端的 ctx 取消（憲法 5.3），
	// WaitGroup.Wait 沒有這個能力。
	done chan struct{}
	// life 是背景 worker 的生命週期 ctx，每筆寫入的 ctx 都由它派生。Close 逾期時
	// stop 一呼叫，卡住的那筆會立刻中斷、剩下的只排空不寫，等待才回得來。
	life context.Context
	stop context.CancelFunc
	// mu 保護 closed 與對 queue 的送出，避免 Close 之後還有人往已關閉的
	// channel 送而 panic。
	mu     sync.RWMutex
	closed bool
}

// auditJob 是佇列裡的一件工作。barrier 為真的那種不寫 db，只用來標記「排在它
// 前面的都處理完了」（見 Flush）——關閉逾期後其餘工作會被略過，barrier 仍要
// 執行，否則等在它上面的 Flush 永遠不會返回。
type auditJob struct {
	run     func(context.Context)
	barrier bool
}

// NewAuditLog 以已開啟的 DB 建立審計儲存並啟動背景寫入；logger 用於記錄寫入
// 失敗與丟棄，不得為 nil。呼叫端必須在關閉 DB **之前** 呼叫 Close，否則佇列裡
// 還沒寫出去的記錄會隨進程消失。DB 的生命週期由呼叫端持有。
func NewAuditLog(db *DB, logger *slog.Logger) *AuditLog {
	life, stop := context.WithCancel(context.Background())
	l := &AuditLog{
		db:     db.bg,
		logger: logger,
		queue:  make(chan auditJob, auditQueueSize),
		done:   make(chan struct{}),
		life:   life,
		stop:   stop,
	}
	go l.run()
	return l
}

// run 是背景寫入 worker：逐筆取出並以自己的時間預算執行。寫入的 ctx 派生自
// life，關閉逾期後（life 已取消）改為只取走不執行——佇列照樣被排空所以沒有人
// 會卡在送出上，只是不再花時間寫 db，讓 Close 能在期限內返回。
func (l *AuditLog) run() {
	defer close(l.done)
	var skipped int
	for job := range l.queue {
		if !job.barrier && l.life.Err() != nil {
			skipped++
			continue
		}
		ctx, cancel := context.WithTimeout(l.life, auditWriteBudget)
		job.run(ctx)
		cancel()
	}
	if skipped > 0 {
		// 一行帶數量，不是每筆一行：關閉時噴 256 行錯誤日誌只會淹掉真正的訊息。
		l.logger.Error("audit_write_dropped",
			"count", skipped, "reason", "關閉逾期，剩餘記錄未寫入")
	}
}

// Flush 等待目前已排入佇列的寫入全部完成。做法是排一個屏障工作進同一條佇列
// ——worker 走到它時，先前排入的必然都已寫完。
//
// 界限由 ctx 給（憲法 5.3）。它不能沿用 Close 的關閉預算：那個預算要進 Close
// 才啟動，而 Flush 是獨立的公開方法——沒有人呼叫 Close 時，佇列裡的故障寫入
// 各用滿 auditWriteBudget，等下去就是 auditQueueSize×5 秒。ctx 結束時回傳包住
// 原因的錯誤，屏障留在佇列裡由 worker 自行消化，不影響後續。
func (l *AuditLog) Flush(ctx context.Context) error {
	done := make(chan struct{})
	barrier := auditJob{run: func(context.Context) { close(done) }, barrier: true}

	l.mu.RLock()
	if l.closed {
		// 關閉中或已關閉：佇列已關，屏障排不進去。排空的語義此時由 Close 承擔，
		// 所以等 worker 真的結束才算數——直接返回會讓「Flush 之後記錄都已落地」
		// 這句話在關閉期間悄悄不成立。
		l.mu.RUnlock()
		return l.awaitWorker(ctx)
	}
	// 送出在 RLock 內，佇列滿時會停在這裡；ctx 讓它停得有界，Close 的 Lock 也就
	// 不會被無限期擋著。closed 只在持有寫鎖時翻轉，所以這裡的 queue 必然還開著。
	select {
	case l.queue <- barrier:
		l.mu.RUnlock()
	case <-ctx.Done():
		l.mu.RUnlock()
		return fmt.Errorf("排入審計屏障: %w", ctx.Err())
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待審計排空: %w", ctx.Err())
	}
}

// awaitWorker 等背景 worker 結束，等待本身可被 ctx 取消。
func (l *AuditLog) awaitWorker(ctx context.Context) error {
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待審計 worker 結束: %w", ctx.Err())
	}
}

// Close 排空佇列並停下背景寫入；重複呼叫是安全的。整段關閉有期限，逾期就放棄
// 剩餘記錄（見 auditShutdownBudget）——審計是旁路，不該讓使用者等在退出上。
//
// **每一個**呼叫端都等到 worker 真的結束才返回，後到的那個也一樣：Close 返回代表
// 「可以關 DB 了」，後到者若看到 closed 就先走，它的呼叫端會在第一個 Close 還在
// 排空時關掉連線，那些寫入就全數失敗。
func (l *AuditLog) Close() error {
	// 期限從進入 Close 就開始算，涵蓋等鎖與排空兩段：正在阻塞送出的 Flush 持著
	// RLock，得靠 stop 把它解開，Lock 才拿得到。
	timer := time.AfterFunc(auditShutdownBudget, l.stop)
	defer timer.Stop()

	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()

	<-l.done
	l.stop() // 走到這裡 worker 已結束，取消只為釋放 ctx
	return nil
}

// RecordLLMCall 排入一行 llm_calls。
func (l *AuditLog) RecordLLMCall(ctx context.Context, call core.LLMCall) {
	id := l.nextID("llm")
	l.enqueue(ctx, "llm_calls", id, func(writeCtx context.Context) error {
		_, err := l.db.ExecContext(writeCtx,
			`INSERT INTO llm_calls
			     (call_id, session_id, provider, model, prompt_tokens, completion_tokens,
			      total_tokens, latency_ms, status, started_at, completed_at, cost_micro_usd)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, call.SessionID, call.Provider, call.Model,
			call.Usage.PromptTokens, call.Usage.CompletionTokens, call.Usage.TotalTokens,
			call.Latency.Milliseconds(), call.Status,
			formatTimestamp(call.StartedAt), formatTimestamp(call.CompletedAt),
			// nil 原樣落成 SQL NULL——「沒配置定價所以沒算」與「算出來是零」在
			// 報表上是兩件事，driver 的可空整數剛好表達得出這個差別。
			call.CostMicroUSD)
		return err
	}, "session_id", call.SessionID, "provider", call.Provider)
}

// RecordToolInvocation 排入一行 tool_invocations。token_cost 一律寫 NULL（定案
// 2026-08-07）：一輪 LLM 回應可帶多個 tool_calls，任何歸因口徑都是編造精度，
// 錯誤精度比缺值更害；llm_calls 已完整記 token 且以 session_id 關聯，報表層
// 日後 join 得回來，原始資料無損。
func (l *AuditLog) RecordToolInvocation(ctx context.Context, inv core.ToolInvocation) {
	id := l.nextID("tool")
	l.enqueue(ctx, "tool_invocations", id, func(writeCtx context.Context) error {
		_, err := l.db.ExecContext(writeCtx,
			`INSERT INTO tool_invocations
			     (invocation_id, session_id, profile_name, tool_name, parameters,
			      status, result, error, started_at, completed_at, token_cost)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			id, inv.SessionID, inv.ProfileName, inv.ToolName, inv.Parameters,
			inv.Status, nullable(inv.Result), nullable(inv.Error),
			formatTimestamp(inv.StartedAt), formatTimestamp(inv.CompletedAt))
		return err
	}, "session_id", inv.SessionID, "tool", inv.ToolName)
}

// enqueue 把一筆寫入排進佇列。送出是**非阻塞**的：佇列滿或已關閉就丟棄並記
// 日誌，絕不讓審計反壓到使用者的對話上。ctx 只用來落日誌（帶追蹤資訊），
// 不參與寫入的時間控制——寫入在 worker 上有自己的預算。
func (l *AuditLog) enqueue(ctx context.Context, table, id string, write func(context.Context) error, attrs ...any) {
	logCtx := context.WithoutCancel(ctx)
	job := func(writeCtx context.Context) {
		if err := write(writeCtx); err != nil {
			l.logger.ErrorContext(logCtx, "audit_write_failed",
				append([]any{"table", table, "id", id, "error", err.Error()}, attrs...)...)
		}
	}

	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		l.report(logCtx, "audit_write_dropped", table, id, "審計儲存已關閉", attrs)
		return
	}
	select {
	case l.queue <- auditJob{run: job}:
	default:
		l.report(logCtx, "audit_write_dropped", table, id, "待寫佇列已滿", attrs)
	}
}

// report 記一筆審計旁路的結構化錯誤日誌。漏掉的記錄要看得見，否則審計出現破洞
// 卻沒人知道。
func (l *AuditLog) report(ctx context.Context, msg, table, id, reason string, attrs []any) {
	l.logger.ErrorContext(ctx, msg,
		append([]any{"table", table, "id", id, "reason", reason}, attrs...)...)
}

// nextID 產生審計記錄的主鍵。時間戳加進程內遞增序號：同一個進程內必不重複，
// 跨進程要撞上得在同一奈秒送出同一序號，實務上不可能；真撞上也只是主鍵衝突，
// 記一筆錯誤日誌，不影響對話。
func (l *AuditLog) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), l.seq.Add(1))
}

// nullable 讓空字串落庫成 NULL，而不是一個看不出差別的空字串。
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// formatTimestamp 以與 sessions 相同的固定寬度格式落時間戳，讓 SQL 直接按欄位
// 排序就成立（見 timestampLayout）。
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
