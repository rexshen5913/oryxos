package tool_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestSandboxCheckerCheckHTTPURL 是域名白名單的行為矩陣：host 解析＋通配符匹配，
// 預設拒絕（空白名單全擋）。任何拒絕都必須是 SandboxViolation（可被 errors.Is 識別）。
func TestSandboxCheckerCheckHTTPURL(t *testing.T) {
	tests := []struct {
		name          string
		allowed       []string
		url           string
		wantViolation bool
	}{
		{
			name:    "完全匹配放行",
			allowed: []string{"api.example.com"},
			url:     "https://api.example.com/weather?city=beijing",
		},
		{
			name:          "host 不在白名單被攔截",
			allowed:       []string{"api.example.com"},
			url:           "https://evil.com/steal",
			wantViolation: true,
		},
		{
			name:          "空白名單全部拒絕",
			allowed:       nil,
			url:           "https://api.example.com/",
			wantViolation: true,
		},
		{
			name:    "通配符匹配一級子域名",
			allowed: []string{"*.example.com"},
			url:     "https://api.example.com/x",
		},
		{
			name:    "通配符匹配多級子域名",
			allowed: []string{"*.example.com"},
			url:     "https://a.b.example.com/x",
		},
		{
			name:          "通配符不匹配裸域名",
			allowed:       []string{"*.example.com"},
			url:           "https://example.com/x",
			wantViolation: true,
		},
		{
			name:          "字面後綴相似不構成匹配",
			allowed:       []string{"example.com"},
			url:           "https://evil-example.com/x",
			wantViolation: true,
		},
		{
			name:          "通配符後綴相似不構成匹配",
			allowed:       []string{"*.example.com"},
			url:           "https://evil-example.com/x",
			wantViolation: true,
		},
		{
			name:    "URL 帶 port 時只比對 host",
			allowed: []string{"127.0.0.1"},
			url:     "http://127.0.0.1:8080/weather",
		},
		{
			name:    "大小寫不敏感",
			allowed: []string{"API.Example.com"},
			url:     "https://api.example.COM/x",
		},
		{
			name:          "非 http/https scheme 拒絕",
			allowed:       []string{"example.com"},
			url:           "ftp://example.com/file",
			wantViolation: true,
		},
		{
			name:          "無法解析的 URL 拒絕",
			allowed:       []string{"example.com"},
			url:           "://bad-url",
			wantViolation: true,
		},
		{
			name:          "缺 host 的 URL 拒絕",
			allowed:       []string{"example.com"},
			url:           "https:///path-only",
			wantViolation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tool.NewSandboxChecker(tt.allowed).CheckHTTPURL(tt.url)
			if tt.wantViolation {
				if !errors.Is(err, tool.ErrSandboxViolation) {
					t.Errorf("CheckHTTPURL(%q) = %v, 期望 SandboxViolation", tt.url, err)
				}
			} else if err != nil {
				t.Errorf("CheckHTTPURL(%q) = %v, 期望放行", tt.url, err)
			}
		})
	}
}

// TestSandboxViolationErrorOmitsQuery 驗證校驗錯誤訊息不內嵌 URL query——
// 錯誤會落日誌與回填 LLM，query 常帶 api key，任何分支都不得原樣帶出。
func TestSandboxViolationErrorOmitsQuery(t *testing.T) {
	const secret = "S3CRET-VALUE"
	urls := []string{
		"https://evil.com/x?api_key=" + secret,  // host 不在白名單
		"ftp://example.com/x?api_key=" + secret, // scheme 拒絕
		"://bad?api_key=" + secret,              // 無法解析
	}
	checker := tool.NewSandboxChecker([]string{"trusted.example.com"})
	for _, u := range urls {
		err := checker.CheckHTTPURL(u)
		if !errors.Is(err, tool.ErrSandboxViolation) {
			t.Fatalf("CheckHTTPURL(%q) = %v, 期望 SandboxViolation", u, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("CheckHTTPURL(%q) 錯誤訊息洩漏 query: %q", u, err.Error())
		}
	}
}
