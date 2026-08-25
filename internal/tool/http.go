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
	//
	// 回應內文的大小上限不在這裡：那是 maxResponseBytes，與其他 Tool 的回填上限
	// 一起住在 limits.go 的共用常數區塊。
	httpRequestTimeout = 30 * time.Second
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
			decision, err := checker.CheckHTTPURL(req.URL.String())
			if decision == SandboxAllow {
				return nil
			}
			if err == nil {
				// **這一層的 nil 就是放行**——http.Client 拿它決定跟不跟這一跳。所以
				// 非放行卻沒帶理由時必須自己補一個，不能把 nil 交出去（fail closed）。
				// 這是四個回填點裡唯一一個「沒有錯誤」會被讀成允許的地方，其餘三個回填
				// 的是文字，走 sandboxRefusal。
				return fmt.Errorf("%w: redirect 目標未獲放行", ErrSandboxViolation)
			}
			return err
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
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", t.name, err), ErrorKind: core.ToolErrorInvalidArgs}
	}
	if in.URL == "" {
		return core.ToolResult{Error: fmt.Sprintf("%s 缺必填參數 url", t.name), ErrorKind: core.ToolErrorInvalidArgs}
	}
	// 判斷的依據是**決策**而不是「有沒有錯誤」：兩者今天等價，但第三態（SandboxAsk）
	// 落地後就不是了，而屆時把「要問人」讀成「放行」是最糟的方向（見 SandboxDecision）。
	// 不標 Retryable——重跑一次白名單的答案不會變。
	if decision, err := t.checker.CheckHTTPURL(in.URL); decision != SandboxAllow {
		return core.ToolResult{Error: sandboxRefusal(err), ErrorKind: core.ToolErrorSandbox}
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
		target := core.RedactErrorText(in.URL)
		if errors.Is(err, ErrSandboxViolation) {
			// redirect 校驗失敗：SandboxViolation 不可重試。
			return core.ToolResult{Error: fmt.Sprintf("%s 請求被攔截（%s）: %v", t.name, target, cause), ErrorKind: core.ToolErrorSandbox}
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
