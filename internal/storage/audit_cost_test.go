// llm_calls 成本欄位落庫的測試（ticket #49，AC 第一條）。
//
// 與 internal/core 的 agent_cost_test.go 分工：那裡驗「Provider 回應的 token 用量
// 一路走到正確的數字」，這裡驗**儲存層原樣落庫**——算得對但寫的時候被縮放或被吞掉，
// 是那一端看不見的失敗。
package storage

import (
	"context"
	"database/sql"
	"testing"
)

// queryCost 取出唯一那筆 llm_calls 的成本欄位。
func queryCost(t *testing.T, db *DB) sql.NullInt64 {
	t.Helper()
	var cost sql.NullInt64
	if err := db.fg.QueryRowContext(context.Background(),
		`SELECT cost_micro_usd FROM llm_calls`).Scan(&cost); err != nil {
		t.Fatalf("查詢 cost_micro_usd: %v", err)
	}
	return cost
}

// TestRecordLLMCallStoresCost 驗成本原樣落庫，且**單位是百萬分之一美元**。
//
// 取 3000 這個值不是隨意的：它等於 0.003 美元，低於一美分。落庫路徑上若有任何
// 除法或單位換算（存美元、存美分），這格會得到 0——那正是這個單位要防的事。
func TestRecordLLMCallStoresCost(t *testing.T) {
	audit, db, _ := newTestAudit(t, discardLogger())

	cost := int64(3000)
	call := sampleLLMCall()
	call.CostMicroUSD = &cost
	audit.RecordLLMCall(context.Background(), call)
	mustFlush(t, audit)

	got := queryCost(t, db)
	if !got.Valid {
		t.Fatal("cost_micro_usd 是空值, 期望 3000")
	}
	if got.Int64 != 3000 {
		t.Errorf("cost_micro_usd = %d, 期望 3000（0.003 美元；存美元或美分都會歸零）", got.Int64)
	}
}

// TestRecordLLMCallStoresNilCostAsNull 驗沒算成本時落 SQL NULL 而不是 0。
//
// 這是「沒算」與「不用錢」在資料上分得開的最後一道：core 那端算出空值之後，儲存層
// 若把它當成零值寫成 0，整條語義就在最後一步斷掉，而成本報表看起來會很省。
func TestRecordLLMCallStoresNilCostAsNull(t *testing.T) {
	audit, db, _ := newTestAudit(t, discardLogger())

	call := sampleLLMCall() // CostMicroUSD 維持 nil
	audit.RecordLLMCall(context.Background(), call)
	mustFlush(t, audit)

	if got := queryCost(t, db); got.Valid {
		t.Errorf("cost_micro_usd = %d, 期望 SQL NULL", got.Int64)
	}
}
