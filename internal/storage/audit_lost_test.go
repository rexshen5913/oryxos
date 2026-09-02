// 審計旁路「漏了幾筆」的計數（ticket #53 外部審查第一輪）。
//
// 審計是旁路：佇列滿了就丟棄、寫入失敗就記日誌，兩者都**絕不**中斷使用者的對話。
// 那條規則不變。但「不中斷對話」與「沒有人知道漏了幾筆」是兩件事——後者讓任何據此
// 計算的指標都可能偏低，而偏低的指標配上「不得超過」這種上限斷言，會直接產生綠燈。
package storage

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// TestLostWritesZeroWhenEveryRecordLands 是對照組：正常路徑一筆都不該算漏。
//
// 沒有這一格，下面兩格證明不了什麼——一個永遠回非零的計數器也會讓它們通過。
func TestLostWritesZeroWhenEveryRecordLands(t *testing.T) {
	ctx := context.Background()
	audit, db, _ := newTestAudit(t, discardLogger())

	audit.RecordLLMCall(ctx, sampleLLMCall())
	audit.RecordToolInvocation(ctx, sampleToolInvocation())
	mustFlush(t, audit)

	if got := audit.LostWrites(); got != 0 {
		t.Errorf("LostWrites() = %d，期望 0（兩筆都該落庫）", got)
	}
	// 兩個獨立來源要對得上：計數器說沒漏，資料庫裡就該真的有兩筆。
	if got := countRows(t, db, "llm_calls") + countRows(t, db, "tool_invocations"); got != 2 {
		t.Errorf("實際落庫 %d 筆，期望 2——計數器與資料庫對不上", got)
	}
}

// TestLostWritesCountsFailedDatabaseWrites 驗寫入資料庫失敗的那些筆數得出來。
//
// **讓真實依賴真的失敗**，不塞一個會回錯誤的假物件：先把 SQLite 關掉，之後每一筆
// 寫入都會從 ExecContext 回錯誤——那正是 db 故障時的真實形狀。
func TestLostWritesCountsFailedDatabaseWrites(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%s): %v", dbPath, err)
	}
	audit := NewAuditLog(db, discardLogger())
	t.Cleanup(func() { _ = audit.Close() })

	if err := db.Close(); err != nil {
		t.Fatalf("關閉資料庫: %v", err)
	}

	audit.RecordLLMCall(ctx, sampleLLMCall())
	audit.RecordToolInvocation(ctx, sampleToolInvocation())
	mustFlush(t, audit)

	if got := audit.LostWrites(); got != 2 {
		t.Errorf("LostWrites() = %d，期望 2（兩筆都寫不進去）", got)
	}
}

// TestLostWritesCountsDroppedOnFullQueue 驗佇列滿時丟棄的那些筆數得出來。
//
// 手法沿用 TestRecordDoesNotBlockCaller：先塞一個會卡住的工作把 worker 佔住，佇列
// 就必然被填滿——「滿了要丟棄」是確定會走到的分支，不必靠生產者比消費者快這種時序
// 巧合。佇列容量 auditQueueSize，多送 16 筆，所以正好丟 16 筆。
func TestLostWritesCountsDroppedOnFullQueue(t *testing.T) {
	ctx := context.Background()
	audit, _, _ := newTestAudit(t, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))

	parked := make(chan struct{})
	release := make(chan struct{})
	audit.queue <- auditJob{run: func(context.Context) {
		close(parked)
		<-release
	}}
	<-parked

	const overflow = 16
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range auditQueueSize + overflow {
			audit.RecordLLMCall(ctx, sampleLLMCall())
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

	if got := audit.LostWrites(); got != overflow {
		t.Errorf("LostWrites() = %d，期望 %d（佇列容量 %d，送了 %d 筆）",
			got, overflow, auditQueueSize, auditQueueSize+overflow)
	}
}
