// 事件流的整合測試（ticket #47）：記錄型 EventSink 掛在既有 AgentService.Process
// seam 上當**觀測面**——與既有測試用記錄型伺服器觀測 Provider 請求是同一個手法，
// 不是新的驅動點。LLM 以 httptest 回放錄製回應（ADR-0002），Session 儲存、審計、
// Tool 在 seam 之下一律用真的（憲法 4.3）。
//
// **本檔組出的 Agent 一律不掛任何 Tool 中介層**，所以序列裡只會有循環側播報的五種
// 事件。這不是為了避開下一張票（#52），而是它要釘的正是「循環側的事件序列不因為
// 中介層掛不掛而變」；#52 的「拿掉中介層則沒有 Tool 事件」是同一條性質的另一面。
package core_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// recordingSink 是記錄型 EventSink：把播報過的事件原樣留下供斷言。
//
// 帶鎖是因為它是**出向介面的實作**，而介面沒有承諾呼叫端一定在單一 goroutine 裡
// 播報；測試的實作不該比契約更寬。
type recordingSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (s *recordingSink) Emit(_ context.Context, e core.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// snapshot 回傳已播報事件的副本。
func (s *recordingSink) snapshot() []core.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Event(nil), s.events...)
}

// kinds 回傳已播報事件的種類序列——本票斷言的主要對象是**種類與順序**，
// 不是每個欄位的精確值（後者會把測試綁死在實作細節上）。
func (s *recordingSink) kinds() []core.EventKind {
	events := s.snapshot()
	out := make([]core.EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// only 篩出某一種事件，供斷言該種類的欄位（如 iteration 序號、重試次數）。
func (s *recordingSink) only(kind core.EventKind) []core.Event {
	var out []core.Event
	for _, e := range s.snapshot() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// panicSink 是每次播報都 panic 的 EventSink：驗證壞掉的展現層不拖垮引擎。
type panicSink struct{}

func (panicSink) Emit(context.Context, core.Event) {
	panic("展現層炸了")
}

// newEventAgent 組出無 Tool、事件送進 sink 的 AgentService。
//
// 刻意不改既有的 newAgentOn 一族：那些 helper 服務的測試不關心事件，讓它們多帶一個
// 參數只會讓每一支都出現一個看起來像漏填了什麼的 NopEventSink。
func newEventAgent(t *testing.T, baseURL string, sink core.EventSink) *core.AgentService {
	t.Helper()
	return newEventAgentOn(t, baseURL, sink, newStore(t), discardLogger())
}

// newEventAgentOn 是 newEventAgent 的完整形式：另可指定 Session 儲存（斷言持久化
// 失敗）與引擎日誌的去向（斷言播報端吸收 panic 時記了什麼）。
func newEventAgentOn(t *testing.T, baseURL string, sink core.EventSink,
	st *testStore, logger *slog.Logger) *core.AgentService {
	t.Helper()
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(testProfile(), svc, noTools(t), newMemory(t, st.sessions()),
		st.audit, noBootstrap(t), sink, nil, logger)
}

// newEventToolAgent 組出帶內建 Tool（按 subset 過濾）、事件送進 sink 的 AgentService。
// logger 收 Tool 執行的結構化日誌，供斷言實際執行次數。
//
// middlewares 掛在 Profile 過濾出的那個 Executor 上。**本檔一律不傳**，所以序列裡
// 不會有 Tool 事件；ticket #52 的整合測試傳事件中介層進來，兩邊的差別剛好就是那兩種
// 事件（見 agent_tool_event_test.go）。
func newEventToolAgent(t *testing.T, baseURL string, subset, allowed []string,
	sink core.EventSink, logger *slog.Logger, middlewares ...tool.Middleware) *core.AgentService {
	t.Helper()
	root, _ := newTestWorkspace(t)
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: allowed}),
		root, testShellRuntime(t, root.Name()), tool.NewShellLimiter()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	exec, err := r.Subset(subset, nil, logger, middlewares...)
	if err != nil {
		t.Fatalf("Subset(%v): %v", subset, err)
	}
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	st := newStore(t)
	return core.NewAgentService(testProfile(), svc, exec, newMemory(t, st.sessions()),
		st.audit, noBootstrap(t), sink, nil, discardLogger())
}

// TestEventKindsComplete 釘住「列舉七種一次定義完整」：#47 播報其中五種、#52 播報
// 另外兩種，但型別只定義一次。分兩次改會讓同一個型別被兩張票各動一次，而中間那個
// 形狀不是定案。
//
// 值本身也一併釘住：事件種類會被 CLI 與日後的 Web Service 當成線路上的字串消費，
// 改一個字就是一次對外破壞。
func TestEventKindsComplete(t *testing.T) {
	tests := []struct {
		kind core.EventKind
		want string
	}{
		{core.EventTurnStarted, "turn_started"},
		{core.EventIteration, "iteration"},
		{core.EventToolStarted, "tool_started"},
		{core.EventToolFinished, "tool_finished"},
		{core.EventToolRetrying, "tool_retrying"},
		{core.EventAssistantText, "assistant_text"},
		{core.EventTurnFinished, "turn_finished"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.kind) != tt.want {
				t.Errorf("EventKind = %q, 期望 %q", tt.kind, tt.want)
			}
		})
	}
}

// TestProcessEmitsEventSequence 斷言一個 turn 播報出來的**事件種類與順序**。
//
// 兩格的差別只有中間多不多一輪 Tool 呼叫，序列因此看得出 iteration 是跟著 Provider
// 呼叫走的：一次呼叫一個 iteration 事件，不是一個 turn 一個。
func TestProcessEmitsEventSequence(t *testing.T) {
	tests := []struct {
		name      string
		fixtures  []string
		subset    []string
		userInput string
		want      []core.EventKind
	}{
		{
			name:      "直接回覆",
			fixtures:  []string{"reply_direct.json"},
			userInput: "你好",
			want: []core.EventKind{
				core.EventTurnStarted,
				core.EventIteration,
				core.EventAssistantText,
				core.EventTurnFinished,
			},
		},
		{
			name:      "一輪 Tool 呼叫後回覆",
			fixtures:  []string{"reply_weather_tool_call.json", "reply_weather_final.json"},
			subset:    []string{"http_get"},
			userInput: "查一下北京天氣",
			// 第一次 LLM 回應只有 tool_calls、content 為空，因此沒有 assistant_text：
			// 播報的是 LLM **產出過**的文字，空字串不是產出。
			want: []core.EventKind{
				core.EventTurnStarted,
				core.EventIteration,
				core.EventIteration,
				core.EventAssistantText,
				core.EventTurnFinished,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
			}))
			t.Cleanup(target.Close)

			fixtures := make([]string, 0, len(tt.fixtures))
			for _, name := range tt.fixtures {
				fixtures = append(fixtures, strings.ReplaceAll(readFixture(t, name), "{{TARGET_URL}}", target.URL))
			}
			srv := newReplayServer(t, fixtures...)

			sink := &recordingSink{}
			var agent *core.AgentService
			if len(tt.subset) == 0 {
				agent = newEventAgent(t, srv.URL, sink)
			} else {
				agent = newEventToolAgent(t, srv.URL, tt.subset, []string{"127.0.0.1"}, sink, discardLogger())
			}
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, tt.userInput); err != nil {
				t.Fatalf("Process: %v", err)
			}

			assertKinds(t, sink.kinds(), tt.want)
		})
	}
}

// TestProcessEventsCarrySessionAndTime 斷言每則事件都帶得出「是哪一場對話、什麼時候」
// ——這兩個欄位是 Web Service 日後要把事件分流給對的連線的依據，缺了就得回頭改循環。
func TestProcessEventsCarrySessionAndTime(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	sink := &recordingSink{}
	agent := newEventAgent(t, srv.URL, sink)
	session := core.NewSession("cli", "local", "default")

	before := time.Now()
	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	after := time.Now()

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatal("沒有播報任何事件")
	}
	for i, e := range events {
		if e.SessionID != session.ID {
			t.Errorf("events[%d](%s).SessionID = %q, 期望 %q", i, e.Kind, e.SessionID, session.ID)
		}
		if e.At.Before(before) || e.At.After(after) {
			t.Errorf("events[%d](%s).At = %v, 不在本次 Process 的時間區間內", i, e.Kind, e.At)
		}
	}
}

// TestProcessIterationNumbersMatchProviderCalls 斷言 iteration 事件序號遞增，
// 且筆數與**實際的 Provider 呼叫次數**一致——後者由記錄型伺服器獨立觀測，
// 不是拿事件自己驗自己。
func TestProcessIterationNumbersMatchProviderCalls(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	var bodies [][]byte
	srv := newRecordingReplayServer(t, &bodies,
		strings.ReplaceAll(readFixture(t, "reply_multi_round_1.json"), "{{TARGET_URL}}", target.URL),
		strings.ReplaceAll(readFixture(t, "reply_multi_round_2.json"), "{{TARGET_URL}}", target.URL),
		readFixture(t, "reply_multi_round_final.json"),
	)

	sink := &recordingSink{}
	agent := newEventToolAgent(t, srv.URL, []string{"http_get"}, []string{"127.0.0.1"}, sink, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "查北京天氣和空氣品質"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	iterations := sink.only(core.EventIteration)
	if len(iterations) != len(bodies) {
		t.Fatalf("iteration 事件數 = %d, 實際 Provider 呼叫數 = %d", len(iterations), len(bodies))
	}
	for i, e := range iterations {
		if e.Iteration != i+1 {
			t.Errorf("iterations[%d].Iteration = %d, 期望 %d（自 1 起遞增）", i, e.Iteration, i+1)
		}
	}
}

// TestProcessEmitsToolRetrying 斷言 Tool 失敗重試時播報 tool_retrying，且帶的次數
// 與實際重試次數一致——「慢是因為在重試」正是這則事件要讓使用者看見的東西。
func TestProcessEmitsToolRetrying(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 一律斷線不回應：client 端得到可重試的網路錯誤，重試到耗盡。
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
	}))
	t.Cleanup(target.Close)

	srv := newReplayServer(t,
		strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", target.URL),
		readFixture(t, "reply_retry_final.json"),
	)

	sink := &recordingSink{}
	agent := newEventToolAgent(t, srv.URL, []string{"http_get"}, []string{"127.0.0.1"}, sink, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	retrying := sink.only(core.EventToolRetrying)
	// 首次執行不算重試，之後三次各播報一則（需求 8.2：最多三次）。
	if len(retrying) != 3 {
		t.Fatalf("tool_retrying 事件數 = %d, 期望 3", len(retrying))
	}
	for i, e := range retrying {
		if e.Iteration != i+1 {
			t.Errorf("retrying[%d].Iteration = %d, 期望 %d（第幾次重試）", i, e.Iteration, i+1)
		}
		if e.ToolName != "http_get" {
			t.Errorf("retrying[%d].ToolName = %q, 期望 http_get", i, e.ToolName)
		}
	}
}

// TestProcessFailedTurnStillEmits 斷言 turn 失敗時 turn_finished 的 OK 為 false，
// 且 **rollback 之後事件仍然播報過**——事件記的是發生過的事，不隨 Session rollback
// 撤銷。這是事件流與 Session 歷史語義不同的地方，值得單獨釘住。
func TestProcessFailedTurnStillEmits(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_no_choice.json"))
	sink := &recordingSink{}
	agent := newEventAgent(t, srv.URL, sink)
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "你好"); err == nil {
		t.Fatal("Process 應該失敗（回應沒有 choices）")
	}

	if len(session.Messages) != 0 {
		t.Errorf("rollback 後歷史長度 = %d, 期望 0: %+v", len(session.Messages), session.Messages)
	}

	assertKinds(t, sink.kinds(), []core.EventKind{
		core.EventTurnStarted,
		core.EventIteration,
		core.EventTurnFinished,
	})
	finished := sink.only(core.EventTurnFinished)
	if len(finished) != 1 {
		t.Fatalf("turn_finished 事件數 = %d, 期望 1", len(finished))
	}
	if finished[0].OK {
		t.Error("失敗的 turn，turn_finished.OK 應為 false")
	}
}

// TestProcessSurvivesPanickingEventSink 斷言壞掉的展現層不拖垮引擎：每次播報都
// panic 的 Sink 之下，Process 照樣回傳正確的最終回應。
//
// 這條規則寫在介面文件裡（旁路不得中斷對話），但文件擋不住一個會 panic 的實作，
// 所以吸收 panic 的責任在播報端。
func TestProcessSurvivesPanickingEventSink(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	agent := newEventAgent(t, srv.URL, panicSink{})
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	const want = "你好！我是 Oryx，很高興為你服務。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}
	if len(session.Messages) != 2 {
		t.Errorf("歷史長度 = %d, 期望 2（user ＋ assistant）: %+v", len(session.Messages), session.Messages)
	}
}

// TestProcessPersistenceFailureStillEmits 斷言 **turn 的邊界在 AgentService.Process
// 而不是 ReActLoop.Run**：ReAct 循環整個跑成功了，但持久化失敗，這一樣是失敗的 turn，
// 一樣要播報 OK 為 false 的 turn_finished、一樣要 rollback。
//
// 這一格是 turn_started／turn_finished 播報位置的**唯一**證據。上面那格
// （TestProcessFailedTurnStillEmits）的失敗發生在循環**內部**，播報點就算被挪進
// ReActLoop.Run 它也照樣綠；只有持久化這條路徑分得出兩者。
//
// 讓儲存失敗的方式是**關掉那個真實的 SQLite**（憲法 4.3：可確定化的依賴用真的），
// 與既有跨重啟測試模擬進程結束是同一個手法，不是塞一個會回錯誤的假物件。
func TestProcessPersistenceFailureStillEmits(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	st := newStore(t)
	sink := &recordingSink{}
	agent := newEventAgentOn(t, srv.URL, sink, st, discardLogger())
	session := core.NewSession("cli", "local", "default")

	// 引擎組好之後才關：Session 儲存與審計都指向這個已經關掉的 db。
	if err := st.close(); err != nil {
		t.Fatalf("關閉 Workspace 資料庫: %v", err)
	}

	_, err := agent.Process(context.Background(), session, "你好")
	if err == nil {
		t.Fatal("Process 應該失敗（Session 寫不進已關閉的儲存）")
	}
	if !strings.Contains(err.Error(), "持久化 Session") {
		t.Errorf("錯誤 = %v, 期望指出是持久化失敗", err)
	}
	if len(session.Messages) != 0 {
		t.Errorf("rollback 後歷史長度 = %d, 期望 0: %+v", len(session.Messages), session.Messages)
	}

	// LLM 那一輪是成功的，所以 assistant_text 播報過——失敗只發生在循環之外。
	assertKinds(t, sink.kinds(), []core.EventKind{
		core.EventTurnStarted,
		core.EventIteration,
		core.EventAssistantText,
		core.EventTurnFinished,
	})
	finished := sink.only(core.EventTurnFinished)
	if len(finished) != 1 {
		t.Fatalf("turn_finished 事件數 = %d, 期望 1", len(finished))
	}
	if finished[0].OK {
		t.Error("持久化失敗的 turn，turn_finished.OK 應為 false")
	}
}

// TestEmitEventPanicLogRedacted 斷言播報端吸收 sink 的 panic 時，寫進日誌的 panic
// 原文**套用與其他落盤路徑相同的去敏規則**。
//
// panic 值是任意字串：一個實作寫 `panic("送不出去: " + url)` 就會讓密鑰進到日誌，
// 而日誌是落盤路徑，落盤路徑一律套同一套規則。
func TestEmitEventPanicLogRedacted(t *testing.T) {
	const secret = "SECRET123"
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	var logBuf bytes.Buffer
	agent := newEventAgentOn(t, srv.URL, secretPanicSink{secret: secret}, newStore(t),
		slog.New(slog.NewJSONHandler(&logBuf, nil)))
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	logs := logBuf.String()
	if strings.Contains(logs, secret) {
		t.Errorf("引擎日誌含 sink panic 帶出來的密鑰: %s", logs)
	}
	if !strings.Contains(logs, "event_sink_panic") {
		t.Errorf("日誌缺 event_sink_panic 記錄，壞掉的 Sink 查不出來: %s", logs)
	}
	// 去敏是遮 query 不是整段丟：host 留著，維運才知道是誰炸的。
	if !strings.Contains(logs, "sink.example.com") {
		t.Errorf("日誌把 panic 細節整段丟了: %s", logs)
	}
}

// secretPanicSink 播報時以一個內嵌密鑰的訊息 panic。
type secretPanicSink struct{ secret string }

func (s secretPanicSink) Emit(context.Context, core.Event) {
	panic("送不出去: https://sink.example.com/push?token=" + s.secret)
}

// TestProcessEventTextRedacted 斷言事件的 Text 套用**與審計相同**的去敏規則：
// 多一條輸出路徑不該多一條密鑰外洩路徑。
func TestProcessEventTextRedacted(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_secret_url.json"))
	sink := &recordingSink{}
	agent := newEventAgent(t, srv.URL, sink)
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "查一下那個服務")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 回填給使用者與 Session 歷史的內容**不去敏**——那是 LLM 實際說的話，去敏是
	// 落盤與旁路輸出的規則，不是對話內容的規則。這一格順帶釘住兩邊沒有互相汙染。
	if !strings.Contains(resp, "SECRET123") {
		t.Errorf("最終回應不應被去敏: %q", resp)
	}

	texts := sink.only(core.EventAssistantText)
	if len(texts) != 1 {
		t.Fatalf("assistant_text 事件數 = %d, 期望 1", len(texts))
	}
	if strings.Contains(texts[0].Text, "SECRET123") {
		t.Errorf("事件 Text 仍含密鑰: %q", texts[0].Text)
	}
	if !strings.Contains(texts[0].Text, "[REDACTED]") {
		t.Errorf("事件 Text 應保留去敏標記，實際 = %q", texts[0].Text)
	}
}

// TestNopEventSinkDoesNothing 釘住不做事的預設實作存在且真的不做事：不關心執行過程
// 的呼叫端傳入它即維持現有行為（既有組裝點走的正是這條）。
func TestNopEventSinkDoesNothing(t *testing.T) {
	var sink core.EventSink = core.NopEventSink{}
	// 不 panic、不阻塞即為通過；沒有可觀察的輸出正是這個實作的全部意義。
	sink.Emit(context.Background(), core.Event{Kind: core.EventTurnStarted})
}

// assertKinds 比對事件種類序列。
func assertKinds(t *testing.T, got, want []core.EventKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("事件序列 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件序列 = %v, 期望 %v（第 %d 則不同）", got, want, i)
		}
	}
}
