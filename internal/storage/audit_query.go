package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rexshen5913/oryxos/internal/core"
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

// SessionMetrics 是一場 Session 的指標摘要，四項全部**算自審計表**。
//
// 為什麼不在 core.Session 上長計數器：與 ToolNamesForSession 同一條理由（ticket #53
// 定案）——審計表本來就是在記這些事的地方，而 Session 是要持久化、要能原樣重放給
// Provider 的資料結構，讓評測用途的欄位長在上面，那些欄位會一路跟著它走。
type SessionMetrics struct {
	// Iterations 是這一場 Session 的 iteration 數，也就是對 Provider 呼叫了幾次
	// （CONTEXT.md：iteration 是一個 turn 之內 ReAct 循環的一次 LLM 呼叫）。
	//
	// **失敗的呼叫一樣算一次。** 一次送出去又壞掉的請求，時間與 token 都已經花掉了，
	// 不計入會讓一個重試很多次的 Agent 看起來比實際省。
	Iterations int
	// ToolFailures 是失敗的 Tool 呼叫次數，failed 與 timeout 都算。
	ToolFailures int
	// TotalTokens 是這一場全部 Provider 呼叫的 token 用量加總。
	TotalTokens int
	// CostMicroUSD 是成本加總，單位百萬分之一美元；**nil 代表沒算**，與
	// core.LLMCall.CostMicroUSD 是同一套語義（ticket #49）。
	CostMicroUSD *int64
}

// MetricsForSession 回傳這一場 Session 的指標摘要。
//
// 兩條查詢而不是一條帶子查詢的：兩張表回答的是兩個獨立的問題，湊在一句 SQL 裡只會
// 讓它變成一段要逐層拆解才讀得懂的東西，而省下的一次往返在評測裡是每個用例一次。
//
// 走前景連線池、不建索引，理由同 ToolNamesForSession。
func (db *DB) MetricsForSession(ctx context.Context, sessionID string) (SessionMetrics, error) {
	var m SessionMetrics
	// calls 與 priced 分開數：COUNT(*) 數全部的呼叫，COUNT(cost_micro_usd) 只數
	// 算得出成本的那些——兩者的差就是「有幾次沒算」，而那是下面那個判斷的依據。
	var calls, priced int
	var tokens, cost sql.NullInt64
	if err := db.fg.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(cost_micro_usd), SUM(total_tokens), SUM(cost_micro_usd)
		   FROM llm_calls WHERE session_id = ?`, sessionID).Scan(&calls, &priced, &tokens, &cost); err != nil {
		return SessionMetrics{}, fmt.Errorf("查詢 Session %s 的 LLM 呼叫指標: %w", sessionID, err)
	}
	m.Iterations = calls
	// SUM 在沒有任何資料列時回 NULL，不是 0；掃進 NullInt64 再取值，零值就是 0。
	m.TotalTokens = int(tokens.Int64)
	// **每一次呼叫都算得出成本，總額才算得出來。**
	//
	// 少了任何一筆就回 nil，而不是回那個部分和：一個少算了一半的金額比沒有金額更糟
	// ——它看起來像事實，讀報表的人不會去追它漏了什麼。這與 ticket #49 在單筆上
	// 「沒算就落 NULL、不寫零」是同一條規則，只是這裡的判斷單位是整場 Session。
	if calls > 0 && priced == calls && cost.Valid {
		total := cost.Int64
		m.CostMicroUSD = &total
	}

	// 狀態明列 failed 與 timeout，不寫成 `status <> 'completed'`：後者會把 running
	// 也數成失敗——那個狀態是欄位的合法值，核心階段只是刻意不寫（見 core/audit.go）。
	// 明列的代價是日後多一種失敗狀態時要回來加，而那正是應該有人重新想一次的時刻。
	if err := db.fg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_invocations
		  WHERE session_id = ? AND status IN (?, ?)`,
		sessionID, core.AuditStatusFailed, core.AuditStatusTimeout).Scan(&m.ToolFailures); err != nil {
		return SessionMetrics{}, fmt.Errorf("查詢 Session %s 的 Tool 失敗次數: %w", sessionID, err)
	}
	return m, nil
}
