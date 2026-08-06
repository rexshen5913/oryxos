package core

import "time"

// Session 是使用者與 Agent 一次對話的上下文容器，由 Channel、使用者、Profile
// 聯合標識。本切片為記憶體版；SQLite 持久化屬後續 ticket。
type Session struct {
	Channel     string
	UserID      string
	ProfileName string
	Messages    []Message
}

// NewSession 建立一個空對話歷史的 Session。
func NewSession(channel, userID, profileName string) *Session {
	return &Session{Channel: channel, UserID: userID, ProfileName: profileName}
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
