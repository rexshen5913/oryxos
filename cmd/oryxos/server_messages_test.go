// `oryxos server` 對 Session 發訊息：Demo 四、turn 錯誤分類、忙碌保護、跨重啟與優雅關閉，ticket #79。
//
// 與 server_test.go 同一個 seam：runServer 接收呼叫端開好的 listener，測試對它發真實的 HTTP
// 請求。seam 之下全部用真的（SQLite、MCP 起本地 stdio server）；Provider 以回放伺服器代替
// （ADR-0002），需要控制「什麼時候回覆」的格用 gatedProvider。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/channel/cli"
	"github.com/rexshen5913/oryxos/internal/core"
)

// messagePath 是一個 Session 發訊息的路徑。
func messagePath(id string) string { return sessionPath(id) + "/messages" }

// messageBody 把一則訊息編成請求 body。
func messageBody(t *testing.T, message string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatalf("編碼請求 body: %v", err)
	}
	return string(body)
}

// postMessage 對一個 Session 發一則訊息，原樣回傳回應。
func postMessage(t *testing.T, s *runningServer, id, message string) (*http.Response, []byte) {
	t.Helper()
	return s.send(t, http.MethodPost, messagePath(id), messageBody(t, message), nil)
}

// messageReply 是發訊息成功時的回應形狀。
type messageReply struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

// mustReply 發一則訊息，期望 200，回傳解析後的回應。
func mustReply(t *testing.T, s *runningServer, id, message string) messageReply {
	t.Helper()
	resp, body := postMessage(t, s, id, message)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("發訊息狀態碼 = %d, 期望 200\nbody: %.300s", resp.StatusCode, body)
	}
	var reply messageReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("解析發訊息的回應: %v\nbody: %s", err, body)
	}
	if reply.SessionID != id {
		t.Errorf("回應的 session_id = %q, 期望 %q", reply.SessionID, id)
	}
	return reply
}

// asyncResult 是背景發出的請求的結果。
type asyncResult struct {
	status int
	body   []byte
	err    error
}

// sendAsync 在背景發出 req，結果經由回傳的 channel 交回。背景 goroutine 裡不能呼叫 t.Fatal，
// 所以錯誤放進結果，由主測試判斷。
func sendAsync(client *http.Client, req *http.Request) <-chan asyncResult {
	results := make(chan asyncResult, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			results <- asyncResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		results <- asyncResult{status: resp.StatusCode, body: body, err: err}
	}()
	return results
}

// newMessageRequest 組出一個發訊息的請求，ctx 由呼叫端決定（斷線那一格要能取消它）。
func newMessageRequest(t *testing.T, ctx context.Context, s *runningServer, id, message string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+messagePath(id), strings.NewReader(messageBody(t, message)))
	if err != nil {
		t.Fatalf("建立請求: %v", err)
	}
	return req
}

// waitResult 等背景請求的結果，最多等 20 秒。
func waitResult(t *testing.T, results <-chan asyncResult) asyncResult {
	t.Helper()
	select {
	case res := <-results:
		return res
	case <-time.After(20 * time.Second):
		t.Fatal("背景請求 20 秒內沒有結果")
		return asyncResult{}
	}
}

// gatedProvider 是一個由測試控制何時回覆的回放伺服器：每個請求進來先送一個 arrived，再等測試
// 往 release 送值才回覆下一份錄製回應；等待期間請求被取消（turn 被取消），就送一個 canceled。
type gatedProvider struct {
	url      string
	arrived  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func newGatedProvider(t *testing.T, fixtures ...string) *gatedProvider {
	t.Helper()
	p := &gatedProvider{
		arrived:  make(chan struct{}, 16),
		release:  make(chan struct{}, 16),
		canceled: make(chan struct{}, 16),
	}
	var mu sync.Mutex
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		idx := served
		served++
		mu.Unlock()
		p.arrived <- struct{}{}
		select {
		case <-p.release:
		case <-r.Context().Done():
			p.canceled <- struct{}{}
			return
		}
		if idx >= len(fixtures) {
			t.Errorf("LLM 請求數超出錄製回應數 %d", len(fixtures))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[idx]))
	}))
	t.Cleanup(srv.Close)
	// 後註冊先執行：測試若半途失敗，先放掉還卡著的 handler，srv.Close 才不會等下去。
	t.Cleanup(func() { close(p.release) })
	p.url = srv.URL
	return p
}

// waitArrived 等 Provider 收到下一個請求，也就是 turn 已經走到呼叫 LLM 那一步。
func (p *gatedProvider) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-p.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("Provider 10 秒內沒有收到請求")
	}
}

// failingProvider 依序回放 fixtures，用完之後一律回 500，用來製造「Provider 呼叫失敗」。
func failingProvider(t *testing.T, fixtures ...string) string {
	t.Helper()
	var mu sync.Mutex
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		idx := served
		served++
		mu.Unlock()
		if idx >= len(fixtures) {
			http.Error(w, `{"error":{"message":"upstream boom"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[idx]))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// slowProvider 過了 delay 才回 500；請求在那之前被取消就直接返回。
//
// 先讀完 body：net/http 要等 handler 讀完 body，才開始在背景偵測對方斷線（startBackgroundRead）。
// 不讀的話，r.Context() 不會隨著 client 取消而結束，handler 會一直等到 delay。
func slowProvider(t *testing.T, delay time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
			http.Error(w, `{"error":{"message":"too late"}}`, http.StatusInternalServerError)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// historyJSON 是 sessions 表裡一個 Session 的 messages_json 原文。
func historyJSON(t *testing.T, dir, id string) string {
	t.Helper()
	_, _, messagesJSON, found := sessionColumns(t, dir, id)
	if !found {
		t.Fatalf("sessions 表裡沒有 %s", id)
	}
	return messagesJSON
}

// systemPrompt 取第 n 個 LLM 請求裡的 system 訊息內容。
func systemPrompt(t *testing.T, reqs [][]byte, n int) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	decodeLLMRequest(t, reqs, n, &req)
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatalf("第 %d 個 LLM 請求沒有 system 訊息", n)
	return ""
}

// mcpProfile 是一份引用 demo MCP server、只開 demo__echo 的 default Profile（name 那行之後）。
const mcpProfile = "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - demo\ntools:\n  - demo__echo\n"

// TestServerMessageDemoFourChain 是 Demo 四的整條鏈路（spec #73 Testing Decisions，ticket #79 AC）：
// 建立 → 發訊息（中間一次 Tool 呼叫）→ 查歷史 → 歸檔 → 再發訊息得到 409 session_archived。
//
// Tool 呼叫的參數帶一個憑證形狀的值：歷史去敏要在**真的跑過一個 turn** 之後仍然成立——回應中
// 被遮蔽，sessions 表中的原文不動（對話歷史要能原樣重放給 Provider）。
func TestServerMessageDemoFourChain(t *testing.T) {
	const canary = "sk-web-history-canary-79"
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "web_reply_tool_call_with_secret.json"),
		readFixture(t, "chat_reply_after_mcp_tool.json"))
	dir := setupChatWorkspace(t, provider.URL)
	writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntry(t, "demo", "echo"))
	writeProfile(t, dir, mcpProfile)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	reply := mustReply(t, s, created.SessionID, "幫我呼叫外部工具")
	if reply.Reply != "外部 MCP 工具已回覆，這是整理後的結果。" {
		t.Errorf("reply = %q, 期望最終回應", reply.Reply)
	}

	history := getSession(t, s, created.SessionID)
	var roles []string
	for _, m := range history.Messages {
		roles = append(roles, m.Role)
	}
	if want := []string{"user", "assistant", "tool", "assistant"}; !slices.Equal(roles, want) {
		t.Fatalf("歷史的角色順序 = %v, 期望 %v", roles, want)
	}
	if calls := history.Messages[1].ToolCalls; len(calls) != 1 || calls[0].Name != "demo__echo" {
		t.Errorf("第二條的 tool_calls = %+v, 期望一筆 demo__echo", calls)
	} else if strings.Contains(calls[0].Arguments, canary) {
		t.Errorf("GET 回應的 Tool 參數含有 %q，沒有去敏", canary)
	}
	if id := history.Messages[2].ToolCallID; id == nil || *id != "call_web_secret_1" {
		t.Errorf("tool 訊息的 tool_call_id = %v, 期望 call_web_secret_1", id)
	}
	if !strings.Contains(historyJSON(t, dir, created.SessionID), canary) {
		t.Errorf("sessions 表裡的原文沒有 %q：去敏不該改寫資料庫", canary)
	}

	if resp, body := s.send(t, http.MethodDelete, sessionPath(created.SessionID), "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	resp, body := postMessage(t, s, created.SessionID, "還在嗎")
	assertErrorShape(t, resp, body, http.StatusConflict, "session_archived")
	if len(reqs) != 2 {
		t.Errorf("Provider 收到 %d 個請求, 期望 2（對已歸檔的 Session 發訊息不該呼叫 Provider）", len(reqs))
	}
}

// TestServerMessageAcrossRestart 釘住跨重啟延續（spec #73 使用者故事 17）：第一個 server 發過一則
// 訊息之後關閉，第二個 server 對同一個 ID 發訊息，送給 Provider 的請求帶著上一個 turn 的歷史。
func TestServerMessageAcrossRestart(t *testing.T) {
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, provider.URL)

	first := startServer(t, dir)
	created := createSession(t, first, "default", "alice")
	mustReply(t, first, created.SessionID, "第一句：我叫小明")
	if err := first.stop(); err != nil {
		t.Fatalf("收掉第一個 server: %v", err)
	}

	second := startServer(t, dir)
	if got := mustReply(t, second, created.SessionID, "第二句：我叫什麼？"); got.Reply != "回應二：我記得你剛才說的話。" {
		t.Errorf("第二個 server 的 reply = %q", got.Reply)
	}
	if len(reqs) != 2 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 2", len(reqs))
	}
	for _, want := range []string{"第一句：我叫小明", "回應一：你好，我是 Oryx。"} {
		if !strings.Contains(string(reqs[1]), want) {
			t.Errorf("第二個 server 送給 Provider 的請求沒有上一個 turn 的 %q", want)
		}
	}
}

// TestServerMessageMultipleProfiles 釘住多 Profile 並存（spec #73 Testing Decisions）：兩份 Profile
// 各接一個回放伺服器，各自對自己的 Session 發訊息——請求打到各自的 Provider，系統提示詞各不相同。
func TestServerMessageMultipleProfiles(t *testing.T) {
	var alphaReqs, betaReqs [][]byte
	alpha := newRecordingReplayServer(t, &alphaReqs, readFixture(t, "chat_reply_1.json"))
	beta := newRecordingReplayServer(t, &betaReqs, readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, alpha.URL)
	writeWorkspaceConfig(t, dir, alpha.URL,
		"  beta:\n    api_key: literal-key\n    base_url: "+beta.URL+"\nhttp:\n  allowed_domains: []\n")
	writeProfile(t, dir, "identity:\n  agent_name: Alpha\n  prompt: 你是 Alpha 客服助理。\n"+
		"provider:\n  name: openrouter\n  model: m\n")
	writeNamedProfile(t, dir, "analyst", "identity:\n  agent_name: Beta\n  prompt: 你是 Beta 工單分析師。\n"+
		"provider:\n  name: beta\n  model: m\n")
	s := startServer(t, dir)

	alphaSession := createSession(t, s, "default", "alice")
	betaSession := createSession(t, s, "analyst", "alice")
	if got := mustReply(t, s, alphaSession.SessionID, "你好").Reply; got != "回應一：你好，我是 Oryx。" {
		t.Errorf("default 的 reply = %q, 期望來自它自己的 Provider", got)
	}
	if got := mustReply(t, s, betaSession.SessionID, "你好").Reply; got != "回應二：我記得你剛才說的話。" {
		t.Errorf("analyst 的 reply = %q, 期望來自它自己的 Provider", got)
	}
	if len(alphaReqs) != 1 || len(betaReqs) != 1 {
		t.Fatalf("兩個 Provider 各收到 %d、%d 個請求, 期望各 1", len(alphaReqs), len(betaReqs))
	}
	if p := systemPrompt(t, alphaReqs, 0); !strings.Contains(p, "Alpha 客服助理") || strings.Contains(p, "Beta 工單分析師") {
		t.Errorf("default 的系統提示詞不對:\n%s", p)
	}
	if p := systemPrompt(t, betaReqs, 0); !strings.Contains(p, "Beta 工單分析師") || strings.Contains(p, "Alpha 客服助理") {
		t.Errorf("analyst 的系統提示詞不對:\n%s", p)
	}
}

// TestServerMessageTurnOutcomes 是 turn 結果的分類表（spec #73 第八節，ticket #79 AC）：
//
//  1. turn 逾時 → 504 turn_timeout（判斷依據是 turn 的 context 狀態）
//  2. Provider 呼叫失敗 → 503 provider_error
//  3. 其他失敗 → 500 internal_error
//  4. 已執行過 Tool 之後才失敗 → 錯誤訊息保留「外部效果不會撤銷」的註記
//  5. 兩條提前結束的路徑回傳的是文字，照常 200：達到最大 iteration 數而強制終止，以及 Provider
//     連續回傳空回應而放棄
//
// 失敗的每一格都另外斷言 sessions 表裡的歷史沒有改變（turn 以整輪為單位 rollback），以及錯誤日誌
// 有一筆帶著分類的 turn_failed。
func TestServerMessageTurnOutcomes(t *testing.T) {
	tests := []struct {
		name string
		// provider 回傳這一格的 Provider 端點。
		provider func(t *testing.T) string
		// profile 是 default Profile 在 name 那行之後的內容。
		profile string
		// withMcp 為真時宣告 demo MCP server。
		withMcp bool
		opts    serverOptions
		// afterCreate 在 Session 建好之後、發訊息之前調整 Workspace；nil 代表不調整。
		afterCreate func(t *testing.T, dir string)
		wantStatus  int
		// wantCode 空字串代表期望 200。
		wantCode string
		// wantText 是錯誤訊息或 reply 裡必須出現的片段。
		wantText string
		// wantAbsent 非空時，回應與整份日誌裡都不能出現它（錯誤文字去敏）。
		wantAbsent string
	}{
		{
			name:       "turn 逾時：504 turn_timeout",
			provider:   func(t *testing.T) string { return slowProvider(t, 3*time.Second) },
			profile:    "provider:\n  name: openrouter\n  model: m\n",
			opts:       serverOptions{turnTimeout: 150 * time.Millisecond},
			wantStatus: http.StatusGatewayTimeout, wantCode: "turn_timeout",
		},
		{
			name:       "Provider 回 5xx：503 provider_error",
			provider:   func(t *testing.T) string { return failingProvider(t) },
			profile:    "provider:\n  name: openrouter\n  model: m\n",
			wantStatus: http.StatusServiceUnavailable, wantCode: "provider_error", wantText: "Provider openrouter 呼叫失敗",
		},
		{
			// 啟動時的校驗通過了，檔案在那之後才被刪掉：每個 turn 重讀 Bootstrap 時才失敗，
			// 還沒走到呼叫 LLM。
			name:     "跟 Provider 無關的失敗：500 internal_error",
			provider: func(t *testing.T) string { return newReplayServer(t).URL },
			profile:  "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - SOUL.md\n",
			afterCreate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, workspaceDir, "SOUL.md")); err != nil {
					t.Fatalf("刪除 SOUL.md: %v", err)
				}
			},
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error", wantText: "SOUL.md",
		},
		{
			name: "執行過 Tool 之後才失敗：保留外部效果不會撤銷的註記",
			provider: func(t *testing.T) string {
				return failingProvider(t, readFixture(t, "web_reply_tool_call_with_secret.json"))
			},
			profile: mcpProfile, withMcp: true,
			wantStatus: http.StatusServiceUnavailable, wantCode: "provider_error", wantText: "外部效果不會因回退而撤銷",
		},
		{
			// base_url 把 key 放在 query 上（有些 Provider 這樣收），而那個位址連不上：Go 的連線錯誤會把
			// 完整網址寫進訊息，key 跟著出來。它得在回應與日誌裡都被遮掉。
			name: "錯誤訊息裡帶著網址的 query：去敏",
			provider: func(t *testing.T) string {
				closed := httptest.NewServer(http.NotFoundHandler())
				closed.Close()
				return closed.URL + "/v1?api_key=turn-error-canary-79"
			},
			profile:    "provider:\n  name: openrouter\n  model: m\n",
			wantStatus: http.StatusServiceUnavailable, wantCode: "provider_error", wantText: "?[REDACTED]",
			wantAbsent: "turn-error-canary-79",
		},
		{
			name: "達到最大 iteration 數：200，reply 是終止說明",
			provider: func(t *testing.T) string {
				return newReplayServer(t, readFixture(t, "web_reply_tool_call_with_secret.json")).URL
			},
			profile: mcpProfile + "settings:\n  max_iterations: 1\n", withMcp: true,
			wantStatus: http.StatusOK, wantText: "已達最大迭代次數 1",
		},
		{
			name: "Provider 連續回空回應而放棄：200，reply 是停止說明",
			provider: func(t *testing.T) string {
				empty := readFixture(t, "web_reply_empty.json")
				return newReplayServer(t, empty, empty, empty).URL
			},
			profile:    "provider:\n  name: openrouter\n  model: m\n",
			wantStatus: http.StatusOK, wantText: "Provider 連續 3 次回傳空回應",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, tt.provider(t))
			if tt.withMcp {
				writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntry(t, "demo", "echo"))
			}
			writeProfile(t, dir, tt.profile)
			s := startServerWithOptions(t, dir, tt.opts)
			created := createSession(t, s, "default", "alice")
			if tt.afterCreate != nil {
				tt.afterCreate(t, dir)
			}

			resp, body := postMessage(t, s, created.SessionID, "開始吧")
			if tt.wantCode == "" {
				if resp.StatusCode != tt.wantStatus {
					t.Fatalf("狀態碼 = %d, 期望 %d\nbody: %.300s", resp.StatusCode, tt.wantStatus, body)
				}
			} else {
				assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
				if got := historyJSON(t, dir, created.SessionID); got != "[]" {
					t.Errorf("turn 失敗之後 sessions 表的歷史 = %.200s, 期望維持 []（整輪 rollback）", got)
				}
				// 回給呼叫端的是一句訊息；運維人員查的是日誌，原因與分類要在那裡也查得到。
				if !logHasEntry(t, dir, `"msg":"turn_failed"`, `"error_code":"`+tt.wantCode+`"`) {
					t.Errorf("錯誤日誌沒有 %s 的 turn_failed:\n%s", tt.wantCode, readWorkspaceLog(t, dir))
				}
			}
			if !strings.Contains(string(body), tt.wantText) {
				t.Errorf("回應沒有含 %q\nbody: %.400s", tt.wantText, body)
			}
			if tt.wantAbsent != "" {
				if strings.Contains(string(body), tt.wantAbsent) {
					t.Errorf("回應含有 %q，錯誤文字沒有去敏\nbody: %.400s", tt.wantAbsent, body)
				}
				// 整份日誌都不能有：turn_failed 之外，Provider 那一行 llm_call 也記著同一個錯誤。
				for line := range strings.SplitSeq(readWorkspaceLog(t, dir), "\n") {
					if strings.Contains(line, tt.wantAbsent) {
						t.Errorf("日誌含有 %q，錯誤文字沒有去敏:\n%s", tt.wantAbsent, line)
					}
				}
			}
		})
	}
}

// TestServerMessageInputLimits 是 message 的輸入表格（spec #73 第八節，ticket #79 AC）：空的或只有
// 空白回 400；以位元組計的上限 32 KiB，恰好 32 KiB 通過、多 1 位元組回 413 message_too_large。
//
// 最後一格把 32 KiB 的中文以 \uXXXX 跳脫送出（Python json.dumps 的預設），body 膨脹成兩倍：
// #78 訂的 body 上限必須裝得下它，message 本身仍以解碼後的位元組計。
func TestServerMessageInputLimits(t *testing.T) {
	var reqs [][]byte
	provider := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, provider.URL)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	chinese := strings.Repeat("中", (32<<10)/3) // 10922 字 × 3 bytes = 32766 bytes
	var escaped strings.Builder
	for _, r := range chinese {
		fmt.Fprintf(&escaped, `\u%04x`, r)
	}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		// wantCode 空字串代表期望 200。
		wantCode string
	}{
		{name: "message 是空字串：400", body: `{"message":""}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "message 只有空白：400", body: `{"message":"  \n\t "}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "缺少 message：400", body: `{}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "恰好 32 KiB：通過", body: messageBody(t, strings.Repeat("a", 32<<10)), wantStatus: http.StatusOK},
		{name: "32 KiB 多 1 位元組：413", body: messageBody(t, strings.Repeat("a", 32<<10+1)),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "message_too_large"},
		{name: "32 KiB 的中文以 \\u 跳脫送出：通過", body: `{"message":"` + escaped.String() + `"}`, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := s.send(t, http.MethodPost, messagePath(created.SessionID), tt.body, nil)
			if tt.wantCode != "" {
				assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
				if !strings.Contains(string(body), "message") {
					t.Errorf("錯誤回應沒有指出 message\nbody: %s", body)
				}
				return
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("狀態碼 = %d, 期望 %d\nbody: %.300s", resp.StatusCode, tt.wantStatus, body)
			}
		})
	}
	// 壞掉的 JSON 另外送：它的錯誤訊息講的是 body，不一定提到 message，不適用上面那條斷言。
	if resp, body := s.send(t, http.MethodPost, messagePath(created.SessionID), `{"message":`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("壞掉的 JSON 狀態碼 = %d, 期望 400\nbody: %s", resp.StatusCode, body)
	}
	if len(reqs) != 2 {
		t.Fatalf("Provider 收到 %d 個請求, 期望 2（只有兩格通過）", len(reqs))
	}
	if !strings.Contains(string(reqs[0]), strings.Repeat("a", 32<<10)) {
		t.Error("恰好 32 KiB 的 message 沒有完整送到 Provider")
	}

	// 輸入被擋下的請求，請求日誌也要記下路徑上的 Session ID（spec #73 第八節「有 Session ID 時一併
	// 記上」，Codex gate 第 1 輪）：依 session_id 追查時，失敗的請求不能漏掉。逐行檢查，因為只看
	// 「有一行 400 帶著它」的話，三種 400 裡有一種記到就過了。
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	pathField := `"path":"` + messagePath(created.SessionID) + `"`
	wantSessionID := `"session_id":"` + created.SessionID + `"`
	var logged int
	for line := range strings.SplitSeq(readWorkspaceLog(t, dir), "\n") {
		if !strings.Contains(line, `"msg":"http_request"`) || !strings.Contains(line, pathField) {
			continue
		}
		logged++
		if !strings.Contains(line, wantSessionID) {
			t.Errorf("請求日誌沒有記下 Session ID:\n%s", line)
		}
	}
	if want := len(tests) + 1; logged != want {
		t.Errorf("%s 的請求日誌有 %d 行, 期望 %d 行（每個請求一行）", messagePath(created.SessionID), logged, want)
	}
}

// TestServerMessageSessionState 釘住發訊息之前的 Session 檢查（ticket #79 AC）：不存在與 CLI 的
// Session 回 404；Session 所屬的 Profile 在這次啟動中不可用或不存在，回 503 profile_unavailable 並
// 指名是哪份 Profile（spec #73 使用者故事 26）。
func TestServerMessageSessionState(t *testing.T) {
	t.Run("不存在的 Session：404", func(t *testing.T) {
		s := startServer(t, setupChatWorkspace(t, newReplayServer(t).URL))
		resp, body := postMessage(t, s, "web:ghost:default:1", "你好")
		assertErrorShape(t, resp, body, http.StatusNotFound, "session_not_found")
	})

	t.Run("CLI 的 Session：404", func(t *testing.T) {
		dir := setupChatWorkspace(t, newReplayServer(t).URL)
		cliSession := core.NewSession(cli.ChannelName, cli.LocalUserID, "default")
		seedHistory(t, dir, *cliSession)
		s := startServer(t, dir)
		resp, body := postMessage(t, s, cliSession.ID, "你好")
		assertErrorShape(t, resp, body, http.StatusNotFound, "session_not_found")
	})

	for _, tt := range []struct {
		name         string
		breakProfile func(t *testing.T, dir string)
	}{
		{name: "所屬 Profile 在這次啟動中不可用：503", breakProfile: func(t *testing.T, dir string) {
			writeProfile(t, dir, "provider: [unclosed\n")
		}},
		{name: "所屬 Profile 在這次啟動中不存在：503", breakProfile: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, workspaceDir, "profiles", "default.yaml")); err != nil {
				t.Fatalf("刪除 default.yaml: %v", err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			// 另一份 Profile 保持可用，server 才起得來（可用數為 0 時不啟動）。
			writeNamedProfile(t, dir, "spare", "provider:\n  name: openrouter\n  model: m\n")
			first := startServer(t, dir)
			created := createSession(t, first, "default", "alice")
			if err := first.stop(); err != nil {
				t.Fatalf("收掉第一個 server: %v", err)
			}
			tt.breakProfile(t, dir)
			second := startServer(t, dir)
			resp, body := postMessage(t, second, created.SessionID, "你好")
			assertErrorShape(t, resp, body, http.StatusServiceUnavailable, "profile_unavailable")
			if !strings.Contains(string(body), "default") {
				t.Errorf("錯誤回應沒有指名是哪份 Profile\nbody: %s", body)
			}
		})
	}
}

// TestServerMessageBusy 釘住「同一個 Session 同時只跑一個 turn」（spec #73 第四節，ticket #79 AC）：
// turn 進行中，對同一個 Session 再發訊息與 DELETE 都**立即**回 409 session_busy，不排隊；turn 結束
// 之後標記移除，可以正常發下一則。
//
// 不排隊的理由：排隊的請求會悄悄吃掉自己的逾時預算，而呼叫端看不出它在等什麼。
func TestServerMessageBusy(t *testing.T) {
	p := newGatedProvider(t, readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, p.url)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	first := sendAsync(s.client, newMessageRequest(t, context.Background(), s, created.SessionID, "第一則"))
	p.waitArrived(t)

	for _, tt := range []struct{ name, method, path, body string }{
		{"再發一則訊息", http.MethodPost, messagePath(created.SessionID), messageBody(t, "第二則")},
		{"歸檔", http.MethodDelete, sessionPath(created.SessionID), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started := time.Now()
			resp, body := s.send(t, tt.method, tt.path, tt.body, nil)
			assertErrorShape(t, resp, body, http.StatusConflict, "session_busy")
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("session_busy 花了 %v 才回來，期望立即回應（不排隊）", elapsed)
			}
		})
	}

	p.release <- struct{}{}
	if res := waitResult(t, first); res.err != nil || res.status != http.StatusOK {
		t.Fatalf("第一則的結果 = %d（%v）, 期望 200\nbody: %s", res.status, res.err, res.body)
	}
	p.release <- struct{}{} // 下一則不必等
	if got := mustReply(t, s, created.SessionID, "第三則").Reply; got != "回應二：我記得你剛才說的話。" {
		t.Errorf("turn 結束之後的下一則 reply = %q", got)
	}
}

// TestServerMessageClientDisconnect 釘住呼叫端斷線（spec #73 使用者故事 51，ticket #79 AC）：turn 進行中
// 呼叫端斷線，turn 被取消（Provider 那一端看到請求被取消），sessions 表的歷史沒有改變，不寫回應
// 只落日誌；「進行中」標記也隨之移除，之後照常能發訊息。
func TestServerMessageClientDisconnect(t *testing.T) {
	p := newGatedProvider(t, readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, p.url)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	ctx, cancel := context.WithCancel(context.Background())
	result := sendAsync(s.client, newMessageRequest(t, ctx, s, created.SessionID, "講到一半就走"))
	p.waitArrived(t)
	cancel()
	if res := waitResult(t, result); res.err == nil {
		t.Fatalf("取消的請求拿到了回應 %d，期望 client 端回報錯誤", res.status)
	}
	select {
	case <-p.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("呼叫端斷線之後，送給 Provider 的請求沒有被取消：turn 還在跑")
	}

	// 下一則：要等 server 那一側收完尾（落日誌、移除標記），所以 session_busy 時稍候重試。
	p.release <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, body := postMessage(t, s, created.SessionID, "我回來了")
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("斷線之後的下一則 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	if !logHasEntry(t, dir, `"msg":"turn_client_gone"`, `"session_id":"`+created.SessionID+`"`) {
		t.Errorf("日誌沒有記下呼叫端斷線:\n%s", readWorkspaceLog(t, dir))
	}
	// 請求日誌不能把它記成 200（Spec 審查）：沒寫回應時 statusRecorder 停在初始值 200，被取消、已經
	// rollback 的 turn 在日誌裡看起來就像成功了。
	if !logHasEntry(t, dir, `"msg":"http_request"`, `"method":"POST"`, `"status":499`, `"session_id":"`+created.SessionID+`"`) {
		t.Errorf("請求日誌沒有把斷線的那一則記成 499:\n%s", readWorkspaceLog(t, dir))
	}
	var history []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(historyJSON(t, dir, created.SessionID)), &history); err != nil {
		t.Fatalf("解析歷史: %v", err)
	}
	for _, m := range history {
		if m.Content == "講到一半就走" {
			t.Error("斷線那一則留在了歷史裡：被取消的 turn 沒有 rollback")
		}
	}
}

// TestServerMessageGracefulShutdown 釘住優雅關閉時進行中的 turn（spec #73 使用者故事 9 與 Testing
// Decisions，ticket #79 AC）：turn 進行中取消 context，Shutdown 等它跑完，這個請求仍然拿到 200 與完整
// 回應；runServer 返回時，這個 turn 的審計記錄已經寫進 llm_calls。
//
// #75 的優雅關閉測試造不出 handler 正在跑的請求，也看不到審計（它的端點都不產生審計記錄）；有了 turn，
// 這兩條才驗得到。新連線被拒絕、goroutine 回到基線，由 #75 那支測試守著。
//
// 「關閉一刻都不等」由中間那段抓到：取消之後睡 200ms，runServer 不可以已經返回。這是負向的斷言，
// 在很慢的機器上可能看不到過早的返回，但不會誤報。
func TestServerMessageGracefulShutdown(t *testing.T) {
	p := newGatedProvider(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, p.url)
	s := startServer(t, dir)
	created := createSession(t, s, "default", "alice")

	result := sendAsync(s.client, newMessageRequest(t, context.Background(), s, created.SessionID, "你好"))
	p.waitArrived(t)
	s.cancel() // 觸發優雅關閉：turn 還卡在 Provider
	time.Sleep(200 * time.Millisecond)
	select {
	case <-s.finished:
		t.Fatal("turn 還在進行，runServer 卻已經返回")
	default:
	}
	p.release <- struct{}{}

	res := waitResult(t, result)
	if res.err != nil || res.status != http.StatusOK {
		t.Fatalf("關閉期間完成的 turn = %d（%v）, 期望 200\nbody: %s", res.status, res.err, res.body)
	}
	if !strings.Contains(string(res.body), "回應一：你好，我是 Oryx。") {
		t.Errorf("回應不完整: %s", res.body)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}

	db := openWorkspaceDB(t, dir)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉資料庫: %v", err)
		}
	}()
	var calls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_calls WHERE session_id = ?`, created.SessionID).Scan(&calls); err != nil {
		t.Fatalf("查詢 llm_calls: %v", err)
	}
	if calls != 1 {
		t.Errorf("runServer 返回之後 llm_calls 有 %d 筆, 期望 1 筆：關閉期間完成的 turn，審計要在關掉 SQLite 之前寫完", calls)
	}
}

// TestServerTurnTimeoutFlag 釘住 --turn-timeout 旗標（spec #73 第二節）：預設 60 秒；0 或負值在
// 綁定位址之前就被拒絕。
func TestServerTurnTimeoutFlag(t *testing.T) {
	t.Run("--help 列出 --turn-timeout 與預設值", func(t *testing.T) {
		root := newRootCmd()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"server", "--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("server --help: %v", err)
		}
		if !strings.Contains(out.String(), "--turn-timeout") || !strings.Contains(out.String(), "1m0s") {
			t.Errorf("server --help 沒有列出 --turn-timeout 與預設值 1m0s:\n%s", out.String())
		}
	})
	for _, value := range []string{"0s", "-5s"} {
		t.Run("--turn-timeout "+value+" 被拒絕", func(t *testing.T) {
			root := newRootCmd()
			var out strings.Builder
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"server", "--addr", "127.0.0.1:0", "--turn-timeout", value})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := root.ExecuteContext(ctx)
			if err == nil || !strings.Contains(err.Error(), "--turn-timeout") {
				t.Errorf("--turn-timeout %s 的錯誤 = %v, 期望指出 --turn-timeout", value, err)
			}
		})
	}
}

// TestServerDeadlinesFollowTurnTimeout 釘住兩個跟著 turn 上限走的期限（spec #73 第二節，Spec 審查）：
//
//   - **寫入期限大於「讀取期限＋turn 上限」**：寫入期限從讀完標頭就開始計時，turn 的計時卻要等 body
//     讀完才開始，而 body 最晚在讀取期限到時讀完。只大於 turn 上限的話，body 傳得慢的請求 turn 一
//     逾時，504 還沒寫出去，連線就先被切斷了。
//   - **優雅關閉的等待也大於「讀取期限＋turn 上限」**：按下 Ctrl+C 時還在讀 body 的請求，turn 還沒開
//     始，要等它讀完、跑完。
//
// 兩者都從連線上看不出來（預設要等一分多鐘），所以直接看算出來的值。
func TestServerDeadlinesFollowTurnTimeout(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts serverOptions
		// handler 是一個請求的 handler 最多會跑多久：讀 body 加上 turn。
		handler time.Duration
	}{
		{name: "預設值", opts: serverOptions{}, handler: defaultReadTimeout + defaultTurnTimeout},
		{name: "調長的 turn 上限", opts: serverOptions{turnTimeout: 5 * time.Minute}, handler: defaultReadTimeout + 5*time.Minute},
		{name: "調長的讀取期限", opts: serverOptions{readTimeout: 2 * time.Minute}, handler: 2*time.Minute + defaultTurnTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := newHTTPServer(http.NotFoundHandler(), tt.opts).WriteTimeout; got <= tt.handler {
				t.Errorf("WriteTimeout = %v，不大於讀取期限＋turn 上限 %v", got, tt.handler)
			}
			if got := shutdownWait(tt.opts); got <= tt.handler {
				t.Errorf("優雅關閉的等待上限 = %v，不大於讀取期限＋turn 上限 %v", got, tt.handler)
			}
		})
	}
}
