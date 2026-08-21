// write_file 最小鏈路的整合測試（ticket #31）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、檔案、
// ToolRegistry、SQLite 在 seam 之下**全部用真的**（憲法 4.3）。
//
// 與 read_file 那組的差別在斷言落點：讀是「內容有沒有回填進對話」，寫是「**磁碟上
// 真的多了那個檔案**」。回應文字漂亮但檔案沒寫出來，是這條鏈路唯一該怕的失敗形態，
// 所以每一格都真的把檔案讀回來比對。
package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// summaryContent 是 fixture 裡 LLM 決定寫入的內容，測試據它斷言磁碟上那一份。
const summaryContent = "# 本週摘要\n\n- write_file 接上 ReAct 循環\n"

// TestProcessWriteFileScenario 是本票的主場景：Profile 列了 write_file，使用者說
// 「把結果存成一個檔」，LLM 自己決定呼叫它，寫入成功回填、第二輪確認——而且
// **Process 返回之後檔案還在磁碟上**（那正是這條 Tool 存在的理由）。
func TestProcessWriteFileScenario(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_write_file_tool_call.json"),
		readFixture(t, "reply_write_file_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedFileWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.WriteFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "幫我把這週的摘要存成 notes/summary.md")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "notes/summary.md") {
		t.Errorf("回應 = %q, 期望第二輪確認寫到哪個檔", resp)
	}

	// **檔案真的在磁碟上，內容逐字相符。** 這一格才是這條 Tool 的存在理由；上面的
	// 回應文字是 fixture 給的，它本身證明不了任何事。
	written, err := os.ReadFile(filepath.Join(dir, "notes", "summary.md"))
	if err != nil {
		t.Fatalf("Process 返回後讀不到寫出的檔案: %v", err)
	}
	if string(written) != summaryContent {
		t.Errorf("磁碟上的內容 = %q, 期望 %q", written, summaryContent)
	}

	// 回填鏈路：user → assistant(tool_calls) → tool → assistant，且 tool 訊息帶著
	// **實際寫入的位元組數**（AC：回填內容含寫入的位元組數）。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 ||
		msgs[1].ToolCalls[0].Name != tool.WriteFileToolName {
		t.Errorf("messages[1] 應為帶 write_file tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_write_file_1" {
		t.Errorf("messages[2] 應為回應 call_write_file_1 的 tool 訊息: %+v", msgs[2])
	}
	if got := toolContentNumber(t, msgs[2].Content, "bytes_written"); got != len(summaryContent) {
		t.Errorf("回填的 bytes_written = %d, 期望 %d（磁碟上真的寫了那麼多）", got, len(summaryContent))
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
	if inv.profileName != "default" || inv.toolName != tool.WriteFileToolName {
		t.Errorf("profile_name／tool_name = %s／%s, 期望 default／write_file", inv.profileName, inv.toolName)
	}
	if !strings.Contains(inv.parameters, "notes/summary.md") {
		t.Errorf("parameters 未落庫呼叫參數: %q", inv.parameters)
	}
	if inv.status != "completed" {
		t.Errorf("status = %q, 期望 completed", inv.status)
	}
	if !inv.result.Valid || !strings.Contains(inv.result.String, "bytes_written") {
		t.Errorf("result 未落庫 Tool 結果: %+v", inv.result)
	}
	assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)
}

// TestProcessWriteFileSandboxRejectionRecovers 是**負向 fixture**：LLM 給出越界路徑 →
// SandboxViolation 錯誤回填 → 第二輪 LLM **換一條路**（改寫到白名單內）並告訴使用者
// 要改哪段設定。
//
// 三件事一起釘住：
//
//  1. 拒絕的那一次**沒有重跑**——tool_invocations 恰好兩列（拒絕一次、換路成功一次）。
//     這一格就是「Sandbox 拒絕不標 Retryable」在**寫入**路徑上的斷言。
//  2. 白名單外的路徑上**什麼都沒被建出來**。這是寫入路徑特有的：讀路徑被拒最多是
//     沒讀到，寫路徑被拒若還是落了檔，Sandbox 就等於沒有。
//  3. 錯誤訊息**可行動**：指出要去改 config.yaml 的哪一段。
func TestProcessWriteFileSandboxRejectionRecovers(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_write_file_denied_tool_call.json"),
		readFixture(t, "reply_write_file_after_denied.json"),
		readFixture(t, "reply_write_file_recovered_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedFileWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.WriteFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "把這段內容存到 secrets/leak.txt")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "file.allowed_paths") {
		t.Errorf("回應 = %q, 期望第二輪告訴使用者要改哪段設定", resp)
	}

	// 白名單外的檔案一個都不該出現。
	if _, err := os.Stat(filepath.Join(dir, "secrets", "leak.txt")); !os.IsNotExist(err) {
		t.Errorf("白名單外的路徑被寫出了檔案（stat err = %v）", err)
	}
	// 換的那條路真的寫成功了——否則「換一條路」只是回應文字上的說法。
	written, err := os.ReadFile(filepath.Join(dir, "notes", "summary.md"))
	if err != nil {
		t.Fatalf("換路之後的檔案沒寫出來: %v", err)
	}
	if string(written) != summaryContent {
		t.Errorf("換路後磁碟上的內容 = %q, 期望 %q", written, summaryContent)
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
}
