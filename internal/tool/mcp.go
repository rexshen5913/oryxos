// mcp.go 是 OryxOS 的 MCP Client：把外部 MCP server 的工具接成一般的 OryxTool。
//
// 兩個子模組都住在本 package（技術方案 §6，AC 明列不另闢 package）：
//
//   - McpClientService  連線維護與工具註冊（啟動時連線 → initialize → tools/list）
//   - McpToolAdapter    把單一 MCP 工具適配成 OryxTool，呼叫時經協議轉發
//
// 它們與內建 Tool 共用同一個 OryxTool 抽象與同一個 ToolRegistry，拆細沒有收益；
// ReAct 循環因此不感知工具來自哪裡（憲法 2.1、技術方案 §6.1）。
//
// **協議自實作、只用標準庫**（維護者定案 2026-08-14）。核心階段只需要 stdio 上的四則
// 訊息：initialize、notifications/initialized、tools/list、tools/call，自實作約兩百行、
// 完全可控，符合憲法 1.4 的標準庫優先與 §3.2 的反過度工程；技術方案 §1.2 提過的
// 「官方 Go MCP SDK 加 mark3labs/mcp-go」是規劃期的 proposal，不是 confirmed decision，
// 且引入後仍要為 OryxTool 寫一層 adapter，省下的比看起來少。
//
// 交換的代價是協議正確性由我們自己負責，所以自動化測試的另一端起的是**真實的本地
// stdio server 子進程**、走真實的 JSON-RPC 往返，而且那個 server 對規格刻意較真
// （缺 protocolVersion、交握未完成就發請求都會被它拒絕），用來交叉檢查本檔的實作
// （憲法 4.3、ADR-0002：MCP 不適用 LLM 那條錄製回放的例外）。
//
// **這不違反憲法 2.3 的顯式註冊**：工具清單來自使用者手寫的 mcp_servers.yaml 加
// Profile 的 mcp_servers 欄位，是配置驅動而非反射或型別掃描（技術方案 §6.6）。
package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/core"
)

const (
	// McpToolSeparator 是 MCP 工具註冊名裡 server 前綴與工具名之間的分隔符。
	//
	// 取**雙**底線是使用者可見的契約（它決定 Profile tools 欄位要打的字）：單底線在
	// 工具名本身含底線時看不出切點（search_pr 到底是哪個 server 的什麼），冒號在 YAML
	// 裡是否需要引號隨位置而變。落選形狀與理由見 spec #3 的 Further Notes。
	McpToolSeparator = "__"

	// mcpProtocolVersion 是 initialize 交握宣告的 MCP 協議版本。
	mcpProtocolVersion = "2025-06-18"
	// mcpClientName／mcpClientVersion 是交握時的自我介紹，server 端的日誌會看到。
	mcpClientName    = "oryxos"
	mcpClientVersion = "0.1.0"

	// mcpTransportStdio 是核心階段唯一支援的 transport。
	mcpTransportStdio = "stdio"
)

// mcpSupportedProtocolVersions 是本 client 認得的協議版本，交握時用來判斷 server 回的
// 版本能不能繼續。
//
// **為什麼是一份清單而不是只認 mcpProtocolVersion**：規範允許 server 回一個與我們要求
// 的**不同**的版本（它支援的那個），client 支援就繼續、不支援才斷開。而本 client 只用
// 四則訊息（initialize、initialized、tools/list、tools/call），這四則在這幾個版本裡的
// 形狀相同——只認最新的那個會把大量固定在舊版的 server 擋在門外，那不是嚴謹是失能。
//
// 反過來，完全不檢查也不行：那等於宣稱「什麼版本我都能講」，日後某個不相容的版本出來
// 時會表現成半路壞掉，而不是啟動時一句清楚的話。
var mcpSupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxToolFunctionNameRunes 是註冊名的長度上限。
//
// 數字來自 OpenAI 兼容協議對 function name 的約束（`^[a-zA-Z0-9_-]{1,64}$`）：註冊名
// 會原樣送進每一輪 LLM 請求的 tools 欄位，不合格的名字會讓**整輪呼叫**被端點以 400
// 打回——連完全沒用到那個工具的純對話也一起死。所以這個約束要在啟動時擋，不能等到
// 對話中途。
const maxToolFunctionNameRunes = 64

// validateToolFunctionName 校驗一個註冊名送得進 Function Calling。
//
// 為什麼是 MCP 才需要這道：內建 Tool 的名字是我們自己取的（http_get、save_memory），
// 恆合格；MCP 的註冊名由**使用者寫的 server 名**加上**外部 server 自報的工具名**拼成，
// 兩截都不受我們控制。`mcp_servers: { "github.com/foo": … }` 是一個看起來很合理的宣告，
// 卻會產出送不出去的 function name。
//
// 這與 Profile 的 skills 欄位要過 ValidateSkillName 是同一條原則：使用者可寫的字串
// 在變成識別符之前先校驗，設定筆誤不要拖到執行期才炸。
func validateToolFunctionName(server, toolName, registered string) error {
	if n := utf8.RuneCountInString(registered); n > maxToolFunctionNameRunes {
		return fmt.Errorf("MCP server %q 的工具 %q 組出的註冊名 %q 長 %d 字元，超過 LLM "+
			"Function Calling 的上限 %d；請在 %s 把這個 server 的宣告名改短",
			server, toolName, registered, n, maxToolFunctionNameRunes, core.McpServersFile)
	}
	for _, r := range registered {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("MCP server %q 的工具 %q 組出的註冊名 %q 含不合法字元 %q"+
			"（LLM Function Calling 只允許英數、底線與連字號）；若是 server 宣告名的問題，"+
			"請在 %s 改名",
			server, toolName, registered, string(r), core.McpServersFile)
	}
	return nil
}

// McpToolName 組出一個 MCP 工具在 ToolRegistry 裡的註冊名：<server>__<tool>。
//
// 帶 server 前綴有三個理由：撞名共存是硬需求（同一個 Agent 宣告兩個各有 search 的
// server 時，現行 Registry.Register 對重名直接報錯）；Profile 的 tools 欄位要寫得出
// 「我要哪一個」；審計表的 tool_name 要看得出來源。代價是 server 改名會讓 Profile
// 失效——那是**期望的**行為，設定筆誤不該靜默。
func McpToolName(server, toolName string) string {
	return server + McpToolSeparator + toolName
}

// ── JSON-RPC 2.0 的訊息形狀 ──────────────────────────────────────────────

// rpcRequest 是**我們送出**的請求或通知：ID 為 nil 即通知（不期待回應）。
//
// 這裡用 int 是安全的：規範允許 request ID 是字串或整數，而送出的那一端由我們自己挑，
// 挑整數最簡單。**讀進來的 ID 不能這樣假設**——見 rpcMessage。
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcError 是協議層錯誤（工具執行層的失敗不走這裡，見 mcpToolResult）。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcReply 是我們對 server **發起的請求**所回的回應（例如 ping）。
//
// ID 是 json.RawMessage 而不是具體型別，因為它必須**原樣帶回** server 送來的那個 id：
// 規範允許 id 是字串或整數（官方 ping 範例用的就是字串），而回應要能讓 server 對得上
// 它自己的請求。轉成 int 再轉回去會把 "123" 變成 123，那是另一個 id。
type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcMessage 是從 server 讀進來的一則訊息。
//
// stdio 上是**雙向**的 JSON-RPC：server 不只回應我們，也可以自己發請求（規範允許任一
// 方隨時 ping）與通知。三者靠兩個欄位分辨，順序不能反：
//
//	有 method ＋ 有 id  → server 發起的**請求**，我們必須回應
//	有 method ＋ 無 id  → 通知，不必回應
//	無 method           → 對我們某個請求的**回應**，按 id 派給等待端
//
// 只看 id 是不夠的——這正是一個會咬人的坑：JSON-RPC 的 id 是**各自**的命名空間，
// server 的 ping 完全可以用 id=1，而那時我們的 initialize 也是 id=1。只看 id 就會把
// 那個 ping 當成 initialize 的回覆交給等待端（result 是空的），真正的回覆隨後到達卻
// 因為沒有人在等而被丟掉，同時我們還漏掉了規範要求的 ping 回應。
//
// **ID 必須是 json.RawMessage，不能是 *int**：規範允許 request ID 是**字串或整數**，
// 官方的 ping 範例用的就是字串。宣告成 *int 的話，一個字串 id 的 ping 會讓整則訊息在
// json.Unmarshal 就失敗、被當成雜訊丟掉——我們不回覆，server 於是判定連線已死。存原始
// bytes 才能同時做到「回覆時原樣帶回」與「認得出哪些是我們自己送出的整數 id」。
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// hasRPCID 判斷一則訊息帶不帶 id。
//
// 缺欄位與 null 都算沒有：規範明文禁止用 null 當 request ID，所以 `"id": null` 的訊息
// 不該被當成一個需要回應的請求。
func hasRPCID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// ourRequestID 把一則回應的 id 解回我們送出時用的那個整數。
//
// 我們送出的 id 一律是整數（見 rpcRequest），而規範要求 server 回應時原樣帶回同一個
// id，所以解不出整數就不是我們發過的請求——那可能是 server 自己發起的請求（但那條路
// 已由 method 分流走了），或是一則不合規的訊息。
func ourRequestID(raw json.RawMessage) (int, bool) {
	var id int
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, false
	}
	return id, true
}

// mcpToolDecl 是 tools/list 回的單一工具宣告。
type mcpToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mcpToolResult 是 tools/call 的結果。
//
// **IsError 與協議層錯誤是兩回事**：工具跑了但失敗（參數不對、外部 API 回 4xx）走
// 這個旗標，協議層的錯誤才走 rpcError。兩者最後都以 ToolResult.Error 回填給 LLM，
// 但混為一談會讓「server 壞了」與「工具說不行」在日誌裡分不出來。
type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// text 把結果的 content 併成一段給 LLM 讀的文字。
//
// 只取 type 為 text 的區塊：核心階段的 Tool 結果是字串，圖片與嵌入資源等區塊沒有
// 回填的位置。完全沒有文字區塊時回整包 content 的 JSON，而不是空字串——讓 LLM 看到
// 「有東西但我讀不懂」，比看到「什麼都沒有」誠實。
func (r mcpToolResult) text(raw json.RawMessage) string {
	var parts []string
	for _, block := range r.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return string(raw)
	}
	return strings.Join(parts, "\n")
}

// ── 一條 stdio 連線 ──────────────────────────────────────────────────────

// writeDeadlineSetter 是「可以設定寫入期限」這個能力。
//
// 存在的理由：往子進程的 stdin 寫入是**阻塞路徑**——server 停止讀取時 pipe 會塞滿，
// `Write` 就此卡住，而 `io.Writer` 沒有任何辦法把 ctx 傳進去。os 的 pipe 支援設定期限，
// 把期限設成「現在」會讓卡住的 Write 立刻以 os.ErrDeadlineExceeded 返回——這是唯一
// 不必開一條「寫完就不管、注定洩漏」的 goroutine 就能讓 ctx 生效的做法。
type writeDeadlineSetter interface {
	SetWriteDeadline(t time.Time) error
}

// mcpConn 是一條到某個 MCP server 的 stdio 連線：一個子進程加它的 JSON-RPC 往返。
//
// **讀取集中在一條 readLoop goroutine**，呼叫端只等自己那個 id 的回應。這個形狀解掉
// 三件事：呼叫端的 ctx 能真的生效（阻塞在 channel 而不是阻塞在 read 上，憲法 5.3）；
// 逾時後才到的回應會被丟掉而不是被下一次呼叫誤讀成自己的答案；server 中途死掉時
// readLoop 收到 EOF，所有還在等的呼叫一次收到明確錯誤，而不是永遠掛住。
type mcpConn struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	logger *slog.Logger

	// 交握協商出來的結果，initialize 成功後才有值，之後唯讀（只給日誌用）。
	protocolVersion string
	serverName      string
	serverVersion   string

	// writeSlot 序列化寫入 stdin：一則訊息一行，兩個併發的寫入會把行交錯。
	//
	// **是容量 1 的 channel 而不是 sync.Mutex**，因為取得寫入權這件事本身也必須可取消：
	// 一個卡在 Write 上的呼叫（server 停止讀取、pipe 塞滿）會讓後面每個等鎖的呼叫連
	// 自己的 ctx 都看不到——`sync.Mutex.Lock` 沒有辦法中途放棄（憲法 5.3）。
	writeSlot chan struct{}
	// stdinDeadline 是 stdin 的「設定寫入期限」能力，用來讓卡在 Write 上的呼叫在 ctx
	// 取消時立刻返回（見 write）。
	//
	// 斷言的是**能力（介面）而不是具體型別**：exec.Cmd.StdinPipe 回的是一個內嵌
	// *os.File 的私有型別，方法是提升上來的，斷言成 *os.File 會失敗。
	stdinDeadline writeDeadlineSetter

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcMessage
	// failure 記下連線為何不可用（server 退出、讀取失敗）。一旦設定就不再變，後續
	// 呼叫一律以它失敗——核心階段不自動重連（spec #3 Further Notes）。
	failure error
	// done 在 readLoop 結束時關閉，讓等待中的呼叫不必等到自己的 ctx 逾時。
	done chan struct{}
	// readers 追蹤讀 stdout 與 stderr 的兩條 goroutine。close() 必須等它們讀完才能
	// 呼叫 cmd.Wait()：Wait 會把那兩個 pipe 關掉，而 os/exec 明載「在所有讀取完成前
	// 呼叫 Wait 是不正確的」——搶在讀取前關會讓 readLoop 拿到「檔案已關閉」而不是
	// EOF，失效原因就記錯了。
	readers sync.WaitGroup
}

// dialMcpStdio 啟動 spec 宣告的子進程並完成 initialize 交握。
//
// ctx 只約束**交握**，不約束子進程的壽命：那個進程要活到整場對話結束，由 close()
// 收掉。所以這裡用 exec.Command 而不是 exec.CommandContext——後者會在 ctx 結束時殺掉
// server，而傳進來的 ctx 可能只是啟動階段的那一個。
func dialMcpStdio(ctx context.Context, spec core.McpServerSpec, logger *slog.Logger) (*mcpConn, error) {
	// transport 的判斷在這裡：能不能連得看得懂什麼協議，這是撥號的前提而不是額外校驗。
	if spec.Transport != "" && spec.Transport != mcpTransportStdio {
		return nil, fmt.Errorf("MCP server %q 宣告的 transport %q 不支援：核心階段只支援 %s",
			spec.Name, spec.Transport, mcpTransportStdio)
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("MCP server %q 沒有宣告 command，無法啟動", spec.Name)
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	// 宣告的 env **疊在**目前環境之上而不是取代它：多數 MCP server 需要 PATH、HOME
	// 這類基本變數才跑得起來（node、python 尤其）。使用者宣告的是「額外要給的憑證」，
	// 不是「這個進程的完整環境」。
	cmd.Env = append(os.Environ(), envPairs(spec.Env)...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("接上 MCP server %q 的 stdin: %w", spec.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("接上 MCP server %q 的 stdout: %w", spec.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("接上 MCP server %q 的 stderr: %w", spec.Name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("啟動 MCP server %q（%s）: %w", spec.Name, spec.Command[0], err)
	}

	conn := &mcpConn{
		name:      spec.Name,
		cmd:       cmd,
		stdin:     stdin,
		logger:    logger,
		pending:   make(map[int]chan rpcMessage),
		done:      make(chan struct{}),
		writeSlot: make(chan struct{}, 1),
	}
	if setter, ok := stdin.(writeDeadlineSetter); ok {
		conn.stdinDeadline = setter
	} else {
		// 實務上到不了：exec 的 StdinPipe 一定是個內嵌 *os.File 的型別。留這一句是為了
		// 日後 stdlib 換實作時，「ctx 取消對寫入失效」這件事看得見而不是靜默退化。
		logger.Warn("mcp_stdin_no_write_deadline", "server", spec.Name)
	}
	conn.readers.Add(2)
	go conn.readLoop(stdout)
	// MCP 規範把 server 的 stderr 定為它的日誌管道。轉進結構化日誌而不是丟掉：
	// 「這個 server 為什麼沒給我工具」的答案通常只寫在那裡。
	go conn.forwardStderr(stderr)

	if err := conn.initialize(ctx); err != nil {
		// 交握失敗就把已經起來的子進程收掉，不留孤兒。
		if cerr := conn.close(); cerr != nil {
			logger.Warn("mcp_server_close_failed", "server", spec.Name, "error", cerr.Error())
		}
		return nil, err
	}
	return conn, nil
}

// envPairs 把 env map 轉成 exec.Cmd 要的 KEY=VALUE 形式。
func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for key, val := range env {
		pairs = append(pairs, key+"="+val)
	}
	return pairs
}

// mcpInitializeResult 是 initialize 回應裡我們會用到的部分。
type mcpInitializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		// Tools 為 nil 代表這個 server **沒有宣告** tools 能力。指標型別是刻意的：
		// 要分得出「沒有這個 key」與「有但是空物件」——後者是合法的宣告。
		Tools *struct {
			// ListChanged 宣告 server 會在工具變動時送通知。核心階段不處理那個通知
			// （刻意不做，見 spec #3 Further Notes），這個欄位只是讓型別自我說明。
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// initialize 走完 MCP 的交握：initialize 請求 → 驗回應 → 送 initialized 通知。
//
// **通知不能省**：規範要求 client 收到 initialize 結果後發它，server 才進入可服務狀態。
// 少了它，一個照規格實作的 server 會拒絕後續所有請求。
//
// **回應也不能丟**。規範把交握定義成一次協商，協商結果決定了後面能做什麼：
//
//   - protocolVersion：server 可以回一個**與我們要求的不同**的版本（它支援的那個），
//     client 支援就繼續、不支援就該斷開。整包丟掉等於宣稱「什麼版本我都能講」。
//   - capabilities.tools：規範要求只使用**協商成功**的能力。沒宣告 tools 的 server 上
//     呼叫 tools/list 是違規的，而且它對 OryxOS 沒有用（我們接它就是為了工具）——
//     這時清楚失敗遠好過讓一個 -32601 從別的地方冒出來。
func (c *mcpConn) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": mcpClientName, "version": mcpClientVersion},
	}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("MCP server %q 的 initialize 交握失敗: %w", c.name, err)
	}

	var result mcpInitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("解析 MCP server %q 的 initialize 回應: %w", c.name, err)
	}
	if result.ProtocolVersion == "" {
		return fmt.Errorf("MCP server %q 的 initialize 回應沒有 protocolVersion，無法確認協議版本", c.name)
	}
	if !slices.Contains(mcpSupportedProtocolVersions, result.ProtocolVersion) {
		return fmt.Errorf("MCP server %q 使用的協議版本 %q 不支援（OryxOS 支援 %s）",
			c.name, result.ProtocolVersion, strings.Join(mcpSupportedProtocolVersions, "、"))
	}
	if result.Capabilities.Tools == nil {
		return fmt.Errorf("MCP server %q 沒有宣告 tools 能力，無法從它取回任何工具"+
			"（OryxOS 接 MCP server 只用 tools 這一項）", c.name)
	}

	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("MCP server %q 的 initialized 通知失敗: %w", c.name, err)
	}
	// 協商結果留下來給連線成功的日誌用：使用者要判斷 Agent 的能力範圍時，「連上的是
	// 哪個 server 的哪個版本、講的是哪版協議」是第一手資訊。
	c.protocolVersion = result.ProtocolVersion
	c.serverName = result.ServerInfo.Name
	c.serverVersion = result.ServerInfo.Version
	return nil
}

// listTools 取回這個 server 的工具清單。
//
// 跟著 nextCursor 把每一頁都取回來：協議允許 server 分頁，只取第一頁會讓後面的工具
// **安靜地消失**——使用者只會看到 Agent 莫名其妙不會做某件事。頁數有上限，免得一個
// 回傳固定 cursor 的壞 server 讓啟動永遠跑不完。
func (c *mcpConn) listTools(ctx context.Context) ([]mcpToolDecl, error) {
	const maxPages = 100
	var decls []mcpToolDecl
	cursor := ""
	for range maxPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q 的 tools/list 失敗: %w", c.name, err)
		}
		var page struct {
			Tools      []mcpToolDecl `json:"tools"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("解析 MCP server %q 的 tools/list 結果: %w", c.name, err)
		}
		decls = append(decls, page.Tools...)
		if page.NextCursor == "" {
			return decls, nil
		}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("MCP server %q 的 tools/list 分頁超過 %d 頁，放棄", c.name, maxPages)
}

// callTool 轉發一次工具呼叫。args 必須是 JSON object（協議要求）。
func (c *mcpConn) callTool(ctx context.Context, toolName string, args json.RawMessage) (mcpToolResult, json.RawMessage, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return mcpToolResult{}, nil, err
	}
	var result mcpToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return mcpToolResult{}, nil, fmt.Errorf("解析 MCP server %q 的 tools/call 結果: %w", c.name, err)
	}
	return result, raw, nil
}

// call 送出一個請求並等它的回應。
//
// 阻塞在 ctx 與 done 之間而不是阻塞在 read 上，逾時與取消因此能真的貫穿（憲法 5.3）。
func (c *mcpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	reply := make(chan rpcMessage, 1)
	c.pending[id] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	// **回應優先於連線結束與取消**：三個 case 同時就緒時 select 是隨機挑的，而
	// 「server 答完就退出」（答完即結束的包裝腳本、回完最後一句就崩的 server）會讓
	// 回應與 c.done 同時就緒——那時挑到 c.done 等於把一個好的答案丟掉、換一句
	// 「連線已結束」。ctx 逾時同理。所以那兩條 case 都先非阻塞地看一眼 reply。
	//
	// 反過來排（先無條件檢查 reply）不行：那會讓「還沒有回應」的常況多繞一圈，也
	// 寫不出「等」這個語義。
	select {
	case <-ctx.Done():
		if resp, ok := pollResponse(reply); ok {
			return c.result(method, resp)
		}
		return nil, fmt.Errorf("等待 MCP server %q 的 %s 回應: %w", c.name, method, ctx.Err())
	case <-c.done:
		if resp, ok := pollResponse(reply); ok {
			return c.result(method, resp)
		}
		return nil, c.failureErr()
	case resp := <-reply:
		return c.result(method, resp)
	}
}

// pollResponse 非阻塞地看 reply 裡有沒有已經到達的回應。
func pollResponse(reply <-chan rpcMessage) (rpcMessage, bool) {
	select {
	case resp := <-reply:
		return resp, true
	default:
		return rpcMessage{}, false
	}
}

// result 把一則回應轉成 call 的回傳值：協議層錯誤轉成 Go 錯誤，其餘回 result 原文。
func (c *mcpConn) result(method string, resp rpcMessage) (json.RawMessage, error) {
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP server %q 的 %s 回錯誤（code %d）: %s",
			c.name, method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// notify 送出一則不期待回應的通知。
func (c *mcpConn) notify(ctx context.Context, method string, params any) error {
	return c.send(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// send 送出一則請求或通知。
func (c *mcpConn) send(ctx context.Context, msg rpcRequest) error {
	return c.write(ctx, msg, msg.Method+" 請求")
}

// write 把一則訊息寫成一行 JSON。what 只用在錯誤訊息裡，指出是哪一種訊息寫失敗。
//
// 一行一則是 stdio transport 的分幀方式，成立的前提是訊息本身不含裸換行——
// encoding/json 會把字串裡的換行轉義成 \n，所以這個前提由編碼器保證。
//
// **這是一條阻塞路徑，所以吃 ctx**（憲法 5.3）。server 交握後停止讀 stdin 時，一個較大
// 的 tools/call 會把 pipe 塞滿，`Write` 就此卡住；沒有下面兩道，呼叫端已經給的逾時與
// 取消完全不會生效：
//
//   - 取得寫入權用可取消的 select，而不是 sync.Mutex.Lock（後者沒辦法中途放棄）
//   - ctx 結束時把 stdin 的寫入期限設成「現在」，卡住的 Write 立刻返回
//
// 寫入失敗一律**判定整條連線不可用**。理由是 pipe 的寫入超過 PIPE_BUF 就不是原子的：
// 卡住的那次很可能已經寫出半截 JSON，server 之後讀到的每一則訊息都會錯位。與其讓後續
// 呼叫在一條已經錯亂的連線上得到莫名其妙的結果，不如立刻給出明確的失敗。
func (c *mcpConn) write(ctx context.Context, msg any, what string) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("編碼 MCP server %q 的 %s: %w", c.name, what, err)
	}

	select {
	case c.writeSlot <- struct{}{}:
		defer func() { <-c.writeSlot }()
	case <-ctx.Done():
		return fmt.Errorf("等待寫入 MCP server %q 的機會（%s）: %w", c.name, what, ctx.Err())
	case <-c.done:
		return c.failureErr()
	}

	// **拿到寫入權之後、產生任何副作用之前，再確認一次 ctx。**
	//
	// 上面那個 select 在「ctx 已經取消」且「寫入權剛好有空」時兩個 case 同時就緒，而
	// Go 是隨機挑的——沒有這道檢查，一個早就被取消的呼叫有大約一半機率仍然把請求送出去，
	// 於是外部工具**照樣執行了一次**（那是使用者以為自己取消掉的動作）。
	//
	// 順帶也擋掉另一個形態：那次寫入可能被下面的期限機制打斷成失敗，讓一條其實健康的
	// 連線被判死。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("不送出給 MCP server %q 的 %s（呼叫已取消）: %w", c.name, what, err)
	}

	if c.stdinDeadline != nil {
		// ran 讓我們等得到「已經啟動的回呼」跑完。
		//
		// **`stop()` 不會等回呼結束**——它回傳 false 只代表回呼已經被啟動（文件明寫
		// 「The stop function does not wait for f to complete」）。少了這道等待，
		// 「ctx 取消」與「寫入成功」競態時會留下一個**過期的期限**：我們先返回、釋放
		// writeSlot，回呼稍後才把期限設成過去，於是**下一則訊息**的正常寫入立刻以
		// deadline exceeded 失敗、還把整條連線判死。壞的是後面那次無關的呼叫，是最難查
		// 的一種形態。
		ran := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(ran)
			_ = c.stdinDeadline.SetWriteDeadline(time.Now())
		})
		defer func() {
			if !stop() {
				<-ran // 回呼已啟動：等它動完期限，我們才有資格清掉
			}
			// 成功或失敗都把期限清乾淨，不留給下一則訊息。這一段在**還持有 writeSlot 時**
			// 跑完（defer 是後進先出，釋放 slot 的那個 defer 註冊得更早、跑得更晚），
			// 所以下一個寫入者一定看到乾淨的狀態。
			_ = c.stdinDeadline.SetWriteDeadline(time.Time{})
		}()
	}

	payload := append(line, '\n')
	n, err := c.stdin.Write(payload)
	if err != nil {
		// **只有真的寫出了位元組才判連線死。** pipe 的寫入超過 PIPE_BUF 就不是原子的，
		// 送出半截 JSON 的話 server 之後讀到的每一則訊息都會錯位，那條連線救不回來。
		// 但 n == 0 是另一回事：一個字都沒出去，連線還是乾淨的（最常見的來源正是
		// 「呼叫在寫入前一刻被取消」），這時把它判死等於誤殺。
		if n > 0 {
			c.fail(fmt.Errorf("MCP server %q 的連線已中斷（只送出 %d／%d 位元組的訊息）: %w",
				c.name, n, len(payload), err))
		}
		return fmt.Errorf("寫入 MCP server %q 的 stdin（%s）: %w", c.name, what, err)
	}
	return nil
}

// readLoop 逐行讀 server 的輸出並分派給等待中的呼叫，直到 stdout 關閉。
func (c *mcpConn) readLoop(stdout io.ReadCloser) {
	defer c.readers.Done()
	defer close(c.done)
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		// 最後一行可能沒有換行符就 EOF 了，所以先處理內容再看 err。
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			c.failReading(err)
			return
		}
	}
}

// dispatch 分派一則從 server 讀進來的訊息。
//
// 分類的依據是 method 而不是 id（見 rpcMessage 的說明）：stdio 上的 JSON-RPC 是雙向的，
// server 也會發請求，而它的 id 與我們的 id 在不同的命名空間裡、可以撞號。
func (c *mcpConn) dispatch(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		// 壞掉的一行不該拖垮整條連線：有些 server 會往 stdout 印非協議的雜訊。
		c.logger.Warn("mcp_message_unparsable", "server", c.name, "error", err.Error())
		return
	}

	if msg.Method != "" {
		if !hasRPCID(msg.ID) {
			// server 發的通知。核心階段不處理任何通知，包括
			// notifications/tools/list_changed（刻意不做，見 spec #3 Further Notes）。
			return
		}
		c.handleServerRequest(msg)
		return
	}

	id, ours := ourRequestID(msg.ID)
	if !ours {
		// 不是我們送出過的 id 形狀（缺 id、null、或字串）。一則沒有 method 也對不上任何
		// 請求的訊息不合規，記下來比靜默丟掉好——真實世界對不上的時候要查得到。
		c.logger.Warn("mcp_response_id_unrecognized", "server", c.name, "id", string(msg.ID))
		return
	}

	c.mu.Lock()
	reply, waiting := c.pending[id]
	if waiting {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !waiting {
		// 逾時之後才到的回應。丟掉——留著會讓下一次呼叫讀到上一次的答案，那比逾時
		// 更難查。
		return
	}
	reply <- msg
}

// handleServerRequest 回應 server 主動發來的請求。
//
// **ping 必須回**：規範要求收到 ping 的一方及時回一個空的 result，發送方據此判斷連線
// 還活著。不回的後果是對方認為連線已死並斷開——而我們會看到一個沒頭沒尾的 EOF。
//
// 回應的 id **原樣帶回** msg.ID（可能是字串也可能是整數，見 rpcMessage）：換算過的 id
// 對 server 來說是另一個請求的 id，等於沒回。
//
// 寫入用 context.Background() 是因為這裡**沒有呼叫端的 ctx**——這則回覆是 server 要的，
// 不屬於任何一次 Tool 呼叫。實務上它只有幾十位元組（遠小於 pipe 緩衝），只有在 server
// 同時停止讀 stdin 又寫滿我們的 stdout 時才可能卡住，而那時連線已經壞了。那種情形下
// readLoop 停擺不會拖住任何 Tool 呼叫（它們各自的 ctx 現在真的生效了），只會讓 close()
// 等到子進程退出為止——與 close() 沒有整體期限是**同一個**已知缺口，歸 ticket #22。
//
// 回應直接在 readLoop 這條 goroutine 上寫出去。訊息只有幾十位元組、遠小於 pipe 緩衝，
// 正常情況不會阻塞；改成每個請求開一條 goroutine 反而多出洩漏面與亂序。真正會卡住的
// 情形是 server 同時停止讀 stdin 又寫滿我們的 stdout——那是壞掉的 server，逾時保護
// 歸 ticket #22。
func (c *mcpConn) handleServerRequest(msg rpcMessage) {
	switch msg.Method {
	case "ping":
		if err := c.write(context.Background(), rpcReply{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}, "ping 回覆"); err != nil {
			c.logger.Warn("mcp_ping_reply_failed", "server", c.name, "error", err.Error())
		}
	default:
		// 交握時我們一個 client 能力都沒宣告（sampling、roots、elicitation 都沒有），
		// 照規範不該收到別的請求。回 method not found 而不是沉默：沉默會讓 server 一直
		// 等一個永遠不來的回應。
		err := c.write(context.Background(), rpcReply{
			JSONRPC: "2.0", ID: msg.ID,
			Error: &rpcError{Code: -32601, Message: "OryxOS 沒有宣告這個 client 能力：" + msg.Method},
		}, "不支援的請求的回覆")
		if err != nil {
			c.logger.Warn("mcp_request_reject_failed", "server", c.name, "method", msg.Method, "error", err.Error())
		}
		c.logger.Warn("mcp_unsupported_server_request", "server", c.name, "method", msg.Method)
	}
}

// forwardStderr 把 server 的 stderr 逐行轉成結構化日誌。
func (c *mcpConn) forwardStderr(stderr io.ReadCloser) {
	defer c.readers.Done()
	reader := bufio.NewReader(stderr)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			c.logger.Info("mcp_server_stderr", "server", c.name, "line", trimmed)
		}
		if err != nil {
			return
		}
	}
}

// fail 記下連線失效的原因（只記第一個，那才是根因）。讀取端與寫入端共用。
func (c *mcpConn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure == nil {
		c.failure = err
	}
}

// failReading 把讀取端的錯誤轉成連線失效的原因。
func (c *mcpConn) failReading(err error) {
	if errors.Is(err, io.EOF) {
		c.fail(fmt.Errorf("MCP server %q 的連線已結束（子進程可能已退出）", c.name))
		return
	}
	c.fail(fmt.Errorf("讀取 MCP server %q 的輸出: %w", c.name, err))
}

// failureErr 取連線失效的原因；readLoop 剛結束而還沒記上時給一個不會誤導的預設。
func (c *mcpConn) failureErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	return fmt.Errorf("MCP server %q 的連線已關閉", c.name)
}

// close 關掉這條連線的子進程。
//
// 照 MCP stdio 規範先關 stdin：那是通知 server「沒有更多請求了」的標準方式，讓它
// 自己收乾淨再退出，比直接發訊號溫和。
//
// **關閉的整體期限與逾期強制終止歸 ticket #22**（進程生命週期那張票）。這裡只做把
// 子進程收掉這件事——不收會留下孤兒進程，那是本票自己造出來的問題。
func (c *mcpConn) close() error {
	var errs []error
	if err := c.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		errs = append(errs, fmt.Errorf("關閉 MCP server %q 的 stdin: %w", c.name, err))
	}
	// 先等兩條讀取 goroutine 收完，才能呼叫 Wait（見 readers 欄位的說明）。stdin 一關，
	// 照規範實作的 server 就會退出、它的 stdout 與 stderr 隨之關閉，兩條讀取因此收到
	// EOF 結束。**不理會關閉訊號的 server 會讓這裡等下去——那個期限歸 ticket #22**，
	// 與下面的 Wait 是同一個洞，不是這一行新增的。
	c.readers.Wait()
	if err := c.cmd.Wait(); err != nil {
		// 非零離開碼不算我們的錯誤：server 收到 stdin 關閉後怎麼結束是它的事，
		// 為此讓 oryxos chat 的退出碼變紅只會製造噪音。留在日誌裡可查即可。
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			c.logger.Info("mcp_server_exited", "server", c.name, "status", exitErr.String())
		} else {
			errs = append(errs, fmt.Errorf("等待 MCP server %q 退出: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// ── McpToolAdapter ──────────────────────────────────────────────────────

// McpToolAdapter 把一個 MCP 工具適配成 OryxTool：對外是普通的 Tool，對內經 MCP
// 協議轉發給它所屬的 server。
type McpToolAdapter struct {
	conn *mcpConn
	// server 是宣告名，用來組註冊名（<server>__<tool>）。
	server string
	decl   mcpToolDecl
}

// newMcpToolAdapter 包裝一個從 tools/list 拿到的工具宣告。
func newMcpToolAdapter(conn *mcpConn, server string, decl mcpToolDecl) *McpToolAdapter {
	return &McpToolAdapter{conn: conn, server: server, decl: decl}
}

func (a *McpToolAdapter) Name() string { return McpToolName(a.server, a.decl.Name) }

// Description 原樣沿用 server 給的描述，不加工。
//
// 那段文字是 server 作者寫給模型看的，改寫或加料只會讓「在別的 MCP host 上能用的
// server 到 OryxOS 表現不一樣」。來源已經寫在名字的前綴裡了。
func (a *McpToolAdapter) Description() string { return a.decl.Description }

// InputSchema 回傳 server 宣告的 JSON Schema。
//
// server 沒宣告或宣告成空的時候補一個空物件 schema：Provider 那端需要一個合法的
// schema 才能把工具附進請求，缺了會讓整輪 LLM 呼叫失敗——一個工具的宣告不完整不該
// 讓整個 turn 起不來。
func (a *McpToolAdapter) InputSchema() json.RawMessage {
	if len(bytes.TrimSpace(a.decl.InputSchema)) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return a.decl.InputSchema
}

// Execute 經 MCP 協議轉發這次呼叫，結果包成 core.ToolResult。
//
// 逾時與取消走呼叫端的 ctx（憲法 5.3）。一切失敗都以 ToolResult.Error 回填、**不中斷
// turn**——沿 spec #1 既有的 Tool 失敗語義，讓 LLM 據此換一條路。
//
// 全部標 Retryable: false：server 掛掉重試幾次都一樣（核心階段不自動重連），參數錯
// 更是重試無用。
func (a *McpToolAdapter) Execute(ctx context.Context, input string) core.ToolResult {
	args := json.RawMessage(strings.TrimSpace(input))
	// LLM 對無參數的工具常常送空字串。協議要求 arguments 是物件，補一個空的。
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return core.ToolResult{Error: fmt.Sprintf("%s 的參數不是合法的 JSON：%s", a.Name(), input)}
	}

	result, raw, err := a.conn.callTool(ctx, a.decl.Name, args)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("呼叫 %s 失敗: %v", a.Name(), err)}
	}
	text := result.text(raw)
	if result.IsError {
		// 工具自己說失敗（參數不對、外部 API 回錯）：原文回填，那是給 LLM 換路用的
		// 資訊，不該被我們改寫。
		return core.ToolResult{Error: text}
	}
	return core.ToolResult{OK: true, Content: text}
}

// ── McpClientService ────────────────────────────────────────────────────

// McpClientService 持有這次啟動所連上的每條 MCP 連線，負責在退出時把子進程收掉。
type McpClientService struct {
	conns  []*mcpConn
	logger *slog.Logger
}

// ConnectMcpServers 依 specs 逐一連線、取回工具清單，並把每個工具包成 OryxTool
// 註冊進**同一個** ToolRegistry。
//
// specs 已經是「這個 Profile 引用到的那幾個」（見 core.ResolveMcpServers）：沒被引用的
// 宣告連子進程都不會 spawn。
//
// **連線失敗目前直接上拋**（該 server 之前已連上的那些會先收乾淨）。spec #3 定的最終
// 語義是降級：該 server 的工具不註冊、記錯誤日誌加 CLI 可見警示、其餘 server 與內建
// Tool 照常啟動——那整套失敗語義歸 **ticket #22**。本票先把失敗做成大聲的錯誤而不是
// 安靜的降級，兩者之間絕不選第三條「靜默少幾個工具」。
func ConnectMcpServers(ctx context.Context, registry *Registry, specs []core.McpServerSpec,
	logger *slog.Logger) (*McpClientService, error) {
	svc := &McpClientService{logger: logger}
	for _, spec := range specs {
		conn, err := dialMcpStdio(ctx, spec, logger)
		if err != nil {
			return svc, errors.Join(err, svc.Close())
		}
		// 先收下連線再取工具：後面任何一步失敗，Close 都要能把這個子進程收掉。
		svc.conns = append(svc.conns, conn)

		decls, err := conn.listTools(ctx)
		if err != nil {
			return svc, errors.Join(err, svc.Close())
		}
		for _, decl := range decls {
			adapter := newMcpToolAdapter(conn, spec.Name, decl)
			// 名字送不進 Function Calling 的話，啟動就擋——否則每一輪 LLM 請求都會被
			// 端點以 400 打回，連沒用到這個工具的純對話也一起死。
			if err := validateToolFunctionName(spec.Name, decl.Name, adapter.Name()); err != nil {
				return svc, errors.Join(err, svc.Close())
			}
			if err := registry.Register(adapter); err != nil {
				return svc, errors.Join(
					fmt.Errorf("註冊 MCP server %q 的工具 %q: %w", spec.Name, decl.Name, err),
					svc.Close())
			}
		}
		// 成功側也要有訊號：使用者要能判斷 Agent 現在的能力範圍，不能只在失敗時才有
		// 訊息。取回 0 個工具是合法的（那個 server 就是沒有工具），但看得到才知道。
		logger.InfoContext(ctx, "mcp_server_connected",
			"server", spec.Name, "transport", mcpTransportStdio,
			"protocol", conn.protocolVersion,
			"server_name", conn.serverName, "server_version", conn.serverVersion,
			"tools", len(decls))
	}
	return svc, nil
}

// Close 關掉全部 MCP 子進程。組裝點必須在退出前呼叫，否則留下孤兒進程（同
// AuditLog.Close／DB.Close 的既有形狀）。
//
// 每條連線都試著關，不因為其中一條失敗就跳過其餘的——漏掉的那些會變成孤兒。
//
// 對 nil 接收者是 no-op：組裝點的 defer 在 ConnectMcpServers 回錯誤的路徑上也會跑，
// 那時它拿到的可能只是個半成品，不該因此 panic 蓋掉真正的錯誤。
func (s *McpClientService) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, conn := range s.conns {
		if err := conn.close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.conns = nil
	return errors.Join(errs...)
}
