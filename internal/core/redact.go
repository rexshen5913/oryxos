package core

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// 落盤前的去敏收口。Tool 呼叫的參數與錯誤訊息都可能內嵌密鑰（api key 常放在 URL
// query 裡），原樣寫進日誌或資料庫等於把密鑰永久保存下來。
//
// 這組函式住在 core 是為了讓兩條落盤路徑共用同一套規則：internal/tool 的結構化
// 日誌，與 ReAct 循環寫出的審計記錄。它們是純字串處理、沒有依賴，放這裡不牽動
// 任何依賴方向（tool 與 storage 本來就 import core）。

// sensitiveKeyParts 是參數中須遮蔽值的 key 片段（大小寫不敏感、子串命中）。
var sensitiveKeyParts = []string{"token", "secret", "password", "api_key", "apikey", "authorization", "credential", "cookie"}

// urlPattern 粗匹配文字中內嵌的 http(s) URL，供 query 遮蔽。
var urlPattern = regexp.MustCompile(`https?://[^\s"'）)]+`)

// RedactArgs 遮蔽 Tool 呼叫參數中的敏感值：敏感 key 的值整個換掉、URL query 整段
// 遮蔽（api key 常放 query）、body 只記長度。**不截斷**——截斷是日誌那條路徑為了
// 控制單行長度做的事，審計要的是可查證，不該把參數切一半。
//
// 參數不是合法 JSON 時無法逐欄位判斷哪裡敏感，只回長度：寧可少記，不賭它安全。
func RedactArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return fmt.Sprintf("<非 JSON 參數 %d bytes>", len(args))
	}
	redacted, err := json.Marshal(redactValue("", v))
	if err != nil {
		return fmt.Sprintf("<參數摘要編碼失敗 %d bytes>", len(args))
	}
	return string(redacted)
}

// RedactErrorText 對錯誤文字中內嵌的 URL 做 query 遮蔽。錯誤訊息常內嵌完整 URL
// （如 url.Error），是參數之外的第二條密鑰洩漏路徑。
func RedactErrorText(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, redactURLQuery)
}

// redactValue 遞迴遮蔽參數值；key 是該值在上層物件中的欄位名（陣列元素沿用）。
func redactValue(key string, v any) any {
	if isSensitiveKey(key) {
		return "[REDACTED]"
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = redactValue(k, item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = redactValue(key, item)
		}
		return out
	case string:
		if strings.EqualFold(key, "body") {
			return fmt.Sprintf("[%d bytes]", len(val))
		}
		return redactURLQuery(val)
	default:
		return v
	}
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// redactURLQuery 遮蔽 http/https URL 裡兩個藏密鑰的位置：query 整段，以及
// userinfo（`https://alice:SECRET@example.com/` 這種帳密，或 `https://TOKEN@host`
// 這種把 token 塞在使用者名稱的寫法）。只清 query 會把 userinfo 原樣留下來。
func redactURLQuery(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return s
	}
	if u.User == nil && u.RawQuery == "" {
		return s
	}
	if u.User != nil {
		// 使用者名稱本身也可能就是 token，整段換掉而不是只清密碼。
		u.User = url.User("[REDACTED]")
	}
	if u.RawQuery == "" {
		return u.String()
	}
	u.RawQuery = ""
	return u.String() + "?[REDACTED]"
}

// TruncateRunes 把 s 截到 maxRunes 個 rune 以內，超出時以 … 收尾。
func TruncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
