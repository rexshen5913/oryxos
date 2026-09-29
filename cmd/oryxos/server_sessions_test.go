// `oryxos server` 的 Session 生命週期：建立、查詢、歸檔，ticket #78。這張票完全不呼叫 Provider。
//
// 與 server_test.go 同一個 seam：runServer 接收呼叫端開好的 listener，測試對它發真實的 HTTP
// 請求。SQLite 用真的；超過 100 條的歷史用 storage 既有的 Save 佈置。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/channel/cli"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/storage"
)

// sessionObject 是 Session 物件的斷言形狀；GET 多帶 messages 與 total_messages。指標欄位用來分辨
// null 與缺欄位。
type sessionObject struct {
	SessionID     string          `json:"session_id"`
	Profile       string          `json:"profile"`
	UserID        string          `json:"user_id"`
	Status        string          `json:"status"`
	CreatedAt     time.Time       `json:"created_at"`
	LastActiveAt  time.Time       `json:"last_active_at"`
	ArchivedAt    *time.Time      `json:"archived_at"`
	Messages      []messageObject `json:"messages"`
	TotalMessages *int            `json:"total_messages"`
}

// messageObject 是歷史訊息的斷言形狀。
type messageObject struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	ToolCalls []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
	ToolCallID *string `json:"tool_call_id"`
}

// postSession 對 POST /api/v1/sessions 送出 body，原樣回傳回應。
func postSession(t *testing.T, s *runningServer, body string) (*http.Response, []byte) {
	t.Helper()
	return s.send(t, http.MethodPost, "/api/v1/sessions", body, nil)
}

// createSession 建立一個 Session，期望 201，回傳解析後的 Session 物件。
func createSession(t *testing.T, s *runningServer, profile, userID string) sessionObject {
	t.Helper()
	resp, body := postSession(t, s, fmt.Sprintf(`{"profile":%q,"user_id":%q}`, profile, userID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建立 Session 狀態碼 = %d, 期望 201\nbody: %s", resp.StatusCode, body)
	}
	return decodeSessionObject(t, body)
}

// decodeSessionObject 把回應 body 解成 Session 物件。
func decodeSessionObject(t *testing.T, body []byte) sessionObject {
	t.Helper()
	var obj sessionObject
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("解析 Session 物件: %v\nbody: %.300s", err, body)
	}
	return obj
}

// sessionPath 是一個 Session 的資源路徑。
func sessionPath(id string) string { return "/api/v1/sessions/" + id }

// getSession 取 GET /api/v1/sessions/{id}，期望 200。
func getSession(t *testing.T, s *runningServer, id string) sessionObject {
	t.Helper()
	resp, body := s.send(t, http.MethodGet, sessionPath(id), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 狀態碼 = %d, 期望 200\nbody: %.300s", id, resp.StatusCode, body)
	}
	return decodeSessionObject(t, body)
}

// workspaceDB 是 Workspace 內那個 SQLite 檔的路徑。
func workspaceDB(dir string) string { return filepath.Join(dir, workspaceDir, sessionDBFile) }

// sessionColumns 直接查 sessions 表裡一個 Session 的 channel、status 與 messages_json；不存在時
// found 為假。
func sessionColumns(t *testing.T, dir, id string) (channel, status, messagesJSON string, found bool) {
	t.Helper()
	db, err := sql.Open("sqlite", workspaceDB(dir))
	if err != nil {
		t.Fatalf("開啟 db 檔: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉 db 檔: %v", err)
		}
	}()
	err = db.QueryRowContext(context.Background(),
		`SELECT channel, status, messages_json FROM sessions WHERE session_id = ?`, id).
		Scan(&channel, &status, &messagesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false
	}
	if err != nil {
		t.Fatalf("查詢 sessions 表: %v", err)
	}
	return channel, status, messagesJSON, true
}

// seedHistory 用 storage 既有的 Save 覆寫一個 Session 的對話歷史。server 同時開著同一個 db 檔，
// 這裡另開一條連線，與 chat 和 server 共用 Workspace 的情形相同。
func seedHistory(t *testing.T, dir string, session core.Session) {
	t.Helper()
	db, err := storage.Open(context.Background(), workspaceDB(dir))
	if err != nil {
		t.Fatalf("開啟 Workspace 資料庫: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉 Workspace 資料庫: %v", err)
		}
	}()
	if err := storage.NewSessionManager(db).Save(context.Background(), &session); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestServerSessionLifecycle 走一遍 Session 的生命週期（ticket #78 AC）：建立 → 查詢 → 歸檔 →
// 查詢 → 重複歸檔 → 為同一聯合標識再建立。全程不呼叫 Provider（回放伺服器一份錄製回應都不給）。
func TestServerSessionLifecycle(t *testing.T) {
	var providerRequests [][]byte
	dir := setupChatWorkspace(t, newRecordingReplayServer(t, &providerRequests).URL)
	s := startServer(t, dir)

	before := time.Now()
	created := createSession(t, s, "default", "alice")
	if created.Profile != "default" || created.UserID != "alice" || created.Status != "active" {
		t.Errorf("建立的 Session = %+v, 期望 default／alice／active", created)
	}
	if created.ArchivedAt != nil {
		t.Errorf("active Session 的 archived_at = %v, 期望 null", created.ArchivedAt)
	}
	if created.CreatedAt.Before(before.Add(-time.Second)) || !created.CreatedAt.Equal(created.LastActiveAt) {
		t.Errorf("created_at = %v、last_active_at = %v, 期望剛剛、而且兩者相同", created.CreatedAt, created.LastActiveAt)
	}

	t.Run("建立之後立刻查詢：active、空歷史、落庫的 channel 是 web", func(t *testing.T) {
		got := getSession(t, s, created.SessionID)
		if got.Status != "active" || got.Messages == nil || len(got.Messages) != 0 {
			t.Errorf("GET = status %q、messages %v, 期望 active 與 []", got.Status, got.Messages)
		}
		if got.TotalMessages == nil || *got.TotalMessages != 0 {
			t.Errorf("total_messages = %v, 期望 0", got.TotalMessages)
		}
		if channel, _, _, found := sessionColumns(t, dir, created.SessionID); !found || channel != "web" {
			t.Errorf("sessions 表裡的 channel = %q（找到 %v）, 期望 web", channel, found)
		}
	})

	var archivedAt time.Time
	t.Run("歸檔：200、archived、archived_at 非 null", func(t *testing.T) {
		resp, body := s.send(t, http.MethodDelete, sessionPath(created.SessionID), "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
		}
		archived := decodeSessionObject(t, body)
		if archived.Status != "archived" || archived.ArchivedAt == nil {
			t.Fatalf("歸檔後 = status %q、archived_at %v, 期望 archived 與非 null", archived.Status, archived.ArchivedAt)
		}
		archivedAt = *archived.ArchivedAt
		if got := getSession(t, s, created.SessionID); got.Status != "archived" || got.ArchivedAt == nil {
			t.Errorf("歸檔後 GET = status %q、archived_at %v, 期望 archived 與非 null", got.Status, got.ArchivedAt)
		}
	})

	t.Run("重複歸檔：仍回 200，archived_at 不變", func(t *testing.T) {
		resp, body := s.send(t, http.MethodDelete, sessionPath(created.SessionID), "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第二次 DELETE 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
		}
		if again := decodeSessionObject(t, body); again.ArchivedAt == nil || !again.ArchivedAt.Equal(archivedAt) {
			t.Errorf("第二次 DELETE 的 archived_at = %v, 期望維持 %v", again.ArchivedAt, archivedAt)
		}
	})

	t.Run("歸檔之後：同一聯合標識可以再建立", func(t *testing.T) {
		if next := createSession(t, s, "default", "alice"); next.SessionID == created.SessionID {
			t.Errorf("再建立的 Session ID 與舊的相同（%s）", next.SessionID)
		}
	})

	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	if len(providerRequests) != 0 {
		t.Errorf("Session 生命週期期間 Provider 收到 %d 個請求, 期望 0", len(providerRequests))
	}
}

// TestServerCreateSessionConflict 釘住「同一聯合標識同時至多一個 active Session」在 Web Service 上的樣子
// （spec #73 第五節）：第二次建立回 409 session_exists，並附上既有的 session_id；同一位使用者對
// 另一份 Profile 則兩者都成功。
func TestServerCreateSessionConflict(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	writeNamedProfile(t, dir, "analyst", "provider:\n  name: openrouter\n  model: m\n")
	s := startServer(t, dir)
	first := createSession(t, s, "default", "alice")

	t.Run("同一使用者、同一 Profile：409，附上既有的 session_id", func(t *testing.T) {
		resp, body := postSession(t, s, `{"profile":"default","user_id":"alice"}`)
		assertErrorShape(t, resp, body, http.StatusConflict, "session_exists")
		var conflict struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(body, &conflict); err != nil || conflict.SessionID != first.SessionID {
			t.Errorf("409 回應的 session_id = %q, 期望第一次建立的 %q\nbody: %s", conflict.SessionID, first.SessionID, body)
		}
	})

	t.Run("同一使用者、另一份 Profile：兩者都成功", func(t *testing.T) {
		if other := createSession(t, s, "analyst", "alice"); other.SessionID == first.SessionID {
			t.Errorf("兩份 Profile 拿到同一個 Session ID（%s）", first.SessionID)
		}
	})
}

// TestServerCreateSessionRejectsBadRequests 是建立請求的輸入表格（spec #73 第五、八節，ticket #78
// AC）。每一格都斷言錯誤形狀、訊息指出是哪個欄位、不帶 session_id，以及**sessions 表裡一筆都
// 沒有**：驗證要在落庫之前做完，不是寫進去之後才報錯。
func TestServerCreateSessionRejectsBadRequests(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	writeBrokenProfile(t, dir, "name: broken\nprovider: [unclosed\n")
	s := startServer(t, dir)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		// wantInMessage 是 message 裡必須出現的片段：指出是哪個欄位或哪份 Profile。
		wantInMessage string
	}{
		{name: "user_id 缺失", body: `{"profile":"default"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 是空字串", body: `{"profile":"default","user_id":""}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 含換行", body: `{"profile":"default","user_id":"alice\nbob"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 含字元集以外的字元（斜線）", body: `{"profile":"default","user_id":"alice/bob"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 含字元集以外的字元（空白）", body: `{"profile":"default","user_id":"alice bob"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 超過長度上限", body: `{"profile":"default","user_id":"` + strings.Repeat("a", 129) + `"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "user_id 的型別不是字串", body: `{"profile":"default","user_id":42}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "profile 缺失", body: `{"user_id":"alice"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "profile"},
		{name: "帶了未知欄位（拼錯的 profle）", body: `{"profle":"default","user_id":"alice"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "profle"},
		{name: "JSON 格式錯誤", body: `{"profile":"default",`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "JSON"},
		{name: "body 是空的", body: ``,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "body"},
		{name: "JSON 物件之後還有東西", body: `{"profile":"default","user_id":"alice"}{"x":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "JSON"},
		{name: "body 超過上限", body: `{"profile":"default","user_id":"` + strings.Repeat("a", 300<<10) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "request_too_large", wantInMessage: "body"},
		// body 上限要裝得下 #79 最壞的情形：32 KiB 的內容逐字跳脫成 \uXXXX，放大 6 倍變成 192 KiB。
		// 它該被當成「user_id 太長」擋下（400），而不是在讀 body 時就被當成「body 太大」（413）。
		{name: "32 KiB 的內容以 \\u 跳脫、放大 6 倍：仍在 body 上限之內",
			body:       `{"profile":"default","user_id":"` + strings.Repeat(`\u0061`, 32<<10) + `"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
		{name: "Profile 不存在：404", body: `{"profile":"ghost","user_id":"alice"}`,
			wantStatus: http.StatusNotFound, wantCode: "profile_not_found", wantInMessage: "ghost"},
		{name: "Profile 不可用：503", body: `{"profile":"broken","user_id":"alice"}`,
			wantStatus: http.StatusServiceUnavailable, wantCode: "profile_unavailable", wantInMessage: "broken"},
		// 形狀先於語意：請求本身寫錯（400）要先回報，不必等去找 Profile 才發現。
		{name: "Profile 不存在、user_id 也不合法：先回 400", body: `{"profile":"ghost","user_id":"alice bob"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "user_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := postSession(t, s, tt.body)
			assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
			if !strings.Contains(string(body), tt.wantInMessage) {
				t.Errorf("錯誤回應沒有指出 %q\nbody: %.300s", tt.wantInMessage, body)
			}
			// session_id 只屬於 409 session_exists；其他錯誤帶著它，呼叫端會以為有一場可以接著用。
			if _, has := decodeJSONObject(t, body)["session_id"]; has {
				t.Errorf("錯誤回應帶了 session_id，只有 session_exists 該帶\nbody: %.300s", body)
			}
		})
	}
	if n := len(sessionRows(t, workspaceDB(dir))); n != 0 {
		t.Errorf("被拒絕的請求留下了 %d 筆 sessions 資料列, 期望 0", n)
	}

	t.Run("字元集的四個符號與剛好 128 的長度都接受", func(t *testing.T) {
		userID := "a.b-c_d@e" + strings.Repeat("x", 128-len("a.b-c_d@e"))
		if got := createSession(t, s, "default", userID); got.UserID != userID {
			t.Errorf("user_id = %q, 期望 %q", got.UserID, userID)
		}
	})
}

// TestServerSessionHistoryWindow 釘住歷史的回傳窗口與去敏（spec #73 第八節，ticket #78 AC）：
// 只回最近 100 條、由舊到新，total_messages 是完整的條數；tool 呼叫參數套用與審計相同的去敏
// 規則，而 sessions 表裡的原文不動——對話歷史必須能原樣重放給 Provider。
func TestServerSessionHistoryWindow(t *testing.T) {
	const (
		total        = 150
		apiKeyCanary = "sk-history-canary-78"
		queryCanary  = "query-canary-78"
	)
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	session := core.Session{ID: created.SessionID, Channel: "web", UserID: "alice", ProfileName: "default"}
	for i := range total {
		msg := core.Message{Role: core.RoleUser, Content: fmt.Sprintf("訊息 %03d", i)}
		if i == 120 { // 落在最近 100 條之內
			msg = core.Message{Role: core.RoleAssistant, Content: fmt.Sprintf("訊息 %03d", i), ToolCalls: []core.ToolCall{{
				ID: "call-1", Name: "http_get",
				Arguments: `{"url":"https://api.example/v1?key=` + queryCanary + `","api_key":"` + apiKeyCanary + `"}`,
			}}}
		}
		session.Append(msg)
	}
	seedHistory(t, dir, session)

	resp, body := s.send(t, http.MethodGet, sessionPath(created.SessionID), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET 狀態碼 = %d, 期望 200\nbody: %.300s", resp.StatusCode, body)
	}
	got := decodeSessionObject(t, body)
	if len(got.Messages) != 100 {
		t.Fatalf("回傳 %d 條, 期望最近的 100 條", len(got.Messages))
	}
	if got.TotalMessages == nil || *got.TotalMessages != total {
		t.Errorf("total_messages = %v, 期望 %d", got.TotalMessages, total)
	}
	if first, last := got.Messages[0].Content, got.Messages[99].Content; first != "訊息 050" || last != "訊息 149" {
		t.Errorf("回傳的範圍是 %q 到 %q, 期望「訊息 050」到「訊息 149」（最近 100 條、由舊到新）", first, last)
	}
	withCall := got.Messages[120-50]
	if len(withCall.ToolCalls) != 1 || withCall.ToolCalls[0].Name != "http_get" {
		t.Fatalf("第 120 條的 tool_calls = %+v, 期望一筆 http_get", withCall.ToolCalls)
	}
	if plain := got.Messages[0]; plain.ToolCalls == nil || len(plain.ToolCalls) != 0 || plain.ToolCallID != nil {
		t.Errorf("沒有 tool 呼叫的訊息：tool_calls = %v、tool_call_id = %v, 期望 [] 與 null", plain.ToolCalls, plain.ToolCallID)
	}
	for _, canary := range []string{apiKeyCanary, queryCanary} {
		if bytes.Contains(body, []byte(canary)) {
			t.Errorf("GET 回應含有 %q，tool 參數沒有去敏", canary)
		}
		if _, _, messagesJSON, _ := sessionColumns(t, dir, created.SessionID); !strings.Contains(messagesJSON, canary) {
			t.Errorf("sessions 表裡的原文沒有 %q：去敏不該改寫資料庫（歷史要能原樣重放）", canary)
		}
	}
}

// TestServerSessionNotFound 釘住不存在的 Session 回 404 session_not_found，查詢與歸檔都一樣。
func TestServerSessionNotFound(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			resp, body := s.send(t, method, sessionPath("web:ghost:default:1"), "", nil)
			assertErrorShape(t, resp, body, http.StatusNotFound, "session_not_found")
		})
	}
}

// TestServerSessionEndpointsIgnoreCliSessions 釘住 Session 端點只認 Web Service 建立的 Session。
//
// spec #73 第四節的並行設計建立在「chat 與 server 永遠不會碰到同一個 Session，因為兩者的接入
// 來源不同」這個前提上：server 的「進行中」標記只在單一進程內有效。若 Web Service 能依 ID 讀取或歸檔
// CLI 的 Session，這個前提就破了——到了 #79，server 還會對它跑 turn，與 chat 同時寫同一段歷史。
// 所以 CLI 的 Session 在這些端點上一律當成不存在，而且歸檔請求不會動到它。
func TestServerSessionEndpointsIgnoreCliSessions(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	cliSession := core.NewSession(cli.ChannelName, cli.LocalUserID, "default")
	cliSession.Append(core.Message{Role: core.RoleUser, Content: "CLI 的對話"})
	seedHistory(t, dir, *cliSession)
	s := startServer(t, dir)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			resp, body := s.send(t, method, sessionPath(cliSession.ID), "", nil)
			assertErrorShape(t, resp, body, http.StatusNotFound, "session_not_found")
			if bytes.Contains(body, []byte("CLI 的對話")) {
				t.Errorf("回應含有 CLI Session 的內容\nbody: %s", body)
			}
		})
	}
	if _, status, _, _ := sessionColumns(t, dir, cliSession.ID); status != "active" {
		t.Errorf("CLI Session 的 status = %q, 期望仍是 active（Web Service 的歸檔不該動到它）", status)
	}
}

// TestWebSessionIsolatedFromChat 釘住另一個方向（spec #73 使用者故事 27，ticket #78 AC）：Web
// Service 建立的 Session，不會被 `chat --new` 歸檔，也不會被 chat 恢復。
//
// 兩邊的使用者與 Profile 刻意相同（local／default），只有接入來源不同：聯合標識若漏了 channel，
// 這一格就會轉紅。web 那一場先佈置一條帶 canary 的歷史；chat 若恢復了它，送給 Provider 的請求
// 裡就會出現 canary。
func TestWebSessionIsolatedFromChat(t *testing.T) {
	const canary = "web-history-canary-78"
	var providerRequests [][]byte
	provider := newRecordingReplayServer(t, &providerRequests, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, provider.URL)
	s := startServer(t, dir)
	web := createSession(t, s, "default", cli.LocalUserID)
	webSession := core.Session{ID: web.SessionID, Channel: "web", UserID: cli.LocalUserID, ProfileName: "default"}
	webSession.Append(core.Message{Role: core.RoleUser, Content: canary})
	seedHistory(t, dir, webSession)

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "你好", newConversation: true}); err != nil {
		t.Fatalf("chat --new: %v\n輸出:\n%s", err, out.String())
	}

	if _, status, _, _ := sessionColumns(t, dir, web.SessionID); status != "active" {
		t.Errorf("chat --new 之後，Web Session 的 status = %q, 期望仍是 active", status)
	}
	if len(providerRequests) != 1 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 chat 的那 1 個", len(providerRequests))
	}
	if bytes.Contains(providerRequests[0], []byte(canary)) {
		t.Error("chat 送給 Provider 的請求含有 Web Session 的歷史：chat 恢復了 Web 建立的 Session")
	}
}

// TestServerRequestLogRecordsSessionID 釘住請求日誌「有 Session ID 時一併記上」（spec #73 第八節、使用者
// 故事 53）：排查時要能從一個 Session 找出打過它的每一個請求。
//
// 建立那一格最要緊：新的 ID 只出現在回應裡，請求的路徑上沒有，不由 handler 交給日誌就記不到。
// 409 那一格記的是擋下這次建立的既有 Session。
func TestServerRequestLogRecordsSessionID(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")
	if resp, body := postSession(t, s, `{"profile":"default","user_id":"alice"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("第二次建立狀態碼 = %d, 期望 409\nbody: %s", resp.StatusCode, body)
	}
	getSession(t, s, created.SessionID)
	if resp, body := s.send(t, http.MethodDelete, sessionPath(created.SessionID), "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}

	wantSessionID := `"session_id":"` + created.SessionID + `"`
	for _, tt := range []struct{ method, status string }{
		{http.MethodPost, `"status":201`},
		{http.MethodPost, `"status":409`},
		{http.MethodGet, `"status":200`},
		{http.MethodDelete, `"status":200`},
	} {
		if !logHasEntry(t, dir, `"msg":"http_request"`, `"method":"`+tt.method+`"`, tt.status, wantSessionID) {
			t.Errorf("%s（%s）的請求日誌沒有記下 %s:\n%s", tt.method, tt.status, wantSessionID, readWorkspaceLog(t, dir))
		}
	}
}
