package core_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestRedactHidesCredentials 是落盤前去敏的矩陣。URL 有兩個藏密鑰的位置——query
// 與 userinfo——只清 query 會把 `https://alice:SECRET@host/` 的密碼原樣留下來，
// 而把 token 塞在使用者名稱（`https://TOKEN@host/`）也是常見寫法。
func TestRedactHidesCredentials(t *testing.T) {
	const secret = "SUPER-SECRET"

	tests := []struct {
		name string
		// redact 是要驗的去敏函式（參數路徑或錯誤訊息路徑）。
		redact func(string) string
		in     string
		// wantKept 是去敏後仍應看得見的部分（審計要留得住可查證的資訊）。
		wantKept []string
	}{
		{
			name:     "參數：query 裡的密鑰",
			redact:   core.RedactArgs,
			in:       `{"url":"https://api.example.com/x?api_key=` + secret + `"}`,
			wantKept: []string{"api.example.com"},
		},
		{
			name:     "參數：userinfo 裡的帳密",
			redact:   core.RedactArgs,
			in:       `{"url":"https://alice:` + secret + `@api.example.com/x"}`,
			wantKept: []string{"api.example.com"},
		},
		{
			name:     "參數：token 塞在使用者名稱",
			redact:   core.RedactArgs,
			in:       `{"url":"https://` + secret + `@api.example.com/x"}`,
			wantKept: []string{"api.example.com"},
		},
		{
			name:     "參數：敏感 key 的值",
			redact:   core.RedactArgs,
			in:       `{"authorization":"Bearer ` + secret + `"}`,
			wantKept: []string{"authorization"},
		},
		{
			name:     "錯誤訊息：內嵌 URL 的 query",
			redact:   core.RedactErrorText,
			in:       `http_get 請求失敗（https://api.example.com/x?api_key=` + secret + `）: 連線逾時`,
			wantKept: []string{"api.example.com", "連線逾時"},
		},
		{
			name:     "錯誤訊息：內嵌 URL 的 userinfo",
			redact:   core.RedactErrorText,
			in:       `http_get 請求失敗（https://alice:` + secret + `@api.example.com/x）: 連線逾時`,
			wantKept: []string{"api.example.com", "連線逾時"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.redact(tt.in)
			if strings.Contains(got, secret) {
				t.Errorf("密鑰未被遮蔽: %q", got)
			}
			for _, want := range tt.wantKept {
				if !strings.Contains(got, want) {
					t.Errorf("去敏過頭，遺失可查證的資訊 %q: %q", want, got)
				}
			}
		})
	}
}
