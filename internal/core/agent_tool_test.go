// Tool 接入的整合測試：一樣從 AgentService.Process seam 驅動，LLM 以 httptest
// 回放（ADR-0002）；Tool 的目標端點用另一個 httptest.Server 充當真實 HTTP 依賴
// （憲法 4.3），ToolRegistry、SandboxChecker、HTTP Tool 在 seam 之下用真的。
package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// newToolAgent 組出帶 Tool 的 AgentService：真實 ToolRegistry 顯式註冊
// http_get、http_post，按 subset（Profile tools 欄位）過濾，白名單為 allowed；
// Session 儲存用落在 t.TempDir() 的真實 SQLite。
func newToolAgent(t *testing.T, baseURL string, profile *core.Profile, subset, allowed []string) *core.AgentService {
	t.Helper()
	return newToolAgentOn(t, baseURL, profile, subset, allowed, discardLogger(), newStore(t))
}

// newToolAgentOn 是 newToolAgent 的完整形式：另可指定 Tool 執行日誌的去向
// （斷言重試次數）與 Session 儲存（斷言落庫）。
//
// extra 是要與內建 Tool 一起註冊進**同一個** Registry 的額外 Tool——原生 Go Tool 示例
// 走這條（agent_plugin_tool_test.go）。共用這個 helper 不只是省行數：本票要證的正是
// 「方式三的組裝與斷言形狀與內建 Tool 完全一樣」，示例若需要自己一套 helper 才組得
// 起來，那句話就先破了。
func newToolAgentOn(t *testing.T, baseURL string, profile *core.Profile, subset, allowed []string,
	logger *slog.Logger, st *testStore, extra ...tool.OryxTool) *core.AgentService {
	t.Helper()
	// 不關心檔案的測試拿到一個空的 Workspace：File Tool 照樣註冊（它是內建 Tool，
	// 這條鏈路不因為某個測試不用它就少一段），只是白名單為空、全部拒絕。
	root, _ := newTestWorkspace(t)
	return newToolAgentIn(t, baseURL, profile, subset,
		tool.SandboxConfig{AllowedDomains: allowed}, root, logger, st, extra...)
}

// newToolAgentIn 是 newToolAgentOn 的完整形式：Sandbox 三段設定與 Workspace 根都由
// 呼叫端給。File Tool 的測試走這條——它要一個**真的**放了檔案的 Workspace。
func newToolAgentIn(t *testing.T, baseURL string, profile *core.Profile, subset []string,
	sandbox tool.SandboxConfig, root *os.Root, logger *slog.Logger, st *testStore,
	extra ...tool.OryxTool) *core.AgentService {
	t.Helper()
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(sandbox), root); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	for _, et := range extra {
		if err := r.Register(et); err != nil {
			t.Fatalf("註冊 %s: %v", et.Name(), err)
		}
	}
	exec, err := r.Subset(subset, nil, logger)
	if err != nil {
		t.Fatalf("Subset(%v): %v", subset, err)
	}
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(profile, svc, exec, newMemory(t, st.sessions()), st.audit, noBootstrap(t), discardLogger())
}

// newRecordingReplayServer 同 newReplayServer，另記錄每次 LLM 請求的 body，
// 供斷言 LLM 邊界上的 tools 列表與 tool 訊息（外部行為，不是內部呼叫序列）。
func newRecordingReplayServer(t *testing.T, bodies *[][]byte, fixtures ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("讀取 LLM 請求 body: %v", err)
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		idx := served
		served++
		mu.Unlock()
		if idx >= len(fixtures) {
			t.Errorf("LLM 請求數超出錄製回應數 %d", len(fixtures))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[idx]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// llmRequest 是斷言 LLM 請求用的最小反序列化形狀。
type llmRequest struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func parseLLMRequest(t *testing.T, body []byte) llmRequest {
	t.Helper()
	var req llmRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析 LLM 請求 %q: %v", body, err)
	}
	return req
}

// TestProcessWeatherToolScenario 是 Demo 一主場景（單輪 tool 呼叫）：
// 「查一下北京天氣並告訴我穿什麼」——兩輪 LLM 加一輪 Tool、結果正確回填、正常終止。
func TestProcessWeatherToolScenario(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/weather" || r.URL.Query().Get("city") != "beijing" {
			t.Errorf("天氣端點收到非預期請求: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
	}))
	t.Cleanup(weather.Close)

	// 錄製 fixture 的 tool_calls 參數指向該端點（host 已加入白名單）。
	toolCallFixture := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", weather.URL)
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs, toolCallFixture, readFixture(t, "reply_weather_final.json"))
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get", "http_post"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "查一下北京天氣並告訴我穿什麼")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	const want = "北京目前 5°C、晴。建議穿厚外套或風衣，早晚溫差大，可再加一條圍巾。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}

	// Session 歷史含完整 tool_calls 序列：user → assistant(tool_calls) → tool → assistant。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "http_get" {
		t.Errorf("messages[1] 應為帶 http_get tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_weather_1" {
		t.Errorf("messages[2] 應為回應 call_weather_1 的 tool 訊息: %+v", msgs[2])
	}
	if !strings.Contains(msgs[2].Content, "temp_c") {
		t.Errorf("tool 結果未回填天氣內容: %q", msgs[2].Content)
	}
	if msgs[3].Role != core.RoleAssistant || msgs[3].Content != want {
		t.Errorf("messages[3] 應為最終回應: %+v", msgs[3])
	}

	// LLM 邊界：第二輪請求帶 assistant 的 tool_calls 與 tool 結果訊息（OpenAI 兼容協議）。
	if len(llmReqs) != 2 {
		t.Fatalf("LLM 請求數 = %d, 期望 2", len(llmReqs))
	}
	second := parseLLMRequest(t, llmReqs[1])
	last := second.Messages[len(second.Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call_weather_1" {
		t.Errorf("第二輪請求最後一條應為 tool 訊息（tool_call_id=call_weather_1）: %+v", last)
	}
	prev := second.Messages[len(second.Messages)-2]
	if prev.Role != "assistant" || len(prev.ToolCalls) != 1 || prev.ToolCalls[0].Function.Name != "http_get" {
		t.Errorf("第二輪請求倒數第二條應為帶 tool_calls 的 assistant 訊息: %+v", prev)
	}
}

// TestProcessToolsFilteredByProfile 驗證 Profile tools 過濾生效：Registry 註冊了
// http_get 與 http_post，Profile 只列 http_get，未列出的不進 LLM 的 tool 列表。
func TestProcessToolsFilteredByProfile(t *testing.T) {
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, nil)
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	req := parseLLMRequest(t, llmReqs[0])
	var got []string
	for _, tl := range req.Tools {
		got = append(got, tl.Function.Name)
	}
	if len(got) != 1 || got[0] != "http_get" {
		t.Errorf("LLM 請求的 tools = %v, 期望 [http_get]", got)
	}
}

// TestProcessSandboxViolationBackfilled 驗證白名單外的 host 被攔截：
// SandboxViolation 作為 tool 結果回填給 LLM，循環正常收尾（不是硬錯誤）。
func TestProcessSandboxViolationBackfilled(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_tool_call.json"), // 呼叫 api.example.com，不在白名單
		readFixture(t, "reply_after_tool_error.json"),
	)
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"trusted.example.com"})
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "查一下北京天氣")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "無法存取") {
		t.Errorf("最終回應 = %q, 期望 LLM 對失敗的收尾", resp)
	}

	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != core.RoleTool || !strings.Contains(msgs[2].Content, "SandboxViolation") {
		t.Errorf("messages[2] 應為含 SandboxViolation 的 tool 訊息: %+v", msgs[2])
	}
	if !strings.Contains(msgs[2].Content, "api.example.com") {
		t.Errorf("違規訊息未含被攔截的 host: %q", msgs[2].Content)
	}
}

// TestProcessSequentialToolCalls 驗證一次回應含多個 tool 呼叫時按宣告順序執行、
// 不並行：目標端點觀察到的請求順序即宣告順序，結果各自回填對應 ToolCallID。
func TestProcessSequentialToolCalls(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(target.Close)

	fixture := strings.ReplaceAll(readFixture(t, "reply_two_tool_calls.json"), "{{TARGET_URL}}", target.URL)
	srv := newReplayServer(t, fixture, readFixture(t, "reply_weather_final.json"))
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "兩個都查"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if len(paths) != 2 || paths[0] != "/first" || paths[1] != "/second" {
		t.Errorf("目標端點收到的請求順序 = %v, 期望 [/first /second]", paths)
	}
	msgs := session.Messages
	if len(msgs) != 5 {
		t.Fatalf("歷史長度 = %d, 期望 5: %+v", len(msgs), msgs)
	}
	if msgs[2].ToolCallID != "call_a" || msgs[3].ToolCallID != "call_b" {
		t.Errorf("tool 結果順序 = [%s %s], 期望 [call_a call_b]", msgs[2].ToolCallID, msgs[3].ToolCallID)
	}
}

// TestProcessErrorAfterToolNotesSideEffects 驗證失敗 turn 的 rollback 誠實性：
// 本輪已執行過 Tool 時，錯誤訊息必須註記「外部效果不會因回退而撤銷」——
// rollback 只還原 Session 狀態，POST 這類已產生的外部副作用無法撤銷，
// 不得無條件宣稱重試安全。
func TestProcessErrorAfterToolNotesSideEffects(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"temp_c":5}`)
	}))
	t.Cleanup(weather.Close)

	// 第一輪 LLM 回 tool 呼叫（Tool 真的執行），第二輪 LLM 直接 500。
	toolCallFixture := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", weather.URL)
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(toolCallFixture))
			return
		}
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")

	_, err := agent.Process(context.Background(), session, "查天氣")
	if err == nil {
		t.Fatal("第二輪 LLM 故障應報錯, 實際成功")
	}
	if !strings.Contains(err.Error(), "外部效果不會因回退而撤銷") {
		t.Errorf("已執行過 Tool 的失敗 turn，錯誤 %q 未註記副作用不可撤銷", err.Error())
	}
	if len(session.Messages) != 0 {
		t.Errorf("失敗 turn 後 Session 殘留 %d 條訊息", len(session.Messages))
	}
}
