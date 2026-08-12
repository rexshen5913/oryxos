// 循環強健性的整合測試（ticket #6）：補齊 ADR-0002 五個 fixture 分支中的
// 多輪連續 tool 呼叫、tool 失敗重試、達 MAX_ITERATIONS 強制終止，加上
// max_history_turns 截斷與併發紀律（goroutine、取消／超時）。一律從
// AgentService.Process seam 驅動；LLM 以 httptest 回放（ADR-0002），Tool 的
// 目標端點用另一個 httptest.Server 充當真實 HTTP 依賴（憲法 4.3）。
package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
)

// newToolAgentLogged 同 newToolAgent，但 Tool 執行日誌落到 logger，供斷言
// tool_invocation 筆數——重試次數在結構化日誌上的外部可觀察行為（User Story 20）。
func newToolAgentLogged(t *testing.T, baseURL string, profile *core.Profile, subset, allowed []string, logger *slog.Logger) *core.AgentService {
	t.Helper()
	return newToolAgentOn(t, baseURL, profile, subset, allowed, logger, newStore(t))
}

// TestProcessMultiRoundToolCalls 是 ADR-0002 分支「多輪連續 tool 呼叫」：
// 兩輪 tool 各自打真實目標端點，結果按輪次累積到 Session 歷史並正常終止。
func TestProcessMultiRoundToolCalls(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/weather":
			fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
		case "/air":
			fmt.Fprint(w, `{"city":"beijing","aqi":42,"level":"良"}`)
		default:
			t.Errorf("目標端點收到非預期路徑: %s", r.URL.Path)
		}
	}))
	t.Cleanup(target.Close)

	srv := newReplayServer(t,
		strings.ReplaceAll(readFixture(t, "reply_multi_round_1.json"), "{{TARGET_URL}}", target.URL),
		strings.ReplaceAll(readFixture(t, "reply_multi_round_2.json"), "{{TARGET_URL}}", target.URL),
		readFixture(t, "reply_multi_round_final.json"),
	)
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "查北京天氣和空氣品質，給我穿衣與外出建議")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	const want = "北京目前 5°C、晴，空氣品質良（AQI 42）。適合外出，建議穿厚外套。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 2 || gotPaths[0] != "/weather" || gotPaths[1] != "/air" {
		t.Errorf("目標端點收到的請求順序 = %v, 期望 [/weather /air]", gotPaths)
	}

	// 歷史按輪次累積：user → assistant(call_m1) → tool → assistant(call_m2) → tool → assistant(final)。
	msgs := session.Messages
	if len(msgs) != 6 {
		t.Fatalf("歷史長度 = %d, 期望 6: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "call_m1" {
		t.Errorf("messages[1] 應為帶 call_m1 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_m1" || !strings.Contains(msgs[2].Content, "temp_c") {
		t.Errorf("messages[2] 應為 call_m1 的天氣結果: %+v", msgs[2])
	}
	if msgs[3].Role != core.RoleAssistant || len(msgs[3].ToolCalls) != 1 || msgs[3].ToolCalls[0].ID != "call_m2" {
		t.Errorf("messages[3] 應為帶 call_m2 的 assistant 訊息: %+v", msgs[3])
	}
	if msgs[4].Role != core.RoleTool || msgs[4].ToolCallID != "call_m2" || !strings.Contains(msgs[4].Content, "aqi") {
		t.Errorf("messages[4] 應為 call_m2 的空氣品質結果: %+v", msgs[4])
	}
	if msgs[5].Role != core.RoleAssistant || msgs[5].Content != want {
		t.Errorf("messages[5] 應為最終回應: %+v", msgs[5])
	}
}

// TestProcessToolRetry 是 ADR-0002 分支「tool 執行失敗後重試」的表格驅動測試
// （需求 8.2：可重試失敗按指數退避重試、最多三次）。重試次數以兩個外部行為
// 斷言：真實目標端點觀察到的請求數、tool_invocation 結構化日誌筆數；指數退避
// 以目標端點觀察到的相鄰請求間隔下界斷言（100→200→400ms 的指數形狀）。
func TestProcessToolRetry(t *testing.T) {
	const weatherJSON = `{"city":"beijing","temp_c":5,"condition":"晴"}`

	tests := []struct {
		name         string
		failFirst    int // 目標端點先以斷線方式失敗的次數；-1 表示永遠失敗
		llmFixtures  []string
		allowed      []string
		wantHits     int
		wantToolLogs int
		wantToolMsg  string // Session 中 tool 訊息須含此子串
		wantFinal    string
		wantMinGaps  []time.Duration // 相鄰請求間隔的下界（指數退避）
	}{
		{
			name:         "可重試失敗後重試成功",
			failFirst:    1,
			llmFixtures:  []string{"reply_weather_tool_call.json", "reply_weather_final.json"},
			allowed:      []string{"127.0.0.1"},
			wantHits:     2,
			wantToolLogs: 2,
			wantToolMsg:  "temp_c",
			wantFinal:    "北京目前 5°C、晴。建議穿厚外套或風衣，早晚溫差大，可再加一條圍巾。",
			wantMinGaps:  []time.Duration{100 * time.Millisecond},
		},
		{
			name:         "重試耗盡錯誤回填 LLM",
			failFirst:    -1,
			llmFixtures:  []string{"reply_weather_tool_call.json", "reply_retry_final.json"},
			allowed:      []string{"127.0.0.1"},
			wantHits:     4, // 首次 + 三次重試
			wantToolLogs: 4,
			wantToolMsg:  "已重試 3 次",
			wantFinal:    "抱歉，天氣服務多次連線失敗，暫時無法取得北京天氣，請稍後再試。",
			wantMinGaps:  []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond},
		},
		{
			name:         "不可重試錯誤不重試",
			failFirst:    -1,
			llmFixtures:  []string{"reply_tool_call.json", "reply_after_tool_error.json"}, // 呼叫 api.example.com
			allowed:      []string{"trusted.example.com"},                                 // 白名單外：SandboxViolation
			wantHits:     0,
			wantToolLogs: 1, // 僅執行一次，未重試
			wantToolMsg:  "SandboxViolation",
			wantFinal:    "抱歉，我無法存取該天氣服務：目標域名不在允許清單內。請把該域名加入 http.allowed_domains 後再試。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var hitTimes []time.Time
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				hitTimes = append(hitTimes, time.Now())
				n := len(hitTimes)
				mu.Unlock()
				if tt.failFirst < 0 || n <= tt.failFirst {
					// 斷線不回應：client 端得到可重試的網路錯誤。
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Error("httptest server 不支援 Hijacker")
						return
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Errorf("Hijack: %v", err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, weatherJSON)
			}))
			t.Cleanup(target.Close)

			fixtures := make([]string, 0, len(tt.llmFixtures))
			for _, name := range tt.llmFixtures {
				fixtures = append(fixtures, strings.ReplaceAll(readFixture(t, name), "{{TARGET_URL}}", target.URL))
			}
			srv := newReplayServer(t, fixtures...)
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
			agent := newToolAgentLogged(t, srv.URL, testProfile(), []string{"http_get"}, tt.allowed, logger)
			session := core.NewSession("cli", "local", "default")

			resp, err := agent.Process(context.Background(), session, "查一下北京天氣")
			if err != nil {
				t.Fatalf("Process: %v", err)
			}

			if resp != tt.wantFinal {
				t.Errorf("回應 = %q, 期望 %q", resp, tt.wantFinal)
			}
			mu.Lock()
			gotTimes := append([]time.Time(nil), hitTimes...)
			mu.Unlock()
			if len(gotTimes) != tt.wantHits {
				t.Fatalf("目標端點請求數 = %d, 期望 %d", len(gotTimes), tt.wantHits)
			}
			if got := strings.Count(logBuf.String(), `"msg":"tool_invocation"`); got != tt.wantToolLogs {
				t.Errorf("tool_invocation 日誌筆數 = %d, 期望 %d", got, tt.wantToolLogs)
			}
			for i, minGap := range tt.wantMinGaps {
				if gap := gotTimes[i+1].Sub(gotTimes[i]); gap < minGap {
					t.Errorf("第 %d 次重試與前次請求的間隔 = %v, 低於指數退避下界 %v", i+1, gap, minGap)
				}
			}

			// 重試耗盡或不可重試：錯誤作為 tool 結果回填，循環正常收尾。
			msgs := session.Messages
			if len(msgs) != 4 {
				t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
			}
			if msgs[2].Role != core.RoleTool || !strings.Contains(msgs[2].Content, tt.wantToolMsg) {
				t.Errorf("messages[2] = %+v, 期望含 %q 的 tool 訊息", msgs[2], tt.wantToolMsg)
			}
			if msgs[3].Role != core.RoleAssistant || msgs[3].Content != tt.wantFinal {
				t.Errorf("messages[3] 應為最終回應: %+v", msgs[3])
			}
		})
	}
}

// TestProcessMaxIterationsForcedTermination 是 ADR-0002 分支「達 MAX_ITERATIONS
// 強制終止」的表格驅動測試：終止回覆＝固定提示語（無進度時不帶進度段），
// settings.max_iterations 的 Profile 覆蓋與零值預設在此一併驗證。turn 不
// rollback——已執行的 tool 結果保留在 Session 歷史（完整可查）。
func TestProcessMaxIterationsForcedTermination(t *testing.T) {
	tests := []struct {
		name          string
		maxIterations int // 0 表示零值（未覆蓋，用預設 10）
		wantLLMCalls  int
	}{
		{name: "覆蓋為 1", maxIterations: 1, wantLLMCalls: 1},
		{name: "覆蓋為 2", maxIterations: 2, wantLLMCalls: 2},
		{name: "零值用預設 10", maxIterations: 0, wantLLMCalls: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 每輪都回 tool 呼叫（目標在白名單外，錯誤回填後循環繼續）；
			// 回放數量即迭代上限，超出會被回放 server 記為測試錯誤。
			fixtures := make([]string, tt.wantLLMCalls)
			for i := range fixtures {
				fixtures[i] = readFixture(t, "reply_tool_call.json")
			}
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, fixtures...)
			p := testProfile()
			p.Settings.MaxIterations = tt.maxIterations
			agent := newToolAgent(t, srv.URL, p, []string{"http_get"}, nil)
			session := core.NewSession("cli", "local", "default")

			resp, err := agent.Process(context.Background(), session, "查天氣")
			if err != nil {
				t.Fatalf("強制終止應回覆固定提示語而非報錯: %v", err)
			}

			want := fmt.Sprintf("已達最大迭代次數 %d，仍未完成任務，已強制終止。", tt.wantLLMCalls)
			if resp != want {
				t.Errorf("回應 = %q, 期望 %q", resp, want)
			}
			if len(llmReqs) != tt.wantLLMCalls {
				t.Errorf("LLM 請求數 = %d, 期望 %d", len(llmReqs), tt.wantLLMCalls)
			}

			// turn 保留：user + 每輪（assistant + tool）+ 終止回覆。
			msgs := session.Messages
			wantLen := 1 + tt.wantLLMCalls*2 + 1
			if len(msgs) != wantLen {
				t.Fatalf("歷史長度 = %d, 期望 %d", len(msgs), wantLen)
			}
			last := msgs[len(msgs)-1]
			if last.Role != core.RoleAssistant || last.Content != want {
				t.Errorf("歷史最後一條應為終止回覆: %+v", last)
			}
		})
	}
}

// TestProcessMaxIterationsTerminationIncludesProgress 固定強制終止回覆的
// 「已知進度」段：最後一輪 LLM 內容非空時附在固定提示語之後。
func TestProcessMaxIterationsTerminationIncludesProgress(t *testing.T) {
	fixture := readFixture(t, "reply_tool_call_with_progress.json")
	srv := newReplayServer(t, fixture, fixture)
	p := testProfile()
	p.Settings.MaxIterations = 2
	agent := newToolAgent(t, srv.URL, p, []string{"http_get"}, nil)
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "查天氣和空氣品質")
	if err != nil {
		t.Fatalf("強制終止應回覆固定提示語而非報錯: %v", err)
	}

	const want = "已達最大迭代次數 2，仍未完成任務，已強制終止。最後一輪進度：已取得北京天氣資料，接下來查詢空氣品質。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}
}

// TestProcessHistoryTruncation 是 max_history_turns 截斷的表格驅動測試：
// 進 prompt 的歷史只留近期 N 輪（一輪自一條 user 訊息起算）、system prompt
// 永遠保留、截斷點落在 user 訊息邊界；Profile 覆蓋與零值預設一併驗證。
// 斷言對象是 LLM 邊界上實際送出的請求（外部行為），Session 歷史本身不截斷。
func TestProcessHistoryTruncation(t *testing.T) {
	tests := []struct {
		name            string
		maxHistoryTurns int // 0 表示零值（未覆蓋，用預設 20）
		priorTurns      int // 本輪之前已完成的對話輪數
		wantUsers       int // 最後一輪請求中 user 訊息數
		wantFirstUser   string
	}{
		{name: "覆蓋為 1 僅當前輪", maxHistoryTurns: 1, priorTurns: 3, wantUsers: 1, wantFirstUser: "msg-4"},
		{name: "覆蓋為 3 留近期三輪", maxHistoryTurns: 3, priorTurns: 4, wantUsers: 3, wantFirstUser: "msg-3"},
		{name: "不足 N 輪不截斷", maxHistoryTurns: 3, priorTurns: 1, wantUsers: 2, wantFirstUser: "msg-1"},
		{name: "零值用預設 20", maxHistoryTurns: 0, priorTurns: 21, wantUsers: 20, wantFirstUser: "msg-3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total := tt.priorTurns + 1
			fixtures := make([]string, total)
			for i := range fixtures {
				fixtures[i] = readFixture(t, "reply_direct.json")
			}
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, fixtures...)
			p := testProfile()
			p.Settings.MaxHistoryTurns = tt.maxHistoryTurns
			svc := provider.NewService(map[string]provider.Config{
				"openai": {APIKey: "test-key", BaseURL: srv.URL},
			}, discardLogger())
			agent := newAgentWithProfile(t, p, svc)
			session := core.NewSession("cli", "local", "default")

			for i := 1; i <= total; i++ {
				if _, err := agent.Process(context.Background(), session, fmt.Sprintf("msg-%d", i)); err != nil {
					t.Fatalf("第 %d 輪 Process: %v", i, err)
				}
			}

			req := parseLLMRequest(t, llmReqs[len(llmReqs)-1])
			if req.Messages[0].Role != "system" || req.Messages[0].Content != "你是 Oryx。" {
				t.Errorf("messages[0] 應為 system prompt: %+v", req.Messages[0])
			}
			if req.Messages[1].Role != "user" {
				t.Errorf("截斷點應落在 user 訊息邊界，messages[1] = %+v", req.Messages[1])
			}
			var users []string
			for _, m := range req.Messages {
				if m.Role == "user" {
					users = append(users, m.Content)
				}
			}
			if len(users) != tt.wantUsers {
				t.Errorf("user 訊息數 = %d, 期望 %d: %v", len(users), tt.wantUsers, users)
			}
			if len(users) > 0 && users[0] != tt.wantFirstUser {
				t.Errorf("最早保留的 user 訊息 = %q, 期望 %q", users[0], tt.wantFirstUser)
			}
			// 保留區間 = user/assistant 交錯的完整近期輪：system + N user + (N-1) assistant。
			if wantLen := 2 * tt.wantUsers; len(req.Messages) != wantLen {
				t.Errorf("請求訊息總數 = %d, 期望 %d", len(req.Messages), wantLen)
			}
			// Session 歷史本身不截斷（完整可查）。
			if got := len(session.Messages); got != total*2 {
				t.Errorf("Session 歷史長度 = %d, 期望 %d（不因截斷丟失）", got, total*2)
			}
		})
	}
}

// TestProcessCancelDuringRetryBackoff 驗證重試的退避等待走 context：目標端點
// 連線被拒（可重試），ctx 在首次退避期間逾時，循環立即中斷、不睡滿退避階梯。
func TestProcessCancelDuringRetryBackoff(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	targetURL := target.URL
	target.Close() // 立即關閉：連線被拒 → 可重試的網路錯誤

	fixture := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", targetURL)
	srv := newReplayServer(t, fixture)
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := agent.Process(ctx, session, "查天氣")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ctx 逾時應中斷循環, 實際成功")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("錯誤鏈 %v 未含 context.DeadlineExceeded", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("取消後 %v 才返回，退避等待未被 ctx 中斷", elapsed)
	}
	if len(session.Messages) != 0 {
		t.Errorf("失敗 turn 後 Session 殘留 %d 條訊息", len(session.Messages))
	}
}

// TestProcessTimeoutDuringToolCall 驗證 Tool 的 HTTP 阻塞路徑走 context：
// 目標端點拖延回應時，ctx 逾時即中斷 Tool 請求與循環，不等目標回應。
func TestProcessTimeoutDuringToolCall(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond) // 拖過 ctx 的 50ms deadline
	}))
	t.Cleanup(target.Close)

	fixture := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", target.URL)
	srv := newReplayServer(t, fixture)
	agent := newToolAgent(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"})
	session := core.NewSession("cli", "local", "default")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := agent.Process(ctx, session, "查天氣")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ctx 逾時應中斷循環, 實際成功")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("錯誤鏈 %v 未含 context.DeadlineExceeded", err)
	}
	if elapsed >= 250*time.Millisecond {
		t.Errorf("ctx 50ms 逾時但 %v 才返回，Tool 阻塞路徑未被中斷", elapsed)
	}
}

// TestProcessNoGoroutineAccumulation 驗證 goroutine 數不隨請求累積
// （需求 §12 風險應對、憲法 5.3）：暖機後連跑多輪，goroutine 數回到基線附近。
func TestProcessNoGoroutineAccumulation(t *testing.T) {
	const warmup, rounds = 3, 20
	fixtures := make([]string, warmup+rounds)
	for i := range fixtures {
		fixtures[i] = readFixture(t, "reply_direct.json")
	}
	srv := newReplayServer(t, fixtures...)
	agent := newAgent(t, srv.URL, discardLogger())
	session := core.NewSession("cli", "local", "default")
	ctx := context.Background()

	// 暖機：讓 HTTP 連線池等一次性 goroutine 就位後再取基線。
	for range warmup {
		if _, err := agent.Process(ctx, session, "你好"); err != nil {
			t.Fatalf("暖機 Process: %v", err)
		}
	}
	baseline := runtime.NumGoroutine()

	for i := range rounds {
		if _, err := agent.Process(ctx, session, "你好"); err != nil {
			t.Fatalf("第 %d 輪 Process: %v", i+1, err)
		}
	}

	// 留時間讓已完成請求的 goroutine 退場，再斷言未累積。
	const slack = 3
	deadline := time.Now().Add(2 * time.Second)
	got := runtime.NumGoroutine()
	for got > baseline+slack && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		got = runtime.NumGoroutine()
	}
	if got > baseline+slack {
		t.Errorf("goroutine 隨請求累積：基線 %d、%d 輪後 %d", baseline, rounds, got)
	}
}
