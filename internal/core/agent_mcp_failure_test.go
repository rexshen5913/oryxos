// 本檔驗 MCP 的失敗語義在**引擎層**的表現（ticket #22）：啟動時連不上就降級、工具
// 呼叫失敗以 ToolResult.Error 回填而不中斷 turn。
//
// 一律經既有的兩個 seam 驅動（AgentService.Process ＋ Provider base URL 回放），
// MCP 那一側起真實的本地 stdio server 子進程（見 mcp_server_test.go），不 mock 協議。
//
// 時間上限本身（連線／單次呼叫／關閉）不在這裡驗——那三個在 production 是秒級常數，
// 要把它們設成毫秒級才測得動，歸 internal/tool/mcp_failure_test.go 的白盒測試。
package core_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// unstartableMcpSpec 回傳一份指向不存在執行檔的宣告。
//
// 這是連不上最常見的現實形態：機器上沒裝 node、路徑打錯、套件沒安裝好。刻意用
// t.TempDir() 底下一個保證不存在的路徑，而不是 /nonexistent 這種可能被別人建出來的。
func unstartableMcpSpec(t *testing.T, name string) core.McpServerSpec {
	t.Helper()
	return core.McpServerSpec{
		Name:      name,
		Transport: core.McpTransportStdio,
		Command:   []string{filepath.Join(t.TempDir(), "no-such-mcp-server")},
	}
}

// TestMcpStartupFailureMatrix 是**啟動側的失敗矩陣**。
//
// 共通的斷言是同一句、也是本票的核心語義：**ConnectMcpServers 不論如何都不回錯誤**，
// 一個外部依賴掛掉不該讓整個 Agent 起不來（使用者故事 28）。差別在後果——連不上的
// 記進 Failures() 讓組裝點喊出來，沒有工具的照常連上、只是工具數為 0。
func TestMcpStartupFailureMatrix(t *testing.T) {
	tests := []struct {
		name string
		spec func(t *testing.T) core.McpServerSpec
		// wantFailures 是期望被記下的連線失敗筆數。
		wantFailures int
		// wantEchoUsable 是 demo__echo 這個工具最後能不能用。
		wantEchoUsable bool
	}{
		{
			name:           "server 起不來：記成失敗，它的工具不可用",
			spec:           func(t *testing.T) core.McpServerSpec { return unstartableMcpSpec(t, "demo") },
			wantFailures:   1,
			wantEchoUsable: false,
		},
		{
			// **回空不是失敗**：一個沒有工具的 server 是合法的（工具還沒設定好、或它
			// 主要提供別的能力）。把它當錯誤會讓一個無關緊要的 server 擋下啟動。
			name:           "tools/list 回空：連得上，不算失敗，只是沒有工具",
			spec:           func(t *testing.T) core.McpServerSpec { return mcpSpec(t, "demo") },
			wantFailures:   0,
			wantEchoUsable: false,
		},
		{
			name:           "一切正常：工具可用",
			spec:           func(t *testing.T) core.McpServerSpec { return mcpSpec(t, "demo", "echo") },
			wantFailures:   0,
			wantEchoUsable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			clients, err := tool.ConnectMcpServers(t.Context(), registry,
				[]core.McpServerSpec{tt.spec(t)}, discardLogger())
			t.Cleanup(func() {
				if cerr := clients.Close(); cerr != nil {
					t.Errorf("關閉 MCP server: %v", cerr)
				}
			})
			if err != nil {
				t.Fatalf("連線失敗不該中斷啟動: %v", err)
			}

			if got := len(clients.Failures()); got != tt.wantFailures {
				t.Errorf("Failures() = %d 筆, 期望 %d: %v", got, tt.wantFailures, clients.Failures())
			}
			// 「工具在不在」經 Subset 斷言——那是 Profile 過濾實際走的路。
			_, subsetErr := registry.Subset([]string{tool.McpToolName("demo", "echo")}, nil, discardLogger())
			if usable := subsetErr == nil; usable != tt.wantEchoUsable {
				t.Errorf("demo__echo 可用 = %v, 期望 %v（Subset: %v）", usable, tt.wantEchoUsable, subsetErr)
			}
		})
	}
}

// TestMcpConnectFailureKeepsOtherServersUsable 釘住降級的**範圍**：剛好是掛掉的那一個。
//
// 沒有這條的話，一個「只要有任何 server 連不上就整批不註冊」的實作也會讓上面的矩陣
// 通過——而那正是「一個外部依賴掛掉不該讓整個 Agent 起不來」要防的事。
func TestMcpConnectFailureKeepsOtherServersUsable(t *testing.T) {
	registry := tool.NewRegistry()
	// 順序刻意把壞的放前面：壞的那個若讓迴圈提前結束，好的那個就永遠連不上。
	clients, err := tool.ConnectMcpServers(t.Context(), registry, []core.McpServerSpec{
		unstartableMcpSpec(t, "broken"),
		mcpSpec(t, "alive", "echo"),
	}, discardLogger())
	t.Cleanup(func() {
		if cerr := clients.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})
	if err != nil {
		t.Fatalf("一個 server 連不上不該中斷啟動: %v", err)
	}

	failures := clients.Failures()
	if len(failures) != 1 || failures[0].Server != "broken" {
		t.Fatalf("期望只有 broken 被記成失敗，實際: %v", failures)
	}
	if _, err := registry.Subset([]string{tool.McpToolName("alive", "echo")}, nil, discardLogger()); err != nil {
		t.Errorf("活著的 server 的工具應該照常可用: %v", err)
	}
}

// TestMcpToolErrorLetsLlmTakeAnotherRoute 是本票的 fixture 場景：**MCP 工具回錯誤之後
// LLM 換一條路回覆使用者**。
//
// 要守住的是 spec #1 既有的 Tool 失敗語義——失敗以 ToolResult.Error 回填、turn 不中斷。
// 中斷 turn 的話使用者只會看到一句沒頭沒尾的錯誤，而 LLM 明明可以說「那個工具這次不行，
// 我先用手上的資料回答你」。
//
// 這裡製造的是**工具執行層**的失敗（協議一切正常，是工具自己說做不到），那是外部工具
// 最常見的形態：外部 API 回 4xx／5xx、權杖過期、參數不被接受。
func TestMcpToolErrorLetsLlmTakeAnotherRoute(t *testing.T) {
	const toolErr = "外部 API 回 503，暫時無法查詢"
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool_error.json"))

	spec := mcpSpecWithEnv(t, "demo", map[string]string{mcpServerToolErrorEnv: toolErr}, "echo")
	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgent(t, srv.URL, prof, []core.McpServerSpec{spec})

	reply, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "幫我查一下")
	if err != nil {
		t.Fatalf("MCP 工具失敗不該中斷 turn: %v", err)
	}
	if !strings.Contains(reply, "沒有成功") {
		t.Errorf("最終回應不是 LLM 換路後的回覆: %q", reply)
	}

	// 錯誤**原文**要回填進 tool 訊息：那是 LLM 換路的唯一依據，被我們改寫或吞掉的話，
	// 模型只會知道「失敗了」而不知道該不該重試、要不要換工具。
	if msg := toolMessageOf(t, reqs, 1); !strings.Contains(msg, toolErr) {
		t.Errorf("tool 訊息未帶工具的失敗原文: %q", msg)
	}
	// 走到第二次 LLM 呼叫本身就是「turn 沒有中斷」的證據。
	if len(reqs) != 2 {
		t.Errorf("LLM 呼叫次數 = %d, 期望 2（工具失敗後仍要讓 LLM 回話）", len(reqs))
	}
}

// TestMcpToolErrorStillAudited 釘住失敗的呼叫**同樣落審計**。
//
// 成功的呼叫落庫、失敗的不落，等於出事的時候查不到——而那正是最需要查的時候（憲法 6.2）。
func TestMcpToolErrorStillAudited(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	st := openStore(t, dbPath)

	srv := newRecordingReplayServer(t, new([][]byte),
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool_error.json"))

	spec := mcpSpecWithEnv(t, "demo",
		map[string]string{mcpServerToolErrorEnv: "外部 API 回 503"}, "echo")
	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgentOn(t, srv.URL, prof, []core.McpServerSpec{spec}, st)

	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "幫我查一下"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	st.flush(t)

	var found bool
	for _, row := range queryToolInvocations(t, dbPath) {
		if row.toolName != "demo__echo" {
			continue
		}
		found = true
		if row.status == core.AuditStatusCompleted {
			t.Errorf("失敗的呼叫被記成 %q，事後查不出它其實失敗了", row.status)
		}
	}
	if !found {
		t.Error("tool_invocations 沒有 demo__echo 的資料列——失敗的呼叫繞過了審計路徑")
	}
}
