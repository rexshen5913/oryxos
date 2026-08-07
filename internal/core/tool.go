package core

import (
	"context"
	"encoding/json"
)

// ToolDefinition 是一個 Tool 對 LLM 的宣告：名稱、描述與輸入 JSON Schema，
// 由 Provider 按 Function Calling 格式附進 LLM 請求。
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolResult 是一次 Tool 執行的結果：成功標識、結果內容、錯誤資訊、是否可重試。
// Retryable 標示的失敗由 ReAct 循環按指數退避重試（最多三次，需求 8.2）；
// 重試耗盡或不可重試時，錯誤作為 tool 結果回填給 LLM。
type ToolResult struct {
	OK        bool
	Content   string // OK 時的結果內容
	Error     string // 失敗時的錯誤資訊，作為 tool 結果回填給 LLM
	Retryable bool
}

// ToolExecutor 是 ReAct 循環執行 Tool 的介面，由 internal/tool 以
// ToolRegistry 過濾出的可用子集實作。Tool 的查找、Sandbox 校驗與執行
// 都在實作內；循環只看 ToolResult（憲法 2.2）。
type ToolExecutor interface {
	// Definitions 回傳可用 Tool 的宣告列表，附進每輪 LLM 請求。
	Definitions() []ToolDefinition
	// Execute 執行一次 Tool 呼叫；一律以 ToolResult 回報，不 panic。
	Execute(ctx context.Context, call ToolCall) ToolResult
}
