package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

const (
	// httpRequestTimeout 是單次 HTTP Tool 請求的超時上限；呼叫端仍可用 ctx 更早取消。
	httpRequestTimeout = 30 * time.Second
	// maxResponseBytes 限制回填給 LLM 的回應內文大小（資源佔用限制，需求 5.6）。
	maxResponseBytes = 1 << 20 // 1 MiB
)

// httpTool 是內建 HTTP Tool（http_get、http_post）的共用實作：
// 執行前經 SandboxChecker 域名白名單校驗，redirect 的每一跳也重驗。
type httpTool struct {
	name        string
	description string
	schema      json.RawMessage
	method      string
	checker     *SandboxChecker
	client      *http.Client
}

// NewHTTPGet 建立內建 Tool http_get。
func NewHTTPGet(checker *SandboxChecker) OryxTool {
	return &httpTool{
		name:        "http_get",
		description: "對域名白名單內的 URL 發起 HTTP GET 請求，回傳狀態碼與回應內文。",
		schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "要請求的 URL（http/https，host 須在白名單內）"}
			},
			"required": ["url"]
		}`),
		method:  http.MethodGet,
		checker: checker,
		client:  newHTTPClient(checker),
	}
}

// NewHTTPPost 建立內建 Tool http_post。
func NewHTTPPost(checker *SandboxChecker) OryxTool {
	return &httpTool{
		name:        "http_post",
		description: "對域名白名單內的 URL 發起 HTTP POST 請求（body 以 application/json 送出），回傳狀態碼與回應內文。",
		schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "要請求的 URL（http/https，host 須在白名單內）"},
				"body": {"type": "string", "description": "請求內文（JSON 字串）"}
			},
			"required": ["url"]
		}`),
		method:  http.MethodPost,
		checker: checker,
		client:  newHTTPClient(checker),
	}
}

// newHTTPClient 建立帶超時的 HTTP client；CheckRedirect 對 redirect 的每一跳
// 重做白名單校驗，白名單內的端點不能借 redirect 打到白名單外。
func newHTTPClient(checker *SandboxChecker) *http.Client {
	return &http.Client{
		Timeout: httpRequestTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return checker.CheckHTTPURL(req.URL.String())
		},
	}
}

func (t *httpTool) Name() string                 { return t.name }
func (t *httpTool) Description() string          { return t.description }
func (t *httpTool) InputSchema() json.RawMessage { return t.schema }

// httpInput 是 http_get／http_post 的輸入參數。
type httpInput struct {
	URL  string `json:"url"`
	Body string `json:"body"`
}

// httpOutput 是回填給 LLM 的結果內容：狀態碼與回應內文。非 2xx 不算 Tool 失敗，
// 狀態碼照樣回給 LLM，由 LLM 決定下一步。
type httpOutput struct {
	Status    int    `json:"status"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (t *httpTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in httpInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", t.name, err)}
	}
	if in.URL == "" {
		return core.ToolResult{Error: fmt.Sprintf("%s 缺必填參數 url", t.name)}
	}
	if err := t.checker.CheckHTTPURL(in.URL); err != nil {
		return core.ToolResult{Error: err.Error()}
	}

	var body io.Reader
	if t.method == http.MethodPost {
		body = strings.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(ctx, t.method, in.URL, body)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("建立 %s 請求: %v", t.name, err)}
	}
	if t.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// url.Error 會內嵌完整 URL（query 可能帶密鑰）：只保留底層原因，
		// 目標以 query 遮蔽後的形式呈現。
		cause := err
		var uerr *url.Error
		if errors.As(err, &uerr) {
			cause = uerr.Err
		}
		target := redactURLQuery(in.URL)
		if errors.Is(err, ErrSandboxViolation) {
			// redirect 校驗失敗：SandboxViolation 不可重試。
			return core.ToolResult{Error: fmt.Sprintf("%s 請求被攔截（%s）: %v", t.name, target, cause)}
		}
		return core.ToolResult{Error: fmt.Sprintf("%s 請求失敗（%s）: %v", t.name, target, cause), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 讀取回應: %v", t.name, err), Retryable: true}
	}
	out := httpOutput{Status: resp.StatusCode, Body: string(data)}
	if len(data) > maxResponseBytes {
		out.Body = string(data[:maxResponseBytes])
		out.Truncated = true
	}
	content, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("編碼 %s 結果: %v", t.name, err)}
	}
	return core.ToolResult{OK: true, Content: string(content)}
}
