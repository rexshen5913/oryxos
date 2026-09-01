// llm_calls 成本欄位的遷移測試（ticket #49）。
//
// 為什麼需要獨立一支：其餘 storage 測試都在 t.TempDir() 開全新的 db，建表語句
// 直接帶著新欄位跑，**永遠驗不到既有 Workspace 那條路徑**。而那條路徑正是會出事
// 的一條——CREATE TABLE IF NOT EXISTS 對已存在的表什麼都不做，欄位不會自己長出來，
// 於是每一筆 INSERT 都撞 "no such column"。審計是旁路、不會中斷對話，使用者只會在
// 某天發現審計表從某個版本之後就沒有新資料了。無聲的失敗要有測試盯著。
package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// legacyLLMCallsSchema 是 ticket #49 之前的建表語句，逐字保留當時的十一個欄位。
//
// **刻意寫死、不從 auditSchema 推導**：它代表的是「已經寫進使用者硬碟的那份過去」，
// 跟著現在的 schema 變動就失去意義了——那樣的話兩邊一起改，遷移漏做也測不出來。
const legacyLLMCallsSchema = `
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
	completed_at      TEXT NOT NULL
);`

// writeLegacyDB 在 path 造一個「上一個版本留下的」資料庫：llm_calls 是舊的十一欄，
// 並帶一行既有資料。回傳那行的 call_id。
func writeLegacyDB(t *testing.T, path string) string {
	t.Helper()
	dsn, err := dataSourceName(path)
	if err != nil {
		t.Fatalf("dataSourceName(%s): %v", path, err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("開啟舊版資料庫: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉舊版資料庫: %v", err)
		}
	}()
	if _, err := db.Exec(legacyLLMCallsSchema); err != nil {
		t.Fatalf("建立舊版 llm_calls: %v", err)
	}
	const id = "llm-legacy-1"
	if _, err := db.Exec(
		`INSERT INTO llm_calls
		     (call_id, session_id, provider, model, prompt_tokens, completion_tokens,
		      total_tokens, latency_ms, status, started_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "cli:local:default:1", "openrouter", "anthropic/claude-sonnet-4",
		100, 50, 150, 42, core.AuditStatusCompleted,
		"2026-08-01T00:00:00.000000000Z", "2026-08-01T00:00:01.000000000Z"); err != nil {
		t.Fatalf("寫入舊版資料: %v", err)
	}
	return id
}

// hasColumn 回傳表上有沒有這個欄位。走 PRAGMA table_info 而不是 SELECT 該欄位——
// 後者在欄位不存在時是錯誤，分不出「沒有欄位」與「查詢寫壞了」。
func hasColumn(t *testing.T, db *DB, table, column string) bool {
	t.Helper()
	rows, err := db.fg.QueryContext(context.Background(),
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		t.Fatalf("查詢 %s 的欄位: %v", table, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("關閉查詢: %v", err)
		}
	}()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		t.Fatalf("走訪欄位清單: %v", err)
	}
	return found
}

// TestOpenAddsCostColumnToLegacyTable 驗既有 Workspace 打開後補上成本欄位，且原有
// 資料一列不少。ALTER TABLE ADD COLUMN 是 SQLite 少數支援良好的 DDL，既有列的新欄位
// 值為 NULL——那正是「那些呼叫當時沒算成本」的正確表達。
func TestOpenAddsCostColumnToLegacyTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	legacyID := writeLegacyDB(t, dbPath)

	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("開啟既有 Workspace 的資料庫: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉資料庫: %v", err)
		}
	}()

	if !hasColumn(t, db, "llm_calls", "cost_micro_usd") {
		t.Fatal("既有 Workspace 的 llm_calls 沒有補上 cost_micro_usd，之後每一筆審計都會寫失敗")
	}

	// 舊資料還在，且新欄位是 NULL（不是 0）——那些呼叫當時就沒算成本。
	var (
		gotID string
		cost  *int64
	)
	if err := db.fg.QueryRowContext(context.Background(),
		`SELECT call_id, cost_micro_usd FROM llm_calls WHERE call_id = ?`, legacyID).
		Scan(&gotID, &cost); err != nil {
		t.Fatalf("遷移後讀不回舊資料: %v", err)
	}
	if gotID != legacyID {
		t.Errorf("call_id = %q, 期望 %q", gotID, legacyID)
	}
	if cost != nil {
		t.Errorf("舊資料的 cost_micro_usd = %d, 期望空值——那次呼叫當時沒算成本", *cost)
	}
}

// TestOpenMigrationIsIdempotent 驗遷移可以重複跑。每次啟動 OryxOS 都會 Open 一次，
// 第二次之後若因為「欄位已存在」而報錯，使用者的第二次啟動就掛了。
func TestOpenMigrationIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	writeLegacyDB(t, dbPath)

	for i := 1; i <= 3; i++ {
		db, err := Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("第 %d 次開啟: %v", i, err)
		}
		if !hasColumn(t, db, "llm_calls", "cost_micro_usd") {
			t.Fatalf("第 %d 次開啟後欄位不見了", i)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("第 %d 次關閉: %v", i, err)
		}
	}
}

// TestOpenMigrationIsConcurrencySafe 驗多個進程同時首次升級不會有人啟動失敗。
//
// OryxOS 是 CLI，使用者同時開兩個終端跑 oryxos chat 是支援的情境。「先查欄位在不在、
// 不在才 ALTER」在那個情境下有 TOCTOU 競態：兩邊都查到不存在，先執行的那個成功，
// 後執行的撞 duplicate column 而整個 Open 失敗——**第二個終端啟動不了**，錯誤訊息
// 還是使用者看不懂的 SQL 訊息（外部審查抓到的）。
//
// 這支測試不保證每次都能重現競態（窗口很小），它的價值在防迴歸：把冪等處理拿掉
// 之後它會開始不穩定，而穩定綠的實作才是正確的。
func TestOpenMigrationIsConcurrencySafe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	writeLegacyDB(t, dbPath)

	const racers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, racers)
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 盡量讓它們在同一瞬間進入 Open
			db, err := Open(context.Background(), dbPath)
			if err != nil {
				errs <- err
				return
			}
			errs <- db.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("並行首次升級時有人失敗: %v", err)
		}
	}
}
