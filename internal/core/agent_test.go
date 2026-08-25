// 對話鏈路的整合測試，依 spec #1 的 seam 決策一律從
// AgentService.Process(session, message) → response 驅動；LLM 以 httptest.Server
// 回放 testdata/ 下錄製好的 OpenAI 兼容回應（替換點是 Provider 的 base URL 配置，
// 憲法 4.4、ADR-0002），其餘模組在 seam 之下用真的、不 mock。
package core_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// testProfile 回傳指向 provider name "openai" 的最簡 Profile。
func testProfile() *core.Profile {
	return &core.Profile{
		Name:     "default",
		Identity: core.Identity{AgentName: "Oryx", Prompt: "你是 Oryx。"},
		Provider: core.ProviderRef{Name: "openai", Model: "gpt-4o-mini", Temperature: 0.7},
		Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20},
	}
}

// newReplayServer 起一個回放錄製回應的 httptest.Server：按請求順序逐一回放
// fixtures，超出錄製數量的請求視為測試錯誤。
func newReplayServer(t *testing.T, fixtures ...string) *httptest.Server {
	t.Helper()
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served >= len(fixtures) {
			t.Errorf("LLM 請求數超出錄製回應數 %d", len(fixtures))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[served]))
		served++
	}))
	t.Cleanup(srv.Close)
	return srv
}

// readFixture 讀取 testdata/ 下的錄製回應。
func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("讀取 fixture %s: %v", name, err)
	}
	return string(data)
}

// newAgent 以指向 baseURL 的真實 ProviderService 組出無 Tool 的 AgentService，
// Session 儲存用落在 t.TempDir() 的真實 SQLite。
func newAgent(t *testing.T, baseURL string, logger *slog.Logger) *core.AgentService {
	t.Helper()
	return newAgentOn(t, baseURL, logger, newStore(t))
}

// newAgentOn 同 newAgent，但用指定的 Session 儲存——跨重啟與落庫斷言的測試
// 要對同一個 db 檔先後組兩次引擎。
func newAgentOn(t *testing.T, baseURL string, logger *slog.Logger, st *testStore) *core.AgentService {
	t.Helper()
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, logger)
	return core.NewAgentService(testProfile(), svc, noTools(t), newMemory(t, st.sessions()), st.audit, noBootstrap(t), core.NopEventSink{}, discardLogger())
}

// newAgentWithProfile 以指定的 Profile 與 ProviderService 組出無 Tool 的
// AgentService；Session、Memory、審計都用落在 t.TempDir() 的真實實作。
func newAgentWithProfile(t *testing.T, profile *core.Profile, svc core.ProviderService) *core.AgentService {
	t.Helper()
	db := newStore(t)
	return core.NewAgentService(profile, svc, noTools(t), newMemory(t, db.sessions()), db.audit, noBootstrap(t), core.NopEventSink{}, discardLogger())
}

// newMemory 以指定的 Session 儲存與一份落在 t.TempDir()、尚未建立的 MEMORY.md
// 組出 Memory 門面。不關心長期記憶的測試因此拿到空記憶，system prompt 維持只有
// identity.prompt——長期記憶的行為由 agent_memory_test.go 專門覆蓋。
func newMemory(t *testing.T, sessions core.SessionStore) core.MemoryService {
	t.Helper()
	root, _ := workspaceRoot(t)
	return memory.NewService(sessions, memory.NewLongTermMemory(root, memoryRelPath))
}

// noTools 回傳空的 Tool 子集（無 Tool 的 Agent）。
// noBootstrap 回傳一個指向空 Workspace 的 Bootstrap 載入器：三份檔案都不存在，
// 每一層都是空的。用真的 os.Root ＋ t.TempDir()（憲法 4.3），不是 nil 也不是 stub
// ——「缺檔視為該層為空」本來就是既定行為，這裡走的是真實路徑。
func noBootstrap(t *testing.T) core.ContextLoader {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return config.NewContextLoader(root)
}

func noTools(t *testing.T) core.ToolExecutor {
	t.Helper()
	exec, err := tool.NewRegistry().Subset(nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	return exec
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestProcessDirectReply(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
	agent := newAgent(t, srv.URL, discardLogger())
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	const want = "你好！我是 Oryx，很高興為你服務。"
	if resp != want {
		t.Errorf("回應 = %q, 期望 %q", resp, want)
	}

	assertHistory(t, session, []core.Message{
		{Role: core.RoleUser, Content: "你好"},
		{Role: core.RoleAssistant, Content: want},
	})
}

// assertHistory 斷言 Session 對話歷史的 role 與 content 序列，並檢查時間戳已填。
func assertHistory(t *testing.T, session *core.Session, want []core.Message) {
	t.Helper()
	if len(session.Messages) != len(want) {
		t.Fatalf("歷史長度 = %d, 期望 %d: %+v", len(session.Messages), len(want), session.Messages)
	}
	for i, msg := range session.Messages {
		if msg.Role != want[i].Role || msg.Content != want[i].Content {
			t.Errorf("messages[%d] = {%s %q}, 期望 {%s %q}", i, msg.Role, msg.Content, want[i].Role, want[i].Content)
		}
		if msg.Timestamp.IsZero() {
			t.Errorf("messages[%d] 時間戳未填", i)
		}
	}
}
