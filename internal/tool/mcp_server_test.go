// 本檔提供 internal/tool 這一側的**真實 stdio MCP server**，專門用來製造失敗形態：
// 不回應 initialize、不回應 tools/call、收到呼叫就自殺、關閉時不理會 stdin。
// 手法與 internal/core 那份相同——測試二進制兼任 server，TestMain 在跑任何測試之前
// 早退到服務迴圈，子進程、真的 stdin／stdout、真的 JSON-RPC（憲法 4.3、ADR-0002）。
//
// **為什麼這裡要有第三份 server**（core 與 cmd 各已有一份）：Go 的 _test.go 不跨
// package 共用，而本票要驗的三個時間上限（連線、單次呼叫、關閉）都必須把期限設成
// 毫秒級才測得動，那需要直接組 mcpConn 與 McpToolAdapter——只有 `package tool` 的
// 白盒測試碰得到。沿 #21 對 newReplayServer／readFixture 的既有取捨：與其為共用而
// 多開一個 package，不如各留一份小的。這份刻意只實作這幾條測試需要的東西。
//
// 這份 server 對協議的嚴格程度低於 core 那份（那裡才是協議正確性的交叉檢查點），
// 本檔的主題是**時間與生命週期**，不是協議語義。
package tool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

const (
	// toolMcpServerEnv 非空即進入 server 模式（不跑任何測試）。
	toolMcpServerEnv = "ORYXOS_TEST_TOOL_MCP_SERVER"
	// toolMcpToolsEnv 是這個 server 要暴露的工具名（逗號分隔）。
	toolMcpToolsEnv = "ORYXOS_TEST_TOOL_MCP_TOOLS"
	// toolMcpModeEnv 選一種失敗形態，空字串即一切正常。合法值見下面幾個常數。
	toolMcpModeEnv = "ORYXOS_TEST_TOOL_MCP_MODE"
	// toolMcpGrandchildPidEnv 指向一個檔案，server 把它生出來的孫行程 PID 寫進去。
	// 測試據此驗「孫行程也被收掉了」——那是 client 端唯一看得到這件事的方式。
	toolMcpGrandchildPidEnv = "ORYXOS_TEST_TOOL_MCP_GRANDCHILD_PID"
	// toolMcpSpawnMarkerEnv 指向一個檔案，server 一啟動就寫進去。存在即證明子進程
	// 真的被 spawn 過。
	toolMcpSpawnMarkerEnv = "ORYXOS_TEST_TOOL_MCP_MARKER"
)

// server 的失敗形態。每一個都對應 ticket #22 失敗矩陣裡的一列。
const (
	// modeHangInitialize：讀得到請求但**不回應 initialize**，交握因此永遠等不到答案。
	// 這是「server 起得來但半死不活」——比進程起不來更難查，也更常見（依賴沒裝好、
	// 卡在自己的初始化上）。
	modeHangInitialize = "hang_initialize"
	// modeHangCall：交握與 tools/list 都正常，但**不回應 tools/call**。
	// 用來驗單次呼叫有自己的時間上限。
	modeHangCall = "hang_call"
	// modeDieOnCall：收到 tools/call 就直接退出，不回應。用來驗 server 中途死掉。
	modeDieOnCall = "die_on_call"
	// modeIgnoreStdinClose：回完 tools/list 就不再讀 stdin，並且撐得比任何合理的關閉
	// 期限都久。用來驗關閉的整體期限與逾期強制終止。
	modeIgnoreStdinClose = "ignore_stdin_close"
	// modeLeakPipeToGrandchild：回完 tools/list 之後生一個**繼承 stdout／stderr 的
	// 孫行程**，再自己撐著不退出。
	//
	// 這模擬的是最常見的真實佈署：`npx -y some-mcp-server` 不是 server 本身，是啟動器
	// ——真正的 server 是它的子進程，也就是 OryxOS 的孫行程，而且繼承了同一份 pipe
	// 寫入端。只殺直接子進程的話孫行程還抓著 pipe，讀取端永遠等不到 EOF。
	modeLeakPipeToGrandchild = "leak_pipe_to_grandchild"
	// modeLeakPipeToDetachedGrandchild：同上，但孫行程**自成一個 process group**，
	// 因此殺整棵樹殺不到它（自己 daemonize 的 server 就長這樣）。
	//
	// 用來逼出 close 的第二道防線：殺不到就自己把讀取端關掉，讀取 goroutine 才收得了
	// 工。沒有這一格的話那道防線是死碼——而它同時也是非 Unix 平台唯一的保障。
	modeLeakPipeToDetachedGrandchild = "leak_pipe_to_detached_grandchild"
	// modeSlowInitialize：交握**會成功，只是慢**——回應 initialize 之前先睡
	// slowInitializeDelay。屬 issue #26 而不是 ticket #22 的失敗矩陣。
	//
	// 與 modeHangInitialize 的分工是刻意的：hang 那個用來驗「連線這一層自己有期限」，
	// 這個用來驗**多個 server 之間有沒有並行**。並行與否的差別是總耗時等於「最慢的那
	// 一個」還是「全部加起來」，而量這件事需要一個**會如期完成**的延遲——拿逾時來量
	// 不可行：逾時場景要壓到毫秒級才跑得動，而連線期限是給冷啟動用的 30 秒常數。
	modeSlowInitialize = "slow_initialize"
	// modeSlowInitializeThenDie：睡 slowInitializeDelay 之後直接退出，不回應
	// initialize——連線因此「慢，而且失敗」。
	//
	// 用來製造**完成時間不同的兩個失敗**：並行之下誰先連完誰先到，要驗 Failures() 有
	// 沒有被拉回宣告順序，就需要一個註定比另一個晚到的失敗。
	modeSlowInitializeThenDie = "slow_initialize_then_die"
)

// slowInitializeDelay 是 modeSlowInitialize 回應 initialize 之前刻意拖的時間。
//
// 取值要同時滿足兩邊：大到足以蓋過子進程 spawn 的抖動（那是幾十毫秒級，並行測試的
// 訊號不能被它淹掉），小到整條測試仍在一秒級。
const slowInitializeDelay = 400 * time.Millisecond

// ignoreStdinCloseLifetime 是 modeIgnoreStdinClose 撐著不退出的時間。
//
// 取一個明顯大於測試關閉期限（毫秒級）的值：小了的話「server 自己退出」會假冒成
// 「強制終止生效」，那條測試就變成假綠的。用 Sleep 而不是永久阻塞，是為了萬一
// 強制終止真的沒生效時，子進程仍會自己收掉、不留給後續測試。
const ignoreStdinCloseLifetime = 30 * time.Second

// TestMain 讓測試二進制在子進程模式下改當 MCP server。
//
// 一般模式（沒有 toolMcpServerEnv）照常跑測試，行為與沒有這個 TestMain 時相同。
func TestMain(m *testing.M) {
	if os.Getenv(toolMcpServerEnv) != "" {
		serveToolTestMcpServer(os.Stdin, os.Stdout)
		return
	}
	os.Exit(m.Run())
}

// serveToolTestMcpServer 讀 stdin 的逐行 JSON-RPC 訊息、回應到 stdout，直到 stdin 關閉。
func serveToolTestMcpServer(in io.Reader, out io.Writer) {
	if marker := os.Getenv(toolMcpSpawnMarkerEnv); marker != "" {
		// 失敗只能忽略：這裡是子進程，沒有 *testing.T 可以回報。marker 沒寫出來的
		// 後果是斷言「子進程被 spawn 過」的測試轉紅，那正確地指向有東西壞了。
		_ = os.WriteFile(marker, []byte("spawned\n"), 0o644)
	}
	srv := &toolTestMcpServer{out: out, mode: os.Getenv(toolMcpModeEnv)}
	if raw := os.Getenv(toolMcpToolsEnv); raw != "" {
		srv.tools = strings.Split(raw, ",")
	}

	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			srv.handle(line)
		}
		if err != nil {
			return
		}
	}
}

type toolTestMcpServer struct {
	tools []string
	out   io.Writer
	mode  string
}

func (s *toolTestMcpServer) handle(line []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}

	switch req.Method {
	case "initialize":
		if s.mode == modeHangInitialize {
			// 收下但不回應：client 的交握會一直等，直到它自己的期限到期。
			return
		}
		if s.mode == modeSlowInitializeThenDie {
			// 慢，而且失敗：交握等到一半，對面就沒了。
			time.Sleep(slowInitializeDelay)
			os.Exit(1)
		}
		if s.mode == modeSlowInitialize {
			// 慢，但會回：連線最終成功，只是佔掉一段可量測的時間。
			time.Sleep(slowInitializeDelay)
		}
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.writeResult(req.ID, map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tool-test", "version": "0.0.1-test"},
		})

	case "notifications/initialized":
		// 通知不回應。

	case "tools/list":
		decls := make([]map[string]any, 0, len(s.tools))
		for _, name := range s.tools {
			decls = append(decls, map[string]any{
				"name":        name,
				"description": "把收到的 text 原樣回覆（tool 套件的測試工具）",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			})
		}
		s.writeResult(req.ID, map[string]any{"tools": decls})
		switch s.mode {
		case modeIgnoreStdinClose:
			// 活著、但不再讀 stdin，也不理會 stdin 被關閉這個「請你收工」的訊號。
			time.Sleep(ignoreStdinCloseLifetime)
			os.Exit(0)
		case modeLeakPipeToGrandchild, modeLeakPipeToDetachedGrandchild:
			spawnPipeHoldingGrandchild(s.mode == modeLeakPipeToDetachedGrandchild)
			// 自己也不再讀 stdin：這樣關閉一定走到強制終止那條路。
			time.Sleep(ignoreStdinCloseLifetime)
			os.Exit(0)
		}

	case "tools/call":
		switch s.mode {
		case modeHangCall:
			// 收下但不回應。
			return
		case modeDieOnCall:
			// 連回應都不給就死掉：呼叫端會看到連線結束而不是一個錯誤回覆。
			os.Exit(1)
		}
		var params struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if !slices.Contains(s.tools, params.Name) {
			s.writeResult(req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "沒有名為 " + params.Name + " 的工具"}},
				"isError": true,
			})
			return
		}
		s.writeResult(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "收到：" + params.Arguments.Text}},
		})
	}
}

func (s *toolTestMcpServer) writeResult(id json.RawMessage, result any) {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return
	}
	_, _ = s.out.Write(append(line, '\n'))
}

// toolMcpSpec 回傳一份指向本地真實 stdio MCP server 的宣告：命令就是這個測試二進制
// 本身，以環境變數要它進 server 模式並挑一種失敗形態。
func toolMcpSpec(t *testing.T, name, mode string, tools ...string) core.McpServerSpec {
	t.Helper()
	return toolMcpSpecWithEnv(t, name, mode, nil, tools...)
}

// toolMcpSpecWithEnv 同上，另外疊上指定的環境變數（marker、孫行程 PID 檔一類）。
func toolMcpSpecWithEnv(t *testing.T, name, mode string, extra map[string]string, tools ...string) core.McpServerSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取得測試二進制路徑: %v", err)
	}
	env := map[string]string{
		toolMcpServerEnv: "1",
		toolMcpToolsEnv:  strings.Join(tools, ","),
		toolMcpModeEnv:   mode,
	}
	maps.Copy(env, extra)
	return core.McpServerSpec{
		Name:      name,
		Transport: core.McpTransportStdio,
		Command:   []string{exe},
		Env:       env,
	}
}
