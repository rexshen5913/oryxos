package core

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Session 是使用者與 Agent 一次對話的上下文容器，由 Channel、使用者、Profile
// 聯合標識。
type Session struct {
	ID          string // 持久化主鍵，由聯合標識、建立時刻與隨機後綴生成（見 NewSession）
	Channel     string
	UserID      string
	ProfileName string
	Messages    []Message
}

// SessionStore 是 Session 持久化的出向介面，由 internal/storage 以 SQLite 實作。
// 介面定義在 core 是為了守住依賴方向（storage 依賴 core，core 不反向依賴
// storage）；實作由組裝點顯式注入（憲法 5.2）。
type SessionStore interface {
	// Save 持久化 session 當前的對話歷史，並更新其最後活躍時間。
	Save(ctx context.Context, session *Session) error
}

// NopSessionStore 是不寫入的 Session 持久化，給無狀態呼叫用（spec #73 第七節）：每次呼叫都是一個
// 全新的 Session、跑一個 turn 就結束，歷史不寫進 sessions 表。
//
// **不寫入，但和真的儲存一樣拒絕已取消的 context**（Codex gate 第 1 輪）：持久化是 turn 的最後一步，
// ReAct 循環在最後一個 iteration 的 Tool 被取消時不回錯誤（Tool 回的是失敗的結果，循環以「已達最大
// 迭代次數」正常返回），是持久化替它擋下——SQLite 拒絕已取消的 context，turn 失敗、rollback，Web
// Service 回 504 或當作呼叫端斷線。這裡無條件回 nil 的話，同一個 turn 在無狀態呼叫裡會被當成成功。
type NopSessionStore struct{}

// Save 不寫入任何東西；context 已取消或逾時時回傳原因。
func (NopSessionStore) Save(ctx context.Context, _ *Session) error { return ctx.Err() }

// NewSession 建立一個空對話歷史的 Session。ID 由聯合標識、建立時刻與隨機後綴生成：
// 同一聯合標識「同時」至多一個 active Session，但先後可以有多個（歸檔後再開
// 新的），所以主鍵不能只由聯合標識決定。
//
// **只靠建立時刻不夠**（spec #73 第七節）：無狀態呼叫讓同一位使用者對同一份 Profile 並行
// 建立 Session，而 macOS 的時鐘只精確到微秒，同一微秒內的兩個會拿到同一個 ID，審計記錄
// 就被混成一次。後綴用隨機數而不是遞增的流水號：流水號得有一份整個進程共用的狀態（憲法
// 5.2），隨機數不必，也涵蓋 chat、建立 Session 與無狀態呼叫所有路徑。要的是不重複、不是
// 猜不到（ID 本來就帶著使用者與時間），所以用 math/rand/v2。
func NewSession(channel, userID, profileName string) *Session {
	return &Session{
		ID:          fmt.Sprintf("%s:%s:%s:%d:%016x", channel, userID, profileName, time.Now().UnixNano(), rand.Uint64()),
		Channel:     channel,
		UserID:      userID,
		ProfileName: profileName,
	}
}

// Append 追加一條訊息到對話歷史；未填時間戳時補上當下時間。
func (s *Session) Append(msg Message) {
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	s.Messages = append(s.Messages, msg)
}

// Truncate 把對話歷史截回前 n 條（失敗 turn 的 rollback 用）。
func (s *Session) Truncate(n int) {
	if n < 0 || n >= len(s.Messages) {
		return
	}
	s.Messages = s.Messages[:n]
}
