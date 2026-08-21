// read_file 最小鏈路的整合測試（ticket #30）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、檔案、
// ToolRegistry、SQLite 在 seam 之下**全部用真的**（憲法 4.3）。
//
// 斷言落在外部可觀察的產物上：回應內容、回填進對話的 tool 訊息、tool_invocations
// 落庫的資料列，以及**送往 LLM 的工具宣告清單**。不斷言內部呼叫序列。
package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// noteContent 是 Workspace 內那份被允許讀取的檔案內容；fixture 的第二輪回應就是
// 據它作答的，所以斷言到確切內容——「有一列 tool 訊息」不足以證明檔案真的被讀過。
const noteContent = "把 read_file 接進 ReAct 循環。\n"

// secretContent 是白名單**之外**那份檔案的內容。它存在的唯一目的是被斷言「從頭到尾
// 沒有出現在任何地方」：拒絕不只要回錯誤，還要真的沒把內容讀出來。
const secretContent = "SECRET-API-KEY-DO-NOT-READ"

// newTestWorkspace 在 t.TempDir() 開一個 Workspace 根，回傳 root 與它的絕對路徑。
func newTestWorkspace(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return root, dir
}

// seedFileWorkspace 在 Workspace 內建出兩份真實檔案：白名單內的 notes/todo.md，
// 與白名單外的 secrets/api-key.txt。
func seedFileWorkspace(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range map[string]string{
		filepath.Join("notes", "todo.md"):       noteContent,
		filepath.Join("secrets", "api-key.txt"): secretContent,
	} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("建立 %s 的父目錄: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("寫入 %s: %v", rel, err)
		}
	}
}

// TestProcessReadFileScenario 是本票的主場景：config.yaml 開了 file.allowed_paths、
// Profile 列了 read_file，LLM 自己決定呼叫它，檔案內容回填進對話、第二輪據內容回答，
// 而且這次呼叫落 tool_invocations 審計表（與既有內建 Tool 一視同仁）。
func TestProcessReadFileScenario(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_read_file_tool_call.json"),
		readFixture(t, "reply_read_file_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedFileWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ReadFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes/todo.md 裡寫了什麼？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "把 read_file 接進 ReAct 循環") {
		t.Errorf("回應 = %q, 期望第二輪據檔案內容作答", resp)
	}

	// 回填鏈路：user → assistant(tool_calls) → tool → assistant。tool 訊息的內容要是
	// **真的從磁碟讀出來的那一份**，斷言到確切內容——回填一句固定字串也能通過長度檢查。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 ||
		msgs[1].ToolCalls[0].Name != tool.ReadFileToolName {
		t.Errorf("messages[1] 應為帶 read_file tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_read_file_1" {
		t.Errorf("messages[2] 應為回應 call_read_file_1 的 tool 訊息: %+v", msgs[2])
	}
	if got := toolContentField(t, msgs[2].Content, "content"); got != noteContent {
		t.Errorf("回填的檔案內容 = %q, 期望磁碟上那一份 %q", got, noteContent)
	}

	// 落 tool_invocations——欄位斷言對齊 agent_audit_test.go 對 http_get 的那一組。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	inv := invocations[0]
	if inv.sessionID != session.ID {
		t.Errorf("session_id = %q, 期望 %q", inv.sessionID, session.ID)
	}
	if inv.profileName != "default" || inv.toolName != tool.ReadFileToolName {
		t.Errorf("profile_name／tool_name = %s／%s, 期望 default／read_file", inv.profileName, inv.toolName)
	}
	if !strings.Contains(inv.parameters, "notes/todo.md") {
		t.Errorf("parameters 未落庫呼叫參數: %q", inv.parameters)
	}
	if inv.status != "completed" {
		t.Errorf("status = %q, 期望 completed", inv.status)
	}
	if !inv.result.Valid || !strings.Contains(inv.result.String, "read_file 接進 ReAct 循環") {
		t.Errorf("result 未落庫 Tool 結果: %+v", inv.result)
	}
	assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)
}

// TestProcessReadFileSandboxRejectionRecovers 是**負向 fixture**：LLM 給出越界路徑 →
// SandboxViolation 錯誤回填 → 第二輪 LLM 據錯誤訊息**換一條路**（改讀白名單內的檔案）
// 並告知使用者要改哪段設定。
//
// 三件事一起釘住：
//
//  1. 拒絕的那一次**沒有重跑**。ReAct 循環只對標了 Retryable 的失敗退避重試，而
//     Sandbox 拒絕重跑幾次結果都一樣——所以 tool_invocations 恰好兩列（拒絕一次、
//     成功一次），不是「拒絕 ×4 ＋ 成功一次」。這一格就是「不標 Retryable」在新
//     回填路徑上的斷言。
//  2. 被拒檔案的內容**從頭到尾沒出現過**：不在回應裡、不在對話歷史裡、也不在審計表裡。
//  3. 錯誤訊息**可行動**：它要指出去改 config.yaml 的哪一段，否則 LLM 只能瞎猜。
func TestProcessReadFileSandboxRejectionRecovers(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_read_file_denied_tool_call.json"),
		readFixture(t, "reply_read_file_after_denied.json"),
		readFixture(t, "reply_read_file_recovered_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedFileWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ReadFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "幫我看一下 API key 是什麼？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "file.allowed_paths") {
		t.Errorf("回應 = %q, 期望第二輪告訴使用者要改哪段設定", resp)
	}

	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 2 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 2（拒絕一次、換路成功一次，中間不重試）: %+v",
			len(invocations), invocations)
	}
	denied, recovered := invocations[0], invocations[1]
	if denied.status != "failed" {
		t.Errorf("被拒的呼叫 status = %q, 期望 failed", denied.status)
	}
	if !denied.errText.Valid || !strings.Contains(denied.errText.String, "SandboxViolation") {
		t.Errorf("被拒的呼叫 error 未落庫 SandboxViolation: %+v", denied.errText)
	}
	if !strings.Contains(denied.errText.String, "file.allowed_paths") {
		t.Errorf("被拒的錯誤訊息沒告訴使用者要改哪段設定: %q", denied.errText.String)
	}
	if recovered.status != "completed" {
		t.Errorf("換路後的呼叫 status = %q, 期望 completed", recovered.status)
	}

	// 被拒的檔案內容一個字都不該外流。
	if strings.Contains(resp, secretContent) {
		t.Errorf("回應洩漏了白名單外的檔案內容: %q", resp)
	}
	for i, m := range session.Messages {
		if strings.Contains(m.Content, secretContent) {
			t.Errorf("messages[%d] 洩漏了白名單外的檔案內容: %q", i, m.Content)
		}
	}
	for i, inv := range invocations {
		if strings.Contains(inv.result.String, secretContent) || strings.Contains(inv.errText.String, secretContent) {
			t.Errorf("tool_invocations[%d] 洩漏了白名單外的檔案內容: %+v", i, inv)
		}
	}
}

// TestProcessReadFileVisibleOnlyWhenListed 是**兩層可見性**的矩陣，兩層各有斷言：
//
//   - Profile 的 tools **沒列** read_file 時，它完全不出現在送往 LLM 的工具宣告裡
//     ——沒開的能力連被嘗試的機會都沒有。
//   - 列了則出現（真的被呼叫得到由上面的主場景證明）。
//
// **只驗正向那一層不算通過**：read_file 是內建 Tool，RegisterBuiltins 一律註冊它，
// 所以「在場但沒被列到就不該出現」才是過濾真的有作用的證據。完全不過濾的實作在
// 只有正向斷言時照樣全綠。
func TestProcessReadFileVisibleOnlyWhenListed(t *testing.T) {
	tests := []struct {
		name        string
		tools       []string
		wantVisible bool
	}{
		{name: "Profile 列了 read_file 就出現在工具宣告裡", tools: []string{tool.ReadFileToolName}, wantVisible: true},
		{name: "Profile 沒列 read_file 就完全不出現", tools: []string{"http_get"}},
		{name: "Profile 一個 Tool 都沒列時同樣不出現", tools: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
			root, dir := newTestWorkspace(t)
			seedFileWorkspace(t, dir)
			agent := newToolAgentIn(t, srv.URL, testProfile(), tt.tools,
				tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), newStore(t))
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			declared := declaredToolNames(t, llmReqs, 0)
			if got := slices.Contains(declared, tool.ReadFileToolName); got != tt.wantVisible {
				t.Errorf("送往 LLM 的工具清單 %v 含 read_file = %v, 期望 %v", declared, got, tt.wantVisible)
			}
			if !slices.Equal(declared, tt.tools) {
				t.Errorf("送往 LLM 的工具清單 = %v, 期望與 Profile 的 tools %v 完全一致", declared, tt.tools)
			}
		})
	}
}

// toolContentField 從回填給 LLM 的 JSON 結果裡取一個欄位，讓斷言落在**內容**上而不是
// 整串 JSON 的字面形狀（欄位順序或多一個 omitempty 欄位不該讓測試變紅）。
func toolContentField(t *testing.T, content, field string) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("tool 結果不是合法 JSON（%q）: %v", content, err)
	}
	s, ok := out[field].(string)
	if !ok {
		t.Fatalf("tool 結果缺字串欄位 %q: %v", field, out)
	}
	return s
}
