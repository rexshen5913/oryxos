package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// decodeToolArgs 把 Tool 呼叫參數解析成一個**完整的** JSON 文件，數字保留原始字面
// （json.Number）。s 不是合法 JSON、或第一個 JSON 值後面還有尾隨內容時回 ok=false。
//
// 兩條落盤／比對路徑共用它：RedactArgs（審計與日誌）與 normalizeToolArgs（死循環
// 守衛的 key）。共用不只是省行數——兩邊對「什麼算一份合法參數」的答案必須一致，
// 否則守衛的 key 與日誌記下的那份會是不同的東西。
//
// **兩件事都不能少，各自有踩過的理由：**
//
//  1. **UseNumber。** 解成 float64 再序列化的話，超過 2^53 的整數會被截到同一個值
//     （實測 9007199254740993 → 9007199254740992），小數字面也會被改寫（1.0 → 1）。
//     審計記的是事實，守衛的 key 要能區分兩個不同的 numeric ID，兩者都輸不起這個。
//  2. **要求第二次 Decode 剛好回 io.EOF。** Decoder.Decode 只吃第一個 JSON 值，後面
//     還有東西它**不報錯**（json.Unmarshal 會）。而 Decoder.More() 擋不住這件事——
//     它回答的是「當前 array/object 裡還有沒有下一個元素」，所以 `{"a":1}]` 的尾隨
//     `]` 會被它讀成「容器結束」而回 false（實測確認），一個不合法的參數字串於是
//     安靜地與合法的收斂成同一個結果。再 Decode 一次則會撞上語法錯誤而擋下。
//
// 尾隨的空白不算內容：`{"a":1}\n` 是合法的 JSON 文件，第二次 Decode 會乾淨地回 io.EOF。
func decodeToolArgs(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// sensitiveKeyParts 是參數中須遮蔽值的 key 片段（大小寫不敏感、子串命中）。
var sensitiveKeyParts = []string{"token", "secret", "password", "api_key", "apikey", "authorization", "credential", "cookie"}

// urlPattern 粗匹配文字中內嵌的 http(s) URL，供 query 遮蔽。
var urlPattern = regexp.MustCompile(`https?://[^\s"'）)]+`)

// RedactArgs 遮蔽 Tool 呼叫參數中的敏感值：敏感 key 的值整個換掉、URL query 整段
// 遮蔽（api key 常放 query）、body 只記長度。**不截斷**——截斷是日誌那條路徑為了
// 控制單行長度做的事，審計要的是可查證，不該把參數切一半。
//
// 參數不是合法 JSON 時無法逐欄位判斷哪裡敏感，只回長度：寧可少記，不賭它安全。
//
// **遮蔽是這個函式該做的事，改動精度不是**：解析走 decodeToolArgs，數字因此以
// json.Number 的形式原樣通過（見該函式，以及 redactValue 為什麼接不到它）。
func RedactArgs(args string) string {
	v, ok := decodeToolArgs(args)
	if !ok {
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
	// json.Number 是具名型別，**接不到**這個 case（type switch 精確比對型別），
	// 於是落到 default 原樣回傳——那正是保留數字字面所需要的。敏感的數值欄位不靠
	// 這裡處理，上面的 isSensitiveKey 已經先攔下了。
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
