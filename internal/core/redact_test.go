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

// TestRedactArgsKeepsNumericPrecision 釘住去敏**只遮敏感值，不順手改動別的東西**。
//
// **這是外部審查抓出來的。** RedactArgs 原本走 json.Unmarshal 解到 any，所有數字因此
// 先變成 float64——超過 2^53 的整數會被截到同一個值（9007199254740993 →
// 9007199254740992）。兩條落盤路徑都受影響：
//
//   - **審計**（tool_invocations.parameters）記下的參數與 LLM 實際送出的不同，
//     而審計的全部價值就在於它記的是事實
//   - **死循環守衛的日誌**（ticket #54）宣稱帶的是規範化後的 key，實際上帶的是一個
//     被改過的版本——那條日誌存在的理由就是「除錯時看得出是哪個參數在循環」
//
// 遮蔽是這個函式該做的事，改動精度不是。
func TestRedactArgsKeepsNumericPrecision(t *testing.T) {
	// 2^53+1：float64 表示不了，會被截成 2^53。
	const bigID = "9007199254740993"

	tests := []struct {
		name       string
		args       string
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:       "大整數原樣保留",
			args:       `{"id":` + bigID + `}`,
			wantSubstr: []string{bigID},
		},
		{
			// 同一次呼叫裡兩件事都要成立：密鑰被遮、數字沒被動。
			name:       "遮敏感值的同時不動數字",
			args:       `{"id":` + bigID + `,"api_key":"sk-live-XYZ"}`,
			wantSubstr: []string{bigID, "[REDACTED]"},
			notSubstr:  []string{"sk-live-XYZ", "9007199254740992"},
		},
		{
			// 數值型的敏感欄位照樣整個換掉——遮蔽看的是欄位名，不是值的型別。
			name:       "數值型的敏感欄位仍被遮蔽",
			args:       `{"api_token":` + bigID + `}`,
			wantSubstr: []string{"[REDACTED]"},
			notSubstr:  []string{bigID},
		},
		{
			// 小數的字面也不該被改寫（1.0 不該變成 1）。
			name:       "小數保留原始字面",
			args:       `{"ratio":1.0}`,
			wantSubstr: []string{"1.0"},
		},
		{
			// 尾隨內容讓整串不是一個合法的 JSON 文件，維持既有的保守行為：只記長度。
			// 這一格是護欄——換成 Decoder 之後若不檢查文件是否結束，這裡會安靜放行。
			name:       "尾隨內容仍只記長度",
			args:       `{"id":1}]`,
			wantSubstr: []string{"非 JSON 參數"},
			notSubstr:  []string{"[REDACTED]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := core.RedactArgs(tt.args)
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("RedactArgs(%q) = %q, 期望含 %q", tt.args, got, want)
				}
			}
			for _, bad := range tt.notSubstr {
				if strings.Contains(got, bad) {
					t.Errorf("RedactArgs(%q) = %q, 不該含 %q", tt.args, got, bad)
				}
			}
		})
	}
}
