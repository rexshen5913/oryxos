package memory

import (
	"context"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Service 是 Memory 的統一門面（技術方案 §5.1）：對 ReAct 鏈路只暴露一個介面，
// 內部把會話記憶委託給 Session 儲存（SQLite）、長期記憶委託給 LongTermMemory
// （MEMORY.md）。實作 core.MemoryService，由組裝點顯式注入（憲法 5.2）。
//
// 門面內部只有兩個委託對象。**不叫「三層門面」**——情景記憶屬擴展階段刻意未
// 實作，多算一層會誘導後人去補它（CONTEXT.md、技術方案 §5）。
type Service struct {
	sessions core.SessionStore
	longTerm *LongTermMemory
}

// NewService 以會話記憶與長期記憶兩個委託對象組出門面；兩者都不得為 nil。
func NewService(sessions core.SessionStore, longTerm *LongTermMemory) *Service {
	return &Service{sessions: sessions, longTerm: longTerm}
}

// SaveSession 把會話記憶的持久化委託給 Session 儲存。純委託不再加一層無資訊的
// 錯誤包裝——語境由呼叫端（AgentService）與儲存端各自標註。
func (s *Service) SaveSession(ctx context.Context, session *core.Session) error {
	return s.sessions.Save(ctx, session)
}

// LongTermMemory 取一份長期記憶快照，供 turn 開始時注入 system prompt。
func (s *Service) LongTermMemory(ctx context.Context) (string, error) {
	return s.longTerm.Load(ctx)
}
