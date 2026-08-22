// list_dir 多迭代鏈路的整合測試（ticket #32）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、目錄樹、
// ToolRegistry、SQLite 在 seam 之下**全部用真的**（憲法 4.3）。
//
// 本票與另外兩張 FileTool 的差別在這裡：驗的不是「列得出一份清單」，而是
// **「列完之後據清單決定下一步」**——一個 turn 內兩次 tool call、兩次迭代。單獨列
// 一份清單沒什麼用，那條鏈路才是這個 Tool 存在的理由。
package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// seedListDirWorkspace 在 seedFileWorkspace 的基礎上多建一個 notes/drafts/ 子目錄，
// 讓回填的清單裡**同時有目錄與檔案**——「哪一個能拿去 read_file」正是 is_dir 這個
// 欄位存在的理由，清單裡全是檔案的話那個欄位驗不到東西。
func seedListDirWorkspace(t *testing.T, dir string) {
	t.Helper()
	seedFileWorkspace(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "notes", "drafts"), 0o755); err != nil {
		t.Fatalf("建立 notes/drafts/: %v", err)
	}
}

// listDirEntries 從回填給 LLM 的 list_dir 結果裡取出條目陣列。
func listDirEntries(t *testing.T, content string) []struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
} {
	t.Helper()
	var out struct {
		Entries []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("list_dir 結果不是合法 JSON（%q）: %v", content, err)
	}
	return out.Entries
}

// TestProcessListDirThenReadFileScenario 是本票的主場景，也是它與另外兩張 FileTool
// 票唯一的差別：**一個 turn 內兩次迭代**——先 list_dir，再據回填的清單挑一個檔去
// read_file，最後據檔案內容作答。
//
// 「據清單」這三個字是被斷言的，不是被相信的：第二次呼叫的路徑必須落在第一次回填的
// 條目名稱裡。少了那一條，兩個各自獨立的單次呼叫也會讓這個測試全綠——而那正是這張
// 票明文說「不是」的東西。
func TestProcessListDirThenReadFileScenario(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_list_dir_tool_call.json"),
		readFixture(t, "reply_list_dir_then_read_file.json"),
		readFixture(t, "reply_list_dir_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedListDirWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(),
		[]string{tool.ListDirToolName, tool.ReadFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes 這個資料夾裡有什麼？幫我看一下其中的檔案")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "把 read_file 接進 ReAct 循環") {
		t.Errorf("回應 = %q, 期望最後據檔案內容作答", resp)
	}

	// 回填鏈路：user → assistant(list_dir) → tool → assistant(read_file) → tool →
	// assistant。**六則訊息**就是「兩次迭代」在對話歷史上的形狀；兩個獨立的單次呼叫
	// 各自只會留下四則。
	msgs := session.Messages
	if len(msgs) != 6 {
		t.Fatalf("歷史長度 = %d, 期望 6（兩次 tool call、兩次迭代）: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 ||
		msgs[1].ToolCalls[0].Name != tool.ListDirToolName {
		t.Fatalf("messages[1] 應為帶 list_dir tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_list_dir_1" {
		t.Fatalf("messages[2] 應為回應 call_list_dir_1 的 tool 訊息: %+v", msgs[2])
	}

	// 回填的項目含**名稱、是否為目錄、大小**，而且是磁碟上那棵樹的真實形狀。
	entries := listDirEntries(t, msgs[2].Content)
	if len(entries) != 2 {
		t.Fatalf("回填條目數 = %d, 期望 2（drafts/ 與 todo.md）: %+v", len(entries), entries)
	}
	if entries[0].Name != "drafts" || !entries[0].IsDir {
		t.Errorf("條目[0] = %+v, 期望 drafts 且 is_dir 為 true", entries[0])
	}
	if entries[1].Name != "todo.md" || entries[1].IsDir {
		t.Errorf("條目[1] = %+v, 期望 todo.md 且 is_dir 為 false", entries[1])
	}
	if want := int64(len(noteContent)); entries[1].Size != want {
		t.Errorf("todo.md 的 size = %d, 期望磁碟上的真實大小 %d", entries[1].Size, want)
	}

	// **第二次呼叫是「據清單」來的**：它讀的那個檔名必須出現在上一步的條目裡。
	if msgs[3].Role != core.RoleAssistant || len(msgs[3].ToolCalls) != 1 ||
		msgs[3].ToolCalls[0].Name != tool.ReadFileToolName {
		t.Fatalf("messages[3] 應為帶 read_file tool_calls 的 assistant 訊息: %+v", msgs[3])
	}
	readPath := toolCallArgField(t, msgs[3].ToolCalls[0].Arguments, "path")
	var listed bool
	for _, e := range entries {
		if !e.IsDir && filepath.Join("notes", e.Name) == filepath.Clean(readPath) {
			listed = true
		}
	}
	if !listed {
		t.Errorf("第二輪讀的 %q 不在第一輪回填的清單 %+v 裡——那不是「據清單決定下一步」", readPath, entries)
	}
	if got := toolContentField(t, msgs[4].Content, "content"); got != noteContent {
		t.Errorf("回填的檔案內容 = %q, 期望磁碟上那一份 %q", got, noteContent)
	}

	// **兩次呼叫都落 tool_invocations**——順序也要對得上那條鏈路。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 2 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 2（list_dir 一次、read_file 一次）: %+v",
			len(invocations), invocations)
	}
	listInv, readInv := invocations[0], invocations[1]
	if listInv.toolName != tool.ListDirToolName || readInv.toolName != tool.ReadFileToolName {
		t.Fatalf("落庫的 tool_name = %s／%s, 期望 list_dir／read_file", listInv.toolName, readInv.toolName)
	}
	for _, inv := range invocations {
		if inv.sessionID != session.ID || inv.profileName != "default" {
			t.Errorf("%s 的 session_id／profile_name = %s／%s, 期望 %s／default",
				inv.toolName, inv.sessionID, inv.profileName, session.ID)
		}
		if inv.status != "completed" {
			t.Errorf("%s 的 status = %q, 期望 completed", inv.toolName, inv.status)
		}
		assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)
	}
	if !strings.Contains(listInv.parameters, "notes") {
		t.Errorf("list_dir 的 parameters 未落庫呼叫參數: %q", listInv.parameters)
	}
	if !listInv.result.Valid || !strings.Contains(listInv.result.String, "todo.md") {
		t.Errorf("list_dir 的 result 未落庫目錄清單: %+v", listInv.result)
	}
	if !strings.Contains(readInv.parameters, "notes/todo.md") {
		t.Errorf("read_file 的 parameters 未落庫呼叫參數: %q", readInv.parameters)
	}
}

// TestProcessListDirSandboxRejectionRecovers 是**負向 fixture**：LLM 給出越界目錄 →
// SandboxViolation 錯誤回填 → 第二輪 LLM 據錯誤訊息**換一條路**（改列白名單內的目錄）
// 並告知使用者要改哪段設定。
//
// 三件事一起釘住，形狀與 read_file 那條同構：
//
//  1. 拒絕的那一次**沒有重跑**——ReAct 循環只對標了 Retryable 的失敗退避重試，而
//     Sandbox 拒絕重跑幾次結果都一樣。所以 tool_invocations 恰好兩列，不是「拒絕 ×4
//     ＋ 成功一次」。這一格就是「不標 Retryable」釘在**列目錄**路徑上的斷言。
//  2. 白名單外的**檔名**從頭到尾沒出現過。list_dir 洩漏的不是檔案內容而是檔名，
//     也就是「哪裡有東西可讀」的地圖。
//  3. 錯誤訊息**可行動**：它要指出去改 config.yaml 的哪一段。
func TestProcessListDirSandboxRejectionRecovers(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_list_dir_denied_tool_call.json"),
		readFixture(t, "reply_list_dir_after_denied.json"),
		readFixture(t, "reply_list_dir_recovered_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedListDirWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ListDirToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "secrets 資料夾裡有什麼？")
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

	// 白名單外的檔名一個字都不該外流——回應、對話歷史、審計表三處都查。
	const deniedName = "api-key.txt"
	if strings.Contains(resp, deniedName) {
		t.Errorf("回應洩漏了白名單外的檔名: %q", resp)
	}
	for i, m := range session.Messages {
		if strings.Contains(m.Content, deniedName) {
			t.Errorf("messages[%d] 洩漏了白名單外的檔名: %q", i, m.Content)
		}
	}
	for i, inv := range invocations {
		if strings.Contains(inv.result.String, deniedName) || strings.Contains(inv.errText.String, deniedName) {
			t.Errorf("tool_invocations[%d] 洩漏了白名單外的檔名: %+v", i, inv)
		}
	}
}

// toolCallArgField 從 LLM 給的 tool call 參數 JSON 裡取一個字串欄位。
func toolCallArgField(t *testing.T, arguments, field string) string {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		t.Fatalf("tool call 參數不是合法 JSON（%q）: %v", arguments, err)
	}
	s, ok := args[field].(string)
	if !ok {
		t.Fatalf("tool call 參數缺字串欄位 %q: %v", field, args)
	}
	return s
}
