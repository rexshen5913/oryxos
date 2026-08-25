package tool

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Executor 實作 core.ToolExecutor：持有 Profile 過濾後的可用 Tool 子集，
// 執行 LLM 的 Tool 呼叫請求並落結構化日誌。
type Executor struct {
	names  []string // 子集的宣告順序（Profile tools 順序）
	tools  map[string]OryxTool
	logger *slog.Logger
	// chain 是組好的中介層洋蔥，最內層是 dispatch。沒掛中介層時它**就是** dispatch，
	// 執行路徑與這個欄位出現之前完全一樣（見 TestExecutorWithoutMiddlewareUnchanged）。
	chain ToolFunc
}

// Definitions 回傳可用 Tool 的宣告列表（按子集順序），附進每輪 LLM 請求。
func (e *Executor) Definitions() []core.ToolDefinition {
	if len(e.names) == 0 {
		return nil
	}
	defs := make([]core.ToolDefinition, 0, len(e.names))
	for _, name := range e.names {
		t := e.tools[name]
		defs = append(defs, core.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return defs
}

// Execute 執行一次 Tool 呼叫：走完中介層鏈路、落日誌（Tool 名、參數摘要、狀態、
// 耗時）。任何失敗以錯誤 ToolResult 回報（回填給 LLM），不 panic。
//
// 日誌落在鏈路**外面**，所以它記的是這次呼叫最終的結果——被中介層拒絕的呼叫一樣
// 記得到，與 tool_invocations 審計「每次執行一筆」的語義維持一致。
func (e *Executor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	start := time.Now()
	result := e.invoke(ctx, call)

	attrs := []any{
		"tool", call.Name,
		"args", summarizeArgs(call.Arguments),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if result.OK {
		e.logger.InfoContext(ctx, "tool_invocation", append(attrs, "status", "completed")...)
	} else {
		e.logger.ErrorContext(ctx, "tool_invocation", append(attrs, "status", "failed", "error", core.RedactErrorText(result.Error))...)
	}
	return result
}

// invoke 走完中介層鏈路，並把途中的 panic 收成該次 Tool 的失敗結果。
//
// **這是 repo 裡唯一的 recover()，位置是刻意的。** 中介層是可插拔的接縫，日後掛上來
// 的東西（Tool Policy、第三方擴充）不在本 package 的控制範圍內；沒有這道保護的話，
// 其中一個的空指標就會在對話中途直接殺掉 CLI。收成失敗結果之後，錯誤照樣回填給
// LLM、照樣落日誌與審計，與其他 Tool 失敗走同一條路。
//
// 不標 Retryable：同一段程式碼再跑一次會再 panic 一次，重試只是把三次退避白等掉。
// ErrorKind 留零值（未分類）：panic 不是任何一種**領域**失敗，硬套一個類型會讓
// 給 LLM 的指引指向錯的方向。
//
// **panic 的原文不進對外訊息。** 它會被回填給 LLM、寫進 Session 歷史、隨 Session
// 落庫，而 panic 值是**任意字串**——中介層寫 `panic(fmt.Sprintf("呼叫 %s 失敗", url))`
// 就把一個帶 api key 的 URL 送了出去。對 LLM 來說那段細節也沒有用：訊息已經說明這是
// OryxOS 的 bug、不是它的參數問題，附上 `nil pointer dereference` 只會誘導它去「修正」
// 一個它改不動的東西。細節留在日誌，且**套用與其他落盤路徑相同的去敏規則**
// （core.RedactErrorText）——同一個檔案下面那行既有的 tool_invocation 日誌就是這樣做的。
func (e *Executor) invoke(ctx context.Context, call core.ToolCall) (result core.ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.ErrorContext(ctx, "tool_middleware_panic",
				"tool", call.Name, "panic", core.RedactErrorText(fmt.Sprint(r)))
			result = core.ToolResult{
				Error: fmt.Sprintf("Tool %q 執行過程發生內部錯誤（OryxOS 的 bug，不是你的參數問題）；細節已落在伺服器日誌", call.Name),
			}
		}
	}()
	return e.chain(ctx, call)
}

// dispatch 是洋蔥的最內層：查子集並執行。
func (e *Executor) dispatch(ctx context.Context, call core.ToolCall) core.ToolResult {
	t, ok := e.tools[call.Name]
	if !ok {
		return core.ToolResult{Error: fmt.Sprintf("Tool %q 不在此 Agent 的可用 Tool 子集（檢查 Profile 的 tools 欄位）", call.Name)}
	}
	return t.Execute(ctx, call.Arguments)
}

// summarizeArgs 產生可安全落日誌的參數摘要：去敏後再截斷控制單行長度。去敏規則
// 與審計落庫共用同一套實作（core.RedactArgs），兩條落盤路徑不該各有一套。
func summarizeArgs(args string) string {
	return core.TruncateRunes(core.RedactArgs(args), 200)
}
