package core

import (
	"context"
	"fmt"
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
// 下一輪。Tool 失敗（含 SandboxViolation）不是硬錯誤：錯誤作為 tool 結果回填
// 給 LLM，由 LLM 決定下一步（可重試失敗的指數退避屬後續 ticket，issue #6）。
// 達 settings.max_iterations 時明確報錯終止（強制終止的固定提示語回覆屬 #6）。
func (l *ReActLoop) Run(ctx context.Context, profile *Profile, session *Session) (string, error) {
	// 預設在讀取點成立：手組（未經 LoadProfile）的 Profile 帶零值時
	// 不得零輪終止（spec：預設 10，Profile settings 可覆蓋）。
	maxIterations := profile.Settings.MaxIterations
	if maxIterations <= 0 {
		maxIterations = defaultMaxIterations
	}
	defs := l.tools.Definitions()
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

		for _, call := range resp.ToolCalls {
			result := l.tools.Execute(ctx, call)
			content := result.Content
			if !result.OK {
				content = "Tool 執行失敗: " + result.Error
			}
			session.Append(Message{Role: RoleTool, Content: content, ToolCallID: call.ID})
		}
	}
	return "", fmt.Errorf("達最大迭代次數 %d 仍未產生最終回應，循環強制終止", maxIterations)
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
