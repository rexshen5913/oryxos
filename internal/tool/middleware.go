package tool

import (
	"context"
	"log/slog"

	"github.com/rexshen5913/oryxos/internal/core"
)

// ToolFunc 是洋蔥的一層執行體：吃一次 Tool 呼叫、回一個結果。最內層是 Executor
// 自己的查找與執行，外面每包一層中介層就多一個 ToolFunc。
type ToolFunc func(ctx context.Context, call core.ToolCall) core.ToolResult

// Middleware 是掛在 **Profile 過濾後的 Executor** 上的可插拔中介層。
//
// 簽名採**洋蔥形式**：接收這次呼叫與一個「執行下一層」的函式，回傳 ToolResult。
//
// **單層而非兩層。** 拒絕語義用「不呼叫 next、直接回錯誤結果」表達就完整了，不需要
// 獨立的前置攔截型別（憲法 3.2 偏好簡單）。
//
// **先掛載者位於洋蔥外層**：Subset 收到的第一個中介層最先進入、最後離開。
//
// **掛在 Executor 上，不掛在 Registry 上。** 「這個 Agent 的 Tool 要不要審批」本來
// 就該因 Agent 而異，那是多 Profile 定位的必然結果；掛在全域註冊表上就做不到。
//
// 第一個使用者是事件播報（見 NewEventMiddleware）。Tool Policy（issue #39）是可預見
// 的第二個，屬擴展階段——**本 package 不提供任何政策內容**。
type Middleware func(ctx context.Context, call core.ToolCall, next ToolFunc) core.ToolResult

// NewEventMiddleware 回傳播報 tool_started 與 tool_finished 的中介層。
//
// 事件由**中介層**播報而不是寫死在 Executor 內部，這是接縫存在的理由本身：若寫死在
// 執行器裡，Tool Policy 落地時仍要回頭改同一個地方。
//
// tool_finished 走 defer，所以下層 panic 時它**仍然播報**（OK 為 false 的零值結果）。
// 成對是消費端依賴的性質：只進不出的話，CLI 畫面上那句「正在執行 xxx」會永遠停住。
//
// tool_retrying 刻意**不在**這裡：重試是 ReAct 循環的決策、不屬於單次執行，中介層
// 每次也只看得到一次執行、拿不到重試次數。
func NewEventMiddleware(sink core.EventSink, logger *slog.Logger) Middleware {
	return func(ctx context.Context, call core.ToolCall, next ToolFunc) (result core.ToolResult) {
		core.EmitEvent(ctx, sink, logger, core.Event{Kind: core.EventToolStarted, ToolName: call.Name})
		defer func() {
			core.EmitEvent(ctx, sink, logger, core.Event{
				Kind: core.EventToolFinished, ToolName: call.Name, OK: result.OK,
			})
		}()
		return next(ctx, call)
	}
}

// chainMiddlewares 把中介層由內而外包在 base 上，讓**先掛載者落在最外層**。
//
// 從尾往頭包是達成那個順序的做法：最後一個先被包（貼著 base），第一個最後被包
// （在最外面）。組裝只在 Subset 發生一次，每次呼叫走的是已經組好的那條鏈路。
func chainMiddlewares(base ToolFunc, middlewares []Middleware) ToolFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw, next := middlewares[i], base
		base = func(ctx context.Context, call core.ToolCall) core.ToolResult {
			return mw(ctx, call, next)
		}
	}
	return base
}
