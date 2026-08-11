package core

import (
	"context"
	"fmt"
	"time"
)

// Session 是使用者與 Agent 一次對話的上下文容器，由 Channel、使用者、Profile
// 聯合標識。
type Session struct {
	ID          string // 持久化主鍵，由聯合標識加建立時刻生成（見 NewSession）
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

// NewSession 建立一個空對話歷史的 Session。ID 由聯合標識加建立時刻生成：
// 同一聯合標識「同時」至多一個 active Session，但先後可以有多個（歸檔後再開
// 新的），所以主鍵不能只由聯合標識決定。
func NewSession(channel, userID, profileName string) *Session {
	return &Session{
		ID:          fmt.Sprintf("%s:%s:%s:%d", channel, userID, profileName, time.Now().UnixNano()),
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
