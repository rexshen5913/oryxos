// Shell Tool 鏈路的整合測試（ticket #33）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——而 seam 之下
// **命令是真的跑的**（憲法 4.3，不 mock exec）、Workspace 是真的、SQLite 是真的。
//
// 四個場景各驗一件事：命令真的執行得到；白名單真的擋得住；**錯誤訊息教得動 LLM**
// （管線寫法那條）；非零 exit code 不算 Tool 失敗。
package core_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// newShellAgent 組出一個列了 shell 的 Agent：命令白名單由呼叫端給，PATH 用真實的
// （這些測試真的跑 echo、ls），工作目錄就是那個 Workspace。
func newShellAgent(t *testing.T, baseURL string, allowedCommands []string,
	root *os.Root, dir string, db *testStore) *core.AgentService {
	t.Helper()
	return newToolAgentWithShell(t, baseURL, testProfile(), []string{tool.ShellToolName},
		tool.SandboxConfig{AllowedCommands: allowedCommands},
		testShellRuntime(t, dir), root, discardLogger(), db)
}

// TestProcessShellScenario 是主場景：Profile 列了 shell、config.yaml 開了命令白名單，
// LLM 自己決定呼叫它，stdout ＋ exit code 回填、第二輪據結果回答，而且這次呼叫落
// tool_invocations。
//
// 同時釘住**參數摘要去敏**：fixture 刻意讓 LLM 把一個帶 api_key 的 URL 當參數傳進去，
// 斷言落庫的 parameters 是遮蔽過的。
//
// **這裡的斷言範圍要說清楚**：去敏套用在**參數**上（`react.go` 的 `RedactArgs`），
// 不套用在 Tool 的**結果**上——`echo` 把那串 URL 原樣印回來，所以 result 欄位裡有它。
// 那是既有語義（`http_get` 的回應內文同樣不遮蔽），不是這條鏈路的破口：結果是 Tool
// 從外部拿回來的東西，遮蔽它等於讓 LLM 看不到自己要的答案。
func TestProcessShellScenario(t *testing.T) {
	const secret = "SUPER-SECRET-VALUE"
	srv := newReplayServer(t,
		readFixture(t, "reply_shell_tool_call.json"),
		readFixture(t, "reply_shell_final.json"),
	)
	root, dir := newTestWorkspace(t)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newShellAgent(t, srv.URL, []string{"echo"}, root, dir, db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "幫我 echo 一下那個端點")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "exit code 是 0") {
		t.Errorf("回應 = %q, 期望第二輪據命令結果作答", resp)
	}

	// 回填鏈路：user → assistant(tool_calls) → tool → assistant。tool 訊息裡的 stdout
	// 要是**命令真的印出來的那一份**，斷言到確切內容——回填一句固定字串也能通過長度檢查。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 ||
		msgs[1].ToolCalls[0].Name != tool.ShellToolName {
		t.Fatalf("messages[1] 應為帶 shell tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_shell_1" {
		t.Fatalf("messages[2] 應為回應 call_shell_1 的 tool 訊息: %+v", msgs[2])
	}
	if got := toolContentField(t, msgs[2].Content, "stdout"); !strings.Contains(got, "fetching https://") {
		t.Errorf("回填的 stdout = %q, 期望是 echo 真的印出來的那一行", got)
	}
	if got := toolContentNumber(t, msgs[2].Content, "exit_code"); got != 0 {
		t.Errorf("回填的 exit_code = %d, 期望 0", got)
	}

	// 落 tool_invocations——欄位斷言對齊 agent_audit_test.go 那一組。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	inv := invocations[0]
	if inv.sessionID != session.ID || inv.profileName != "default" || inv.toolName != tool.ShellToolName {
		t.Errorf("session_id／profile_name／tool_name = %s／%s／%s, 期望 %s／default／shell",
			inv.sessionID, inv.profileName, inv.toolName, session.ID)
	}
	if inv.status != "completed" {
		t.Errorf("status = %q, 期望 completed", inv.status)
	}
	if !strings.Contains(inv.parameters, "echo") {
		t.Errorf("parameters 未落庫呼叫參數: %q", inv.parameters)
	}
	// 去敏：URL 的 query 整段被遮蔽，密鑰不原樣進審計表。
	if strings.Contains(inv.parameters, secret) {
		t.Errorf("parameters 原樣落了密鑰: %q", inv.parameters)
	}
	if !strings.Contains(inv.parameters, "REDACTED") {
		t.Errorf("parameters 未經去敏: %q", inv.parameters)
	}
	assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)
}

// TestProcessShellSandboxRejectionRecovers 是**負向 fixture**：LLM 給出非白名單命令
// → SandboxViolation 回填 → 第二輪告知使用者要往哪段設定加。
//
// 三件事一起釘住：
//
//  1. 拒絕的那一次**沒有重跑**（tool_invocations 恰好一列）——Sandbox 拒絕不標
//     Retryable，這一格是把它釘在**命令**路徑上。
//  2. 那個命令**真的沒有被執行**：fixture 要 `rm -rf notes`，斷言 notes 目錄還在。
//     只斷言「回了錯誤」的話，一個先執行再回錯誤的實作也會全綠。
//  3. 錯誤訊息**可行動**：指得出是哪個命令名、要往 config.yaml 的哪一段加。
func TestProcessShellSandboxRejectionRecovers(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_shell_denied_tool_call.json"),
		readFixture(t, "reply_shell_denied_final.json"),
	)
	root, dir := newTestWorkspace(t)
	notes := filepath.Join(dir, "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newShellAgent(t, srv.URL, []string{"echo"}, root, dir, db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "把 notes 資料夾砍掉")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "shell.allowed_commands") {
		t.Errorf("回應 = %q, 期望第二輪告訴使用者要改哪段設定", resp)
	}

	// rm 真的沒跑。
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("notes 目錄不見了——被拒的命令實際上被執行了: %v", err)
	}

	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1（拒絕一次，不重試）: %+v", len(invocations), invocations)
	}
	denied := invocations[0]
	if denied.status != "failed" {
		t.Errorf("被拒的呼叫 status = %q, 期望 failed", denied.status)
	}
	if !denied.errText.Valid || !strings.Contains(denied.errText.String, "SandboxViolation") {
		t.Errorf("被拒的呼叫 error 未落庫 SandboxViolation: %+v", denied.errText)
	}
	for _, want := range []string{"rm", "shell.allowed_commands"} {
		if !strings.Contains(denied.errText.String, want) {
			t.Errorf("錯誤訊息 %q 未含 %q（要說得出是哪個命令、要往哪加）", denied.errText.String, want)
		}
	}
}

// TestProcessShellPipelineSyntaxRecovers 是**最可能真的發生**的那條失敗路徑：LLM 的
// 訓練分佈裡 shell tool 預設吃 shell 語法，所以它很可能生出 `{command: "ls | wc -l"}`。
//
// 這一格驗的是「**錯誤訊息教得動 LLM**」——第一輪整串被拒（那不是任何一個程式名），
// 第二輪改用單一命令並成功。這與 InputSchema 的描述是同一件事的兩面：描述負責事前教，
// 錯誤訊息負責事後教，兩邊都要，因為描述擋不住全部。
func TestProcessShellPipelineSyntaxRecovers(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_shell_pipeline_tool_call.json"),
		readFixture(t, "reply_shell_after_pipeline.json"),
		readFixture(t, "reply_shell_pipeline_final.json"),
	)
	root, dir := newTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "todo.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("建立 todo.md: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newShellAgent(t, srv.URL, []string{"ls"}, root, dir, db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes 底下有幾個檔案？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "todo.md") {
		t.Errorf("回應 = %q, 期望第二輪改用單一命令後真的拿到結果", resp)
	}

	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 2 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 2（管線寫法被拒一次、單一命令成功一次）: %+v",
			len(invocations), invocations)
	}
	rejected, recovered := invocations[0], invocations[1]
	if rejected.status != "failed" {
		t.Errorf("管線寫法的 status = %q, 期望 failed", rejected.status)
	}
	// 錯誤訊息要說得出「command 是一個程式名」這件事，否則 LLM 只能瞎猜。
	if !rejected.errText.Valid || !strings.Contains(rejected.errText.String, "程式名") {
		t.Errorf("錯誤訊息 %q 沒說 command 只能放一個程式名", rejected.errText.String)
	}
	if recovered.status != "completed" {
		t.Errorf("改用單一命令後 status = %q, 期望 completed", recovered.status)
	}
	if !recovered.result.Valid || !strings.Contains(recovered.result.String, "todo.md") {
		t.Errorf("成功那次的 result 未落庫命令輸出: %+v", recovered.result)
	}
}

// TestProcessShellNonZeroExitCodeGuidesNextStep 驗證**非零 exit code 不算 Tool 失敗**
// （與 HTTP Tool 對非 2xx 的既有語義同構）：exit code ＋ stderr 照樣回填，由 LLM 決定
// 下一步。
//
// 決定性的斷言是審計表的 status——它必須是 **completed**。報成 failed 會讓 ReAct 循環
// 把「這個目錄不存在」當成 Tool 壞掉，而那正是 Agent 最需要知道的那個事實。
func TestProcessShellNonZeroExitCodeGuidesNextStep(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_shell_exit_code_tool_call.json"),
		readFixture(t, "reply_shell_exit_code_final.json"),
	)
	root, dir := newTestWorkspace(t)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newShellAgent(t, srv.URL, []string{"ls"}, root, dir, db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "列一下 definitely-not-here")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "不存在") {
		t.Errorf("回應 = %q, 期望第二輪據 stderr 判斷下一步", resp)
	}

	toolMsg := session.Messages[2]
	if got := toolContentNumber(t, toolMsg.Content, "exit_code"); got == 0 {
		t.Errorf("回填的 exit_code = 0, 期望非零: %q", toolMsg.Content)
	}
	if got := toolContentField(t, toolMsg.Content, "stderr"); got == "" {
		t.Errorf("回填的 stderr 是空的, 期望帶 ls 的錯誤訊息: %q", toolMsg.Content)
	}

	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	if invocations[0].status != "completed" {
		t.Errorf("status = %q, 期望 completed——非零 exit code 不算 Tool 失敗", invocations[0].status)
	}
}

// TestProcessShellVisibleOnlyWhenListed 是**兩層可見性**的矩陣，兩層各有斷言：
//
//   - Profile 的 tools **沒列** shell 時，它完全不出現在送往 LLM 的工具宣告裡——
//     沒開的能力連被嘗試的機會都沒有。
//   - 列了則出現（真的被呼叫得到由上面的主場景證明）。
//
// **只驗正向那一層不算通過**：shell 是內建 Tool，RegisterBuiltins 一律註冊它，所以
// 「在場但沒被列到就不該出現」才是過濾真的有作用的證據。完全不過濾的實作在只有正向
// 斷言時照樣全綠。
func TestProcessShellVisibleOnlyWhenListed(t *testing.T) {
	tests := []struct {
		name        string
		tools       []string
		wantVisible bool
	}{
		{name: "Profile 列了 shell 就出現在工具宣告裡", tools: []string{tool.ShellToolName}, wantVisible: true},
		{name: "Profile 沒列 shell 就完全不出現", tools: []string{"http_get"}},
		{name: "Profile 一個 Tool 都沒列時同樣不出現", tools: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
			root, dir := newTestWorkspace(t)
			agent := newToolAgentWithShell(t, srv.URL, testProfile(), tt.tools,
				tool.SandboxConfig{AllowedCommands: []string{"echo"}},
				testShellRuntime(t, dir), root, discardLogger(), newStore(t))
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			declared := declaredToolNames(t, llmReqs, 0)
			if got := slices.Contains(declared, tool.ShellToolName); got != tt.wantVisible {
				t.Errorf("送往 LLM 的工具清單 %v 含 shell = %v, 期望 %v", declared, got, tt.wantVisible)
			}
			if !slices.Equal(declared, tt.tools) {
				t.Errorf("送往 LLM 的工具清單 = %v, 期望與 Profile 的 tools %v 完全一致", declared, tt.tools)
			}
		})
	}
}
