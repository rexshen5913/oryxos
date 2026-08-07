package core

import (
	"context"
	"fmt"
	"slices"
	"time"
)

const (
	// maxToolRetries 是單次 Tool 呼叫失敗後的重試次數上限（需求 8.2：
	// 可重試失敗按指數退避重試、最多三次）。
	maxToolRetries = 3
	// toolRetryBaseDelay 是指數退避的起始等待（100ms→200ms→400ms）：
	// 針對瞬時網路故障，重試耗盡的額外延遲控制在一秒內。
	toolRetryBaseDelay = 100 * time.Millisecond
)

// ReActLoop 是 Agent 的核心工作機制：呼叫 LLM、視回應決定呼叫 Tool 或給出
// 最終回應，直到終止或達最大迭代次數。自行實作、完全可控（憲法 2.1）。
type ReActLoop struct {
	provider ProviderService
	tools    ToolExecutor
}

// NewReActLoop 以 provider 與 Tool 子集建立 ReAct 循環；tools 不得為 nil。
func NewReActLoop(provider ProviderService, tools ToolExecutor) *ReActLoop {
	return &ReActLoop{provider: provider, tools: tools}
}

// Run 對 session 的當前對話歷史跑 ReAct 循環，回傳 Agent 的最終回應。每輪把
// LLM 回應與 Tool 結果追加到 session 對話歷史（完整可查）：LLM 沒有 Tool 呼叫
// 即為最終回應；有則按宣告順序逐一執行（不並行），結果以 tool 訊息回填後進入
// 下一輪。Tool 失敗（含 SandboxViolation）不是硬錯誤：可重試的失敗先按指數
// 退避重試，重試耗盡或不可重試時錯誤作為 tool 結果回填給 LLM，由 LLM 決定
// 下一步。達 settings.max_iterations 時強制終止，回覆固定提示語＋最後一輪
// LLM 內容（若有），turn 保留不算錯誤——已執行的 Tool 結果留在歷史。
func (l *ReActLoop) Run(ctx context.Context, profile *Profile, session *Session) (string, error) {
	maxIterations := profile.Settings.effectiveMaxIterations()
	defs := l.tools.Definitions()
	var lastContent string // 最後一輪 LLM 內容，強制終止時作為已知進度附上
	for range maxIterations {
		resp, err := l.provider.Chat(ctx, ChatRequest{
			Provider:    profile.Provider.Name,
			Model:       profile.Provider.Model,
			Temperature: profile.Provider.Temperature,
			Messages:    buildMessages(profile, session),
			Tools:       defs,
		})
		if err != nil {
			return "", fmt.Errorf("呼叫 LLM: %w", err)
		}
		session.Append(Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}
		lastContent = resp.Content

		for _, call := range resp.ToolCalls {
			result, retries, err := l.executeWithRetry(ctx, call)
			if err != nil {
				return "", err
			}
			content := result.Content
			if !result.OK {
				if retries > 0 && result.Retryable {
					content = fmt.Sprintf("Tool 執行失敗（已重試 %d 次）: %s", retries, result.Error)
				} else {
					content = "Tool 執行失敗: " + result.Error
				}
			}
			session.Append(Message{Role: RoleTool, Content: content, ToolCallID: call.ID})
		}
	}

	// 強制終止不是錯誤：固定提示語＋已知進度回覆使用者並留在歷史，
	// 已執行的 Tool 結果不因終止而被 rollback 丟棄（ticket #6 已定案）。
	reply := fmt.Sprintf("已達最大迭代次數 %d，仍未完成任務，已強制終止。", maxIterations)
	if lastContent != "" {
		reply += "最後一輪進度：" + lastContent
	}
	session.Append(Message{Role: RoleAssistant, Content: reply})
	return reply, nil
}

// executeWithRetry 執行一次 Tool 呼叫；結果標示可重試的失敗時按指數退避重試
// （最多 maxToolRetries 次），回傳最終結果與實際重試次數。退避等待走 ctx：
// 取消／逾時立即中斷並回傳錯誤（憲法 5.3）——此時本輪已嘗試執行過該 Tool，
// 錯誤註記外部效果不會因回退而撤銷。
func (l *ReActLoop) executeWithRetry(ctx context.Context, call ToolCall) (ToolResult, int, error) {
	result := l.tools.Execute(ctx, call)
	retries := 0
	delay := toolRetryBaseDelay
	for !result.OK && result.Retryable && retries < maxToolRetries {
		select {
		case <-ctx.Done():
			return ToolResult{}, retries, fmt.Errorf("Tool %s 失敗後的重試等待被取消（本輪已嘗試執行過該 Tool，其外部效果不會因回退而撤銷）: %w", call.Name, ctx.Err())
		case <-time.After(delay):
		}
		retries++
		delay *= 2
		result = l.tools.Execute(ctx, call)
	}
	return result, retries, nil
}

// buildMessages 組裝一次 LLM 呼叫的訊息序列：system prompt 僅來自 Profile 的
// identity.prompt（Bootstrap／Memory 注入屬後續 ticket），加上按
// max_history_turns 截斷後的近期對話歷史。
func buildMessages(profile *Profile, session *Session) []Message {
	history := truncateHistory(session.Messages, profile.Settings.effectiveMaxHistoryTurns())
	msgs := make([]Message, 0, len(history)+1)
	if profile.Identity.Prompt != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: profile.Identity.Prompt})
	}
	return append(msgs, history...)
}

// truncateHistory 保留近期 maxTurns 輪對話（一輪自一條 user 訊息起算，當前輪
// 計入 N），超出丟棄、不做總結壓縮。截斷點永遠落在 user 訊息邊界：不會把
// assistant 的 tool_calls 與對應 tool 結果攔腰切開（OpenAI 兼容協議要求成對
// 出現）。只影響進 prompt 的訊息，Session 歷史本身不動（完整可查）。
func truncateHistory(msgs []Message, maxTurns int) []Message {
	seen := 0
	for i, m := range slices.Backward(msgs) {
		if m.Role == RoleUser {
			seen++
			if seen == maxTurns {
				return msgs[i:]
			}
		}
	}
	return msgs
}
