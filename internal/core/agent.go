package core

import (
	"context"
	"fmt"
)

// AgentService 是引擎的唯一對外入口（門面）：CLI Channel 每次輸入調 Process，
// 後續切片的 Web Service 也調同一入口，不另闢鏈路。
type AgentService struct {
	profile *Profile
	loop    *ReActLoop
	memory  MemoryService
}

// NewAgentService 以 profile、provider、Profile 過濾後的 Tool 子集與 Memory 門面
// 組出 Agent 引擎；tools 不得為 nil（無 Tool 的 Agent 傳空子集），memory 不得為
// nil（會話記憶的持久化是成功 turn 的一部分，長期記憶則每個 turn 載入一次）。
func NewAgentService(profile *Profile, provider ProviderService, tools ToolExecutor, memory MemoryService) *AgentService {
	return &AgentService{profile: profile, loop: NewReActLoop(provider, tools, memory), memory: memory}
}

// Process 處理一條使用者訊息：追加到 session 對話歷史、跑 ReAct 循環，成功後
// 持久化該輪，回傳 Agent 的最終回應。失敗時以 turn 為單位 rollback——本輪追加
// 的訊息全部移除，session 回到呼叫前狀態，儲存維持前一個狀態，不留半截對話
// 狀態（協議不合法的殘缺 tool 序列不能留在歷史，也不能留在庫裡）。rollback
// 只還原 Session：本輪若已執行過 Tool（如 http_post），其外部副作用不會撤銷，
// 錯誤訊息會註記，重試是否安全由使用者判斷；失敗細節由錯誤與結構化日誌承載。
func (a *AgentService) Process(ctx context.Context, session *Session, message string) (string, error) {
	checkpoint := len(session.Messages)
	session.Append(Message{Role: RoleUser, Content: message})
	resp, err := a.runTurn(ctx, session)
	if err != nil {
		ranTool := false
		for _, m := range session.Messages[checkpoint:] {
			if m.Role == RoleTool {
				ranTool = true
				break
			}
		}
		session.Truncate(checkpoint)
		if ranTool {
			return "", fmt.Errorf("%w（注意：本輪已執行過 Tool，其外部效果不會因回退而撤銷，重試前請確認）", err)
		}
		// 循環內已逐層以 %w 包裝，這層不再疊同義前綴（訊息清晰）。
		return "", err
	}
	return resp, nil
}

// runTurn 跑完一個 turn 的 ReAct 循環並持久化。持久化算 turn 的一部分：寫不進
// 儲存就不算成功，走與其他失敗相同的 rollback，讓記憶體 Session 與儲存維持
// 一致——寧可讓使用者看到錯誤並重試，也不要在他不知情下遺失整場對話。
func (a *AgentService) runTurn(ctx context.Context, session *Session) (string, error) {
	resp, err := a.loop.Run(ctx, a.profile, session)
	if err != nil {
		return "", err
	}
	if err := a.memory.SaveSession(ctx, session); err != nil {
		return "", fmt.Errorf("持久化 Session: %w", err)
	}
	return resp, nil
}
