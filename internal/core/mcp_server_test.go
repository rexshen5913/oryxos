// 本檔提供 MCP 測試用的**真實 stdio MCP server**：以子進程啟動、走真實的 JSON-RPC
// 往返（憲法 4.3、ADR-0002 明載「MCP Client 起真實的本地 stdio server」）。
// **不 mock MCP 協議、不注入假的 transport**——LLM 是錄製回放的唯一例外，MCP 不是。
//
// 實作手法是讓測試二進制**兼任** server：子進程以環境變數要求 server 模式，TestMain
// 在跑任何測試之前就早退到服務迴圈。這樣不必為測試多建一個 package（憲法 1.3 的
// 8 個 package 結構不動），也不必在測試裡先 go build 一支程式，而協議往返仍然是真的
// ——子進程、真的 stdin／stdout、真的 JSON-RPC。
//
// **這個 server 對協議刻意較真**：缺 protocolVersion 的 initialize、交握還沒完成就
// 來的 tools/list、jsonrpc 欄位不是 "2.0" 的請求，一律回 JSON-RPC 錯誤。client 與
// server 都由我們自己寫，若兩邊共用同一個誤解，測試會全綠而真實世界會壞——嚴格的
// server 就是那道交叉檢查：client 少送一個欄位、漏送 initialized 通知，這裡就轉紅。
package core_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// 子進程 server 模式的環境變數契約。值由 McpServerSpec.Env 帶進子進程，走的是
// 產品程式碼真正在用的那條 env 注入路徑。
const (
	// mcpServerModeEnv 非空即進入 server 模式（不跑任何測試）。
	mcpServerModeEnv = "ORYXOS_TEST_MCP_SERVER"
	// mcpServerNameEnv 是這個 server 的自稱，會出現在它的工具回應裡——測試據此
	// 判斷「答話的是哪一個 server」，那是撞名共存唯一能從外部驗證的方式。
	mcpServerNameEnv = "ORYXOS_TEST_MCP_NAME"
	// mcpServerToolsEnv 是這個 server 要暴露的工具名（逗號分隔）。
	mcpServerToolsEnv = "ORYXOS_TEST_MCP_TOOLS"
	// mcpServerMarkerEnv 指向一個檔案路徑，server 啟動時寫進去。存在即證明子進程
	// 真的被 spawn 過——「未被引用的宣告不 spawn 子進程」只有這樣才驗得到。
	mcpServerMarkerEnv = "ORYXOS_TEST_MCP_MARKER"
	// mcpServerExitAfterCallEnv 非空時，server 回完第一次 tools/call 就立刻退出。
	// 用來製造「回應與連線結束同時就緒」這個時序（見
	// TestMcpToolCallSurvivesServerExitingRightAfterReply）。
	mcpServerExitAfterCallEnv = "ORYXOS_TEST_MCP_EXIT_AFTER_CALL"
	// mcpServerProtocolEnv 覆寫 initialize 回應裡的 protocolVersion（預設回聲 client
	// 要求的那個）。用來驗協議版本協商。
	mcpServerProtocolEnv = "ORYXOS_TEST_MCP_PROTOCOL"
	// mcpServerNoToolsCapEnv 非空時，initialize 回應**不宣告** tools 能力。
	mcpServerNoToolsCapEnv = "ORYXOS_TEST_MCP_NO_TOOLS_CAP"
	// mcpServerPingEnv 是 server 要發的 ping 的 **id 原始 JSON**（例如 `1` 或 `"abc"`）。
	// 非空時，server 在回 initialize **之前**先發那個 ping，之後在收到**原樣帶回同一個
	// id** 的回覆前拒絕 tools/list。
	//
	// 值是原始 JSON 而不是字面值，因為規範允許 request ID 是字串或整數，兩種都要測得到。
	mcpServerPingEnv = "ORYXOS_TEST_MCP_PING_ID"
	// mcpServerStopReadingEnv 是「交握完成後停止讀 stdin 幾秒才退出」的秒數。
	//
	// 用來製造**寫入端阻塞**：server 還活著但不讀了，client 一個較大的 tools/call 就會
	// 把 pipe 塞滿、卡在 Write 上。這是驗「呼叫端的 ctx 對寫入路徑真的生效」唯一的方式。
	//
	// 為什麼要「幾秒後退出」而不是永遠不退：測試結束時 Close() 會等子進程收工（它得等
	// readLoop 讀完 stdout 才能 Wait），server 永不退出的話清理階段就會卡住。秒數要
	// 明顯大於測試的斷言視窗，否則「子進程退出把 Write 解開」會假冒成「ctx 生效」。
	mcpServerStopReadingEnv = "ORYXOS_TEST_MCP_STOP_READING_SECONDS"
	// mcpServerCallLogEnv 指向一個檔案，server 每收到一次 tools/call 就往裡面追加一行。
	//
	// 這是**從 server 那一側**數「真的被呼叫幾次」唯一的辦法。client 端看不出差別：
	// 一次沒送出去的呼叫與一次送出去但結果被丟掉的呼叫，在 client 眼裡都是「失敗」，
	// 而外部工具有沒有真的跑過，只有 server 知道。
	mcpServerCallLogEnv = "ORYXOS_TEST_MCP_CALL_LOG"
)

// TestMain 讓測試二進制在子進程模式下改當 MCP server。
//
// 一般模式（沒有 mcpServerModeEnv）照常跑測試，行為與沒有這個 TestMain 時相同。
func TestMain(m *testing.M) {
	if os.Getenv(mcpServerModeEnv) != "" {
		serveTestMcpServer(os.Stdin, os.Stdout)
		return
	}
	os.Exit(m.Run())
}

// serveTestMcpServer 讀 stdin 的逐行 JSON-RPC 訊息、回應到 stdout，直到 stdin 關閉。
// stdin 關閉就退出，正是 MCP stdio 規範要求 client 用來收乾淨子進程的機制。
func serveTestMcpServer(in io.Reader, out io.Writer) {
	if marker := os.Getenv(mcpServerMarkerEnv); marker != "" {
		// 失敗只能忽略：這裡是子進程，沒有 *testing.T 可以回報。marker 沒寫出來的
		// 後果是斷言「子進程被 spawn 過」的測試轉紅，那正確地指向有東西壞了。
		_ = os.WriteFile(marker, []byte("spawned\n"), 0o644)
	}
	srv := &testMcpServer{
		name:            os.Getenv(mcpServerNameEnv),
		out:             out,
		exitAfterCall:   os.Getenv(mcpServerExitAfterCallEnv) != "",
		protocolVersion: os.Getenv(mcpServerProtocolEnv),
		noToolsCap:      os.Getenv(mcpServerNoToolsCapEnv) != "",
		callLog:         os.Getenv(mcpServerCallLogEnv),
	}
	if raw := os.Getenv(mcpServerStopReadingEnv); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err == nil {
			srv.stopReadingAfterList = time.Duration(secs) * time.Second
		}
	}
	if raw := os.Getenv(mcpServerPingEnv); raw != "" {
		srv.pingID = json.RawMessage(raw)
	}
	if raw := os.Getenv(mcpServerToolsEnv); raw != "" {
		srv.tools = strings.Split(raw, ",")
	}

	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			srv.handle(line)
			if srv.exitNow {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// testMcpServer 是一個最小的 MCP server：initialize 交握、tools/list、tools/call。
type testMcpServer struct {
	name  string
	tools []string
	out   io.Writer
	// initialized 在收到 notifications/initialized 後為真。交握完成前的請求一律
	// 拒絕——client 漏送那個通知的話，測試會在 tools/list 就轉紅。
	initialized bool
	// exitAfterCall 為真時，回完一次 tools/call 就把 exitNow 立起來讓服務迴圈收工。
	exitAfterCall bool
	exitNow       bool
	// protocolVersion 非空時覆寫 initialize 回應的版本（空字串＝回聲 client 要求的）。
	protocolVersion string
	// noToolsCap 為真時 initialize 回應不宣告 tools 能力。
	noToolsCap bool
	// callLog 非空時，每次 tools/call 都往這個檔案追加一行。
	callLog string
	// stopReadingAfterList 非零時，回完 tools/list 就停止讀 stdin，撐這麼久之後才退出
	// （見 mcpServerStopReadingEnv）。
	stopReadingAfterList time.Duration
	// pingID 非 nil 時，server 在回 initialize 之前先用這個 id 發一個 ping，並在收到
	// **原樣帶回同一個 id** 的回覆前拒絕 tools/list。client 若把 ping 誤當成 initialize
	// 的回應、不回 ping、或回了一個換算過的 id，tools/list 就會失敗。
	pingID    json.RawMessage
	pingAcked bool
}

// testRPCRequest 是 server 端解析訊息用的最小形狀。
//
// ID 是 json.RawMessage 而不是 *int：規範允許 request ID 是字串或整數，這個 server 要
// 能原樣回聲 client 送來的任何 id，也要能比對 client 回給我們 ping 的 id 是否一字不差。
type testRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *testMcpServer) handle(line []byte) {
	var req testRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, -32700, "無法解析的 JSON: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeError(req.ID, -32600, "jsonrpc 必須是 \"2.0\"，收到 "+req.JSONRPC)
		return
	}
	// 沒有 method 的訊息是 client 對**我們發出的請求**的回應（本 server 只發 ping）。
	// 只有 id **一字不差**才算回覆到了——換算過的 id 對我們來說是另一個請求。
	if req.Method == "" {
		if s.pingID != nil && bytes.Equal(bytes.TrimSpace(req.ID), bytes.TrimSpace(s.pingID)) {
			s.pingAcked = true
		}
		return
	}

	switch req.Method {
	case "initialize":
		if s.pingID != nil {
			// 在回 initialize **之前**先發 ping。id 由測試指定：整數時刻意與 client 的
			// initialize 撞號（JSON-RPC 的 id 是各自的命名空間，撞號完全合規），字串時
			// 則測 client 有沒有假設 id 一定是整數。
			s.write(map[string]any{"jsonrpc": "2.0", "id": s.pingID, "method": "ping"})
		}
		var params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeError(req.ID, -32602, "initialize 的 params 無法解析: "+err.Error())
			return
		}
		if params.ProtocolVersion == "" {
			s.writeError(req.ID, -32602, "initialize 缺 protocolVersion")
			return
		}
		if len(params.ClientInfo) == 0 {
			s.writeError(req.ID, -32602, "initialize 缺 clientInfo")
			return
		}
		version := params.ProtocolVersion
		if s.protocolVersion != "" {
			version = s.protocolVersion
		}
		capabilities := map[string]any{"tools": map[string]any{}}
		if s.noToolsCap {
			capabilities = map[string]any{}
		}
		result := map[string]any{
			"capabilities": capabilities,
			"serverInfo":   map[string]any{"name": s.name, "version": "0.0.1-test"},
		}
		// 哨兵值 "-" 代表整個欄位不寫（模擬回應缺 protocolVersion 的壞 server）。
		if version != "-" {
			result["protocolVersion"] = version
		}
		s.writeResult(req.ID, result)

	case "notifications/initialized":
		// 通知沒有 id、也不該有回應。多回一個回應會讓 client 讀到對不上任何請求的
		// 訊息——這裡照規範保持安靜。
		s.initialized = true

	case "tools/list":
		if !s.initialized {
			s.writeError(req.ID, -32002, "尚未收到 initialized 通知，交握未完成")
			return
		}
		if s.pingID != nil && !s.pingAcked {
			// 規範要求收到 ping 必須回覆。沒回覆代表 client 要嘛把 ping 誤當成別的訊息、
			// 要嘛根本不處理 server 發起的請求。
			s.writeError(req.ID, -32603, "沒有收到 ping 的回覆，client 未依規範回應 server 的請求")
			return
		}
		decls := make([]map[string]any, 0, len(s.tools))
		for _, name := range s.tools {
			decls = append(decls, map[string]any{
				"name":        name,
				"description": "把收到的 text 原樣回覆（" + s.name + " 的測試工具）",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required":   []string{"text"},
				},
			})
		}
		s.writeResult(req.ID, map[string]any{"tools": decls})
		if s.stopReadingAfterList > 0 {
			// 活著但不再讀 stdin：client 接下來的大請求會把 pipe 塞滿。用 Sleep 而不是
			// 永久阻塞，Go runtime 才不會把「所有 goroutine 都睡著」判成 deadlock，
			// 而且測試的清理階段等得到子進程退出。
			time.Sleep(s.stopReadingAfterList)
			os.Exit(0)
		}

	case "tools/call":
		if !s.initialized {
			s.writeError(req.ID, -32002, "尚未收到 initialized 通知，交握未完成")
			return
		}
		var params struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeError(req.ID, -32602, "tools/call 的 params 無法解析: "+err.Error())
			return
		}
		if s.callLog != "" {
			// 先記帳再回應：測試收到回應時，這一行一定已經寫進去了。
			if f, err := os.OpenFile(s.callLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				_, _ = f.WriteString(params.Name + "\n")
				_ = f.Close()
			}
		}
		if !slices.Contains(s.tools, params.Name) {
			// 未知工具照規範走「result 帶 isError」而不是 protocol error：那是工具
			// 執行層的失敗，不是協議層的。
			s.writeResult(req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "沒有名為 " + params.Name + " 的工具"}},
				"isError": true,
			})
			return
		}
		s.writeResult(req.ID, map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": s.name + "/" + params.Name + " 收到：" + params.Arguments.Text,
			}},
		})
		// 回完就退出：client 那端的回應與「連線已結束」會同時就緒。
		s.exitNow = s.exitAfterCall

	default:
		s.writeError(req.ID, -32601, "不支援的 method "+req.Method)
	}
}

func (s *testMcpServer) writeResult(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *testMcpServer) writeError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

// write 送出一則訊息：一行一則 JSON，這是 stdio transport 的分幀方式。
func (s *testMcpServer) write(msg any) {
	line, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_, _ = s.out.Write(append(line, '\n'))
}

// mcpSpec 回傳一份指向本地真實 stdio MCP server 的宣告：命令就是這個測試二進制
// 本身，以環境變數要它進 server 模式。
func mcpSpec(t *testing.T, name string, tools ...string) core.McpServerSpec {
	t.Helper()
	return mcpSpecWithMarker(t, name, "", tools...)
}

// mcpSpecExitingAfterCall 回傳一份「回完第一次 tools/call 就退出」的 server 宣告。
func mcpSpecExitingAfterCall(t *testing.T, name string, tools ...string) core.McpServerSpec {
	t.Helper()
	spec := mcpSpec(t, name, tools...)
	spec.Env[mcpServerExitAfterCallEnv] = "1"
	return spec
}

// mcpSpecWithEnv 回傳一份額外帶指定環境變數的 server 宣告，供交握相關的模式使用。
func mcpSpecWithEnv(t *testing.T, name string, extra map[string]string, tools ...string) core.McpServerSpec {
	t.Helper()
	spec := mcpSpec(t, name, tools...)
	for key, val := range extra {
		spec.Env[key] = val
	}
	return spec
}

// mcpSpecWithMarker 同 mcpSpec，另要求 server 啟動時寫一個 marker 檔。marker 為空
// 字串時不寫。
func mcpSpecWithMarker(t *testing.T, name, marker string, tools ...string) core.McpServerSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取得測試二進制路徑: %v", err)
	}
	env := map[string]string{
		mcpServerModeEnv:  "1",
		mcpServerNameEnv:  name,
		mcpServerToolsEnv: strings.Join(tools, ","),
	}
	if marker != "" {
		env[mcpServerMarkerEnv] = marker
	}
	return core.McpServerSpec{Name: name, Transport: "stdio", Command: []string{exe}, Env: env}
}
