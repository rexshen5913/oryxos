package core

import "context"

// ChatRequest 是一次 LLM 呼叫的輸入。
type ChatRequest struct {
	Provider    string // provider name，指向註冊表中的客戶端
	Model       string
	Temperature float32
	Messages    []Message
	Tools       []ToolDefinition // 可用 Tool 列表，按 Function Calling 格式附進請求
}

// ChatResponse 是一次 LLM 呼叫的輸出；ToolCalls 非空表示 LLM 要求呼叫 Tool。
// Usage 由 Provider 從回應原樣帶回，供審計落庫記 token 用量。
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     TokenUsage
}

// ProviderService 是 ReAct 循環呼叫 LLM 的介面，由 internal/provider 以
// go-openai 實作。實作只做協議轉換；循環與 Tool 調度由本 package 自行控制
// （憲法 2.1、2.2）。
type ProviderService interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
