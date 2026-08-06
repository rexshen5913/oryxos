package core

import (
	"context"
	"fmt"
)

// ReActLoop 是 Agent 的核心工作機制：呼叫 LLM、視回應決定呼叫 Tool 或給出
// 最終回應，直到終止或達最大迭代次數。自行實作、完全可控（憲法 2.1）。
type ReActLoop struct {
	provider ProviderService
}

// NewReActLoop 以 provider 建立 ReAct 循環。
func NewReActLoop(provider ProviderService) *ReActLoop {
	return &ReActLoop{provider: provider}
}

// Run 對 session 的當前對話歷史跑 ReAct 循環，回傳 Agent 的最終回應，
// LLM 回應追加到 session 對話歷史（完整可查）。本張 ticket 只落地「無 Tool
// 呼叫直接回應」分支，一輪 LLM 呼叫即終止；Tool 結果回填的多輪迭代與
// settings.max_iterations 上限屬後續 ticket（issue #5、#6），在其落地前對
// Tool 呼叫明確報錯，不默默吞掉。
func (l *ReActLoop) Run(ctx context.Context, profile *Profile, session *Session) (string, error) {
	resp, err := l.provider.Chat(ctx, ChatRequest{
		Provider:    profile.Provider.Name,
		Model:       profile.Provider.Model,
		Temperature: profile.Provider.Temperature,
		Messages:    buildMessages(profile, session),
	})
	if err != nil {
		return "", fmt.Errorf("呼叫 LLM: %w", err)
	}
	session.Append(Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

	if len(resp.ToolCalls) > 0 {
		return "", fmt.Errorf("LLM 要求呼叫 Tool %q，但 Tool 執行尚未實作（屬後續 ticket）", resp.ToolCalls[0].Name)
	}
	return resp.Content, nil
}

// buildMessages 組裝一次 LLM 呼叫的訊息序列：system prompt 僅來自 Profile 的
// identity.prompt（Bootstrap／Memory 注入屬後續 ticket），加上 Session 對話歷史
// （max_history_turns 截斷屬後續 ticket，issue #6）。
func buildMessages(profile *Profile, session *Session) []Message {
	msgs := make([]Message, 0, len(session.Messages)+1)
	if profile.Identity.Prompt != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: profile.Identity.Prompt})
	}
	return append(msgs, session.Messages...)
}
