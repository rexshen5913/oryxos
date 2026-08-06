package core

import "context"

// AgentService 是引擎的唯一對外入口（門面）：CLI Channel 每次輸入調 Process，
// 後續切片的 Web Service 也調同一入口，不另闢鏈路。
type AgentService struct {
	profile *Profile
	loop    *ReActLoop
}

// NewAgentService 以 profile 與 provider 組出 Agent 引擎。
func NewAgentService(profile *Profile, provider ProviderService) *AgentService {
	return &AgentService{profile: profile, loop: NewReActLoop(provider)}
}

// Process 處理一條使用者訊息：追加到 session 對話歷史、跑 ReAct 循環，
// 回傳 Agent 的最終回應。失敗時以 turn 為單位 rollback——本輪追加的訊息
// 全部移除，session 回到呼叫前狀態，caller 可安全以同一訊息 retry；
// 失敗細節由錯誤與 Provider 的結構化日誌承載，不留半截對話狀態。
func (a *AgentService) Process(ctx context.Context, session *Session, message string) (string, error) {
	checkpoint := len(session.Messages)
	session.Append(Message{Role: RoleUser, Content: message})
	resp, err := a.loop.Run(ctx, a.profile, session)
	if err != nil {
		session.Truncate(checkpoint)
		// 循環內已逐層以 %w 包裝，這層不再疊同義前綴（訊息清晰）。
		return "", err
	}
	return resp, nil
}
