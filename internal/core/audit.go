package core

import (
	"context"
	"errors"
	"time"
)

// 審計狀態值（需求文檔第 10 章）。
//
// 核心階段只寫**終態**：`running` 也是欄位的允許值，但要產生它得在執行前先落一
// 行、完成後再更新——兩次寫入換一個「執行到一半就掛了」才看得出差別的狀態。
// 核心階段的審計只記已發生的事實，這個狀態留給有需要時再落（憲法 3.1）。
const (
	AuditStatusCompleted = "completed"
	AuditStatusFailed    = "failed"
	AuditStatusTimeout   = "timeout"
)

// TokenUsage 是一次 LLM 呼叫的 token 用量，由 Provider 從回應原樣帶回。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// LLMCall 是一次 LLM 呼叫的審計記錄。
type LLMCall struct {
	SessionID   string
	Provider    string
	Model       string
	Usage       TokenUsage
	Latency     time.Duration
	Status      string
	StartedAt   time.Time
	CompletedAt time.Time
}

// ToolInvocation 是一次 Tool 呼叫的審計記錄。重試會產生多筆——每次實際執行都是
// 一件發生過的事，與既有 tool_invocation 結構化日誌每次執行一筆的語義一致。
type ToolInvocation struct {
	SessionID   string
	ProfileName string
	ToolName    string
	Parameters  string
	Status      string
	Result      string // 成功時的結果
	Error       string // 失敗時的錯誤
	StartedAt   time.Time
	CompletedAt time.Time
}

// AuditStore 是 core 寫出審計記錄的出向介面，由 internal/storage 以 SQLite 實作、
// 在組裝點顯式注入（憲法 5.2）。介面定義在 core 是為了守住依賴方向。
//
// **方法刻意不回傳 error。** 審計是旁路：寫入失敗不得中斷使用者的對話（技術方案
// 與 spec #2 定案）。把這條寫進型別，呼叫端就沒有機會「順手把錯誤往上傳」而讓
// 對話因審計故障失敗——那是這個介面唯一一條不能破的規則。錯誤仍被顯式處理，
// 只是處理的位置在實作端（記結構化錯誤日誌），不在呼叫端。
type AuditStore interface {
	RecordLLMCall(ctx context.Context, call LLMCall)
	RecordToolInvocation(ctx context.Context, inv ToolInvocation)
}

// auditStatus 依 turn 的 ctx 與是否失敗判定審計狀態：ctx 逾時記 timeout，其他
// 失敗記 failed。逾時的判定只看 turn 的 ctx——Provider 或 Tool 自帶的請求超時
// 是那個依賴的故障、不是本輪逾時，記 failed 才對得上事實。
func auditStatus(ctx context.Context, failed bool) string {
	switch {
	case !failed:
		return AuditStatusCompleted
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return AuditStatusTimeout
	default:
		return AuditStatusFailed
	}
}
