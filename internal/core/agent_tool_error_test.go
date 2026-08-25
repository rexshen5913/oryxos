// Tool 錯誤分類與指引的整合測試（ticket #51）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、檔案、
// ToolRegistry、SQLite 在 seam 之下**全部用真的**（憲法 4.3）。
//
// 斷言落在兩個外部產物上，而且刻意是**同一次執行的兩邊**：
//
//   - **送往 Provider 的 tool 訊息**：原始錯誤 ＋ 該類型的指引
//   - **tool_invocations 落庫的 error**：只有原始錯誤，**不含**指引
//
// 兩邊分岔正是本票的核心決策（issue #38 第二項）：審計要的是「Tool 實際回報了什麼」，
// 附加的指引是我們對 LLM 說的話，不屬於那個事實。兩邊寫在同一支測試裡是刻意的——
// 分成兩支的話，日後有人把指引也寫進審計，兩支各自都還能通過。
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

// sentToolMessage 取出一次 LLM 邊界**請求**裡最後一條 tool 訊息的內容——那就是「回填
// 給 LLM 的東西」這個外部產物。
//
// 與既有的 lastToolMessage（讀 Session 對話歷史）看的是同一段字，但取用點不同：本票的
// 斷言對象是**送給 Provider 的請求內容**（issue #38 Testing Decisions），所以從請求
// body 取，不從 Session 取。
func sentToolMessage(t *testing.T, body []byte) string {
	t.Helper()
	var last string
	var found bool
	for _, m := range parseLLMRequest(t, body).Messages {
		if m.Role == "tool" {
			last, found = m.Content, true
		}
	}
	if !found {
		t.Fatalf("這次 LLM 請求裡沒有任何 tool 訊息: %s", body)
	}
	return last
}

// replyLimitation 是本檔兩份 final 錄製回應都必須說出口的那句限制陳述。
//
// 兩支測試的 Agent 都**只註冊 read_file**，所以「列出／確認目錄底下有什麼」這件事它做
// 不到，回覆必須把這一點講明，而不是承諾它。
const replyLimitation = "只有讀檔的能力"

// assertReplyAdmitsItCannotList 斷言 Agent 給使用者的最終回覆**明說了自己做不到**。
//
// **第一版的斷言守不住它宣稱的行為，這裡記下為什麼換掉。** 那一版只檢查回覆不含字面
// `list_dir`，但最初那句有問題的 fixture 是「要不要我先列出 notes 底下有哪些檔案？」
// ——它承諾了一個做不到的能力，卻一個 `list_dir` 都沒有，會被直接放行。**守不住所宣稱
// 行為的斷言比沒有更糟**：它讓後來的人以為那條規則有人看著。
//
// 改成**正面**斷言那句限制陳述（做得到／做不到是回覆自己要交代的事），字面 Tool 名的
// 檢查降為附帶的第二道。
//
// 斷言對象是**錄製回應本身**，所以這是一道 fixture 品質的守衛，不是系統行為的斷言——
// 目的是讓日後有人把回應改回「我來列一下目錄」時立刻轉紅。它與
// core.ToolErrorKind.Guidance 註解裡的第 1 條硬規則是同一件事的另一層：那裡管我們寫給
// LLM 的指引，這裡管 LLM 說給使用者聽的話。
func assertReplyAdmitsItCannotList(t *testing.T, reply string) {
	t.Helper()
	if !strings.Contains(reply, replyLimitation) {
		t.Errorf("最終回覆沒有交代自己做不到（期望含 %q）——這個 Agent 只註冊了 %s，"+
			"回覆不得承諾它沒有的能力: %q", replyLimitation, tool.ReadFileToolName, reply)
	}
	if strings.Contains(reply, tool.ListDirToolName) {
		t.Errorf("最終回覆點名了 %s，但它不在這個 Agent 的工具清單裡: %q",
			tool.ListDirToolName, reply)
	}
}

// TestToolErrorGuidanceReachesLLMButNotAudit 是本票主場景：read_file 讀一個**白名單
// 內但不存在**的檔案（not_found），下一個 iteration 送出的 tool 訊息帶著該類型的指引，
// 而同一次呼叫落庫的 error 不帶。
//
// 選 not_found 而不是 sandbox 是刻意的：sandbox 這一類沒有通用指引（見
// ToolErrorSandbox），拿它當主場景會驗不到「指引真的被附加上去」這件事。
func TestToolErrorGuidanceReachesLLMButNotAudit(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_read_file_tool_call.json"),
		readFixture(t, "reply_read_file_missing_final.json"),
	)
	// Workspace 是真的、白名單也開了 notes——**只是那個檔案不存在**。失敗因此落在
	// not_found 而不是 sandbox，這是這支測試的前提。
	root, dir := newTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ReadFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes/todo.md 裡寫了什麼？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// **這個 Agent 只有 read_file。** 最終回覆不得承諾它做不到的事——建議一個不在
	// 工具清單裡的 Tool 會讓下一個 turn 撞上「不在可用子集」然後白燒一次 iteration，
	// 正是 issue #36 那個病。這一格同時守著 fixture：日後有人把回應改成「我來列一下
	// 目錄」，這裡會轉紅。
	assertReplyAdmitsItCannotList(t, resp)

	guidance := core.ToolErrorNotFound.Guidance()
	if guidance == "" {
		t.Fatal("not_found 的指引是空字串——這支測試的前提不成立")
	}

	// 一、送往 Provider 的 tool 訊息：原始錯誤與指引**都要在**。
	//
	// 只斷言指引在、不斷言原始錯誤在的話，一個「把錯誤換成指引」的實作也會全綠——
	// 而那正好弄丟了 LLM 最需要的那半（是哪個檔案、發生了什麼）。
	backfilled := sentToolMessage(t, reqs[1])
	if !strings.Contains(backfilled, "這個檔案在 Workspace 內不存在") {
		t.Errorf("回填的 tool 訊息遺失原始錯誤: %q", backfilled)
	}
	at := strings.Index(backfilled, guidance)
	if at < 0 {
		t.Fatalf("回填的 tool 訊息未附 not_found 指引:\n訊息: %q\n指引: %q", backfilled, guidance)
	}
	// 順序是**固定**的：原始錯誤在前、指引在後（issue #38 第二項）。反過來的話 LLM
	// 先讀到一段通則、才讀到「是哪個檔案」，而後者才是它決定下一步的依據。
	if errAt := strings.Index(backfilled, "這個檔案在 Workspace 內不存在"); errAt > at {
		t.Errorf("指引排在原始錯誤之前——組裝順序應為「原始錯誤 → 類型指引」: %q", backfilled)
	}

	// 二、審計：同一次呼叫，error 記原始錯誤但**不含**指引。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	audited := invocations[0]
	if !audited.errText.Valid {
		t.Fatalf("失敗的呼叫沒有落 error: %+v", audited)
	}
	if !strings.Contains(audited.errText.String, "這個檔案在 Workspace 內不存在") {
		t.Errorf("審計的 error 遺失原始錯誤: %q", audited.errText.String)
	}
	if strings.Contains(audited.errText.String, guidance) {
		t.Errorf("審計的 error 混進了給 LLM 的指引——審計記的應該只有 Tool 實際回報的事實: %q",
			audited.errText.String)
	}
}

// TestShellWhitelistSpecificMessageKept 釘住 issue #36 那則專屬訊息**原樣存活**：
// 不被通用指引覆蓋，也不被重複附加。
//
// 這是 sandbox 這一類沒有通用指引的直接後果，也是它的驗收方式。#36 量到的是同一個
// 模型從 10 次 iteration 降到 1 次，靠的就是訊息末尾那句「不要逐一嘗試、直接告訴
// 使用者」；在它後面再接一段語義重疊的通用話，只會把那句話稀釋掉。
func TestShellWhitelistSpecificMessageKept(t *testing.T) {
	// #36 那句話的尾巴。斷言到措辭在這裡是**刻意**的：本票要保護的就是這段措辭本身，
	// 它不是一句可有可無的引言。
	const tail = "由他決定要不要加進白名單"

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_shell_denied_tool_call.json"),
		readFixture(t, "reply_shell_denied_final.json"),
	)
	root, dir := newTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	db := openStore(t, filepath.Join(t.TempDir(), "oryxos.db"))
	agent := newShellAgent(t, srv.URL, []string{"echo"}, root, dir, db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "把 notes 資料夾砍掉"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	backfilled := sentToolMessage(t, reqs[1])
	// 不被覆蓋：那句話還在。
	if n := strings.Count(backfilled, tail); n != 1 {
		t.Errorf("#36 的專屬訊息在回填內容裡出現 %d 次, 期望剛好 1 次: %q", n, backfilled)
	}
	// 不被（通用指引）重複附加：那句話是**最後一段**，後面沒有再接東西。
	//
	// 用 HasSuffix 而不是「不含某段通用話」：後者只擋得住今天寫的那段，前者擋得住
	// 任何日後被接上去的東西。
	if !strings.HasSuffix(backfilled, tail) {
		t.Errorf("#36 的專屬訊息後面被接上了別的東西——sandbox 不該有通用指引: %q", backfilled)
	}
}

// TestUnclassifiedToolErrorBackfillUnchanged 是遷移安全性的驗收：**未分類（零值）的
// 回填內容與本票之前逐位元組相同**。
//
// 用的失敗是「read_file 的目標是目錄」——它不屬於本票遷移的三類，所以在本票之後仍然
// 是零值，正好當這條的樣本。斷言寫成**完整字串相等**而不是 Contains：多接一個換行、
// 多附一段話都要立刻轉紅，那才是「逐位元組相同」。
func TestUnclassifiedToolErrorBackfillUnchanged(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_read_file_dir_tool_call.json"),
		readFixture(t, "reply_read_file_dir_final.json"),
	)
	root, dir := newTestWorkspace(t)
	seedFileWorkspace(t, dir) // notes/ 因此是一個真的目錄
	db := openStore(t, filepath.Join(t.TempDir(), "oryxos.db"))
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ReadFileToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes 裡寫了什麼？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// 同上：這個 Agent 也只有 read_file。
	//
	// **注意這裡有一層刻意的不對稱**：read_file 自己的錯誤訊息會說「要看它底下有什麼
	// 請改用 list_dir」（下面 want 那串就含著它），而那則訊息**不在本票範圍內**——它是
	// 既有的 production 措辭，且 internal/tool/file_test.go 有一格明確斷言它。本斷言
	// 管的是**Agent 對使用者的回覆**，不是 Tool 回填的錯誤原文。
	assertReplyAdmitsItCannotList(t, resp)

	want := "Tool 執行失敗: " + "read_file 的目標 notes 是目錄，不是普通檔；要看它底下有什麼請改用 list_dir"
	if got := sentToolMessage(t, reqs[1]); got != want {
		t.Errorf("未分類失敗的回填內容變了——遷移不再是行為零變更。"+
			"若你剛把「目標是目錄」這條錯誤分類了，請改挑另一個仍未分類的失敗當樣本，"+
			"**不要**放寬這條斷言——它守的是「零值不附加任何東西」\n實際: %q\n期望: %q", got, want)
	}
}
