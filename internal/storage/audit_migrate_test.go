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

// legacyLLMCallsSchemaWithCost 是 ticket #49 之後、#56 之前的建表語句，逐字保留當時
// 的十二個欄位。刻意寫死的理由同 legacyLLMCallsSchema。
//
// **為什麼兩份舊 schema 都要留著**：使用者硬碟上的資料庫可能停在任何一個版本，而
// 這兩份代表的是兩種不同的缺口——前者缺兩個欄位，後者只缺一個。ticket #56 的實測
// 就是在一個已經有 cost_micro_usd 的 Workspace 上量到的，那正是後者的形狀。
const legacyLLMCallsSchemaWithCost = `
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
);`

// writeLegacyDB 在 path 造一個「上一個版本留下的」資料庫：llm_calls 是舊的十一欄，
// 並帶一行既有資料。回傳那行的 call_id。
func writeLegacyDB(t *testing.T, path string) string {
	t.Helper()
	return writeLegacyDBWith(t, path, legacyLLMCallsSchema)
}

// writeLegacyDBWith 同上，但由呼叫端指定是哪一個舊版本的建表語句。
//
// 那行既有資料只寫十一個共通欄位：兩份舊 schema 都有它們，而較新那份多出來的
// cost_micro_usd 留 NULL 也正確——那一列本來就代表「當時沒算成本」。
func writeLegacyDBWith(t *testing.T, path, schema string) string {
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
	if _, err := db.Exec(schema); err != nil {
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

// TestOpenAddsCachedPromptTokensColumnToLegacyTable 驗既有 Workspace 打開後補上
// cached_prompt_tokens，且舊資料列在該欄位留 NULL（ticket #56）。
//
// **舊列為什麼必須是 NULL 而不是 0**：那些呼叫發生時 OryxOS 根本沒有記下這個資訊，
// 補 0 會讓「沒記錄」看起來像「沒有命中快取」——而後者是一句具體的事實陳述，稽核者
// 會據此相信那幾筆的成本可以覆算，然後算出對不上的數字。這與 ticket #49 對 NULL 與
// 0 的區分是同一條規則。ALTER TABLE ADD COLUMN 不帶 DEFAULT 時舊列正是 NULL，語義
// 免費拿到，這一格是在防有人日後「順手」補一個 DEFAULT 0。
//
// 兩種舊版本都測：使用者的資料庫可能停在 #49 之前（缺兩欄）或之間（只缺這一欄），
// 而 ticket #56 的實測正是在後者上量到的。
func TestOpenAddsCachedPromptTokensColumnToLegacyTable(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{name: "ticket #49 之前的十一欄", schema: legacyLLMCallsSchema},
		{name: "ticket #49 之後、#56 之前的十二欄", schema: legacyLLMCallsSchemaWithCost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			legacyID := writeLegacyDBWith(t, dbPath, tt.schema)

			db, err := Open(context.Background(), dbPath)
			if err != nil {
				t.Fatalf("開啟既有 Workspace 的資料庫: %v", err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("關閉資料庫: %v", err)
				}
			}()

			if !hasColumn(t, db, "llm_calls", "cached_prompt_tokens") {
				t.Fatal("既有 Workspace 的 llm_calls 沒有補上 cached_prompt_tokens，" +
					"之後每一筆審計都會寫失敗")
			}

			var (
				gotID  string
				cached *int64
			)
			if err := db.fg.QueryRowContext(context.Background(),
				`SELECT call_id, cached_prompt_tokens FROM llm_calls WHERE call_id = ?`, legacyID).
				Scan(&gotID, &cached); err != nil {
				t.Fatalf("遷移後讀不回舊資料: %v", err)
			}
			if gotID != legacyID {
				t.Errorf("call_id = %q, 期望 %q", gotID, legacyID)
			}
			if cached != nil {
				t.Errorf("舊資料的 cached_prompt_tokens = %d, 期望空值——"+
					"那次呼叫當時沒有記下快取資訊，寫 0 會謊稱它沒命中快取", *cached)
			}
		})
	}
}
