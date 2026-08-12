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

// Execute 執行一次 Tool 呼叫：查子集、執行、落日誌（Tool 名、參數摘要、狀態、
// 耗時）。任何失敗以錯誤 ToolResult 回報（回填給 LLM），不 panic。
func (e *Executor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	start := time.Now()
	var result core.ToolResult
	if t, ok := e.tools[call.Name]; ok {
		result = t.Execute(ctx, call.Arguments)
	} else {
		result = core.ToolResult{Error: fmt.Sprintf("Tool %q 不在此 Agent 的可用 Tool 子集（檢查 Profile 的 tools 欄位）", call.Name)}
	}

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

// summarizeArgs 產生可安全落日誌的參數摘要：去敏後再截斷控制單行長度。去敏規則
// 與審計落庫共用同一套實作（core.RedactArgs），兩條落盤路徑不該各有一套。
func summarizeArgs(args string) string {
	return core.TruncateRunes(core.RedactArgs(args), 200)
}
