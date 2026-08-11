package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
)

func TestProcessMultiTurn(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_turn1.json"),
		readFixture(t, "reply_turn2.json"),
	)
	agent := newAgent(t, srv.URL, discardLogger())
	session := core.NewSession("cli", "local", "default")
	ctx := context.Background()

	if _, err := agent.Process(ctx, session, "我的專案用 Go 開發"); err != nil {
		t.Fatalf("第一輪 Process: %v", err)
	}
	resp, err := agent.Process(ctx, session, "我剛才說專案用什麼開發？")
	if err != nil {
		t.Fatalf("第二輪 Process: %v", err)
	}

	const wantTurn2 = "你剛才說你的專案用 Go 開發。"
	if resp != wantTurn2 {
		t.Errorf("第二輪回應 = %q, 期望 %q", resp, wantTurn2)
	}

	assertHistory(t, session, []core.Message{
		{Role: core.RoleUser, Content: "我的專案用 Go 開發"},
		{Role: core.RoleAssistant, Content: "好的，已記下：你的專案用 Go 開發。"},
		{Role: core.RoleUser, Content: "我剛才說專案用什麼開發？"},
		{Role: core.RoleAssistant, Content: wantTurn2},
	})
}

func TestProcessErrors(t *testing.T) {
	tests := []struct {
		name string
		// setup 回傳 AgentService 與呼叫用的 context。
		setup   func(t *testing.T) (*core.AgentService, context.Context)
		wantSub string // 錯誤訊息須含此子串
		wantIs  error  // 非 nil 時，錯誤鏈須含此錯誤
	}{
		{
			name: "Profile 引用的 Provider 未註冊",
			setup: func(t *testing.T) (*core.AgentService, context.Context) {
				svc := provider.NewService(map[string]provider.Config{}, discardLogger())
				return core.NewAgentService(testProfile(), svc, noTools(t), newSessionStore(t)), context.Background()
			},
			wantSub: `Provider "openai" 未註冊`,
		},
		{
			name: "Provider 回非 2xx",
			setup: func(t *testing.T) (*core.AgentService, context.Context) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
				}))
				t.Cleanup(srv.Close)
				return newAgent(t, srv.URL, discardLogger()), context.Background()
			},
			wantSub: "Provider openai 呼叫失敗",
		},
		{
			name: "網路錯誤（端點不可達）",
			setup: func(t *testing.T) (*core.AgentService, context.Context) {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				srv.Close() // 立即關閉：連線被拒
				return newAgent(t, srv.URL, discardLogger()), context.Background()
			},
			wantSub: "Provider openai 呼叫失敗",
		},
		{
			name: "逾時（context deadline）",
			setup: func(t *testing.T) (*core.AgentService, context.Context) {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					// 拖過 client 的 50ms deadline 即可；不等 client 斷線通知，
					// 免得 srv.Close 等待懸掛的 handler。
					time.Sleep(300 * time.Millisecond)
				}))
				t.Cleanup(srv.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				t.Cleanup(cancel)
				return newAgent(t, srv.URL, discardLogger()), ctx
			},
			wantIs: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, ctx := tt.setup(t)
			session := core.NewSession("cli", "local", "default")

			_, err := agent.Process(ctx, session, "你好")
			if err == nil {
				t.Fatal("期望錯誤，實際成功")
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("錯誤鏈 %v 未含 %v", err, tt.wantIs)
			}
			// 失敗 turn 以 turn 為單位 rollback：不留半截狀態，caller 可安全 retry。
			if len(session.Messages) != 0 {
				t.Errorf("失敗 turn 後 Session 殘留 %d 條訊息: %+v", len(session.Messages), session.Messages)
			}
		})
	}
}

// TestProcessRetryAfterFailure 驗證失敗 turn rollback 後，同一訊息可安全重送：
// 不會出現重複的 user message 或失敗 turn 的殘留。
func TestProcessRetryAfterFailure(t *testing.T) {
	var calls int
	fixture := readFixture(t, "reply_direct.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	t.Cleanup(srv.Close)

	agent := newAgent(t, srv.URL, discardLogger())
	session := core.NewSession("cli", "local", "default")
	ctx := context.Background()

	if _, err := agent.Process(ctx, session, "你好"); err == nil {
		t.Fatal("第一次呼叫期望 Provider 錯誤，實際成功")
	} else if strings.Contains(err.Error(), "外部效果") {
		t.Errorf("未執行過 Tool 的失敗 turn 不應帶副作用註記: %q", err.Error())
	}
	resp, err := agent.Process(ctx, session, "你好")
	if err != nil {
		t.Fatalf("retry 應成功，實際錯誤: %v", err)
	}

	const want = "你好！我是 Oryx，很高興為你服務。"
	if resp != want {
		t.Errorf("retry 回應 = %q, 期望 %q", resp, want)
	}
	assertHistory(t, session, []core.Message{
		{Role: core.RoleUser, Content: "你好"},
		{Role: core.RoleAssistant, Content: want},
	})
}

// TestProcessZeroMaxIterationsUsesDefault 驗證迭代上限預設在讀取點成立：
// 手組（未經 LoadProfile）的 Profile 帶零值時，循環以預設 10 跑而非零輪終止。
func TestProcessZeroMaxIterationsUsesDefault(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	p := testProfile()
	p.Settings = core.Settings{} // 零值：未套 LoadProfile 預設
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: srv.URL},
	}, discardLogger())
	agent := core.NewAgentService(p, svc, noTools(t), newSessionStore(t))
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	const want = "你好！我是 Oryx，很高興為你服務。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}
}

func TestProcessLogsLLMCall(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	agent := newAgent(t, srv.URL, logger)
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("解析日誌 JSON %q: %v", buf.String(), err)
	}
	if record["msg"] != "llm_call" {
		t.Errorf("msg = %v, 期望 llm_call", record["msg"])
	}
	if record["provider"] != "openai" {
		t.Errorf("provider = %v, 期望 openai", record["provider"])
	}
	if record["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v, 期望 gpt-4o-mini", record["model"])
	}
	// token 用量來自 fixture 的 usage 段；latency_ms 只驗欄位存在（值不確定）。
	if got := record["total_tokens"]; got != float64(35) {
		t.Errorf("total_tokens = %v, 期望 35", got)
	}
	if _, ok := record["latency_ms"]; !ok {
		t.Error("日誌缺 latency_ms 欄位")
	}
}
