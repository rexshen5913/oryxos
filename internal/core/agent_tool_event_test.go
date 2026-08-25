// Tool 事件的整合測試（ticket #52）：經既有的 AgentService.Process seam 驅動，
// 事件中介層掛在 Profile 過濾出的 Executor 上。**不另立驅動點**——AC 明訂事件由
// 中介層播報這件事，用「拿掉中介層則沒有 Tool 事件」間接驗證。
//
// LLM 以 httptest 回放（ADR-0002），Tool 的目標端點用另一個 httptest.Server 充當
// 真實 HTTP 依賴（憲法 4.3）。
package core_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestProcessEmitsToolEventPairs 斷言一次 Tool 執行播報出**成對**的 tool_started 與
// tool_finished，且 tool_finished 的 OK 反映該次執行結果。
//
// 兩格用同一份錄製回應、只換 Sandbox 白名單：成功那格打得到目標端點，失敗那格在
// 白名單外被擋下。這讓「OK 反映的是執行結果」不必靠額外的佈置就看得出來。
func TestProcessEmitsToolEventPairs(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []string
		fixtures []string
		wantOK   bool
	}{
		{
			name:     "成功的 Tool 執行",
			allowed:  []string{"127.0.0.1"},
			fixtures: []string{"reply_weather_tool_call.json", "reply_weather_final.json"},
			wantOK:   true,
		},
		{
			name:    "被 Sandbox 擋下的 Tool 執行",
			allowed: []string{"trusted.example.com"},
			// 這份錄製回應打的是 api.example.com，白名單外：SandboxViolation。
			fixtures: []string{"reply_tool_call.json", "reply_after_tool_error.json"},
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
			agent := newEventToolAgent(t, srv.URL, []string{"http_get"}, tt.allowed, sink, discardLogger(),
				tool.NewEventMiddleware(sink, discardLogger()))
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			// 完整序列：Tool 事件夾在兩次 Provider 呼叫之間。
			assertKinds(t, sink.kinds(), []core.EventKind{
				core.EventTurnStarted,
				core.EventIteration,
				core.EventToolStarted,
				core.EventToolFinished,
				core.EventIteration,
				core.EventAssistantText,
				core.EventTurnFinished,
			})

			started := sink.only(core.EventToolStarted)
			finished := sink.only(core.EventToolFinished)
			if len(started) != 1 || len(finished) != 1 {
				t.Fatalf("tool_started = %d 則、tool_finished = %d 則, 期望各 1 則", len(started), len(finished))
			}
			if started[0].ToolName != "http_get" || finished[0].ToolName != "http_get" {
				t.Errorf("Tool 名 = %q／%q, 期望都是 http_get", started[0].ToolName, finished[0].ToolName)
			}
			if finished[0].OK != tt.wantOK {
				t.Errorf("tool_finished.OK = %v, 期望 %v", finished[0].OK, tt.wantOK)
			}
			// SessionID 由 ctx 一路帶到中介層——中介層看不到 Session，這一格證的是
			// 那條傳遞真的接通了，不是留了個空字串。
			for i, e := range append(started, finished...) {
				if e.SessionID != session.ID {
					t.Errorf("Tool 事件[%d].SessionID = %q, 期望 %q", i, e.SessionID, session.ID)
				}
			}
		})
	}
}

// TestProcessEmitsToolEventPairPerExecution 斷言 Tool 失敗並重試時，**每一次實際執行
// 各產生一對事件**——與 tool_invocations 每次執行一筆的既有語義一致。
//
// 筆數不是拿事件自己數的：同一次執行的 tool_invocation 結構化日誌獨立數一遍，
// 兩邊必須對得上。
func TestProcessEmitsToolEventPairPerExecution(t *testing.T) {
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

	var logBuf bytes.Buffer
	sink := &recordingSink{}
	agent := newEventToolAgent(t, srv.URL, []string{"http_get"}, []string{"127.0.0.1"}, sink,
		slog.New(slog.NewJSONHandler(&logBuf, nil)), tool.NewEventMiddleware(sink, discardLogger()))
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 首次執行 ＋ 三次重試 = 四次實際執行（需求 8.2）。
	const wantExecutions = 4
	if got := strings.Count(logBuf.String(), `"msg":"tool_invocation"`); got != wantExecutions {
		t.Fatalf("tool_invocation 日誌筆數 = %d, 期望 %d", got, wantExecutions)
	}
	started := sink.only(core.EventToolStarted)
	finished := sink.only(core.EventToolFinished)
	if len(started) != wantExecutions || len(finished) != wantExecutions {
		t.Fatalf("tool_started = %d 則、tool_finished = %d 則, 期望各 %d 則",
			len(started), len(finished), wantExecutions)
	}
	for i, e := range finished {
		if e.OK {
			t.Errorf("finished[%d].OK = true, 期望 false（每一次執行都失敗了）", i)
		}
	}

	// 成對還要**交錯正確**：每個 started 後面接的是自己的 finished，而不是四個
	// started 擠在前面。三次重試各夾一則 tool_retrying。
	assertKinds(t, sink.kinds(), []core.EventKind{
		core.EventTurnStarted,
		core.EventIteration,
		core.EventToolStarted, core.EventToolFinished,
		core.EventToolRetrying,
		core.EventToolStarted, core.EventToolFinished,
		core.EventToolRetrying,
		core.EventToolStarted, core.EventToolFinished,
		core.EventToolRetrying,
		core.EventToolStarted, core.EventToolFinished,
		core.EventIteration,
		core.EventAssistantText,
		core.EventTurnFinished,
	})
}

// TestProcessToolEventsComeFromMiddlewareOnly 是 AC 那格間接驗證：同一個場景掛與
// 不掛事件中介層各跑一次，**差別剛好只有那兩種事件**。
//
// 這證明播報寫在中介層裡、不在 Tool 執行器內部——若哪天有人把它挪進執行器，不掛
// 中介層的那一趟就會冒出 Tool 事件，這一格立刻轉紅。
func TestProcessToolEventsComeFromMiddlewareOnly(t *testing.T) {
	run := func(t *testing.T, withMiddleware bool) []core.EventKind {
		t.Helper()
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
		}))
		t.Cleanup(target.Close)

		srv := newReplayServer(t,
			strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", target.URL),
			readFixture(t, "reply_weather_final.json"),
		)
		sink := &recordingSink{}
		var mws []tool.Middleware
		if withMiddleware {
			mws = append(mws, tool.NewEventMiddleware(sink, discardLogger()))
		}
		agent := newEventToolAgent(t, srv.URL, []string{"http_get"}, []string{"127.0.0.1"},
			sink, discardLogger(), mws...)
		session := core.NewSession("cli", "local", "default")
		if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err != nil {
			t.Fatalf("Process: %v", err)
		}
		return sink.kinds()
	}

	withMW := run(t, true)
	withoutMW := run(t, false)

	assertKinds(t, withoutMW, []core.EventKind{
		core.EventTurnStarted,
		core.EventIteration,
		core.EventIteration,
		core.EventAssistantText,
		core.EventTurnFinished,
	})
	assertKinds(t, withMW, []core.EventKind{
		core.EventTurnStarted,
		core.EventIteration,
		core.EventToolStarted,
		core.EventToolFinished,
		core.EventIteration,
		core.EventAssistantText,
		core.EventTurnFinished,
	})

	// 把 Tool 事件濾掉之後，兩趟必須逐則相同——差別**只有**那兩種，其他一則不多、
	// 一則不少。
	if filtered := withoutToolEvents(withMW); strings.Join(kindStrings(filtered), ",") != strings.Join(kindStrings(withoutMW), ",") {
		t.Errorf("濾掉 Tool 事件後 = %v, 期望與不掛中介層時相同 %v", filtered, withoutMW)
	}
}

// withoutToolEvents 濾掉中介層播報的那兩種事件。
func withoutToolEvents(kinds []core.EventKind) []core.EventKind {
	out := make([]core.EventKind, 0, len(kinds))
	for _, k := range kinds {
		if k == core.EventToolStarted || k == core.EventToolFinished {
			continue
		}
		out = append(out, k)
	}
	return out
}

// kindStrings 把事件種類轉成字串切片，供整串比對。
func kindStrings(kinds []core.EventKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
