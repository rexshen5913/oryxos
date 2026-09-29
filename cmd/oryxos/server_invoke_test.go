// 無狀態呼叫（ticket #80，spec #73 第七節）：POST /api/v1/agents/{name}/invoke 不必先建立 Session。每次
// 呼叫都是一個全新的 Session、跑一個 turn，不寫入 sessions 表，但照常寫審計。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func invokePath(profile string) string { return "/api/v1/agents/" + profile + "/invoke" }

// invokeBody 組出 invoke 的 body；兩個欄位都以 json.Marshal 編碼，含引號與換行也不會壞掉。
func invokeBody(t *testing.T, userID, message string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"user_id": userID, "message": message})
	if err != nil {
		t.Fatalf("編碼 invoke body: %v", err)
	}
	return string(body)
}

// mustInvoke 發一次 invoke，期望 200 並回傳解析好的回應。回應的形狀與發訊息相同（messageReply）。
func mustInvoke(t *testing.T, s *runningServer, profile, userID, message string) messageReply {
	t.Helper()
	resp, body := s.send(t, http.MethodPost, invokePath(profile), invokeBody(t, userID, message), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invoke 狀態碼 = %d, 期望 200\nbody: %.400s", resp.StatusCode, body)
	}
	var got messageReply
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析 invoke 回應: %v\nbody: %s", err, body)
	}
	if got.SessionID == "" || got.Reply == "" {
		t.Fatalf("invoke 回應缺少 session_id 或 reply: %s", body)
	}
	return got
}

// newInvokeRequest 組出一個 invoke 請求，給要在背景發出的測試用。
func newInvokeRequest(t *testing.T, s *runningServer, profile, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.baseURL+invokePath(profile), strings.NewReader(body))
	if err != nil {
		t.Fatalf("建立請求: %v", err)
	}
	return req
}

// countRows 對 Workspace 的 SQLite 跑一條 SELECT COUNT(*) 查詢。審計在背景寫入，查審計表之前要先
// 收掉 server（runServer 返回時佇列已經排空）。
func countRows(t *testing.T, dir, query string, args ...any) int {
	t.Helper()
	db := openWorkspaceDB(t, dir)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉資料庫: %v", err)
		}
	}()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查詢 %q: %v", query, err)
	}
	return n
}

// sessionRowCount 回傳 sessions 表的資料列數。
func sessionRowCount(t *testing.T, dir string) int {
	t.Helper()
	return countRows(t, dir, `SELECT COUNT(*) FROM sessions`)
}

// writeWorkspaceFile 在 Workspace（.oryxos/）底下寫一個檔案。
func writeWorkspaceFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, rel), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %s: %v", rel, err)
	}
}

// TestServerInvokeLeavesNoSessionRow 釘住無狀態呼叫不在 sessions 表留下資料列（spec #73 使用者故事 29，
// ticket #80 AC）。先建一個 Session，讓「列數不變」比的是 1 對 1，不是 0 對 0。
//
// 回應帶回的 ID 查不到 Session（404），但請求日誌記下了它：排查時能拿它對上審計。
func TestServerInvokeLeavesNoSessionRow(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t, readFixture(t, "chat_reply_1.json")).URL)
	s := startServer(t, dir)
	createSession(t, s, "default", "alice")
	before := sessionRowCount(t, dir)

	got := mustInvoke(t, s, "default", "bob", "你好")
	if got.Reply != "回應一：你好，我是 Oryx。" {
		t.Errorf("reply = %q, 期望錄製回應的內容", got.Reply)
	}
	if !strings.HasPrefix(got.SessionID, "web:bob:default:") {
		t.Errorf("session_id = %q, 期望以 web:bob:default: 開頭（接入來源 web、使用者取自請求、Profile 取自路徑）", got.SessionID)
	}
	if resp, body := s.send(t, http.MethodGet, sessionPath(got.SessionID), "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET 無狀態呼叫的 Session = %d, 期望 404\nbody: %s", resp.StatusCode, body)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}

	if after := sessionRowCount(t, dir); after != before {
		t.Errorf("sessions 表 invoke 之前 %d 列、之後 %d 列, 期望不變", before, after)
	}
	if !logHasEntry(t, dir, `"msg":"http_request"`, `"path":"`+invokePath("default")+`"`, `"status":200`, `"session_id":"`+got.SessionID+`"`) {
		t.Errorf("請求日誌沒有記下 invoke 的 Session ID:\n%s", readWorkspaceLog(t, dir))
	}
}

// TestServerInvokeAudit 釘住無狀態呼叫照常寫審計（spec #73 使用者故事 31，ticket #80 AC）：Provider 回放
// 一次 Tool 呼叫，LLM 呼叫與 Tool 呼叫的記錄都記在回應帶回的 session_id 名下。
//
// LLM 呼叫的筆數與回放伺服器**實際收到的請求數**比，不寫死：兩個獨立來源對得上才算數。
func TestServerInvokeAudit(t *testing.T) {
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "web_reply_tool_call_with_secret.json"),
		readFixture(t, "chat_reply_after_mcp_tool.json"))
	dir := setupChatWorkspace(t, provider.URL)
	writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntry(t, "demo", "echo"))
	writeProfile(t, dir, mcpProfile)
	s := startServer(t, dir)

	got := mustInvoke(t, s, "default", "alice", "幫我呼叫外部工具")
	if got.Reply != "外部 MCP 工具已回覆，這是整理後的結果。" {
		t.Errorf("reply = %q, 期望 Tool 呼叫之後的最終回應", got.Reply)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}

	if len(reqs) != 2 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 2（Tool 呼叫前後各一次）", len(reqs))
	}
	if n := countRows(t, dir, `SELECT COUNT(*) FROM llm_calls WHERE session_id = ?`, got.SessionID); n != len(reqs) {
		t.Errorf("llm_calls 記在 %s 名下 %d 筆, 期望與 Provider 收到的請求數 %d 相同", got.SessionID, n, len(reqs))
	}
	// 錄製回應裡恰好一筆 tool_calls。
	if n := countRows(t, dir, `SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`, got.SessionID); n != 1 {
		t.Errorf("tool_invocations 記在 %s 名下 %d 筆, 期望 1", got.SessionID, n)
	}
	if n := sessionRowCount(t, dir); n != 0 {
		t.Errorf("sessions 表有 %d 列, 期望 0", n)
	}
}

// TestServerInvokeConcurrent 釘住同一位使用者對同一份 Profile 並行呼叫（spec #73 使用者故事 30、32，ticket
// #80 AC）：兩個 turn 同時卡在 Provider（沒有被「進行中」標記或唯一索引擋下），放行之後都回 200，
// session_id 不同。
//
// ID 在同一微秒撞上的情形從 HTTP 造不出來，由 core 的 TestNewSessionIDsAreUnique 釘住；這一支守的是
// 兩個呼叫真的能同時進行。
func TestServerInvokeConcurrent(t *testing.T) {
	p := newGatedProvider(t, readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, p.url)
	s := startServer(t, dir)

	first := sendAsync(s.client, newInvokeRequest(t, s, "default", invokeBody(t, "alice", "第一件事")))
	second := sendAsync(s.client, newInvokeRequest(t, s, "default", invokeBody(t, "alice", "第二件事")))
	p.waitArrived(t)
	p.waitArrived(t)
	p.release <- struct{}{}
	p.release <- struct{}{}

	ids := make(map[string]bool)
	for _, result := range []<-chan asyncResult{first, second} {
		res := waitResult(t, result)
		if res.err != nil || res.status != http.StatusOK {
			t.Fatalf("並行的 invoke = %d（%v）, 期望 200\nbody: %s", res.status, res.err, res.body)
		}
		var got messageReply
		if err := json.Unmarshal(res.body, &got); err != nil {
			t.Fatalf("解析 invoke 回應: %v\nbody: %s", err, res.body)
		}
		ids[got.SessionID] = true
	}
	if len(ids) != 2 {
		t.Errorf("兩次並行呼叫的 session_id = %v, 期望兩個不同的 ID", ids)
	}
}

// TestServerInvokeWithActiveSession 釘住無狀態呼叫不碰同一位使用者既有的 Session（ticket #80 AC）：那個
// Session 的歷史不變、仍是 active，送給 Provider 的請求裡也沒有它的歷史。
func TestServerInvokeWithActiveSession(t *testing.T) {
	const sessionMessage = "Session 裡的第一則"
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, provider.URL)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")
	mustReply(t, s, created.SessionID, sessionMessage)
	before := historyJSON(t, dir, created.SessionID)

	mustInvoke(t, s, "default", "alice", "一次性的任務")

	if after := historyJSON(t, dir, created.SessionID); after != before {
		t.Errorf("invoke 之後既有 Session 的歷史變了\n之前: %.300s\n之後: %.300s", before, after)
	}
	if got := getSession(t, s, created.SessionID).Status; got != "active" {
		t.Errorf("既有 Session 的 status = %q, 期望仍是 active", got)
	}
	if len(reqs) != 2 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 2", len(reqs))
	}
	if strings.Contains(string(reqs[1]), sessionMessage) {
		t.Error("invoke 送給 Provider 的請求帶著既有 Session 的歷史：沒有用全新的 Session")
	}
}

// TestServerInvokeUsesSameTurnContext 釘住無狀態呼叫的上下文與 Session 裡的一個 turn 相同（spec #73 使用者
// 故事 33，ticket #80 AC）：長期記憶、Bootstrap 與 Skill 照常載入。
//
// 直接比兩次送給 Provider 的系統提示詞**完全相等**，而不是逐項找片段：少載了哪一層都會不相等，將來多
// 一層也一樣涵蓋得到。片段檢查只用來確認這三層真的有內容可比。
func TestServerInvokeUsesSameTurnContext(t *testing.T) {
	const (
		memoryCanary = "長期記憶-canary-80"
		agentsCanary = "做事方式-canary-80"
		skillCanary  = "摘要技能-canary-80"
	)
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, provider.URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - AGENTS.md\nskills:\n  - digest\n")
	writeWorkspaceFile(t, dir, "AGENTS.md", agentsCanary+"\n")
	writeWorkspaceFile(t, dir, filepath.Join("memory", memoryFile), "- "+memoryCanary+"\n")
	writeSkillFile(t, dir, "digest", "---\nname: digest\ndescription: "+skillCanary+"。需要摘要時使用。\n---\n\n## 步驟\n\n1. 做摘要\n")
	s := startServer(t, dir)

	created := createSession(t, s, "default", "alice")
	mustReply(t, s, created.SessionID, "你好")
	mustInvoke(t, s, "default", "alice", "你好")

	if len(reqs) != 2 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 2", len(reqs))
	}
	sessionPrompt, invokePrompt := systemPrompt(t, reqs, 0), systemPrompt(t, reqs, 1)
	for _, want := range []string{memoryCanary, agentsCanary, skillCanary} {
		if !strings.Contains(sessionPrompt, want) {
			t.Fatalf("Session 那一輪的系統提示詞沒有 %q，這一格沒有東西可比:\n%s", want, sessionPrompt)
		}
	}
	if invokePrompt != sessionPrompt {
		t.Errorf("invoke 的系統提示詞與 Session 裡一個 turn 的不同\nSession:\n%s\ninvoke:\n%s", sessionPrompt, invokePrompt)
	}
}

// TestServerInvokeErrors 是無狀態呼叫的錯誤碼表（spec #73 第八節，ticket #80 AC）：輸入限制與 turn 錯誤
// 分類都與發訊息相同。每一格都另外斷言 sessions 表沒有留下資料列。
//
// 輸入被擋下的幾格用一個不給錄製回應的回放伺服器：請求若走到 Provider，那一格會因為「請求數超出錄製
// 回應數」而失敗。
func TestServerInvokeErrors(t *testing.T) {
	tests := []struct {
		name string
		// provider 回傳這一格的 Provider 端點；nil 代表不該呼叫 Provider。
		provider func(t *testing.T) string
		opts     serverOptions
		// profileBody 非空時覆寫 default Profile 在 name 那行之後的內容。
		profileBody string
		// afterStart 在組裝完成之後、發請求之前調整 Workspace；nil 代表不調整。
		afterStart func(t *testing.T, dir string)
		// profile 是路徑上的 Profile 名。
		profile    string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "Profile 不存在：404", profile: "nope", body: invokeBody(t, "alice", "你好"),
			wantStatus: http.StatusNotFound, wantCode: "profile_not_found"},
		{name: "Profile 不可用：503", profile: "broken", body: invokeBody(t, "alice", "你好"),
			wantStatus: http.StatusServiceUnavailable, wantCode: "profile_unavailable"},
		{name: "缺少 user_id：400", profile: "default", body: `{"message":"你好"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "user_id 含字元集以外的字元：400", profile: "default", body: invokeBody(t, "a/b", "你好"),
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "message 只有空白：400", profile: "default", body: invokeBody(t, "alice", " \n\t "),
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "帶了未知欄位（拼錯的 mesage）：400", profile: "default", body: `{"user_id":"alice","mesage":"你好"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "message 超過 32 KiB：413", profile: "default", body: invokeBody(t, "alice", strings.Repeat("a", 32<<10+1)),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "message_too_large"},
		{name: "body 超過上限：413", profile: "default", body: invokeBody(t, "alice", strings.Repeat("a", 300<<10)),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "request_too_large"},
		{name: "Profile 不存在、user_id 也不合法：先回 400", profile: "nope", body: invokeBody(t, "a/b", "你好"),
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "Provider 故障：503", provider: func(t *testing.T) string { return failingProvider(t) },
			profile: "default", body: invokeBody(t, "alice", "你好"),
			wantStatus: http.StatusServiceUnavailable, wantCode: "provider_error"},
		{name: "turn 逾時：504", provider: func(t *testing.T) string { return slowProvider(t, 3*time.Second) },
			opts: serverOptions{turnTimeout: 150 * time.Millisecond}, profile: "default", body: invokeBody(t, "alice", "你好"),
			wantStatus: http.StatusGatewayTimeout, wantCode: "turn_timeout"},
		{
			// 啟動時的校驗通過了，檔案在那之後才被刪掉：turn 載入 Bootstrap 時才失敗，還沒走到 Provider。
			name:        "跟 Provider 無關的失敗：500",
			profileBody: "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - SOUL.md\n",
			afterStart: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, workspaceDir, "SOUL.md")); err != nil {
					t.Fatalf("刪除 SOUL.md: %v", err)
				}
			},
			profile: "default", body: invokeBody(t, "alice", "你好"),
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providerURL := newReplayServer(t).URL
			if tt.provider != nil {
				providerURL = tt.provider(t)
			}
			dir := setupChatWorkspace(t, providerURL)
			writeBrokenProfile(t, dir, "name: broken\nprovider: [unclosed\n")
			if tt.profileBody != "" {
				writeProfile(t, dir, tt.profileBody)
			}
			s := startServerWithOptions(t, dir, tt.opts)
			if tt.afterStart != nil {
				// 先走完一個請求：startServer 不等組裝，組裝完成之前改動 Workspace 會讓啟動時的校驗失敗。
				if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
					t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
				}
				tt.afterStart(t, dir)
			}

			resp, body := s.send(t, http.MethodPost, invokePath(tt.profile), tt.body, nil)
			assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
			if err := s.stop(); err != nil {
				t.Fatalf("收掉 server: %v", err)
			}
			if n := sessionRowCount(t, dir); n != 0 {
				t.Errorf("sessions 表有 %d 列, 期望 0", n)
			}
		})
	}
}

// TestServerDemoFiveChain 走一遍 Demo 五（spec #73 Testing Decisions，ticket #80 AC）：業務系統依序查系統
// 資訊、Profile 清單、Tool 清單、長期記憶，再做一次無狀態呼叫。每一步都回 200 且欄位齊全。
func TestServerDemoFiveChain(t *testing.T) {
	const memory = "## 2026-09-29\n\n- 使用者喝綠茶，不加糖。\n"
	dir := setupChatWorkspace(t, newReplayServer(t, readFixture(t, "chat_reply_1.json")).URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\ntools:\n  - save_memory\n  - recall_memory\n")
	writeWorkspaceFile(t, dir, filepath.Join("memory", memoryFile), memory)
	s := startServer(t, dir)

	steps := []struct {
		method, path, body string
		wantFields         []string
	}{
		{http.MethodGet, "/api/v1/info", "", []string{"name", "version", "started_at", "profiles", "providers"}},
		{http.MethodGet, "/api/v1/profiles", "", []string{"profiles"}},
		{http.MethodGet, "/api/v1/tools?profile=default", "", []string{"groups"}},
		{http.MethodGet, "/api/v1/memory", "", []string{"content"}},
		{http.MethodPost, invokePath("default"), invokeBody(t, "alice", "你好"), []string{"session_id", "reply"}},
	}
	bodies := make(map[string][]byte, len(steps))
	for _, step := range steps {
		resp, body := s.send(t, step.method, step.path, step.body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s 狀態碼 = %d, 期望 200\nbody: %.400s", step.method, step.path, resp.StatusCode, body)
		}
		fields := decodeJSONObject(t, body)
		for _, want := range step.wantFields {
			if _, ok := fields[want]; !ok {
				t.Errorf("%s %s 的回應缺少 %q\nbody: %.400s", step.method, step.path, want, body)
			}
		}
		bodies[step.path] = body
	}

	var profiles struct {
		Profiles []map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(bodies["/api/v1/profiles"], &profiles); err != nil || len(profiles.Profiles) != 1 {
		t.Fatalf("profiles = %s（%v）, 期望恰好一份", bodies["/api/v1/profiles"], err)
	}
	for _, want := range []string{"name", "description", "agent_name", "provider", "status"} {
		if _, ok := profiles.Profiles[0][want]; !ok {
			t.Errorf("Profile 清單的一筆缺少 %q: %s", want, bodies["/api/v1/profiles"])
		}
	}

	var tools struct {
		Groups []struct {
			Profile string                       `json:"profile"`
			Tools   []map[string]json.RawMessage `json:"tools"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(bodies["/api/v1/tools?profile=default"], &tools); err != nil || len(tools.Groups) != 1 {
		t.Fatalf("tools = %s（%v）, 期望恰好一組", bodies["/api/v1/tools?profile=default"], err)
	}
	if tools.Groups[0].Profile != "default" || len(tools.Groups[0].Tools) != 2 {
		t.Errorf("tools 的那一組 = %+v, 期望 default 的兩個 Tool", tools.Groups[0])
	}
	for _, tool := range tools.Groups[0].Tools {
		for _, want := range []string{"name", "description", "server"} {
			if _, ok := tool[want]; !ok {
				t.Errorf("Tool 缺少 %q: %v", want, tool)
			}
		}
	}

	var memoryResp struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(bodies["/api/v1/memory"], &memoryResp); err != nil || memoryResp.Content != memory {
		t.Errorf("memory 的 content = %q（%v）, 期望 MEMORY.md 的原文", memoryResp.Content, err)
	}
}

// TestServerTurnCanceledDuringLastTool 釘住 turn 在**最後一個 iteration 的 Tool 執行中**被取消時，發訊息
// 與無狀態呼叫的結果一致（Codex gate 第 1 輪）：逾時回 504 turn_timeout；呼叫端斷線不寫回應，請求日誌
// 記 499。
//
// 這條路徑上 ReAct 循環不回錯誤：Tool 被取消時回的是失敗的**結果**，循環把它回填，發現 iteration 已經
// 用完，就以「已達最大迭代次數」正常返回。擋下它的是持久化——SQLite 拒絕已取消的 context，turn 因此
// 失敗。NopSessionStore 若無條件回 nil，無狀態呼叫的這個 turn 就會被當成成功、回 200。
//
// 發訊息的兩格在修正之前就是綠的，是對照：它們證明問題只出在無狀態呼叫，修正之後兩條路行為相同。
func TestServerTurnCanceledDuringLastTool(t *testing.T) {
	const slowToolProfile = "provider:\n  name: openrouter\n  model: m\ntools:\n  - shell\nsettings:\n  max_iterations: 1\n"
	tests := []struct {
		name string
		// invoke 為真時打無狀態呼叫，否則先建立 Session 再發訊息。
		invoke bool
		// disconnect 為真時呼叫端在 Tool 執行中斷線；否則 turn 的期限在 Tool 執行中到期。
		disconnect bool
	}{
		{name: "發訊息：turn 逾時"},
		{name: "發訊息：呼叫端斷線", disconnect: true},
		{name: "無狀態呼叫：turn 逾時", invoke: true},
		{name: "無狀態呼叫：呼叫端斷線", invoke: true, disconnect: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t, readFixture(t, "web_reply_tool_call_slow_shell.json")).URL)
			cfg, err := os.OpenFile(filepath.Join(dir, workspaceDir, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("開啟 config.yaml: %v", err)
			}
			if _, err := cfg.WriteString("shell:\n  allowed_commands: [sleep]\n"); err != nil {
				t.Fatalf("寫入 shell 白名單: %v", err)
			}
			if err := cfg.Close(); err != nil {
				t.Fatalf("關閉 config.yaml: %v", err)
			}
			writeProfile(t, dir, slowToolProfile)
			var opts serverOptions
			if !tt.disconnect {
				opts.turnTimeout = 300 * time.Millisecond
			}
			s := startServerWithOptions(t, dir, opts)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := invokePath("default")
			body := invokeBody(t, "alice", "跑一個很慢的指令")
			if !tt.invoke {
				path = messagePath(createSession(t, s, "default", "alice").SessionID)
				body = messageBody(t, "跑一個很慢的指令")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, strings.NewReader(body))
			if err != nil {
				t.Fatalf("建立請求: %v", err)
			}
			result := sendAsync(s.client, req)

			if !tt.disconnect {
				res := waitResult(t, result)
				if res.err != nil || res.status != http.StatusGatewayTimeout {
					t.Fatalf("Tool 執行中逾時 = %d（%v）, 期望 504\nbody: %s", res.status, res.err, res.body)
				}
				var got struct {
					ErrorCode string `json:"error_code"`
				}
				if err := json.Unmarshal(res.body, &got); err != nil || got.ErrorCode != "turn_timeout" {
					t.Errorf("error_code = %q（%v）, 期望 turn_timeout\nbody: %s", got.ErrorCode, err, res.body)
				}
				return
			}

			time.Sleep(300 * time.Millisecond) // Tool（sleep 5）這時還在跑
			cancel()
			if res := waitResult(t, result); res.err == nil {
				t.Fatalf("斷線的請求拿到了回應 %d，期望 client 端回報錯誤", res.status)
			}
			if err := s.stop(); err != nil {
				t.Fatalf("收掉 server: %v", err)
			}
			if !logHasEntry(t, dir, `"msg":"http_request"`, `"path":"`+path+`"`, `"status":499`) {
				t.Errorf("請求日誌沒有把 Tool 執行中斷線的請求記成 499:\n%s", readWorkspaceLog(t, dir))
			}
			if !logHasEntry(t, dir, `"msg":"turn_client_gone"`) {
				t.Errorf("日誌沒有記下呼叫端斷線:\n%s", readWorkspaceLog(t, dir))
			}
		})
	}
}
