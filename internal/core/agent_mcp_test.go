// MCP 最小鏈路的整合測試（ticket #21）：宣告 → 連線 → tools/list → 註冊進同一個
// ToolRegistry → Profile 兩層過濾 → LLM 真的呼叫到 → 結果回填 → 落審計。
//
// 沿用既有兩個 seam：從 AgentService.Process 驅動、LLM 以 httptest 回放（ADR-0002）。
// MCP 那一側**用真的**：起真實的本地 stdio server 子進程（見 mcp_server_test.go），
// 走真實的 JSON-RPC 往返，不 mock 協議也不注入假 transport（憲法 4.3）。
package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// mcpProfile 回傳一份引用了指定 MCP server 與 Tool 的 Profile。
//
// 兩個欄位分開傳是刻意的：這兩層正是本票要驗的東西（mcp_servers 圈定 server、tools
// 挑具體工具），測試必須能把它們各自設成不同的值。
func mcpProfile(servers, tools []string) *core.Profile {
	prof := testProfile()
	prof.McpServers = servers
	prof.Tools = tools
	return prof
}

// connectMcp 起 specs 指定的真實 stdio MCP server 並把工具註冊進 registry；測試結束
// 時關閉，不留孤兒子進程。
func connectMcp(t *testing.T, registry *tool.Registry, specs []core.McpServerSpec) {
	t.Helper()
	clients, err := tool.ConnectMcpServers(t.Context(), registry, specs, discardLogger())
	if err != nil {
		t.Fatalf("連線 MCP server: %v", err)
	}
	t.Cleanup(func() {
		if err := clients.Close(); err != nil {
			t.Errorf("關閉 MCP server: %v", err)
		}
	})
}

// newMcpAgent 組出一個工具全部來自真實 MCP server 的 AgentService。
func newMcpAgent(t *testing.T, baseURL string, prof *core.Profile, specs []core.McpServerSpec) *core.AgentService {
	t.Helper()
	return newMcpAgentOn(t, baseURL, prof, specs, newStore(t))
}

// newMcpAgentOn 同上，但用指定的 Session／審計儲存——要查 tool_invocations 的測試
// 得自己持有那個 db 檔的路徑。
func newMcpAgentOn(t *testing.T, baseURL string, prof *core.Profile, specs []core.McpServerSpec, st *testStore) *core.AgentService {
	t.Helper()
	registry := tool.NewRegistry()
	connectMcp(t, registry, specs)
	// autoIncluded 傳 nil：MCP 工具**不自動進**可用子集，必須由 Profile 的 tools
	// 欄位列出（那正是第二層過濾）。
	exec, err := registry.Subset(prof.Tools, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}

	root, _ := bootstrapWorkspace(t)
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	loader := config.NewContextLoader(root)
	return core.NewAgentService(prof, svc, exec,
		memory.NewService(st.sessions(), memory.NewLongTermMemory(root, memoryRelPath)),
		st.audit, loader, discardLogger())
}

// mcpToolCallFixture 回放「LLM 決定呼叫某個 MCP 工具」的錄製回應。工具名由呼叫端填，
// 因為它隨 server 名與工具名而變（沿既有 {{TARGET_URL}} 的替換慣例）。
func mcpToolCallFixture(t *testing.T, toolName string) string {
	t.Helper()
	return strings.ReplaceAll(readFixture(t, "reply_mcp_tool_call.json"), "{{TOOL_NAME}}", toolName)
}

// declaredToolNames 取出第 n 次 LLM 邊界請求附上的工具名清單——那是「這個 Agent 現在
// 有哪些工具可用」唯一從外部看得到的地方，兩層過濾都在這裡驗。
func declaredToolNames(t *testing.T, reqs [][]byte, n int) []string {
	t.Helper()
	if len(reqs) <= n {
		t.Fatalf("只收到 %d 次 LLM 請求，取不到第 %d 次", len(reqs), n+1)
	}
	var names []string
	for _, decl := range parseLLMRequest(t, reqs[n]).Tools {
		names = append(names, decl.Function.Name)
	}
	return names
}

// TestMcpToolCalledThroughProcess 是本票主場景：宣告一個 stdio server、Profile 引用
// 它，經 AgentService.Process 驅動後 LLM 真的呼叫到它的工具，結果回填進對話。
//
// 三條斷言撐起整條鏈路：工具以 <server>__<tool> 出現在 LLM 邊界的工具清單（連線與
// 註冊成功）、tool 訊息帶回 server 真的算出來的內容（協議轉發與回填成功）、最終回應
// 正常產出（ReAct 循環不感知工具來自哪裡）。
func TestMcpToolCalledThroughProcess(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool.json"))

	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgent(t, srv.URL, prof, []core.McpServerSpec{mcpSpec(t, "demo", "echo")})

	reply, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "幫我呼叫外部工具")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(reply, "外部工具已經回覆") {
		t.Errorf("最終回應不對: %q", reply)
	}

	if names := declaredToolNames(t, reqs, 0); !slices.Contains(names, "demo__echo") {
		t.Errorf("LLM 邊界的工具清單沒有 demo__echo: %v", names)
	}
	// server 端把自己的名字與工具名寫進回應，所以這條同時證明「轉發到了正確的 server」。
	toolMsg := toolMessageOf(t, reqs, 1)
	if !strings.Contains(toolMsg, "demo/echo 收到：哈囉") {
		t.Errorf("tool 訊息未帶 MCP server 的結果: %q", toolMsg)
	}
}

// TestMcpToolInvocationAudited 釘住 MCP 工具的呼叫與內建 Tool **一視同仁**地落
// tool_invocations，且 tool_name 帶 server 前綴——外部工具做過什麼同樣要可查證，
// 而且查得出來是哪個 server 做的（憲法 6.2）。
func TestMcpToolInvocationAudited(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	st := openStore(t, dbPath)

	srv := newRecordingReplayServer(t, new([][]byte),
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool.json"))

	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgentOn(t, srv.URL, prof, []core.McpServerSpec{mcpSpec(t, "demo", "echo")}, st)
	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫外部工具"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	st.flush(t)

	var found bool
	for _, row := range queryToolInvocations(t, dbPath) {
		if row.toolName == "demo__echo" {
			found = true
			if row.status != core.AuditStatusCompleted {
				t.Errorf("審計狀態 = %q, 期望 %q", row.status, core.AuditStatusCompleted)
			}
		}
	}
	if !found {
		t.Error("tool_invocations 沒有 demo__echo 的資料列——MCP 工具繞過了審計路徑")
	}
}

// TestMcpToolNaming 是命名契約的表格矩陣：工具以 <server>__<tool> 註冊，撞名共存是
// 硬需求（現行 Registry.Register 對重名直接報錯）。
//
// 分隔符取雙底線正是為了「工具名本身含底線」這一格：單底線看不出切點。server 名含
// 底線那格則確認前綴不因此變形。
//
// 每一格都真的把工具呼叫出去、比對回應裡的 server 自稱——只驗註冊名的話，兩個各有
// 同名工具的 server 即使全部轉發到同一個上頭也會通過。
func TestMcpToolNaming(t *testing.T) {
	tests := []struct {
		name string
		// specs 是要連的 server；wantTools 是它們預期的註冊名。
		specs     []core.McpServerSpec
		wantTools []string
		// callTool 是這一格要呼叫的註冊名，wantAnswer 是回應必須含的子串。
		callTool   string
		wantAnswer string
	}{
		{
			name: "同名工具跨 server 共存，各自答自己的",
			specs: []core.McpServerSpec{
				mcpSpec(t, "alpha", "search"),
				mcpSpec(t, "beta", "search"),
			},
			wantTools:  []string{"alpha__search", "beta__search"},
			callTool:   "beta__search",
			wantAnswer: "beta/search 收到：哈囉",
		},
		{
			name:       "工具名本身含底線",
			specs:      []core.McpServerSpec{mcpSpec(t, "github", "search_pr")},
			wantTools:  []string{"github__search_pr"},
			callTool:   "github__search_pr",
			wantAnswer: "github/search_pr 收到：哈囉",
		},
		{
			name:       "server 名含底線",
			specs:      []core.McpServerSpec{mcpSpec(t, "my_server", "echo")},
			wantTools:  []string{"my_server__echo"},
			callTool:   "my_server__echo",
			wantAnswer: "my_server/echo 收到：哈囉",
		},
		{
			name:       "同一個 server 的多個工具",
			specs:      []core.McpServerSpec{mcpSpec(t, "demo", "echo", "search_pr")},
			wantTools:  []string{"demo__echo", "demo__search_pr"},
			callTool:   "demo__search_pr",
			wantAnswer: "demo/search_pr 收到：哈囉",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs,
				mcpToolCallFixture(t, tt.callTool),
				readFixture(t, "reply_after_mcp_tool.json"))

			servers := make([]string, 0, len(tt.specs))
			for _, spec := range tt.specs {
				servers = append(servers, spec.Name)
			}
			prof := mcpProfile(servers, tt.wantTools)
			agent := newMcpAgent(t, srv.URL, prof, tt.specs)

			if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			names := declaredToolNames(t, reqs, 0)
			for _, want := range tt.wantTools {
				if !slices.Contains(names, want) {
					t.Errorf("工具清單缺 %q: %v", want, names)
				}
			}
			if toolMsg := toolMessageOf(t, reqs, 1); !strings.Contains(toolMsg, tt.wantAnswer) {
				t.Errorf("回填內容 = %q, 期望含 %q", toolMsg, tt.wantAnswer)
			}
		})
	}
}

// TestMcpHandshakeNegotiation 驗 initialize 交握**協商的結果真的被檢查**。
//
// 規範把交握定義成一次協商，協商結果決定後面能做什麼：server 可以回一個與我們要求的
// 不同的版本（它支援的那個），而「只使用協商成功的能力」是規範的 MUST。整包丟掉回應
// 等於宣稱「什麼版本我都能講、什麼能力我都假設有」——真正不相容的那天會表現成半路壞掉，
// 而不是啟動時一句清楚的話。
//
// 舊版但相容的那一格同樣重要：只認最新版會把大量固定在舊版的 server 擋在門外，那是
// 過度嚴格，不是嚴謹。
func TestMcpHandshakeNegotiation(t *testing.T) {
	tests := []struct {
		name string
		// env 是要疊給測試 server 的模式開關。
		env map[string]string
		// wantErrSub 為空表示這一格應該連得上。
		wantErrSub string
	}{
		{name: "server 回聲我們要求的版本：連得上", env: nil},
		{
			name: "server 回一個舊的但相容的版本：連得上",
			env:  map[string]string{mcpServerProtocolEnv: "2024-11-05"},
		},
		{
			name:       "server 回一個不認得的版本：清楚失敗並列出支援的版本",
			env:        map[string]string{mcpServerProtocolEnv: "1999-01-01"},
			wantErrSub: "1999-01-01",
		},
		{
			name:       "server 回應缺 protocolVersion：失敗",
			env:        map[string]string{mcpServerProtocolEnv: "-"},
			wantErrSub: "protocolVersion",
		},
		{
			// 沒宣告 tools 能力的 server 上呼叫 tools/list 是違規的，而且它對 OryxOS
			// 沒有用——我們接它就是為了工具。
			name:       "server 沒宣告 tools 能力：失敗並說清楚原因",
			env:        map[string]string{mcpServerNoToolsCapEnv: "1"},
			wantErrSub: "tools 能力",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			clients, err := tool.ConnectMcpServers(t.Context(), registry,
				[]core.McpServerSpec{mcpSpecWithEnv(t, "demo", tt.env, "echo")}, discardLogger())
			t.Cleanup(func() {
				if cerr := clients.Close(); cerr != nil {
					t.Errorf("關閉 MCP server: %v", cerr)
				}
			})

			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("這一格應該連得上: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("交握協商不通過時應該報錯")
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}

// TestMcpClientAnswersServerPing 釘住 stdio 上的 JSON-RPC 是**雙向**的。
//
// server 可以隨時發 ping，而規範要求收到的一方必須及時回覆。三個坑一起在這條裡：
//
//  1. **id 撞號**：JSON-RPC 的 id 是各自的命名空間，server 的 ping 用 id=1 完全合規，
//     而那時我們的 initialize 也是 id=1。只看 id 不看 method 的 client 會把那個 ping
//     當成 initialize 的回應（result 是空的）交給等待端，真正的回應隨後到達卻因為沒人
//     在等而被丟掉。
//  2. **id 的型別**：規範允許 request ID 是**字串或整數**（官方 ping 範例用的就是字串）。
//     把讀進來的 id 宣告成整數的 client，會在 json.Unmarshal 就讓整則訊息解析失敗、
//     當成雜訊丟掉——於是不回覆，server 判定連線已死。
//  3. **漏回覆或改了 id**：不回、或回一個換算過的 id，對 server 來說都等於沒回。
//
// 測試 server 因此在回 initialize **之前**先發 ping，並在收到**原樣帶回同一個 id** 的
// 回覆前拒絕 tools/list——上面三件事任一沒做好，這條就轉紅。
func TestMcpClientAnswersServerPing(t *testing.T) {
	tests := []struct {
		name string
		// pingID 是 server 發 ping 時要用的 id 的**原始 JSON**。
		pingID string
	}{
		// 與 client 的 initialize 撞號（都是 1）。
		{name: "整數 id 且與 client 撞號", pingID: "1"},
		// 規範允許、官方範例採用的形態。
		{name: "字串 id", pingID: `"ping-abc"`},
		{name: "看起來像數字的字串 id", pingID: `"1"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs,
				mcpToolCallFixture(t, "demo__echo"),
				readFixture(t, "reply_after_mcp_tool.json"))

			spec := mcpSpecWithEnv(t, "demo", map[string]string{mcpServerPingEnv: tt.pingID}, "echo")
			prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
			agent := newMcpAgent(t, srv.URL, prof, []core.McpServerSpec{spec})

			if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
				t.Fatalf("Process: %v", err)
			}
			// 交握與 tools/list 都得通過才會有工具可用、才呼叫得到。
			if toolMsg := toolMessageOf(t, reqs, 1); !strings.Contains(toolMsg, "demo/echo 收到：哈囉") {
				t.Errorf("回填內容 = %q，期望含 server 真的算出來的答案", toolMsg)
			}
		})
	}
}

// TestMcpToolNameMustBeUsableAtLLMBoundary 釘住「名字送不進 Function Calling 就啟動
// 報錯」。
//
// 註冊名由**使用者寫的 server 名**加上**外部 server 自報的工具名**拼成，兩截都不受
// OryxOS 控制，而它會原樣送進每一輪 LLM 請求的 tools 欄位。不合格的名字會讓整輪呼叫被
// 端點以 400 打回——**連完全沒用到那個工具的純對話也一起死**，而錯誤訊息只會說某個
// function name 不合法。所以這是啟動就該擋的設定錯誤，不是執行期的意外。
//
// 兩截各壞一次是必要的：只驗 server 名那格，一個回報不合法工具名的 server 仍會漏過去。
func TestMcpToolNameMustBeUsableAtLLMBoundary(t *testing.T) {
	tests := []struct {
		name string
		// server 與 tool 是宣告名與 server 自報的工具名。
		server, tool string
		// wantErrSub 為空表示這一格應該成功。
		wantErrSub string
	}{
		{name: "合法：英數、底線、連字號", server: "github-mcp", tool: "search_pr"},
		{
			// 「把 server 命名成它的來源網址」是很自然的寫法，卻構不出合法的 function name。
			name: "server 名含點與斜線", server: "github.com/foo", tool: "echo",
			wantErrSub: "github.com/foo",
		},
		{
			name: "server 名含空白", server: "my server", tool: "echo",
			wantErrSub: "my server",
		},
		{
			// 這一截是外部 server 給的，使用者改不了——錯誤訊息要指出是哪個 server 的哪個工具。
			name: "server 自報的工具名含不合法字元", server: "demo", tool: "search:pr",
			wantErrSub: "search:pr",
		},
		{
			name: "拼起來超過 64 字元", server: strings.Repeat("s", 40), tool: strings.Repeat("t", 30),
			wantErrSub: "64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			clients, err := tool.ConnectMcpServers(t.Context(), registry,
				[]core.McpServerSpec{mcpSpec(t, tt.server, tt.tool)}, discardLogger())
			// 不論成敗都要收子進程：連線失敗時 ConnectMcpServers 已自行收過，這裡是
			// 第二道（Close 冪等），漏掉會留下孤兒。
			t.Cleanup(func() {
				if cerr := clients.Close(); cerr != nil {
					t.Errorf("關閉 MCP server: %v", cerr)
				}
			})

			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("合法的名字不該報錯: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("名字送不進 Function Calling，啟動時應報錯")
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q（使用者要看得出是哪一截壞了）", err.Error(), tt.wantErrSub)
			}
		})
	}
}

// TestMcpToolCallSurvivesServerExitingRightAfterReply 驗一個真實世界不罕見的時序：
// server 回完答案就立刻退出（跑完一次就結束的包裝腳本、回完最後一句就崩的 server），
// 那個答案仍然要回填進對話、turn 仍然要正常收尾，退出時也不留孤兒進程。
//
// **這條不是那個 select 競態的重現。** `mcpConn.call` 要同時等「回應到了」與「連線斷
// 了」，兩者**同時**就緒時 Go 的 select 隨機挑，挑錯就把好答案換成一句連線錯誤。實測
// 確認過：把那段防護拿掉，這個測試照樣全綠——要撞到那一格，呼叫端得剛好在送出請求
// 到進入 select 之間被排程器換下去、且整個往返在那段空隙裡跑完，測試無法穩定製造。
// 那段防護因此是**推理**出來的，不是這條測試釘住的；不要因為它「有測試」就以為改動
// 那個 select 是安全的。
func TestMcpToolCallSurvivesServerExitingRightAfterReply(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool.json"))

	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgent(t, srv.URL, prof, []core.McpServerSpec{mcpSpecExitingAfterCall(t, "demo", "echo")})
	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
		t.Fatalf("server 回完就退出不該中斷 turn: %v", err)
	}

	if toolMsg := toolMessageOf(t, reqs, 1); !strings.Contains(toolMsg, "demo/echo 收到：哈囉") {
		t.Errorf("回填內容 = %q，期望含 server 真的算出來的答案", toolMsg)
	}
}

// TestMcpToolCallHonoursContextWhileWriteIsBlocked 釘住**寫入路徑吃得到 ctx**
// （憲法 5.3、本票 AC「所有阻塞路徑都吃 context.Context」）。
//
// 場景：server 交握完成後**還活著但停止讀 stdin**。這時一個較大的 tools/call 會把 pipe
// 塞滿，`Write` 就此卡住。若寫入路徑不理 ctx（同步 `Write` ＋ `sync.Mutex.Lock` 都沒有
// 中途放棄的辦法），呼叫端已經給的逾時與取消完全不生效——Tool 呼叫永久掛住，整個 turn
// 跟著死，連 Ctrl-C 都救不了。
//
// **這與 #22 的「單次呼叫自有時間上限」是不同的兩件事**：那張票要加的是我們自己補一個
// 期限；這裡壞的是呼叫端**已經提供**的期限失效。
//
// 三個時間刻意分開，順序不能亂（deadline ≪ 斷言視窗 ≪ server 退出）：
//   - ctx deadline 500ms：修好之後應該在這時返回
//   - 斷言視窗 3s：超過就判定「沒吃到 ctx」
//   - server 撐 6s 才退出：否則「子進程退出把 Write 解開」會假冒成「ctx 生效」
//
// 走 Executor 而不是 AgentService.Process：這一格的主角是**參數大到塞滿 pipe** 的那次
// 呼叫，而錄製回放的 fixture 沒有辦法帶一個幾百 KB 的 arguments。Registry／Executor 是
// 產品程式碼既有的介面（其他測試也這樣組），不是為測試新開的 seam。
func TestMcpToolCallHonoursContextWhileWriteIsBlocked(t *testing.T) {
	const (
		callDeadline = 500 * time.Millisecond
		assertWindow = 3 * time.Second
		// 參數要大於 pipe 緩衝（Linux／macOS 都是 64KB 量級）才會真的卡住。
		argumentBytes = 512 * 1024
	)

	registry := tool.NewRegistry()
	spec := mcpSpecWithEnv(t, "demo", map[string]string{mcpServerStopReadingEnv: "6"}, "echo")
	clients, err := tool.ConnectMcpServers(t.Context(), registry,
		[]core.McpServerSpec{spec}, discardLogger())
	if err != nil {
		t.Fatalf("連線 MCP server: %v", err)
	}
	t.Cleanup(func() {
		if cerr := clients.Close(); cerr != nil {
			t.Logf("關閉 MCP server（server 停止讀取後的預期噪音）: %v", cerr)
		}
	})

	exec, err := registry.Subset([]string{"demo__echo"}, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), callDeadline)
	defer cancel()
	call := core.ToolCall{
		Name:      "demo__echo",
		Arguments: `{"text":"` + strings.Repeat("填", argumentBytes/3) + `"}`,
	}

	// 在 goroutine 裡跑，測試才能在「掛住」時乾淨地失敗，而不是等到 go test 整體逾時。
	done := make(chan core.ToolResult, 1)
	go func() { done <- exec.Execute(ctx, call) }()

	select {
	case result := <-done:
		if result.OK {
			t.Fatal("server 已停止讀取，這次呼叫不該成功")
		}
		// 失敗以 ToolResult.Error 回填、不中斷 turn（沿既有的 Tool 失敗語義）。
		if result.Error == "" {
			t.Error("失敗沒有回填任何錯誤訊息，LLM 無從得知發生了什麼")
		}
	case <-time.After(assertWindow):
		t.Fatalf("ctx 已在 %v 逾時，%v 後呼叫仍未返回——寫入路徑吃不到 ctx，"+
			"卡在往子進程 stdin 的 Write 上", callDeadline, assertWindow)
	}
}

// TestMcpCancelledCallNeverReachesServer 釘住「已取消的呼叫不產生外部副作用」，並且
// 不會弄壞連線。
//
// **client 端看不出這件事**：一次沒送出去的呼叫與一次送出去卻把結果丟掉的呼叫，在
// client 眼裡都只是「失敗」。而使用者按下取消的意思是「這個動作不要發生」——外部工具
// 有沒有真的跑過，只有 server 那一側數得出來。所以測試 server 把每次 tools/call 記進
// 一個檔案，斷言落在那個計數上。
//
// 寫入路徑上有兩道才成立：取得寫入權的 select 會挑到 ctx.Done()，以及**拿到寫入權之後
// 再確認一次 ctx**。少了後者，那個 select 在兩個 case 同時就緒時是隨機挑的，一個早就
// 取消的呼叫有大約一半機率仍然把請求送出去。
//
// 寫入期限那條記帳規則（取消與寫入成功競態時不得留下過期期限）另有確定性的單元測試，
// 見 internal/tool/mcp_write_test.go——那個時序在真實子進程上撞不穩，這裡不假裝驗得到。
func TestMcpCancelledCallNeverReachesServer(t *testing.T) {
	const (
		cancelledCalls = 5
		normalCalls    = 3
	)

	callLog := filepath.Join(t.TempDir(), "calls.log")
	registry := tool.NewRegistry()
	spec := mcpSpecWithEnv(t, "demo", map[string]string{mcpServerCallLogEnv: callLog}, "echo")
	clients, err := tool.ConnectMcpServers(t.Context(), registry,
		[]core.McpServerSpec{spec}, discardLogger())
	if err != nil {
		t.Fatalf("連線 MCP server: %v", err)
	}
	t.Cleanup(func() {
		if cerr := clients.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})

	exec, err := registry.Subset([]string{"demo__echo"}, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	call := core.ToolCall{Name: "demo__echo", Arguments: `{"text":"哈囉"}`}

	for i := range cancelledCalls {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if result := exec.Execute(ctx, call); result.OK {
			t.Fatalf("第 %d 次已取消的呼叫回報成功——它其實執行了外部工具", i+1)
		}
	}

	// 取消過幾次之後，正常的呼叫必須照樣成功：連線沒有被那幾次取消弄壞。
	for i := range normalCalls {
		result := exec.Execute(t.Context(), call)
		if !result.OK {
			t.Fatalf("第 %d 次正常呼叫失敗了——取消把連線弄壞了: %s", i+1, result.Error)
		}
		if !strings.Contains(result.Content, "demo/echo 收到：哈囉") {
			t.Fatalf("第 %d 次的結果不對: %q", i+1, result.Content)
		}
	}

	// 關鍵斷言：server 只被呼叫了正常那幾次。
	if got := countCallLog(t, callLog); got != normalCalls {
		t.Errorf("server 收到 %d 次 tools/call，期望 %d——已取消的呼叫仍然送出了請求，"+
			"外部工具照樣執行了", got, normalCalls)
	}
}

// countCallLog 數測試 server 記下的 tools/call 次數（檔案不存在代表零次）。
func countCallLog(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("讀取 server 的呼叫記錄: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// TestProfileMcpServersIsolatesAgents 驗**第一層**過濾（Agent 級隔離）：Profile 的
// mcp_servers 未列的 server，其工具不出現在該 Agent 的可用集合，而且那個 server 的
// 子進程**根本不會被 spawn**。
//
// 兩個子測試共用同一份宣告，缺一不可：
//   - 「引用兩個」先證明第二個 server 本身是連得上、工具也用得到的，所以下一格的
//     缺席不是因為它壞掉。
//   - 「只引用一個」才是本題：即使它在 mcp_servers.yaml 宣告過、也已被證明連得上，
//     沒被這個 Profile 引用就完全不存在。
//
// marker 檔是「不 spawn」唯一的外部證據：工具清單裡沒有它，可能只是註冊時被過濾掉，
// 子進程還是白開了一個。
func TestProfileMcpServersIsolatesAgents(t *testing.T) {
	t.Run("引用兩個：兩個都連上、兩邊的工具都可用", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "spawned")
		declared := map[string]core.McpServerSpec{
			"wanted":   mcpSpec(t, "wanted", "echo"),
			"unwanted": mcpSpecWithMarker(t, "unwanted", marker, "echo"),
		}

		var reqs [][]byte
		srv := newRecordingReplayServer(t, &reqs,
			mcpToolCallFixture(t, "unwanted__echo"),
			readFixture(t, "reply_after_mcp_tool.json"))

		prof := mcpProfile([]string{"wanted", "unwanted"}, []string{"wanted__echo", "unwanted__echo"})
		agent := newMcpAgent(t, srv.URL, prof, resolveSpecs(t, prof, declared))
		if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
			t.Fatalf("Process: %v", err)
		}

		if names := declaredToolNames(t, reqs, 0); !slices.Contains(names, "unwanted__echo") {
			t.Errorf("引用了卻沒拿到它的工具: %v", names)
		}
		if toolMsg := toolMessageOf(t, reqs, 1); !strings.Contains(toolMsg, "unwanted/echo 收到") {
			t.Errorf("第二個 server 沒有真的答話: %q", toolMsg)
		}
		assertMarker(t, marker, true)
	})

	t.Run("只引用一個：另一個的工具不可用，也不 spawn 子進程", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "spawned")
		declared := map[string]core.McpServerSpec{
			"wanted":   mcpSpec(t, "wanted", "echo"),
			"unwanted": mcpSpecWithMarker(t, "unwanted", marker, "echo"),
		}

		var reqs [][]byte
		srv := newRecordingReplayServer(t, &reqs,
			mcpToolCallFixture(t, "wanted__echo"),
			readFixture(t, "reply_after_mcp_tool.json"))

		prof := mcpProfile([]string{"wanted"}, []string{"wanted__echo"})
		agent := newMcpAgent(t, srv.URL, prof, resolveSpecs(t, prof, declared))
		if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
			t.Fatalf("Process: %v", err)
		}

		names := declaredToolNames(t, reqs, 0)
		if !slices.Contains(names, "wanted__echo") {
			t.Errorf("引用到的 server 工具不見了: %v", names)
		}
		if slices.Contains(names, "unwanted__echo") {
			t.Errorf("未被 mcp_servers 引用的 server 工具出現在可用集合: %v", names)
		}
		assertMarker(t, marker, false)
	})
}

// TestProfileToolsFilterOnTopOfMcpServers 驗**第二層**過濾（工具級控制）：mcp_servers
// 列了、server 也連上了，tools 沒列的工具同樣不可用。
//
// 與第一層分開驗是必要的：兩層合起來測的話，少掉任何一層都不會被發現——第一層漏了，
// 第二層還是會把它擋掉；第二層漏了，第一層那格照樣通過。
func TestProfileToolsFilterOnTopOfMcpServers(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		mcpToolCallFixture(t, "demo__echo"),
		readFixture(t, "reply_after_mcp_tool.json"))

	// server 暴露兩個工具、Profile 也引用了這個 server，但 tools 只挑其中一個。
	prof := mcpProfile([]string{"demo"}, []string{"demo__echo"})
	agent := newMcpAgent(t, srv.URL, prof, []core.McpServerSpec{mcpSpec(t, "demo", "echo", "search_pr")})
	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "呼叫工具"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	names := declaredToolNames(t, reqs, 0)
	if !slices.Contains(names, "demo__echo") {
		t.Errorf("tools 列出的工具不可用: %v", names)
	}
	if slices.Contains(names, "demo__search_pr") {
		t.Errorf("tools 未列的工具出現在可用集合: %v", names)
	}
}

// TestMcpServersOmittedMeansNoMcpTools 釘住免遷移的那一格：mcp_servers 省略或為空的
// Profile 不接任何 MCP server，只有內建 Tool——既有 Profile 的行為因此完全不變。
func TestMcpServersOmittedMeansNoMcpTools(t *testing.T) {
	tests := []struct {
		name    string
		servers []string
	}{
		{name: "欄位省略", servers: nil},
		{name: "空清單", servers: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))

			prof := mcpProfile(tt.servers, nil)
			refs, err := prof.McpServerRefs()
			if err != nil {
				t.Fatalf("McpServerRefs: %v", err)
			}
			// 宣告檔裡有一個 server，但 Profile 沒引用它——解析出來必須是空的。
			specs, err := core.ResolveMcpServers(refs, map[string]core.McpServerSpec{
				"demo": mcpSpec(t, "demo", "echo"),
			})
			if err != nil {
				t.Fatalf("ResolveMcpServers: %v", err)
			}
			if len(specs) != 0 {
				t.Fatalf("要連的 server 數 = %d, 期望 0", len(specs))
			}

			agent := newMcpAgent(t, srv.URL, prof, specs)
			if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安"); err != nil {
				t.Fatalf("Process: %v", err)
			}
			if names := declaredToolNames(t, reqs, 0); len(names) != 0 {
				t.Errorf("沒有引用任何 server，工具清單卻不是空的: %v", names)
			}
		})
	}
}

// resolveSpecs 走產品程式碼真正在用的那條路：Profile 的 mcp_servers 欄位對上宣告檔，
// 解出**只有被引用到的**那幾份 spec。
func resolveSpecs(t *testing.T, prof *core.Profile, declared map[string]core.McpServerSpec) []core.McpServerSpec {
	t.Helper()
	refs, err := prof.McpServerRefs()
	if err != nil {
		t.Fatalf("McpServerRefs: %v", err)
	}
	specs, err := core.ResolveMcpServers(refs, declared)
	if err != nil {
		t.Fatalf("ResolveMcpServers: %v", err)
	}
	return specs
}

// assertMarker 斷言 marker 檔存在與否，也就是那個 server 的子進程有沒有被 spawn 過。
func assertMarker(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case want && err != nil:
		t.Errorf("marker %s 不存在——這個 server 應該要被 spawn: %v", path, err)
	case !want && err == nil:
		t.Errorf("marker %s 存在——未被引用的 server 仍然被 spawn 了子進程", path)
	case !want && !errors.Is(err, os.ErrNotExist):
		t.Errorf("檢查 marker %s: %v", path, err)
	}
}
