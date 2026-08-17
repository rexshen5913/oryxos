// MCP 最小鏈路在**組裝點**的測試（ticket #21）：這裡是唯一真的讀 .oryxos/mcp_servers.yaml
// 的地方，所以 AC 那句「在 mcp_servers.yaml 宣告一個 stdio server、Profile 引用它，經
// AgentService.Process 驅動後 LLM 真的呼叫到它的工具」只有在這一層才驗得完整。
//
// 協議層與兩層過濾的細節由 internal/core/agent_mcp_test.go 覆蓋，本檔不重複。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
)

// writeMcpServers 在 Workspace 寫一份宣告檔，內容原樣寫入（供不合法內容的案例使用）。
func writeMcpServers(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, workspaceDir, core.McpServersFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %s: %v", core.McpServersFile, err)
	}
}

// declareTestMcpServer 宣告一個以測試二進制當 server 的 stdio MCP server。
//
// 工具名刻意經 ${TEST_ORYX_MCP_TOOL} 佔位帶進去：這樣一來，若 env 的佔位沒有被展開，
// server 會宣告一個名字是佔位符字面值的工具，`demo__echo` 就不存在——env 的
// ${ENV_VAR} 機制因此是**端到端**驗到的，不是只驗到解析函式。
func declareTestMcpServer(t *testing.T, dir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取得測試二進制路徑: %v", err)
	}
	t.Setenv("TEST_ORYX_MCP_TOOL", "echo")
	writeMcpServers(t, dir, "mcp_servers:\n  "+name+":\n    transport: stdio\n"+
		"    command: ["+exe+"]\n"+
		"    env:\n"+
		"      "+mcpServerModeEnv+": \"1\"\n"+
		"      "+mcpServerNameEnv+": "+name+"\n"+
		"      "+mcpServerToolsEnv+": ${TEST_ORYX_MCP_TOOL}\n")
}

// TestChatMcpToolEndToEnd 是本票在組裝點的主場景：業務方只動兩份 YAML（宣告檔與
// Profile），Agent 就真的呼叫到外部工具——零 Go 程式碼。
func TestChatMcpToolEndToEnd(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_mcp_tool_call.json"),
		readFixture(t, "chat_reply_after_mcp_tool.json"))
	dir := setupChatWorkspace(t, srv.URL)
	declareTestMcpServer(t, dir, "demo")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - demo\ntools:\n  - demo__echo\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "幫我呼叫外部工具"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if !strings.Contains(out.String(), "外部 MCP 工具已回覆") {
		t.Errorf("輸出未含最終回應: %q", out.String())
	}

	// 工具以 <server>__<tool> 出現在 LLM 邊界的工具清單：宣告 → 連線 → tools/list →
	// 註冊 → 兩層過濾這一整段都通了才會有這個名字。
	if names := llmToolNames(t, reqs, 0); !slices.Contains(names, "demo__echo") {
		t.Errorf("LLM 邊界的工具清單沒有 demo__echo: %v", names)
	}
	// server 端把自己的名字寫進回應，所以這條同時證明轉發到了正確的 server。
	if msg := llmToolMessage(t, reqs, 1); !strings.Contains(msg, "demo/echo 收到：哈囉") {
		t.Errorf("tool 訊息未帶 MCP server 的結果: %q", msg)
	}

	// 連線**成功**側也要看得見：使用者要能判斷 Agent 現在的能力範圍，不能只在失敗時
	// 才有訊息。斷言的是事件鍵與工具數，不綁措辭。
	logs := readWorkspaceLog(t, dir)
	if !strings.Contains(logs, "mcp_server_connected") {
		t.Errorf("日誌沒有連線成功的記錄:\n%s", logs)
	}
	if !strings.Contains(logs, `"tools":1`) {
		t.Errorf("日誌沒有記下取回幾個工具:\n%s", logs)
	}
}

// testMcpServerEntry 組出一份指向測試二進制的 stdio server 宣告（YAML 片段）。
func testMcpServerEntry(t *testing.T, name string, tools ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取得測試二進制路徑: %v", err)
	}
	return "  " + name + ":\n    transport: stdio\n" +
		"    command: [" + exe + "]\n" +
		"    env:\n" +
		"      " + mcpServerModeEnv + ": \"1\"\n" +
		"      " + mcpServerNameEnv + ": " + name + "\n" +
		"      " + mcpServerToolsEnv + ": " + strings.Join(tools, ",") + "\n"
}

// TestChatMcpServerUnavailableDegradesWithWarning 是本票（#22）在組裝點的主場景：
// **一個 MCP server 連不上，Agent 照樣起得來、其餘工具照樣可用，但使用者看得見**。
//
// 這一格同時釘住三件事，少任何一件這個功能就沒有意義：
//
//  1. 啟動不中斷——一個外部依賴掛掉不該讓整個 Agent 起不來（使用者故事 28）。
//  2. CLI 有可見警示——「安靜地少了幾個工具」比起不來更糟：Agent 會表現成莫名其妙
//     不會做某件事，而使用者不會想到去翻日誌（使用者故事 29）。斷言用輸出內容，
//     prior art 是 TestChatEmptyWhitelistWarning。
//  3. 活著的 server 的工具真的還能用——降級不是「大家一起半殘」。
//
// **Profile 的 tools 列了掛掉那個 server 的工具**，這是刻意的：那才是真實的配置
// （使用者當然會列他要用的工具）。少了這一點，降級在現實中幾乎永遠走不到——
// Registry.Subset 會先以「Tool 未註冊」擋下啟動，警示根本印不出來。
func TestChatMcpServerUnavailableDegradesWithWarning(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_mcp_tool_call.json"),
		readFixture(t, "chat_reply_after_mcp_tool.json"))
	dir := setupChatWorkspace(t, srv.URL)

	// demo 是真的起得來的 server；broken 指向一個不存在的執行檔——那正是最常見的
	// 現實形態（機器上沒裝 node、路徑打錯、套件沒安裝好）。
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "demo", "echo")+
		"  broken:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-server]\n")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - demo\n  - broken\ntools:\n  - demo__echo\n  - broken__echo\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "幫我呼叫外部工具"}); err != nil {
		t.Fatalf("一個 server 連不上不該讓啟動失敗: %v", err)
	}

	// 1. CLI 可見警示，且指名是哪個 server——使用者要知道該去修哪一個。
	if !strings.Contains(out.String(), "broken") {
		t.Errorf("輸出沒有指名連不上的 server:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "警告") {
		t.Errorf("輸出沒有可見警示:\n%s", out.String())
	}
	// 2. 對話照常走完。
	if !strings.Contains(out.String(), "外部 MCP 工具已回覆") {
		t.Errorf("輸出未含最終回應:\n%s", out.String())
	}

	// 3. 活著那個 server 的工具照常可用，掛掉那個的不出現在 LLM 邊界——降級的範圍
	// 剛好是那一個 server，不多不少。
	names := llmToolNames(t, reqs, 0)
	if !slices.Contains(names, "demo__echo") {
		t.Errorf("連得上的 server 的工具不見了: %v", names)
	}
	if slices.Contains(names, "broken__echo") {
		t.Errorf("連不上的 server 的工具竟然還在工具清單裡: %v", names)
	}
	// 4. 結構化錯誤日誌（維運要查「Agent 的能力為什麼不完整」）。
	if logs := readWorkspaceLog(t, dir); !strings.Contains(logs, "mcp_server_unavailable") {
		t.Errorf("日誌沒有連線失敗的記錄:\n%s", logs)
	}
}

// TestChatEveryUnavailableMcpServerIsNamedSeparately 釘住**多個 server 同時連不上時，
// 每一個仍各自可見**（issue #26）。
//
// 這是並行連線最容易掉的一塊：一次拿到好幾個錯誤，很自然會想併成一句「有 2 個 MCP
// server 連線失敗」。但「命令找不到」與「交握逾時」要修的東西完全不同——合併之後
// 使用者手上只剩一個數字，一條線索都沒有。
//
// 上一條測試只有一個壞掉的 server，那種形狀下「各自可見」與「合併成一句」長得一模
// 一樣，抓不到這個回歸。
func TestChatEveryUnavailableMcpServerIsNamedSeparately(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "chat_reply_mcp_tool_call.json"),
		readFixture(t, "chat_reply_after_mcp_tool.json"))
	dir := setupChatWorkspace(t, srv.URL)

	// 兩個壞的指向**不同**的不存在路徑：那個路徑就是各自的原始錯誤裡唯一能分辨彼此
	// 的東西，下面據它斷言「原因沒有被摘要掉」。
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "demo", "echo")+
		"  broken_a:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-a]\n"+
		"  broken_b:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-b]\n")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - demo\n  - broken_a\n  - broken_b\n"+
		"tools:\n  - demo__echo\n  - broken_a__echo\n  - broken_b__echo\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "幫我呼叫外部工具"}); err != nil {
		t.Fatalf("兩個 server 連不上不該讓啟動失敗: %v", err)
	}

	got := out.String()
	if n := strings.Count(got, "警告：MCP server"); n != 2 {
		t.Errorf("警示出現 %d 次，期望 2 次（連不上的 server 各一次，不合併）:\n%s", n, got)
	}
	for name, path := range map[string]string{
		"broken_a": "/nonexistent/oryxos-mcp-a",
		"broken_b": "/nonexistent/oryxos-mcp-b",
	} {
		if !strings.Contains(got, name) {
			t.Errorf("輸出沒有指名 %s:\n%s", name, got)
		}
		if !strings.Contains(got, path) {
			t.Errorf("輸出沒有帶上 %s 的原始錯誤（%s）——原因被摘要掉了:\n%s", name, path, got)
		}
	}
	// 降級的範圍剛好是連不上的那兩個：活著的 server 照常。
	if names := llmToolNames(t, reqs, 0); !slices.Contains(names, "demo__echo") {
		t.Errorf("連得上的 server 的工具不見了: %v", names)
	}
}

// TestChatUnreferencedServerMissingCredentialDoesNotBlockStartup 釘住憑證展開的範圍：
// **沒被這個 Profile 引用的 server，缺環境變數也不該擋下啟動**。
//
// 這是兩層隔離的實質內容。一份 mcp_servers.yaml 常同時服務多個 Agent：只接 Slack 的
// Agent 不該因為機器上沒有 GitHub token 而起不來，完全不用 MCP 的 Profile 更不該。
// 若憑證在載入宣告檔時就全部展開（過濾之前），這三格會全紅。
func TestChatUnreferencedServerMissingCredentialDoesNotBlockStartup(t *testing.T) {
	tests := []struct {
		name string
		// profileMcp 是 Profile 的 mcp_servers 段（空字串代表整個欄位省略）。
		profileMcp string
		// profileTools 是 Profile 的 tools 段。
		profileTools string
	}{
		{name: "mcp_servers 欄位省略（既有 Profile 免遷移）"},
		{name: "mcp_servers 為空清單", profileMcp: "mcp_servers: []\n"},
		{
			name:         "只引用憑證齊全的那一個",
			profileMcp:   "mcp_servers:\n  - demo\n",
			profileTools: "tools:\n  - demo__echo\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			exe, err := os.Executable()
			if err != nil {
				t.Fatalf("取得測試二進制路徑: %v", err)
			}
			// demo 的憑證齊全；ghost 引用一個**沒有設定**的環境變數，且沒有被任何
			// Profile 引用。
			writeMcpServers(t, dir, "mcp_servers:\n"+
				"  demo:\n    transport: stdio\n    command: ["+exe+"]\n"+
				"    env:\n"+
				"      "+mcpServerModeEnv+": \"1\"\n"+
				"      "+mcpServerNameEnv+": demo\n"+
				"      "+mcpServerToolsEnv+": echo\n"+
				"  ghost:\n    transport: stdio\n    command: [/nonexistent/server]\n"+
				"    env:\n      GHOST_TOKEN: ${TEST_ORYX_NEVER_SET_TOKEN}\n")
			writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+tt.profileMcp+tt.profileTools)

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
				chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("未被引用的 server 缺憑證不該擋下啟動: %v", err)
			}
			if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
				t.Errorf("輸出未含回應內容: %q", out.String())
			}
		})
	}
}

// TestChatReferencedServerMissingCredentialFailsStartup 是上一條的反面：**被引用的**
// server 缺環境變數就是設定錯誤，啟動即報錯，且訊息要指名是哪個 server 的哪個變數。
//
// 兩條要一起看才完整：只有前一條的話，「一律不報錯」也會通過。
func TestChatReferencedServerMissingCredentialFailsStartup(t *testing.T) {
	srv := newReplayServer(t) // 不期望任何 LLM 呼叫——連一個 turn 都不該跑到
	dir := setupChatWorkspace(t, srv.URL)
	writeMcpServers(t, dir, "mcp_servers:\n  demo:\n    transport: stdio\n    command: [/usr/bin/true]\n"+
		"    env:\n      DEMO_TOKEN: ${TEST_ORYX_NEVER_SET_TOKEN}\n")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - demo\n")

	var out bytes.Buffer
	err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default"})
	if err == nil {
		t.Fatal("被引用的 server 缺憑證應在啟動時報錯")
	}
	for _, want := range []string{"mcp_servers.demo.env.DEMO_TOKEN", "TEST_ORYX_NEVER_SET_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("錯誤 %q 未含 %q", err.Error(), want)
		}
	}
}

// TestChatWithoutMcpServersFile 釘住既有 Workspace **免遷移**：spec #1／#2 建立的
// Workspace 沒有 mcp_servers.yaml（init 自本票起才建），照常啟動、照常對話。
func TestChatWithoutMcpServersFile(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	if err := os.Remove(filepath.Join(dir, workspaceDir, core.McpServersFile)); err != nil {
		t.Fatalf("移除 %s: %v", core.McpServersFile, err)
	}

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "你好"}); err != nil {
		t.Fatalf("沒有 %s 時應照常啟動: %v", core.McpServersFile, err)
	}
	if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
		t.Errorf("輸出未含回應內容: %q", out.String())
	}
}

// TestInitMcpServersTemplate 釘住 init 產出的宣告檔模板：既有註解引導使用者，又要能
// 被自己的載入器讀成「0 個 server」。
//
// 「模板能被載入器讀」這條不能省：一份縮排寫錯的模板會讓每個新 Workspace 一啟動就
// 報錯，而那是 init 自己造出來的。
func TestInitMcpServersTemplate(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	path := filepath.Join(dir, ".oryxos", core.McpServersFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 %s: %v", core.McpServersFile, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s 是空檔，沒有帶註解的模板", core.McpServersFile)
	}

	declared, err := config.LoadMcpServers(path)
	if err != nil {
		t.Fatalf("init 產出的模板讀不進來: %v", err)
	}
	if len(declared) != 0 {
		t.Errorf("模板宣告了 %d 個 server，期望 0（範例要留在註解裡）: %v", len(declared), declared)
	}

	// 模板叫使用者「拿掉每行開頭的 # 就能用」，那句話必須是真的。註解掉的那段範例是
	// 由 mcpServersExample 逐行加前綴產生的，所以直接驗那份原始 YAML 就等於驗註解裡
	// 的內容。
	//
	// 這條不是多餘的：`@modelcontextprotocol/...` 這種值不加引號的話整份檔案會以
	// 「found character that cannot start any token」解析失敗（`@` 是 YAML 保留指示
	// 字元），而註解掉的範例永遠不會被解析，錯了也沒人知道——照著模板做的人才會踩到。
	example := filepath.Join(t.TempDir(), core.McpServersFile)
	if err := os.WriteFile(example, []byte(mcpServersExample), 0o644); err != nil {
		t.Fatalf("寫入範例: %v", err)
	}
	exampleDeclared, err := config.LoadMcpServers(example)
	if err != nil {
		t.Fatalf("模板註解裡的範例是壞的 YAML——照著它做的人會啟動失敗: %v", err)
	}
	spec, ok := exampleDeclared["github"]
	if !ok {
		t.Fatalf("範例沒有宣告出 github: %v", exampleDeclared)
	}
	if spec.Transport != "stdio" || len(spec.Command) == 0 {
		t.Errorf("範例宣告不完整: %+v", spec)
	}
	// 範例示範的 ${ENV_VAR} 佔位要真的能展開（展開是過濾後的第二步，見
	// config.ExpandMcpServerEnv）——否則模板教的寫法是死的。
	t.Setenv("GITHUB_TOKEN", "ghp-test")
	expanded, err := config.ExpandMcpServerEnv([]core.McpServerSpec{spec})
	if err != nil {
		t.Fatalf("範例的憑證展開失敗: %v", err)
	}
	if expanded[0].Env["GITHUB_TOKEN"] != "ghp-test" {
		t.Errorf("範例的 ${ENV_VAR} 佔位展開後 = %q, 期望 ghp-test", expanded[0].Env["GITHUB_TOKEN"])
	}
}

// llmToolNames 取出第 n 次 LLM 邊界請求附上的工具名清單。
func llmToolNames(t *testing.T, reqs [][]byte, n int) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	decodeLLMRequest(t, reqs, n, &req)
	var names []string
	for _, decl := range req.Tools {
		names = append(names, decl.Function.Name)
	}
	return names
}

// llmToolMessage 取出第 n 次 LLM 邊界請求裡最後一則 tool 訊息的內容。
func llmToolMessage(t *testing.T, reqs [][]byte, n int) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	decodeLLMRequest(t, reqs, n, &req)
	var last string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			last = m.Content
		}
	}
	return last
}

// decodeLLMRequest 解析第 n 次 LLM 請求。先檢查數量再索引：請求數不如預期時，
// reqs[n] 會以 index out of range panic 蓋掉真正的失敗原因。
func decodeLLMRequest(t *testing.T, reqs [][]byte, n int, into any) {
	t.Helper()
	if len(reqs) <= n {
		t.Fatalf("只收到 %d 次 LLM 請求，取不到第 %d 次", len(reqs), n+1)
	}
	if err := json.Unmarshal(reqs[n], into); err != nil {
		t.Fatalf("解析第 %d 次 LLM 請求: %v", n+1, err)
	}
}

// readWorkspaceLog 讀 Workspace 的結構化日誌檔。
func readWorkspaceLog(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, workspaceDir, "logs", "oryxos.log"))
	if err != nil {
		t.Fatalf("讀取日誌檔: %v", err)
	}
	return string(data)
}
