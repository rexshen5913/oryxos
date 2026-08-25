// Tool 中介層接縫的測試（ticket #52）。斷言的是**外部可觀察的執行行為**：底層 Tool
// 有沒有被執行、拿到什麼結果、多層之間的進出順序，不是內部怎麼組出那條鏈路。
//
// 本票只交付接縫，不交付政策——「拒絕」在這裡是一個測試用的中介層寫出來的行為，
// 不是 OryxOS 內建的規則（Tool Policy 屬 issue #39，擴展階段）。
package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// recordingTool 是可控的測試 Tool：記下被執行過幾次，並照指定的結果回報。
type recordingTool struct {
	name   string
	calls  int
	result core.ToolResult
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "測試用 Tool" }
func (t *recordingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *recordingTool) Execute(context.Context, string) core.ToolResult {
	t.calls++
	return t.result
}

// tracingMiddleware 回傳一個把「進入」與「離開」記進 trace 的中介層。
func tracingMiddleware(label string, trace *[]string) tool.Middleware {
	return func(ctx context.Context, call core.ToolCall, next tool.ToolFunc) core.ToolResult {
		*trace = append(*trace, "進入 "+label)
		result := next(ctx, call)
		*trace = append(*trace, "離開 "+label)
		return result
	}
}

// newTestExecutor 註冊 target 並過濾出只含它的 Executor，掛上指定的中介層。
func newTestExecutor(t *testing.T, target tool.OryxTool, middlewares ...tool.Middleware) *tool.Executor {
	t.Helper()
	r := tool.NewRegistry()
	if err := r.Register(target); err != nil {
		t.Fatalf("Register: %v", err)
	}
	exec, err := r.Subset([]string{target.Name()}, nil, discardLogger(), middlewares...)
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	return exec
}

// testCall 是一次最簡單的 Tool 呼叫。
func testCall(name string) core.ToolCall {
	return core.ToolCall{ID: "call_1", Name: name, Arguments: `{}`}
}

// TestMiddlewareOnionOrder 斷言**先掛載者位於洋蔥外層**：進入順序與離開順序相反。
//
// 這條之所以要釘住，是因為它決定了日後 Tool Policy（issue #39）掛進來時，會不會
// 被排在事件播報的裡面——排錯了的話，被政策拒絕的呼叫就不會有 tool_started 事件。
func TestMiddlewareOnionOrder(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   []string
	}{
		{
			name:   "單層",
			labels: []string{"A"},
			want:   []string{"進入 A", "離開 A"},
		},
		{
			name:   "兩層：先掛的 A 在外",
			labels: []string{"A", "B"},
			want:   []string{"進入 A", "進入 B", "離開 B", "離開 A"},
		},
		{
			name:   "三層",
			labels: []string{"A", "B", "C"},
			want:   []string{"進入 A", "進入 B", "進入 C", "離開 C", "離開 B", "離開 A"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var trace []string
			mws := make([]tool.Middleware, 0, len(tt.labels))
			for _, label := range tt.labels {
				mws = append(mws, tracingMiddleware(label, &trace))
			}
			target := &recordingTool{name: "probe", result: core.ToolResult{OK: true, Content: "done"}}
			exec := newTestExecutor(t, target, mws...)

			result := exec.Execute(context.Background(), testCall("probe"))

			if !result.OK || result.Content != "done" {
				t.Errorf("結果 = %+v, 期望底層 Tool 的成功結果", result)
			}
			if target.calls != 1 {
				t.Errorf("底層 Tool 執行次數 = %d, 期望 1", target.calls)
			}
			if strings.Join(trace, ",") != strings.Join(tt.want, ",") {
				t.Errorf("進出軌跡 = %v, 期望 %v", trace, tt.want)
			}
		})
	}
}

// TestMiddlewareRejectSkipsTool 斷言**拒絕語義**：一個不呼叫下一層、直接回錯誤結果的
// 中介層之下，底層 Tool 完全沒有被執行。
//
// 這一格是「單層而非兩層」那個決定的驗收（issue #38 第七項）：不需要獨立的前置攔截
// 型別，拒絕用「不呼叫 next」就表達得完整。
func TestMiddlewareRejectSkipsTool(t *testing.T) {
	target := &recordingTool{name: "probe", result: core.ToolResult{OK: true, Content: "不該看到這個"}}
	reject := func(context.Context, core.ToolCall, tool.ToolFunc) core.ToolResult {
		return core.ToolResult{Error: "這個 Agent 不被允許執行 probe"}
	}
	exec := newTestExecutor(t, target, reject)

	result := exec.Execute(context.Background(), testCall("probe"))

	if target.calls != 0 {
		t.Errorf("底層 Tool 執行次數 = %d, 期望 0（被拒絕的呼叫不得抵達 Tool）", target.calls)
	}
	if result.OK {
		t.Errorf("結果 = %+v, 期望失敗", result)
	}
	if !strings.Contains(result.Error, "不被允許") {
		t.Errorf("錯誤 = %q, 期望帶中介層給的拒絕理由", result.Error)
	}
}

// TestMiddlewarePanicBecomesToolFailure 斷言中介層 panic 被收成**該次 Tool 的失敗
// 結果**，而不是往上炸掉整個對話。
//
// repo 裡沒有任何其他 recover()，所以這道保護不存在的話，一個第三方中介層的
// 空指標就會在對話中途直接殺掉 CLI。
func TestMiddlewarePanicBecomesToolFailure(t *testing.T) {
	tests := []struct {
		name       string
		middleware tool.Middleware
		wantCalls  int
	}{
		{
			name: "在呼叫下一層之前 panic",
			middleware: func(context.Context, core.ToolCall, tool.ToolFunc) core.ToolResult {
				panic("中介層炸了")
			},
			wantCalls: 0,
		},
		{
			name: "在下一層回來之後 panic",
			middleware: func(ctx context.Context, call core.ToolCall, next tool.ToolFunc) core.ToolResult {
				next(ctx, call)
				panic("收尾時炸了")
			},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &recordingTool{name: "probe", result: core.ToolResult{OK: true, Content: "done"}}
			exec := newTestExecutor(t, target, tt.middleware)

			result := exec.Execute(context.Background(), testCall("probe"))

			if result.OK {
				t.Errorf("結果 = %+v, 期望失敗", result)
			}
			if result.Error == "" {
				t.Error("失敗結果應帶錯誤訊息，供回填給 LLM")
			}
			if result.Retryable {
				t.Error("panic 不該標記可重試：同一段程式碼再跑一次會再 panic 一次")
			}
			if target.calls != tt.wantCalls {
				t.Errorf("底層 Tool 執行次數 = %d, 期望 %d", target.calls, tt.wantCalls)
			}
		})
	}
}

// TestMiddlewarePanicDoesNotLeakSecrets 斷言 panic 的**原文不會外洩**。
//
// panic 值是任意字串，中介層寫 `panic(fmt.Sprintf("呼叫 %s 失敗", url))` 就會把一個
// 帶 api key 的 URL 交出去。它有兩條出口，這一格兩條都堵：
//
//   - **對外的 ToolResult.Error**：會被回填給 LLM、寫進 Session 歷史、隨 Session 落庫。
//     這裡完全不帶 panic 細節，所以連去敏過的形狀都不該出現。
//   - **結構化日誌**：帶細節（維運要查得到），但套用與其他落盤路徑相同的去敏規則。
func TestMiddlewarePanicDoesNotLeakSecrets(t *testing.T) {
	const secret = "SECRET123"
	const secretURL = "https://api.example.com/v1/push?api_key=" + secret

	target := &recordingTool{name: "probe", result: core.ToolResult{OK: true}}
	var logBuf bytes.Buffer
	r := tool.NewRegistry()
	if err := r.Register(target); err != nil {
		t.Fatalf("Register: %v", err)
	}
	exec, err := r.Subset([]string{"probe"}, nil, slog.New(slog.NewJSONHandler(&logBuf, nil)),
		func(context.Context, core.ToolCall, tool.ToolFunc) core.ToolResult {
			panic("呼叫 " + secretURL + " 失敗")
		})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}

	result := exec.Execute(context.Background(), testCall("probe"))

	if result.OK {
		t.Fatalf("結果 = %+v, 期望失敗", result)
	}
	if strings.Contains(result.Error, secret) {
		t.Errorf("回填給 LLM 的錯誤含密鑰: %q", result.Error)
	}
	// 不只是密鑰——整段 panic 原文都不該進對外訊息（見 Executor.invoke 的註解）。
	if strings.Contains(result.Error, "api_key") || strings.Contains(result.Error, "呼叫 ") {
		t.Errorf("回填給 LLM 的錯誤帶了 panic 原文: %q", result.Error)
	}

	logs := logBuf.String()
	if strings.Contains(logs, secret) {
		t.Errorf("結構化日誌含密鑰: %s", logs)
	}
	if !strings.Contains(logs, "tool_middleware_panic") {
		t.Errorf("日誌缺 tool_middleware_panic 記錄，維運查不到細節: %s", logs)
	}
	// 去敏是「遮掉 query」而不是「整段丟掉」：host 留著，維運才知道是打哪裡炸的。
	if !strings.Contains(logs, "api.example.com") {
		t.Errorf("日誌把 panic 細節整段丟了，維運查不出是哪個端點: %s", logs)
	}
}

// TestExecutorWithoutMiddlewareUnchanged 斷言**不掛任何中介層時行為與現況完全一致**
// ——這是本票遷移安全性的核心：既有的每一個 Subset 呼叫點都沒有傳中介層。
func TestExecutorWithoutMiddlewareUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		callName   string
		toolResult core.ToolResult
		wantOK     bool
		wantErrSub string
	}{
		{
			name:       "成功結果原樣回傳",
			callName:   "probe",
			toolResult: core.ToolResult{OK: true, Content: "done"},
			wantOK:     true,
		},
		{
			name:       "失敗結果原樣回傳",
			callName:   "probe",
			toolResult: core.ToolResult{Error: "壞了", Retryable: true},
			wantErrSub: "壞了",
		},
		{
			name:       "不在子集的 Tool 回既有錯誤訊息",
			callName:   "nobody",
			toolResult: core.ToolResult{OK: true},
			wantErrSub: "不在此 Agent 的可用 Tool 子集",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &recordingTool{name: "probe", result: tt.toolResult}
			exec := newTestExecutor(t, target)

			result := exec.Execute(context.Background(), testCall(tt.callName))

			if result.OK != tt.wantOK {
				t.Errorf("OK = %v, 期望 %v", result.OK, tt.wantOK)
			}
			if tt.wantErrSub != "" && !strings.Contains(result.Error, tt.wantErrSub) {
				t.Errorf("錯誤 = %q, 期望含 %q", result.Error, tt.wantErrSub)
			}
			if tt.wantOK && result.Content != tt.toolResult.Content {
				t.Errorf("內容 = %q, 期望 %q", result.Content, tt.toolResult.Content)
			}
			if result.Retryable != tt.toolResult.Retryable && tt.callName == "probe" {
				t.Errorf("Retryable = %v, 期望 %v（中介層不該改寫既有欄位）", result.Retryable, tt.toolResult.Retryable)
			}
		})
	}
}

// TestMiddlewareIsPerProfile 斷言中介層是 **Profile 級**的：同一個 Registry 過濾出的
// 兩個 Executor 各掛各的，互不影響。
//
// 「這個 Agent 的 Tool 要不要審批」本來就該因 Agent 而異，掛在全域註冊表上就做不到；
// 這一格是那個決定的驗收。
func TestMiddlewareIsPerProfile(t *testing.T) {
	target := &recordingTool{name: "probe", result: core.ToolResult{OK: true, Content: "done"}}
	r := tool.NewRegistry()
	if err := r.Register(target); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var traceA []string
	execA, err := r.Subset([]string{"probe"}, nil, discardLogger(), tracingMiddleware("A", &traceA))
	if err != nil {
		t.Fatalf("Subset A: %v", err)
	}
	var traceB []string
	execB, err := r.Subset([]string{"probe"}, nil, discardLogger(), tracingMiddleware("B", &traceB))
	if err != nil {
		t.Fatalf("Subset B: %v", err)
	}

	execA.Execute(context.Background(), testCall("probe"))

	if len(traceA) == 0 {
		t.Error("Profile A 的中介層沒有被觸發")
	}
	if len(traceB) != 0 {
		t.Errorf("Profile B 的中介層被 A 的執行觸發了: %v", traceB)
	}

	execB.Execute(context.Background(), testCall("probe"))

	if len(traceB) == 0 {
		t.Error("Profile B 的中介層沒有被觸發")
	}
	if target.calls != 2 {
		t.Errorf("底層 Tool 執行次數 = %d, 期望 2（兩個 Executor 共用同一個 Tool 實例）", target.calls)
	}
}

// TestEventMiddlewareEmitsPair 斷言事件中介層播報 tool_started 與 tool_finished
// **成對出現**，且 tool_finished 的 OK 反映該次執行結果。
//
// 這是 package 內的形狀驗證；「經 AgentService.Process 驅動」那半在
// internal/core/agent_tool_event_test.go，兩邊測的是同一件事的不同層。
func TestEventMiddlewareEmitsPair(t *testing.T) {
	tests := []struct {
		name   string
		result core.ToolResult
		wantOK bool
	}{
		{name: "成功", result: core.ToolResult{OK: true, Content: "done"}, wantOK: true},
		{name: "失敗", result: core.ToolResult{Error: "壞了"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &recordingTool{name: "probe", result: tt.result}
			sink := &sliceSink{}
			exec := newTestExecutor(t, target, tool.NewEventMiddleware(sink, discardLogger()))

			exec.Execute(context.Background(), testCall("probe"))

			if len(sink.events) != 2 {
				t.Fatalf("事件數 = %d, 期望 2: %+v", len(sink.events), sink.events)
			}
			if sink.events[0].Kind != core.EventToolStarted {
				t.Errorf("events[0].Kind = %q, 期望 tool_started", sink.events[0].Kind)
			}
			if sink.events[1].Kind != core.EventToolFinished {
				t.Errorf("events[1].Kind = %q, 期望 tool_finished", sink.events[1].Kind)
			}
			if sink.events[1].OK != tt.wantOK {
				t.Errorf("tool_finished.OK = %v, 期望 %v", sink.events[1].OK, tt.wantOK)
			}
			for i, e := range sink.events {
				if e.ToolName != "probe" {
					t.Errorf("events[%d].ToolName = %q, 期望 probe", i, e.ToolName)
				}
			}
		})
	}
}

// TestEventMiddlewareEmitsPairOnPanic 斷言底層 panic 時 tool_finished **仍然播報**、
// 且 OK 為 false。
//
// 成對是消費端（CLI 進度、日後的 Web Service）依賴的性質：只進不出的話，畫面上那個
// 「正在執行 xxx」會永遠停在那裡。
func TestEventMiddlewareEmitsPairOnPanic(t *testing.T) {
	target := &recordingTool{name: "probe"}
	sink := &sliceSink{}
	boom := func(context.Context, core.ToolCall, tool.ToolFunc) core.ToolResult {
		panic("底層炸了")
	}
	// 事件層在外、會 panic 的那層在內：panic 穿過事件層往外傳，defer 仍要播報。
	exec := newTestExecutor(t, target, tool.NewEventMiddleware(sink, discardLogger()), boom)

	result := exec.Execute(context.Background(), testCall("probe"))

	if result.OK {
		t.Errorf("結果 = %+v, 期望失敗", result)
	}
	if len(sink.events) != 2 {
		t.Fatalf("事件數 = %d, 期望 2（成對）: %+v", len(sink.events), sink.events)
	}
	if sink.events[1].Kind != core.EventToolFinished || sink.events[1].OK {
		t.Errorf("events[1] = %+v, 期望 OK 為 false 的 tool_finished", sink.events[1])
	}
}

// sliceSink 是最簡單的記錄型 EventSink。Executor 逐一執行 Tool、不並行
// （spec #5 的 Out of Scope 明列），所以這裡不需要鎖。
type sliceSink struct {
	events []core.Event
}

func (s *sliceSink) Emit(_ context.Context, e core.Event) {
	s.events = append(s.events, e)
}
