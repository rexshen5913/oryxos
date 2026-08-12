// 審計儲存與 DSN 的單元測試。整條鏈路的行為由 internal/core 的整合測試從
// AgentService.Process seam 驅動；這裡補的是**seam 觀察不到**的性質：審計寫入
// 不受呼叫端 ctx 影響、佇列滿了不阻塞，以及 DSN 帶得動 pragma。
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// newTestAudit 在 t.TempDir() 開一個真實 SQLite 與其上的審計儲存。
func newTestAudit(t *testing.T, logger *slog.Logger) (*AuditLog, *DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open(%s): %v", dbPath, err)
	}
	audit := NewAuditLog(db, logger)
	t.Cleanup(func() {
		if err := audit.Close(); err != nil {
			t.Errorf("關閉審計儲存: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("關閉資料庫: %v", err)
		}
	})
	return audit, db, dbPath
}

// countRows 回傳一張表的資料列數。
func countRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.fg.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("計數 %s: %v", table, err)
	}
	return n
}

// TestRecordIgnoresCallerContext 釘住審計的核心性質：寫入**不受呼叫端 ctx 影響**。
// 呼叫端已逾時或被取消，正是最需要留下記錄的時刻（turn 逾時）；同時，呼叫端的
// 剩餘時間再少也不該讓記錄消失——在 db 完全健康時漏記是拿確定的資料遺失去換
// 一個罕見情境。兩者都靠「寫入排到背景、與呼叫端的時鐘無關」達成。
func TestRecordIgnoresCallerContext(t *testing.T) {
	tests := []struct {
		name string
		// callerCtx 造出各種已經沒有時間、或已經結束的呼叫端 ctx。
		callerCtx func(t *testing.T) context.Context
	}{
		{
			name:      "呼叫端沒有 deadline",
			callerCtx: func(*testing.T) context.Context { return context.Background() },
		},
		{
			name: "呼叫端只剩極短時間",
			callerCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
		},
		{
			name: "呼叫端已逾時",
			callerCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
		},
		{
			name: "呼叫端已被取消（沒有 deadline）",
			callerCtx: func(*testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audit, db, _ := newTestAudit(t, discardLogger())
			ctx := tt.callerCtx(t)

			audit.RecordLLMCall(ctx, sampleLLMCall())
			audit.RecordToolInvocation(ctx, sampleToolInvocation())
			mustFlush(t, audit)

			if got := countRows(t, db, "llm_calls"); got != 1 {
				t.Errorf("llm_calls 資料列數 = %d, 期望 1", got)
			}
			if got := countRows(t, db, "tool_invocations"); got != 1 {
				t.Errorf("tool_invocations 資料列數 = %d, 期望 1", got)
			}
		})
	}
}

// TestRecordDoesNotBlockCaller 驗證排入審計**絕不**阻塞呼叫端：審計反壓到對話上
// 就等於中斷對話，而那是這個旁路唯一不能破的規則。
//
// 先塞一個會卡住的工作把 worker 佔住，佇列就必然被填滿——這樣「滿了要丟棄而不是
// 等待」是確定會走到的分支，不必靠生產者比消費者快這種時序巧合。
func TestRecordDoesNotBlockCaller(t *testing.T) {
	var logBuf syncBuffer
	audit, _, _ := newTestAudit(t, slog.New(slog.NewJSONHandler(&logBuf, nil)))

	parked := make(chan struct{})
	release := make(chan struct{})
	audit.queue <- auditJob{run: func(context.Context) {
		close(parked)
		<-release
	}}
	<-parked

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range auditQueueSize + 16 { // 填滿佇列還有剩
			audit.RecordLLMCall(context.Background(), sampleLLMCall())
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("佇列滿時排入審計阻塞了呼叫端")
	}
	close(release)
	mustFlush(t, audit)

	if !strings.Contains(logBuf.String(), "audit_write_dropped") {
		t.Error("佇列滿時丟棄的記錄未落結構化日誌——審計出現破洞卻沒人知道")
	}
}

// TestCloseFlushesPendingWrites 驗證關閉時排空佇列——組裝點在進程結束前呼叫
// Close，佇列裡的記錄不該隨進程消失。
func TestCloseFlushesPendingWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	audit := NewAuditLog(db, discardLogger())
	const records = 20
	for range records {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}
	if err := audit.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := countRows(t, db, "llm_calls"); got != records {
		t.Errorf("Close 後 llm_calls 資料列數 = %d, 期望 %d", got, records)
	}

	// 關閉後再排入只會被丟棄，不能 panic（往已關閉的 channel 送會 panic）。
	audit.RecordLLMCall(context.Background(), sampleLLMCall())
	if err := audit.Close(); err != nil {
		t.Errorf("重複 Close 應該是安全的: %v", err)
	}
}

// TestCloseIsBoundedWhenWritesStall 釘住關閉有整體期限：db 卡住時，佇列裡每一筆
// 都會各自用滿 auditWriteBudget，沒有期限的話最壞要等 auditQueueSize×5 秒（約
// 21 分鐘）——使用者按 Ctrl-C 之後要等一場電影才回得到 shell。逾期後 worker 只
// 排空不寫，Close 因此能在期限內返回，且不留下 goroutine。
func TestCloseIsBoundedWhenWritesStall(t *testing.T) {
	var logBuf syncBuffer
	audit, _, _ := newTestAudit(t, slog.New(slog.NewJSONHandler(&logBuf, nil)))

	// 卡住的寫入＝一筆只在自己的 ctx 結束時才返回的工作，正是 ExecContext 撞上
	// 掛住的 db 時的形狀。佇列填滿，讓「逐筆各等一個 budget」的代價真的存在。
	stalled := make(chan struct{})
	audit.queue <- auditJob{run: func(ctx context.Context) {
		close(stalled)
		<-ctx.Done()
	}}
	<-stalled
	for range auditQueueSize {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}

	closed := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = audit.Close()
		closed <- time.Since(start)
	}()

	select {
	case elapsed := <-closed:
		if elapsed > auditShutdownBudget+2*time.Second {
			t.Errorf("Close 花了 %v, 期望不超過關閉預算 %v（加寬限）", elapsed, auditShutdownBudget)
		}
	case <-time.After(auditShutdownBudget + 2*time.Second):
		t.Fatal("Close 未在關閉預算內返回——寫入卡住時關閉會無限期等下去")
	}

	if !strings.Contains(logBuf.String(), "audit_write_dropped") {
		t.Error("關閉逾期丟棄的記錄未落結構化日誌——審計出現破洞卻沒人知道")
	}
}

// TestFlushDoesNotDeadlockClose 釘住 Flush 與 Close 的鎖順序：Flush 在 RLock 內
// 做阻塞送出，佇列滿且 worker 卡住時它會停在那裡，Close 就拿不到 Lock。正常路徑
// 下 worker 持續排空所以碰不到，但關閉本來就是為了收拾不正常的情況——兩者都必須
// 在關閉預算內返回。
func TestFlushDoesNotDeadlockClose(t *testing.T) {
	audit, _, _ := newTestAudit(t, discardLogger())

	stalled := make(chan struct{})
	audit.queue <- auditJob{run: func(ctx context.Context) {
		close(stalled)
		<-ctx.Done()
	}}
	<-stalled
	for range auditQueueSize {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}

	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		mustFlush(t, audit)
	}()
	// 等 Flush 真的停在 RLock 內的送出上：此時獨佔鎖取不到。這比 sleep 確定——
	// 要驗的正是「Flush 持著 RLock 時 Close 仍能完成」這個交錯。
	waitUntilRLocked(t, audit)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = audit.Close()
	}()

	deadline := time.After(auditShutdownBudget + 2*time.Second)
	select {
	case <-closed:
	case <-deadline:
		t.Fatal("Flush 持著 RLock 阻塞送出時，Close 拿不到 Lock——關閉互卡")
	}
	select {
	case <-flushed:
	case <-deadline:
		t.Fatal("Close 之後 Flush 仍未返回")
	}
}

// waitUntilRLocked 等到有人持著 audit.mu 的讀鎖為止（TryLock 失敗即代表）。
func waitUntilRLocked(t *testing.T, l *AuditLog) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !l.mu.TryLock() {
			return
		}
		l.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Flush 未進入阻塞送出")
}

// TestFlushWaitsForDrainDuringClose 釘住 Flush 在關閉中的語義：直接返回會讓
// 「Flush 之後記錄都已落地」這句話在關閉期間不成立，而 Close 與 Flush 共用的正是
// 同一個排空語義。關閉中呼叫要等 worker 真的結束。
func TestFlushWaitsForDrainDuringClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	audit := NewAuditLog(db, discardLogger())

	// 把 worker 停在一件不看 ctx 的工作上，後面排的記錄就確定還沒寫出去；
	// Flush 若在關閉中提早返回，這時候查表會是 0 筆。
	parked := make(chan struct{})
	release := make(chan struct{})
	audit.queue <- auditJob{run: func(context.Context) {
		close(parked)
		<-release
	}}
	<-parked

	const records = 20
	for range records {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}

	go func() { _ = audit.Close() }()
	waitUntilClosing(t, audit) // 確定走到的是「關閉中」那條分支
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(release)
	}()

	mustFlush(t, audit)
	if got := countRows(t, db, "llm_calls"); got != records {
		t.Errorf("Flush 返回時 llm_calls 資料列數 = %d, 期望 %d——關閉中的 Flush 沒等排空完成", got, records)
	}
}

// waitUntilClosing 等到 Close 已把 closed 立起來。
func waitUntilClosing(t *testing.T, l *AuditLog) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		l.mu.RLock()
		closed := l.closed
		l.mu.RUnlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Close 未進入關閉流程")
}

// TestAuditWriteDoesNotStarveSessionSave 釘住前景／背景分池：背景 worker 佔住
// **背景池唯一那條連線**時，前景的 Session Save 仍要能在自己的短 deadline 內完成。
//
// 佔住的方式是直接向背景池要一條 *sql.Conn 並持有——`SetMaxOpenConns(1)` 之下
// 那就是整個池。單純排一個 sleep 工作不成立：它不取得任何連線，共用單池的版本
// 也會照樣通過，證明不了分池。共用池時這個測試會紅（Save 排在背景後面，等到
// 呼叫端逾期而整輪 rollback），分池後才綠——這正是那條修正的變異證明。
func TestAuditWriteDoesNotStarveSessionSave(t *testing.T) {
	audit, db, _ := newTestAudit(t, discardLogger())
	sessions := NewSessionManager(db)

	held := make(chan struct{})
	release := make(chan struct{})
	connErr := make(chan error, 1)
	audit.queue <- auditJob{run: func(context.Context) {
		conn, err := audit.db.Conn(context.Background())
		if err != nil {
			connErr <- err
			close(held)
			return
		}
		defer func() { _ = conn.Close() }()
		close(held)
		<-release
	}}
	<-held
	defer close(release)

	select {
	case err := <-connErr:
		t.Fatalf("取得背景連線: %v", err)
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := sessions.Save(ctx, core.NewSession("cli", "local", "default")); err != nil {
		t.Fatalf("背景佔住審計連線時，前景 Save 應仍能寫入: %v", err)
	}
}

// TestSessionSaveSurvivesAuditWriteBurst 界定分池之後**仍然存在**的那件事：兩個
// 連線池最終寫的是同一個 SQLite 檔，INSERT 之間仍會在檔案寫鎖上相遇。可接受的
// 語義是——那是對等且有界的等待（兩邊 DSN 都帶 busy_timeout，單筆審計 INSERT
// 只持鎖一個語句的時間），不是前景被排在整批背景工作後面。這裡以持續的審計
// 寫入洪流壓著，斷言前景的每一次 Save 都不因此失敗。
func TestSessionSaveSurvivesAuditWriteBurst(t *testing.T) {
	audit, db, _ := newTestAudit(t, discardLogger())
	sessions := NewSessionManager(db)
	session := core.NewSession("cli", "local", "default")

	const (
		burst = 200
		saves = 20
	)
	go func() {
		for range burst {
			audit.RecordLLMCall(context.Background(), sampleLLMCall())
		}
	}()

	for i := range saves {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := sessions.Save(ctx, session)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 次 Save 在審計寫入洪流下失敗: %v", i+1, err)
		}
	}
}

// TestFlushIsBoundedByCallerContext 釘住 Flush 自己的界限：它是**公開的阻塞方法**，
// 而關閉預算要進 Close 才啟動——沒有呼叫 Close 時，佇列裡 256 筆故障寫入各用滿
// auditWriteBudget，Flush 會等上約 21 分鐘。界限由呼叫端的 ctx 給（憲法 5.3）。
func TestFlushIsBoundedByCallerContext(t *testing.T) {
	audit, _, _ := newTestAudit(t, discardLogger())

	stalled := make(chan struct{})
	audit.queue <- auditJob{run: func(ctx context.Context) {
		close(stalled)
		<-ctx.Done()
	}}
	<-stalled
	for range auditQueueSize {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	returned := make(chan error, 1)
	go func() { returned <- audit.Flush(ctx) }()
	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Flush 回傳 %v, 期望包住 context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush 未在呼叫端 ctx 的期限內返回——沒有 Close 時它沒有任何界限")
	}
}

// TestConcurrentCloseAllWaitForDrain 釘住 Close 的契約對**每一個**呼叫端都成立：
// 後到的那個看到 closed 就直接返回的話，它的呼叫端會以為 worker 已停、接著關掉
// DB，而第一個 Close 正在排空的寫入就撞上已關閉的連線。
func TestConcurrentCloseAllWaitForDrain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	audit := NewAuditLog(db, discardLogger())

	// 停住 worker，讓「排空尚未完成」在兩個 Close 之間確定成立。
	parked := make(chan struct{})
	release := make(chan struct{})
	audit.queue <- auditJob{run: func(context.Context) {
		close(parked)
		<-release
	}}
	<-parked

	const records = 20
	for range records {
		audit.RecordLLMCall(context.Background(), sampleLLMCall())
	}

	go func() { _ = audit.Close() }()
	waitUntilClosing(t, audit) // 第一個 Close 已在排空中
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(release)
	}()

	if err := audit.Close(); err != nil { // 後到的那個
		t.Fatalf("重複 Close: %v", err)
	}
	if got := countRows(t, db, "llm_calls"); got != records {
		t.Errorf("後到的 Close 返回時 llm_calls 資料列數 = %d, 期望 %d——它沒等排空完成", got, records)
	}
}

// TestDataSourceNameCarriesPragma 守住改用 DSN 帶 pragma 的兩項理由：路徑含
// `?`／`#` 時 pragma 不能靜默失效，而且**換掉連線之後仍要生效**——`PRAGMA`
// 只對當下那條連線有效，database/sql 換連線後就會回到 0，這正是不用 Exec 設定
// 的原因。
func TestDataSourceNameCarriesPragma(t *testing.T) {
	// 目錄名刻意帶上會破壞字串拼接式 DSN 的字元。
	dir := filepath.Join(t.TempDir(), "work?space#1 with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建立目錄: %v", err)
	}
	db, err := Open(context.Background(), filepath.Join(dir, "oryxos.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	assertBusyTimeout := func(what string) {
		t.Helper()
		var got int
		if err := db.fg.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&got); err != nil {
			t.Fatalf("讀取 busy_timeout（%s）: %v", what, err)
		}
		if got != 5000 {
			t.Errorf("busy_timeout（%s）= %d, 期望 5000", what, got)
		}
	}
	assertBusyTimeout("首次連線")

	// 逼 database/sql 丟棄閒置連線，下一次查詢會開一條新的。
	db.fg.SetMaxIdleConns(0)
	assertBusyTimeout("換連線之後")
}

// TestFileDSN 是 DSN 路徑正規化的矩陣。
//
// Windows 的磁碟與 UNC 路徑沒有前導斜線（`C:/x`、`//server/share`），不補的話
// url.URL 產出的不是合法的 file URI，SQLite 會指到錯的位置。反斜線轉斜線那一步
// 由 filepath.ToSlash 負責，它是平台相依的（在 darwin／linux 上是 no-op），
// 所以這裡餵的是「ToSlash 之後」的形狀——要驗的是補斜線與跳脫這段自己的邏輯。
func TestFileDSN(t *testing.T) {
	tests := []struct {
		name string
		abs  string
		// wantPrefix 是 DSN 應有的開頭（pragma 一律接在後面）。
		wantPrefix string
	}{
		{name: "unix 絕對路徑", abs: "/home/u/.oryxos/oryxos.db", wantPrefix: "file:///home/u/.oryxos/oryxos.db?"},
		{name: "含空白", abs: "/home/my work/oryxos.db", wantPrefix: "file:///home/my%20work/oryxos.db?"},
		{name: "含 ? 與 #", abs: "/home/w?s#1/oryxos.db", wantPrefix: "file:///home/w%3Fs%231/oryxos.db?"},
		{name: "windows 磁碟路徑（ToSlash 後）", abs: "C:/Users/u/.oryxos/oryxos.db", wantPrefix: "file:///C:/Users/u/.oryxos/oryxos.db?"},
		{name: "windows UNC 路徑（ToSlash 後）", abs: "//server/share/oryxos.db", wantPrefix: "file:////server/share/oryxos.db?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fileDSN(tt.abs)
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("DSN = %q, 期望以 %q 開頭", got, tt.wantPrefix)
			}
			if !strings.HasSuffix(got, busyTimeoutPragma) {
				t.Errorf("DSN = %q, 期望以 pragma %q 收尾", got, busyTimeoutPragma)
			}
			if strings.Contains(got, `\`) {
				t.Errorf("DSN 殘留反斜線，SQLite 認不得那是路徑分隔: %q", got)
			}
		})
	}
}

// sampleLLMCall／sampleToolInvocation 是內容不重要的最小記錄——這些測試驗的是
// 寫入是否發生，不是欄位對應（那由 internal/core 的整合測試從 seam 斷言）。
func sampleLLMCall() core.LLMCall {
	return core.LLMCall{
		SessionID: "cli:local:default:1",
		Provider:  "openai",
		Model:     "gpt-4o-mini",
		Status:    core.AuditStatusCompleted,
	}
}

func sampleToolInvocation() core.ToolInvocation {
	return core.ToolInvocation{
		SessionID:   "cli:local:default:1",
		ProfileName: "default",
		ToolName:    "http_get",
		Parameters:  `{"url":"https://example.com"}`,
		Status:      core.AuditStatusCompleted,
	}
}

// mustFlush 排空背景審計寫入。ctx 給的是寬鬆但有界的上限——這些測試要驗的不是
// Flush 的期限（那由 TestFlushIsBoundedByCallerContext 守），只是不讓卡住的排空
// 把整包測試掛到 go test 的總逾時。
func mustFlush(t *testing.T, l *AuditLog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := l.Flush(ctx); err != nil {
		t.Errorf("排空審計寫入: %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// syncBuffer 是可並行寫入的 bytes.Buffer——背景 worker 與測試主體會同時碰它。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
