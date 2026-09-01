package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// columnMigration 是一次「幫既有表補一個欄位」的演進。
//
// 只表達 ADD COLUMN 一種形狀是刻意的：那是 SQLite 支援良好、且不必重建表的 DDL。
// 真的需要改型別或搬資料時，那是另一種形狀的問題，屆時再顯式處理（技術方案 §9.2）。
type columnMigration struct {
	table  string
	column string
	stmt   string
}

// auditMigrations 是建表語句之後要套用的表結構演進。
//
// **為什麼需要這一步**：建表語句用的是 CREATE TABLE IF NOT EXISTS，對已經存在的表
// 什麼都不做。既有 Workspace 的 llm_calls 因此不會長出新欄位，而 INSERT 帶著新欄位
// 送出去會撞 "no such column"——審計是旁路、不中斷對話，使用者只會在某天發現審計表
// 從某個版本之後就沒有新資料了。無聲的失敗，所以要在啟動時補上。
//
// **為什麼不引入 goose／golang-migrate**：技術方案 §9.2 警告的是「依賴任何**自動**
// 遷移」——ORM 看到 struct 變了就自行下 DDL 那一類，你無法預測它會做什麼。這裡是
// 手寫、讀得懂、冪等的顯式 DDL，正是它要求的「顯式管理」。為一個可空欄位背上一套
// 遷移框架與其版本表，違反憲法 3.1（YAGNI）與 1.4（不引入非必需的重框架）。
var auditMigrations = []columnMigration{
	{
		table:  "llm_calls",
		column: "cost_micro_usd",
		stmt:   `ALTER TABLE llm_calls ADD COLUMN cost_micro_usd INTEGER`,
	},
}

// applyMigrations 套用表結構演進，已經套過的跳過。
//
// 冪等的判準是**問資料庫「這個欄位在不在」**，不是「執行 ALTER 然後忽略重複欄位的
// 錯誤」：後者要比對驅動回傳的錯誤訊息字串，那是會隨驅動版本改變的東西，而且會連
// 真正的失敗（磁碟滿、表被鎖）一起吞掉。
func applyMigrations(ctx context.Context, db *sql.DB) error {
	for _, m := range auditMigrations {
		if err := applyColumnMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// applyColumnMigration 補一個欄位；已經存在（含被另一個進程搶先補上）就跳過。
//
// **先查再 ALTER 有 TOCTOU 競態**：OryxOS 是 CLI，使用者同時開兩個終端跑 oryxos chat
// 是支援的情境。兩邊首次升級時都可能查到欄位不存在，先執行的那個成功，後執行的撞
// duplicate column——第二個終端就啟動不了，而且錯誤訊息是使用者看不懂的 SQL 訊息。
//
// 所以 ALTER 失敗之後**重新查證一次**：欄位已經在了就代表競爭者剛補上，這次演進的
// 目的已經達成，視為成功。這樣仍然沒有比對任何錯誤訊息字串——判斷依據始終是資料庫
// 的事實。真正的失敗（磁碟滿、表被鎖）在重查時欄位依然不存在，原錯誤照樣上拋。
//
// 兩道查詢的分工要說清楚：**冪等性由後面那次重查保證**（突變測試證實單獨拿掉前置
// 檢查，重複執行依然安全）；前置檢查是**快路徑**——已經升級過的 Workspace 是絕大多數
// 情況，先問一句就跳過，不必每次啟動都送出一個註定失敗的 ALTER 去污染資料庫日誌。
func applyColumnMigration(ctx context.Context, db *sql.DB, m columnMigration) error {
	exists, err := columnExists(ctx, db, m.table, m.column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, execErr := db.ExecContext(ctx, m.stmt); execErr != nil {
		exists, err := columnExists(ctx, db, m.table, m.column)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("為 %s 新增欄位 %s: %w", m.table, m.column, execErr)
		}
	}
	return nil
}

// columnExists 問資料庫某張表上有沒有這個欄位。
func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("檢查 %s.%s 是否存在: %w", table, column, err)
	}
	return n > 0, nil
}
