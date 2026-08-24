package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestExecutorInvocationLog 驗證每次 Tool 呼叫落結構化日誌：Tool 名、狀態、耗時。
// 驅動成功與失敗各一次，斷言日誌欄位。
func TestExecutorInvocationLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // httptest handler
	}))
	t.Cleanup(srv.Close)

	checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: []string{hostOf(t, srv.URL)}})
	r := tool.NewRegistry()
	if err := r.Register(tool.NewHTTPGet(checker)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		callURL    string
		wantStatus string
	}{
		{name: "成功呼叫記 completed", callURL: srv.URL + "/x", wantStatus: "completed"},
		{name: "失敗呼叫記 failed", callURL: "https://evil.com/x", wantStatus: "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			exec, err := r.Subset([]string{"http_get"}, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
			if err != nil {
				t.Fatalf("Subset: %v", err)
			}

			exec.Execute(context.Background(), core.ToolCall{
				ID:        "call_1",
				Name:      "http_get",
				Arguments: `{"url":"` + tt.callURL + `"}`,
			})

			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("解析日誌 JSON %q: %v", buf.String(), err)
			}
			if record["msg"] != "tool_invocation" {
				t.Errorf("msg = %v, 期望 tool_invocation", record["msg"])
			}
			if record["tool"] != "http_get" {
				t.Errorf("tool = %v, 期望 http_get", record["tool"])
			}
			if record["status"] != tt.wantStatus {
				t.Errorf("status = %v, 期望 %s", record["status"], tt.wantStatus)
			}
			if _, ok := record["duration_ms"]; !ok {
				t.Error("日誌缺 duration_ms 欄位")
			}
		})
	}
}

// TestExecutorArgsSummaryRedacted 驗證日誌的參數摘要不洩密：敏感 key 的值
// 遮蔽、URL query 遮蔽（api key 常放 query）、body 只記長度、非 JSON 只記長度。
// 摘要與執行結果無關（violation 失敗照樣落日誌），斷言日誌的 args 欄位。
func TestExecutorArgsSummaryRedacted(t *testing.T) {
	// 白名單內但已關閉的端點：連線被拒，url.Error 會內嵌完整 URL，
	// 驗證 error 欄位不成為 query 密鑰的洩漏旁路。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	const secret = "S3CRET-VALUE"
	tests := []struct {
		name     string
		toolName string
		allowed  []string
		args     string
		wantSub  string // args 摘要須含此子串
	}{
		{
			name:     "網路錯誤時 error 欄位不洩漏 URL query",
			toolName: "http_get",
			allowed:  []string{"127.0.0.1"},
			args:     `{"url":"` + deadURL + `/x?api_key=` + secret + `"}`,
			wantSub:  "?[REDACTED]",
		},
		{
			name:     "URL query 整段遮蔽",
			toolName: "http_get",
			args:     `{"url":"https://api.example.com/x?api_key=` + secret + `&q=1"}`,
			wantSub:  "?[REDACTED]",
		},
		{
			name:     "body 只記長度不記內容",
			toolName: "http_post",
			args:     `{"url":"https://api.example.com/x","body":"{\"password\":\"` + secret + `\"}"}`,
			wantSub:  "bytes",
		},
		{
			name:     "敏感 key 的值遮蔽",
			toolName: "http_get",
			args:     `{"url":"https://api.example.com/x","api_key":"` + secret + `"}`,
			wantSub:  "[REDACTED]",
		},
		{
			name:     "非 JSON 參數只記長度",
			toolName: "http_get",
			args:     `token=` + secret,
			wantSub:  "bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 未指定白名單的案例全拒：執行必失敗，但日誌照落。
			r := tool.NewRegistry()
			root, _ := newWorkspace(t)
			if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: tt.allowed}), root, testShellRuntime(t)); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			exec, err := r.Subset([]string{"http_get", "http_post"}, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
			if err != nil {
				t.Fatalf("Subset: %v", err)
			}

			exec.Execute(context.Background(), core.ToolCall{ID: "c1", Name: tt.toolName, Arguments: tt.args})

			// 整筆日誌（args、error、其餘欄位）都不得含敏感值，不只 args。
			if strings.Contains(buf.String(), secret) {
				t.Errorf("日誌洩漏敏感值: %s", buf.String())
			}
			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("解析日誌 JSON %q: %v", buf.String(), err)
			}
			args, _ := record["args"].(string)
			if !strings.Contains(args, tt.wantSub) {
				t.Errorf("args 摘要 = %q, 未含 %q", args, tt.wantSub)
			}
		})
	}
}

// TestExecutorUnknownTool 驗證呼叫不在可用子集的 Tool 時，以錯誤 ToolResult
// 回報（回填給 LLM），不 panic、不可重試。
func TestExecutorUnknownTool(t *testing.T) {
	exec, err := tool.NewRegistry().Subset(nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}

	result := exec.Execute(context.Background(), core.ToolCall{ID: "call_1", Name: "ghost_tool", Arguments: "{}"})

	if result.OK {
		t.Fatal("未知 Tool 應失敗, 實際成功")
	}
	if !strings.Contains(result.Error, "ghost_tool") {
		t.Errorf("錯誤 %q 未含 Tool 名", result.Error)
	}
	if result.Retryable {
		t.Error("未知 Tool 不應標記可重試")
	}
}
