// `oryxos server` 的測試，ticket #75。
//
// 全部經 runServer 這個 seam 驅動（spec #73 Testing Decisions「主 seam」）：測試在本機隨機埠
// 開 listener 交給它，對那個位址發真實的 HTTP 請求；取消 context 就觸發優雅關閉。seam 之下
// 全部用真的——Workspace 檔案、暫存目錄裡的 SQLite、MCP 起真實的本地 stdio server。Provider
// 是唯一的例外，以回放伺服器代替（ADR-0002）。
//
// internal/web 沒有自己的測試：它的外部行為（狀態碼、回應 JSON、標頭、日誌）全部從這一層
// 看得到，spec 也定案不為了測試在那一層另立介面。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runningServer 是一個跑在背景 goroutine 裡的 runServer。
type runningServer struct {
	addr    string
	baseURL string
	client  *http.Client
	cancel  context.CancelFunc
	// finished 在 runServer 返回時關閉，之後才能讀 runErr。用關閉 channel 而不是送值，
	// 「返回了沒」才能被查很多次而不把結果吃掉。
	finished chan struct{}
	runErr   error
	// out 只在 runServer 返回之後讀：它在自己的 goroutine 裡寫，提早讀就是資料競爭。
	out *bytes.Buffer

	stopOnce sync.Once
	stopErr  error
}

// startServer 用零值選項（也就是預設的讀取期限）起一台 server，見 startServerWithOptions。
func startServer(t *testing.T, dir string) *runningServer {
	t.Helper()
	return startServerWithOptions(t, dir, serverOptions{})
}

// startServerWithOptions 在本機隨機埠開 listener，交給 runServer 在背景跑，測試結束時一定收掉。
//
// **不必輪詢「server 起來了沒」**：listener 在呼叫 runServer 之前就綁好了，組裝還沒完成時
// 連進來的請求會在 kernel 的 backlog 裡等到 Serve 開始接手。
func startServerWithOptions(t *testing.T, dir string, opts serverOptions) *runningServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("開 listener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &runningServer{
		addr:     listener.Addr().String(),
		baseURL:  "http://" + listener.Addr().String(),
		client:   &http.Client{Timeout: 15 * time.Second},
		cancel:   cancel,
		finished: make(chan struct{}),
		out:      &bytes.Buffer{},
	}
	go func() {
		s.runErr = runServer(ctx, s.out, dir, opts, listener)
		close(s.finished)
	}()
	t.Cleanup(func() {
		if err := s.stop(); err != nil {
			t.Errorf("收掉 server: %v\n輸出:\n%s", err, s.out.String())
		}
	})
	return s
}

// stop 取消 context 觸發優雅關閉，等 runServer 返回並回傳它的錯誤。重複呼叫回傳同一個結果。
func (s *runningServer) stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		select {
		case <-s.finished:
			s.stopErr = s.runErr
		case <-time.After(20 * time.Second):
			s.stopErr = errors.New("取消 context 之後 20 秒 runServer 仍未返回")
		}
		// 在 server 關閉之後才收：這時連線已經被 server 端關掉，client 端的讀寫 goroutine
		// 看到 EOF 就會退場，不會被算成 server 的殘留。
		s.client.CloseIdleConnections()
	})
	return s.stopErr
}

// send 發一個請求，回傳回應與完整 body。
func (s *runningServer) send(t *testing.T, method, path, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("建立請求 %s %s: %v", method, path, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("關閉回應 body: %v", err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("讀取 %s %s 的回應: %v", method, path, err)
	}
	return resp, data
}

// decodeJSONObject 把回應 body 解成欄位名 → 原始值，供「欄位齊不齊」的斷言使用：解進結構體
// 的話，缺欄位與零值分不出來。
func decodeJSONObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("回應不是 JSON 物件: %v\nbody: %s", err, body)
	}
	return fields
}

// TestServerHealthDoesNotCallProvider 釘住「健康檢查不呼叫 Provider」（spec 使用者故事 41）：
// 負載平衡器每幾秒探測一次，每次探測都打 Provider 的話就是每幾秒一筆費用。
//
// 回放伺服器一份錄製回應都不給，收到任何請求本身就會讓測試失敗；結束後再數一次請求數，
// 兩道確認同一件事。
func TestServerHealthDoesNotCallProvider(t *testing.T) {
	var providerRequests [][]byte
	provider := newRecordingReplayServer(t, &providerRequests)
	dir := setupChatWorkspace(t, provider.URL)
	s := startServer(t, dir)

	resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("health 的 Content-Type = %q, 期望 application/json", ct)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("解析 health 回應: %v\nbody: %s", err, body)
	}
	if health.Status != "ok" {
		t.Errorf("health.status = %q, 期望 ok", health.Status)
	}

	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	if len(providerRequests) != 0 {
		t.Errorf("health 期間 Provider 收到 %d 個請求, 期望 0", len(providerRequests))
	}
}

// TestServerInfo 釘住系統資訊的欄位，以及「不含任何憑證，也不含 base_url」（spec 使用者故事 43）。
//
// config.yaml 放兩個 Provider：一個的 key 走環境變數展開，一個直接寫字面值，兩個都有
// base_url。回應的原始 body 裡四個值都不能出現——斷言在解析之前做，一個多出來的欄位也
// 逃不掉。
func TestServerInfo(t *testing.T) {
	const (
		envKey     = "sk-env-canary-7f3a"
		literalKey = "sk-literal-canary-91bc"
		secondBase = "https://base-url-canary.example/v1"
	)
	provider := newReplayServer(t)
	dir := setupChatWorkspace(t, provider.URL)
	writeWorkspaceConfig(t, dir, provider.URL,
		"  deepseek:\n    api_key: "+literalKey+"\n    base_url: "+secondBase+"\nhttp:\n  allowed_domains: []\n")
	t.Setenv("OPENROUTER_API_KEY", envKey)
	writeNamedProfile(t, dir, "second", "provider:\n  name: deepseek\n  model: m\n")

	before := time.Now()
	s := startServer(t, dir)
	resp, body := s.send(t, http.MethodGet, "/api/v1/info", "", nil)
	after := time.Now()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	for _, secret := range []string{envKey, literalKey, secondBase, provider.URL, "api_key", "base_url"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("info 回應含有 %q，憑證與 base_url 都不該出現\nbody: %s", secret, body)
		}
	}

	fields := decodeJSONObject(t, body)
	for _, key := range []string{"name", "version", "started_at", "profiles", "providers"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("info 回應缺少欄位 %q\nbody: %s", key, body)
		}
	}
	var info struct {
		Name      string    `json:"name"`
		Version   string    `json:"version"`
		StartedAt time.Time `json:"started_at"`
		Profiles  struct {
			Available   *int `json:"available"`
			Unavailable *int `json:"unavailable"`
		} `json:"profiles"`
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("解析 info 回應: %v\nbody: %s", err, body)
	}
	if info.Name != "OryxOS" {
		t.Errorf("info.name = %q, 期望 OryxOS", info.Name)
	}
	if info.Version == "" {
		t.Error("info.version 是空字串，期望取自建置資訊（本機開發建置為 (devel)）")
	}
	if info.StartedAt.Before(before.Add(-time.Second)) || info.StartedAt.After(after) {
		t.Errorf("info.started_at = %v, 期望落在 %v 與 %v 之間", info.StartedAt, before, after)
	}
	// 兩份 Profile 都組得起來，所以不可用是 0；有不可用時的數量見 TestServerProfileFailureMatrix。
	if info.Profiles.Available == nil || *info.Profiles.Available != 2 {
		t.Errorf("info.profiles.available = %v, 期望 2\nbody: %s", info.Profiles.Available, body)
	}
	if info.Profiles.Unavailable == nil || *info.Profiles.Unavailable != 0 {
		t.Errorf("info.profiles.unavailable = %v, 期望 0\nbody: %s", info.Profiles.Unavailable, body)
	}
	if want := []string{"deepseek", "openrouter"}; !slices.Equal(info.Providers, want) {
		t.Errorf("info.providers = %v, 期望 %v（依名稱排序）", info.Providers, want)
	}
}

// TestServerErrorShape 是錯誤形狀的表格（spec 使用者故事 44、45）：ServeMux 自己產生的 404
// 與 405 預設是一段純文字，呼叫端的程式沒辦法依錯誤碼分支。
func TestServerErrorShape(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
		// wantAllow 非空時，405 回應的 Allow 標頭要含它：呼叫端靠它知道該改用哪個方法。
		wantAllow string
	}{
		{name: "不存在的路徑", method: http.MethodGet, path: "/no/such/path",
			wantStatus: http.StatusNotFound, wantCode: "route_not_found"},
		{name: "api 前綴之下不存在的端點", method: http.MethodGet, path: "/api/v1/no-such-endpoint",
			wantStatus: http.StatusNotFound, wantCode: "route_not_found"},
		{name: "已知路徑用錯方法", method: http.MethodPost, path: "/api/v1/health",
			wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed", wantAllow: http.MethodGet},
		{name: "info 用 DELETE", method: http.MethodDelete, path: "/api/v1/info",
			wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed", wantAllow: http.MethodGet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := s.send(t, tt.method, tt.path, "", nil)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("狀態碼 = %d, 期望 %d\nbody: %s", resp.StatusCode, tt.wantStatus, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, 期望 application/json\nbody: %s", ct, body)
			}
			fields := decodeJSONObject(t, body)
			for _, key := range []string{"error_code", "message", "timestamp"} {
				var value string
				if err := json.Unmarshal(fields[key], &value); err != nil || value == "" {
					t.Errorf("錯誤回應的 %q 缺少或不是非空字串（%v）\nbody: %s", key, err, body)
				}
			}
			var shape struct {
				ErrorCode string `json:"error_code"`
				Timestamp string `json:"timestamp"`
			}
			if err := json.Unmarshal(body, &shape); err != nil {
				t.Fatalf("解析錯誤回應: %v", err)
			}
			if shape.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, 期望 %q", shape.ErrorCode, tt.wantCode)
			}
			if _, err := time.Parse(time.RFC3339, shape.Timestamp); err != nil {
				t.Errorf("timestamp = %q 不是 RFC 3339: %v", shape.Timestamp, err)
			}
			if tt.wantAllow != "" && !strings.Contains(resp.Header.Get("Allow"), tt.wantAllow) {
				t.Errorf("Allow 標頭 = %q, 期望含 %s", resp.Header.Get("Allow"), tt.wantAllow)
			}
		})
	}
}

// TestServerCORS 釘住核心階段的 CORS 姿態（技術方案 §7.4、spec 使用者故事 52）：所有回應都
// 允許所有來源、預檢會成功、**不允許攜帶 credentials**。
//
// 錯誤回應那一格是刻意的：CORS 標頭若只加在成功的 handler 上，瀏覽器裡的前端在出錯時連
// 錯誤 body 都讀不到，只看得到一個不透明的網路錯誤。
func TestServerCORS(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)

	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		// wantPreflight 為真時，要求 2xx 並列出允許的方法與標頭。
		wantPreflight bool
	}{
		{name: "成功回應", method: http.MethodGet, path: "/api/v1/health",
			headers: map[string]string{"Origin": "http://frontend.example"}},
		{name: "錯誤回應", method: http.MethodGet, path: "/no/such/path",
			headers: map[string]string{"Origin": "http://frontend.example"}},
		{name: "OPTIONS 預檢", method: http.MethodOptions, path: "/api/v1/health",
			headers: map[string]string{
				"Origin":                         "http://frontend.example",
				"Access-Control-Request-Method":  http.MethodPost,
				"Access-Control-Request-Headers": "Content-Type",
			},
			wantPreflight: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := s.send(t, tt.method, tt.path, "", tt.headers)
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %q, 期望 *", got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
				t.Errorf("Access-Control-Allow-Credentials = %q, 期望不送（核心階段不允許攜帶 credentials）", got)
			}
			if !tt.wantPreflight {
				return
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				t.Fatalf("預檢狀態碼 = %d, 期望 2xx\nbody: %s", resp.StatusCode, body)
			}
			methods := resp.Header.Get("Access-Control-Allow-Methods")
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				if !strings.Contains(methods, method) {
					t.Errorf("Access-Control-Allow-Methods = %q, 期望含 %s", methods, method)
				}
			}
			if headers := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(headers, "Content-Type") {
				t.Errorf("Access-Control-Allow-Headers = %q, 期望含 Content-Type", headers)
			}
		})
	}
}

// TestServerRequestLog 釘住「每個請求一行結構化日誌，不記 body」（spec 使用者故事 53）。
//
// 日誌在 server 關閉之後才讀：那時日誌檔已經關上，每一行都確定寫完了，不必猜要等多久。
//
// 三個 canary 各守一個方向：請求 body 裡的那個守「不記請求 body」，info 回應才有的
// `started_at` 守「不記回應 body」，query string 裡的那個守「路徑不含 query」——之後的
// 端點會帶 query 參數，而 query 與 body 一樣是呼叫端給的內容。
func TestServerRequestLog(t *testing.T) {
	const (
		bodyCanary  = "request-body-canary-5d21"
		queryCanary = "query-canary-e40b"
	)
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	s := startServer(t, dir)

	s.send(t, http.MethodGet, "/api/v1/health?probe="+queryCanary, "", nil)
	s.send(t, http.MethodPost, "/api/v1/health", `{"message":"`+bodyCanary+`"}`, nil)
	if _, body := s.send(t, http.MethodGet, "/api/v1/info", "", nil); !bytes.Contains(body, []byte("started_at")) {
		t.Fatalf("info 回應沒有 started_at，這一格的回應 body canary 失效\nbody: %s", body)
	}
	// 預檢在 CORS 那一層就回應、根本進不到路由，它也要記得到。
	s.send(t, http.MethodOptions, "/api/v1/health", "", map[string]string{
		"Origin": "http://frontend.example", "Access-Control-Request-Method": http.MethodGet,
	})
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}

	logs := readWorkspaceLog(t, dir)
	for _, canary := range []string{bodyCanary, queryCanary, "started_at"} {
		if strings.Contains(logs, canary) {
			t.Errorf("日誌含有 %q，請求 body、query 與回應 body 都不該進日誌:\n%s", canary, logs)
		}
	}

	type requestLine struct {
		Method     string   `json:"method"`
		Path       string   `json:"path"`
		Status     int      `json:"status"`
		DurationMS *float64 `json:"duration_ms"`
	}
	var got []requestLine
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry struct {
			Msg string `json:"msg"`
			requestLine
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日誌行不是 JSON: %v\n%s", err, line)
		}
		if entry.Msg == "http_request" {
			got = append(got, entry.requestLine)
		}
	}
	want := []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/health", http.StatusOK},
		{http.MethodPost, "/api/v1/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/info", http.StatusOK},
		{http.MethodOptions, "/api/v1/health", http.StatusNoContent},
	}
	if len(got) != len(want) {
		t.Fatalf("http_request 日誌 %d 行, 期望 %d 行（每個請求一行）:\n%s", len(got), len(want), logs)
	}
	for i, w := range want {
		if got[i].Method != w.method || got[i].Path != w.path || got[i].Status != w.status {
			t.Errorf("第 %d 行 = %s %s %d, 期望 %s %s %d", i+1,
				got[i].Method, got[i].Path, got[i].Status, w.method, w.path, w.status)
		}
		if got[i].DurationMS == nil || *got[i].DurationMS < 0 {
			t.Errorf("第 %d 行缺少耗時 duration_ms 或為負值", i+1)
		}
	}
}

// TestServerStartupOutput 釘住啟動輸出（ticket #75 AC）：監聽位址、未認證提醒，以及 Profile
// 的提醒**措辭與 chat 逐字相同、並指名是哪份 Profile**。
//
// 「逐字相同」不抄一份字串來比：同一個 Workspace 先跑一次 chat，把 chat 印出來的那行拿來當
// 期望值。這樣日後改了措辭，兩邊一起改就一起綠，只改一邊才會紅——那正是這條要守的事。
//
// 兩份 Profile 只有 default 列了 HTTP Tool，所以那行提醒應該恰好出現一次，而且帶著 default
// 的名字；second 不該分到一行。
func TestServerStartupOutput(t *testing.T) {
	const reminderMark = "http.allowed_domains 為空"
	provider := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, provider.URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\ntools:\n  - http_get\n")
	writeNamedProfile(t, dir, "second", "provider:\n  name: openrouter\n  model: m\n")

	var chatOut bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &chatOut, dir,
		chatOptions{profileName: "default", message: "你好"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	var chatReminder string
	for _, line := range strings.Split(chatOut.String(), "\n") {
		if strings.Contains(line, reminderMark) {
			chatReminder = line
		}
	}
	if chatReminder == "" {
		t.Fatalf("chat 沒有印出白名單提醒，這一格的期望值取不到:\n%s", chatOut.String())
	}

	s := startServer(t, dir)
	// 先走完一個請求再收：組裝還沒完成就取消的話，測到的是「啟動途中被中止」，不是啟動輸出。
	if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	out := s.out.String()

	for _, want := range []string{s.addr, "未啟用認證", "Profile default 已載入", "Profile second 已載入"} {
		if !strings.Contains(out, want) {
			t.Errorf("啟動輸出缺少 %q:\n%s", want, out)
		}
	}
	if !slices.Contains(strings.Split(out, "\n"), "[Profile default] "+chatReminder) {
		t.Errorf("啟動輸出沒有「[Profile default] ＋ chat 的那行提醒」\nchat 那行: %q\nserver 輸出:\n%s", chatReminder, out)
	}
	if n := strings.Count(out, reminderMark); n != 1 {
		t.Errorf("白名單提醒出現 %d 次, 期望 1 次（只有 default 列了 HTTP Tool）:\n%s", n, out)
	}
}

// TestServerCommandAddrFlag 走 cobra 命令路徑，驗證 --addr 的預設值與「無法綁定時命令回傳錯誤」。
//
// 綁定失敗那一格先自己佔住一個埠，再叫 server 去綁同一個。斷言用 errors.Is 對 EADDRINUSE，
// 不比對措辭：要確認的是「錯誤一路傳上來了」，不是某一句話。
func TestServerCommandAddrFlag(t *testing.T) {
	t.Run("--help 列出 --addr 與預設值 :8080", func(t *testing.T) {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"server", "--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("server --help: %v", err)
		}
		if !strings.Contains(out.String(), "--addr") || !strings.Contains(out.String(), ":8080") {
			t.Errorf("server --help 沒有列出 --addr 與預設值 :8080:\n%s", out.String())
		}
	})

	t.Run("位址已被佔用時命令回傳錯誤", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("佔住一個埠: %v", err)
		}
		t.Cleanup(func() {
			if err := occupied.Close(); err != nil {
				t.Errorf("關閉佔位 listener: %v", err)
			}
		})
		dir := setupChatWorkspace(t, newReplayServer(t).URL)
		t.Chdir(dir)

		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"server", "--addr", occupied.Addr().String()})
		// 期限的理由同 runServerExpectingFailure：綁定若意外成功，命令會一直服務下去。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = root.ExecuteContext(ctx)
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("server --addr <已被佔用> 的錯誤 = %v, 期望錯誤鏈含 EADDRINUSE\n輸出:\n%s", err, out.String())
		}
	})
}

// TestServerGracefulShutdown 釘住取消 context 之後的收尾（ticket #75 AC）：Shutdown 會等連線、
// 等待期間新連線被拒絕、收尾排在 Shutdown 之後、MCP 子進程已結束、沒有殘留的 goroutine。
//
// **#75 的端點都是瞬間完成的查詢，怎麼讓 Shutdown 有東西可等**：開一條連線，送出 request line
// 與標頭，但**扣住結尾的空行**。net/http 的 Shutdown 會把這種還沒讀完標頭的連線當成非閒置而
// 等它（開頭 5 秒內；超過 5 秒才當成閒置直接關，所以這一段操作要在幾百毫秒內做完）。這段
// 等待期間驗得到三件事：
//
//   - runServer 還沒返回：它在等，不是一取消就收工。關閉用的 context 若直接沿用已經取消的
//     ctx，Shutdown 會立刻返回，這一條就轉紅。
//   - **MCP 子進程還活著**（marker 還不存在）：收尾（processAssembly.Close）排在 Shutdown
//     之後，不是同時、更不是之前。
//   - 新連線已經被拒絕。
//
// 之後由 client 端把那條連線關掉，Shutdown 等到它、收尾接著走完。
//
// **這一格驗不到「進行中的請求仍拿到完整回應」**：net/http 對 Shutdown 開始之後才讀完的請求
// 不執行 handler、直接關連線（server.go 的 conn.serve 在 readRequest 之後檢查 shuttingDown），
// 所以扣住空行的請求就算送完也拿不到回應。handler 已經在跑的請求，要等 #79 有了 turn 才造得
// 出來，那張票的 AC 會驗。
//
// goroutine 的檢查沿用 TestProcessNoGoroutineAccumulation 的手法：取基線、跑、收掉之後輪詢
// 到回到基線附近。**基線在 server 起來之前取**，所以 server 自己開的每一個 goroutine——Serve
// 迴圈、連線、審計的背景 worker、SQLite 連線池、MCP 的讀取迴圈——沒收掉都會算進去。
//
// 審計佇列的排空這裡看不到：#75 的端點都不產生審計記錄。它由兩段接起來成立——runServer 的
// 收尾走 processAssembly.Close（marker 與 goroutine 回到基線證明它跑完了），Close 先排空審計
// 再關 SQLite（TestProcessAssemblyClosesAuditBeforeStore 守著）。
func TestServerGracefulShutdown(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive.exited")
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntryWithExitMarker(t, "alive", marker, "echo"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - alive\ntools:\n  - alive__echo\n")

	baseline := runtime.NumGoroutine()
	s := startServer(t, dir)
	// 先走完一個正常請求：確認組裝已經結束、Serve 已經在接手，下面那條連線才會被 server 讀到。
	if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}

	pending, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatalf("開啟還沒送完請求的連線: %v", err)
	}
	if _, err := io.WriteString(pending, "GET /api/v1/health HTTP/1.1\r\nHost: oryxos.test\r\n"); err != nil {
		t.Fatalf("送出請求的前半: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // 讓 Serve 接受這條連線、開始讀標頭

	s.cancel()
	select {
	case <-s.finished:
		t.Fatalf("還有連線沒結束，runServer 卻已經返回（錯誤：%v）", s.runErr)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("Shutdown 還在等連線，MCP 子進程卻已經被收掉——收尾跑到 Shutdown 前面去了")
	}
	if conn, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("關閉期間 %s 仍接受新連線", s.addr)
	}

	if err := pending.Close(); err != nil {
		t.Fatalf("關閉還沒送完請求的連線: %v", err)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("取消 context 之後 runServer 回傳錯誤: %v\n輸出:\n%s", err, s.out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("runServer 返回時 MCP 子進程還沒被收掉（marker %s 不存在）", marker)
	}

	assertGoroutinesBackToBaseline(t, baseline)
}

// assertGoroutinesBackToBaseline 等 goroutine 數回到基線附近（最多 5 秒），回不去就報錯並印出所有
// goroutine 的堆疊。**基線要在 server 起來之前取**，server 自己開的每一個 goroutine（Serve 迴圈、連線、
// 審計的背景 worker、SQLite 連線池、MCP 的讀取迴圈）沒收掉都會算進去。容忍 3 個：runtime 與 testing
// 自己偶爾會多出正在結束的 goroutine。
func assertGoroutinesBackToBaseline(t *testing.T, baseline int) {
	t.Helper()
	const slack = 3
	deadline := time.Now().Add(5 * time.Second)
	got := runtime.NumGoroutine()
	for got > baseline+slack && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		got = runtime.NumGoroutine()
	}
	if got > baseline+slack {
		buf := make([]byte, 1<<20)
		t.Errorf("server 關閉之後 goroutine 沒有回到基線：基線 %d、關閉後 %d\n%s", baseline, got, buf[:runtime.Stack(buf, true)])
	}
}

// TestServerReleasesSlowConnections 釘住三個讀取期限（Codex gate 第 1 輪）：慢速的連線在期限到了
// 之後被 server 放掉，正常的請求照樣成功。
//
// 沒有這三個期限時，http.Server 會對一條連線一直等下去：只送半份標頭的、拿到回應之後掛著
// keep-alive 不走的、標頭說有 1000 bytes body 卻只送一小段的——每一條都佔著一個 goroutine 與一個
// 檔案描述符，累積到上限，health 就連不進來了。
//
// **每一格只把要驗的那個期限設短，另外兩個設長，這是刻意的**：net/http 對沒設的讀標頭期限與閒置
// 期限，會 fallback 到 ReadTimeout（server.go 的 readHeaderTimeout／idleTimeout）。另外兩個若也
// 設短，拿掉被驗的那個，fallback 照樣會把連線關掉，這一格就守不住它。
//
// 用裸 TCP 而不是 http.Client：要送的正是 http.Client 送不出來的殘缺請求。判斷「是誰關的」看
// io.ReadAll 的錯誤：server 關掉連線時讀到 EOF，ReadAll 回 nil；client 自己的期限先到，讀到的是
// 逾時錯誤。
func TestServerReleasesSlowConnections(t *testing.T) {
	const (
		short = 200 * time.Millisecond
		long  = 30 * time.Second
		// clientWait 夾在兩者之間：期限有作用，連線在 short 之後就被關掉；期限沒作用，最快的
		// fallback 也要等 long，client 會先放棄。
		clientWait = 3 * time.Second
	)
	tests := []struct {
		name string
		opts serverOptions
		// request 是寫進連線的全部內容，寫完之後 client 就不再送任何東西。
		request string
		// wantResponse 非空時，連線被關掉之前，server 要先送出以它開頭的回應。
		wantResponse string
	}{
		{
			name:    "只送半份標頭",
			opts:    serverOptions{readHeaderTimeout: short, readTimeout: long, idleTimeout: long},
			request: "GET /api/v1/health HTTP/1.1\r\nHost: oryxos.test\r\n",
		},
		{
			name:         "回應之後 keep-alive 連線閒著不動",
			opts:         serverOptions{readHeaderTimeout: long, readTimeout: long, idleTimeout: short},
			request:      "GET /api/v1/health HTTP/1.1\r\nHost: oryxos.test\r\n\r\n",
			wantResponse: "HTTP/1.1 200",
		},
		{
			// handler 不讀 body、立刻回 405。但 net/http 寫出回應之前，會先把 handler 沒讀的 body
			// 排空，排空就卡在沒送來的那些 bytes 上：handler 瞬間完成，連線照樣被佔住。
			name:         "標頭完整但 body 沒送完",
			opts:         serverOptions{readHeaderTimeout: long, readTimeout: short, idleTimeout: long},
			request:      "POST /api/v1/health HTTP/1.1\r\nHost: oryxos.test\r\nContent-Length: 1000\r\n\r\n{\"partial\":",
			wantResponse: "HTTP/1.1 405",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			s := startServerWithOptions(t, dir, tt.opts)
			// 先走完一個正常請求：確認組裝已經結束、Serve 已經在接手，下面的等待才只算慢速連線本身。
			if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
				t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
			}

			conn, err := net.Dial("tcp", s.addr)
			if err != nil {
				t.Fatalf("開啟慢速連線: %v", err)
			}
			defer func() { _ = conn.Close() }() // server 已經關過它，這裡只是保險
			if err := conn.SetDeadline(time.Now().Add(clientWait)); err != nil {
				t.Fatalf("設定 client 端期限: %v", err)
			}
			if _, err := io.WriteString(conn, tt.request); err != nil {
				t.Fatalf("寫出慢速請求: %v", err)
			}
			received, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("server 在 %v 內沒有放掉這條連線（讀到 %v，不是 server 關連線的 EOF）\n已收到: %q",
					clientWait, err, received)
			}
			if !strings.HasPrefix(string(received), tt.wantResponse) {
				t.Errorf("連線被關掉之前收到 %q, 期望以 %q 開頭", received, tt.wantResponse)
			}

			// 期限只放掉慢速的那一條，正常的請求不受影響。
			if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
				t.Errorf("慢速連線被放掉之後，health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
			}
		})
	}
}

// TestNewHTTPServerTimeouts 釘住「沒填的期限換成預設值，不是不設期限」。
//
// **這一條只看得到設定，看不到行為**：預設期限最短也有 10 秒，要從連線上觀察，測試就得等 10 秒
// 以上。設定有沒有真的讓連線被放掉，由 TestServerReleasesSlowConnections 從連線上驗；這裡只驗
// 「零值被換掉」這一步。
//
// 這一步漏掉的後果看不見：http.Server 的零值就是不設期限，忘記填選項的呼叫端會悄悄拿到一台
// 可以被慢速連線佔滿的 server，所有測試照樣是綠的。
func TestNewHTTPServerTimeouts(t *testing.T) {
	tests := []struct {
		name                               string
		opts                               serverOptions
		wantReadHeader, wantRead, wantIdle time.Duration
	}{
		{
			name:           "沒填的期限換成預設值",
			opts:           serverOptions{},
			wantReadHeader: defaultReadHeaderTimeout, wantRead: defaultReadTimeout, wantIdle: defaultIdleTimeout,
		},
		{
			name:           "有填的期限照用",
			opts:           serverOptions{readHeaderTimeout: time.Second, readTimeout: 2 * time.Second, idleTimeout: 3 * time.Second},
			wantReadHeader: time.Second, wantRead: 2 * time.Second, wantIdle: 3 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newHTTPServer(http.NotFoundHandler(), tt.opts)
			for _, field := range []struct {
				name      string
				got, want time.Duration
			}{
				{"ReadHeaderTimeout", srv.ReadHeaderTimeout, tt.wantReadHeader},
				{"ReadTimeout", srv.ReadTimeout, tt.wantRead},
				{"IdleTimeout", srv.IdleTimeout, tt.wantIdle},
			} {
				if field.got <= 0 {
					t.Errorf("%s = %v，等於不設期限", field.name, field.got)
				}
				if field.got != field.want {
					t.Errorf("%s = %v, 期望 %v", field.name, field.got, field.want)
				}
			}
		})
	}
}

// reportedAddrListener 底下綁的是 loopback，Addr() 卻回報另一個位址：讓測試不必真的監聽所有網路
// 介面（macOS 的防火牆會為此跳出詢問），也看得到 server 依監聽位址給的提醒。
type reportedAddrListener struct {
	net.Listener
	addr net.Addr
}

func (l reportedAddrListener) Addr() net.Addr { return l.addr }

// TestServerAuthReminder 釘住啟動時「未啟用認證」的提醒（spec #73 使用者故事 11）：
//
//   - **一律印**：只對本機開放也一樣，CORS 全開，使用者瀏覽的網頁仍可能經由瀏覽器打到它。
//   - **緩解建議看監聽位址**：已經只監聽 loopback 時，「改用 --addr 127.0.0.1:8080」是做過的事，
//     還指定了一個沒在用的埠；換成說明只對本機開放也擋不住的那條路。
func TestServerAuthReminder(t *testing.T) {
	const reminder = "未啟用認證"
	const bindLoopback = "改用 --addr 127.0.0.1:8080"
	const loopbackStill = "只對本機開放也一樣"
	tests := []struct {
		name string
		// reported 非 nil 時，listener 對 runServer 回報這個位址；nil 代表照實回報 127.0.0.1。
		reported   net.Addr
		wantAdvice string
		wantAbsent string
	}{
		{name: "監聽 127.0.0.1", wantAdvice: loopbackStill, wantAbsent: bindLoopback},
		{name: "監聽 ::1", reported: &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, wantAdvice: loopbackStill, wantAbsent: bindLoopback},
		{name: "監聽所有介面", reported: &net.TCPAddr{IP: net.IPv4zero, Port: 8080}, wantAdvice: bindLoopback, wantAbsent: loopbackStill},
		{name: "監聽區網位址", reported: &net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 8080}, wantAdvice: bindLoopback, wantAbsent: loopbackStill},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("開 listener: %v", err)
			}
			realAddr := listener.Addr().String()
			var serving net.Listener = listener
			if tt.reported != nil {
				serving = reportedAddrListener{Listener: listener, addr: tt.reported}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out bytes.Buffer
			finished := make(chan error, 1)
			go func() { finished <- runServer(ctx, &out, dir, serverOptions{}, serving) }()

			// 先走完一個請求：確認啟動輸出都已經印完，再收掉。
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + realAddr + "/api/v1/health")
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("關閉 health 回應: %v", err)
			}
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatalf("runServer: %v\n輸出:\n%s", err, out.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("取消 context 之後 20 秒 runServer 仍未返回")
			}

			got := out.String()
			for _, want := range []string{reminder, tt.wantAdvice, "SECURITY.md"} {
				if !strings.Contains(got, want) {
					t.Errorf("啟動輸出沒有 %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, tt.wantAbsent) {
				t.Errorf("啟動輸出不該有 %q:\n%s", tt.wantAbsent, got)
			}
		})
	}
}
