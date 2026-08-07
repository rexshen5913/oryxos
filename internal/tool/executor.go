package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Executor 實作 core.ToolExecutor：持有 Profile 過濾後的可用 Tool 子集，
// 執行 LLM 的 Tool 呼叫請求並落結構化日誌。
type Executor struct {
	names  []string // 子集的宣告順序（Profile tools 順序）
	tools  map[string]OryxTool
	logger *slog.Logger
}

// Definitions 回傳可用 Tool 的宣告列表（按子集順序），附進每輪 LLM 請求。
func (e *Executor) Definitions() []core.ToolDefinition {
	if len(e.names) == 0 {
		return nil
	}
	defs := make([]core.ToolDefinition, 0, len(e.names))
	for _, name := range e.names {
		t := e.tools[name]
		defs = append(defs, core.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return defs
}

// Execute 執行一次 Tool 呼叫：查子集、執行、落日誌（Tool 名、參數摘要、狀態、
// 耗時）。任何失敗以錯誤 ToolResult 回報（回填給 LLM），不 panic。
func (e *Executor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	start := time.Now()
	var result core.ToolResult
	if t, ok := e.tools[call.Name]; ok {
		result = t.Execute(ctx, call.Arguments)
	} else {
		result = core.ToolResult{Error: fmt.Sprintf("Tool %q 不在此 Agent 的可用 Tool 子集（檢查 Profile 的 tools 欄位）", call.Name)}
	}

	attrs := []any{
		"tool", call.Name,
		"args", summarizeArgs(call.Arguments),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if result.OK {
		e.logger.InfoContext(ctx, "tool_invocation", append(attrs, "status", "completed")...)
	} else {
		e.logger.ErrorContext(ctx, "tool_invocation", append(attrs, "status", "failed", "error", redactErrorText(result.Error))...)
	}
	return result
}

// urlPattern 粗匹配文字中內嵌的 http(s) URL，供 query 遮蔽。
var urlPattern = regexp.MustCompile(`https?://[^\s"'）)]+`)

// redactErrorText 對錯誤文字中內嵌的 URL 做 query 遮蔽後再落日誌——錯誤訊息
// 常內嵌完整 URL（如 url.Error），是 args 之外的第二條密鑰洩漏路徑；來源層
// 已避免內嵌 raw URL，這裡是統一收口的最後防線（未來 Tool 一體適用）。
func redactErrorText(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, redactURLQuery)
}

// sensitiveKeyParts 是參數摘要中須遮蔽值的 key 片段（大小寫不敏感、子串命中）。
var sensitiveKeyParts = []string{"token", "secret", "password", "api_key", "apikey", "authorization", "credential", "cookie"}

// summarizeArgs 產生可安全落日誌的參數摘要：敏感 key 的值遮蔽、URL query 整段
// 遮蔽（api key 常放 query）、body 只記長度、非 JSON 參數只記長度，最後截斷。
// 原樣記錄呼叫參數等於把密鑰寫進日誌，摘要必須先去敏再落盤。
func summarizeArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return fmt.Sprintf("<非 JSON 參數 %d bytes>", len(args))
	}
	summary, err := json.Marshal(redactValue("", v))
	if err != nil {
		return fmt.Sprintf("<參數摘要編碼失敗 %d bytes>", len(args))
	}
	return truncateRunes(string(summary), 200)
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

// redactURLQuery 把帶 query 的 http/https URL 的 query 整段遮蔽。
func redactURLQuery(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery == "" {
		return s
	}
	u.RawQuery = ""
	return u.String() + "?[REDACTED]"
}

func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
