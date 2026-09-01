package storage

import (
	"context"
	"fmt"
)

// ToolNamesForSession 回傳這一場 Session 呼叫過的 Tool 名，去重、依**首次呼叫**的
// 先後排序。
//
// **為什麼需要一條讀取路徑**：評測 harness 要判斷「這個 Tool 有沒有被呼叫過」，而
// 審計表已經是現成的指標來源（ticket #50 定案）。另一條路是在 core.Session 上長一個
// 計數器，那會讓一個乾淨的資料結構冒出評測專用的欄位，而且那個欄位還要一路持久化、
// 重放——審計表則是本來就在記這件事的地方。
//
// 這與 audit.go「核心階段的審計只寫不查」那句註解不衝突：那句講的是**不做查詢介面
// 與報表**（技術方案 §9.2），這裡是一個給定 session_id 的單點查詢，不是報表層。
// 也因此**不建索引**：評測每個用例查一次，對只寫的表建索引仍是純成本。
//
// **狀態不參與過濾：失敗的呼叫也算呼叫過。** 「Agent 有沒有選對工具」與「那次執行成
// 不成功」是兩個問題，前者正是 tool_called 這種斷言要回答的——一個被白名單擋下的
// shell 呼叫，仍然證明了 Agent 決定去呼叫 shell。
//
// 走前景連線池：這是呼叫端會等的查詢，而背景池的單一連線是留給審計寫入的，讓讀取
// 排到那條連線上，等於用一個旁路的查詢去卡住旁路的寫入（見 DB 的欄位說明）。
//
// 排序用 MIN(started_at) 而不是 tool_name：評測失敗時看的人要的是「Agent 依序做了
// 什麼」，字母序把那條軌跡打散了。started_at 是固定寬度格式，字串排序就等於時間排序
// （見 timestampLayout）；同一時刻的並列再以 tool_name 收斂，讓輸出恆定。
func (db *DB) ToolNamesForSession(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := db.fg.QueryContext(ctx,
		`SELECT tool_name FROM tool_invocations
		  WHERE session_id = ?
		  GROUP BY tool_name
		  ORDER BY MIN(started_at), tool_name`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查詢 Session %s 的 Tool 呼叫記錄: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("讀取 Session %s 的 Tool 名: %w", sessionID, err)
		}
		names = append(names, name)
	}
	// rows.Err 必須查：迭代中途的錯誤只從這裡出來，漏掉的話一個被截斷的結果集會被
	// 當成「就是這些 Tool」，而評測會據此宣告某個 Tool 沒被呼叫過。
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("走訪 Session %s 的 Tool 呼叫記錄: %w", sessionID, err)
	}
	return names, nil
}
