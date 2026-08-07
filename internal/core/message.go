package core

import "time"

// Role 是對話訊息的角色，取值對齊 OpenAI 兼容協議。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是 Session 對話歷史中的一條訊息。
type Message struct {
	Role       Role
	Content    string
	Timestamp  time.Time
	ToolCalls  []ToolCall // LLM 要求呼叫 Tool 時非空（assistant 訊息）
	ToolCallID string     // tool 訊息回應的 ToolCall.ID（role 為 tool 時必填）
}

// ToolCall 是 LLM 回應中的一次 Tool 呼叫請求。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON 編碼的呼叫參數
}
