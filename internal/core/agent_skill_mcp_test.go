// Skill ＋ MCP 跨鏈路編排的整合測試（ticket #25）——Demo 三 的招牌能力：
//
//	LLM 讀到 Skill 描述 → 取回正文 → 依正文自己決定先調哪個 MCP 工具
//	→ 結果回填 → 再調**另一個 server** 的工具 → 完成跨系統任務。
//
// 在此之前 Skill 鏈路（#19→#20）與 MCP 鏈路（#21→#22）是兩條平行線，各自只驗自己
// 那一側；本檔是它們的交會點，也是「零程式碼跨工具編排真的成立」在自動化層面唯一
// 的斷言處。**沒有新的產品程式碼**——這裡串的全是那兩張票已經交付的東西。
//
// 沿用既有兩個 seam：從 AgentService.Process 驅動、LLM 以 httptest 回放（ADR-0002）。
// 其餘全部用真的：兩個獨立的 stdio MCP server 子進程走真實 JSON-RPC、SKILL.md 是
// t.TempDir() 下的真實檔案、審計落真實 SQLite（憲法 4.3）。
//
// # 回放能證明什麼、不能證明什麼
//
// LLM 的每一步都是錄好的，所以「模型自己想到要串這兩個工具」不是這裡能證的事——那
// 歸 #24 的人工驗收。這裡證的是**管線**：Skill 正文真的以 tool 訊息進了對話、第一個
// server 真的算出東西、那個結果真的被送進第二個 server 並被它處理掉、三次呼叫都留下
// 可還原順序的審計。管線不通的話，再聰明的模型也串不起來。
package core_test

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// 跨鏈路情境共用的字串。Skill 正文帶一個哨兵，用來驗它**始終不在** system prompt 裡。
const (
	crossSkillName = "daily-pr-digest"
	crossSkillDesc = "把昨天的 PR 整理成摘要發到 Slack。需要做每日 PR 摘要通知時使用。"
	crossSkillBody = "## 步驟\n\nSKILL_BODY_SENTINEL\n\n" +
		"1. 用 gh__list_prs 拉昨天合併的 PR\n" +
		"2. 用 slack__post_message 把結果發到頻道"

	// ghToolResult 是 gh server 對 fixture 帶的參數會回的內容（測試 server 把自己的
	// 名字寫進回應，見 mcp_server_test.go）。
	ghToolResult = "gh/list_prs 收到：昨天合併的 PR"
	// slackToolResult 是 slack server 收到「gh 的輸出」當參數之後會回的內容。
	//
	// **它證明的是轉發，不是模型的推理。** 第二次呼叫的參數是寫死在 fixture 裡的
	// （回放的本質，見檔頭）；這個字串成立代表那份參數**一字不差地**送到了一個
	// 自稱 slack 的獨立子進程，而且它的回應原樣回填了。模型是不是真的看了 gh 的
	// 結果才寫出那份參數，回放證不了——那歸 #24。
	slackToolResult = "slack/post_message 收到：" + ghToolResult
)

// crossChainProfile 是跨鏈路情境的 Profile：引用一份 Skill、兩個 MCP server，
// tools 各挑一個工具。load_skill 由 Subset 的 autoIncluded 帶進來（與組裝點同一條
// 推導），所以不寫在 tools 裡。
func crossChainProfile() *core.Profile {
	prof := profileWithSkills(crossSkillName)
	prof.McpServers = []string{"gh", "slack"}
	prof.Tools = []string{"gh__list_prs", "slack__post_message"}
	return prof
}

// crossChainSpecs 回傳兩個**各自獨立**的 stdio server 宣告，每個都把收到的 tools/call
// 記進自己的檔案。slackEnv 是要加在 slack 那一份上的額外環境變數（失敗那條測試用它
// 讓 slack 的工具自己說失敗）；其餘一律相同，兩條測試才是同一個情境的兩種收尾。
//
// 呼叫記錄是「兩次呼叫確實打到兩個不同的 server」唯一從 server 那一側看得到的證據：
// client 端只看得到兩個名字不同的工具，那與「同一個 server 暴露兩個工具」在外部完全
// 分不出來。
func crossChainSpecs(t *testing.T, slackEnv map[string]string) (specs []core.McpServerSpec, ghLog, slackLog string) {
	t.Helper()
	dir := t.TempDir()
	ghLog = filepath.Join(dir, "gh-calls.log")
	slackLog = filepath.Join(dir, "slack-calls.log")
	slack := map[string]string{mcpServerCallLogEnv: slackLog}
	maps.Copy(slack, slackEnv)
	return []core.McpServerSpec{
		mcpSpecWithEnv(t, "gh", map[string]string{mcpServerCallLogEnv: ghLog}, "list_prs"),
		mcpSpecWithEnv(t, "slack", slack, "post_message"),
	}, ghLog, slackLog
}

// TestSkillDrivenCrossServerOrchestration 是本票主場景，一個 turn 走完四次 LLM 迭代：
// 讀描述 → load_skill 取正文 → 調 gh → 調 slack（參數帶著 gh 的結果）→ 最終回應。
func TestSkillDrivenCrossServerOrchestration(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_load_skill.json"),
		readFixture(t, "reply_skill_then_mcp_tool_call.json"),
		readFixture(t, "reply_chained_mcp_tool_call.json"),
		readFixture(t, "reply_after_mcp_tool.json"))

	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, crossSkillName, crossSkillDesc, crossSkillBody)
	specs, ghLog, slackLog := crossChainSpecs(t, nil)

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	st := openStore(t, dbPath)
	agent := newLoadSkillAgentOn(t, srv.URL, root, crossChainProfile(), st, specs...)

	reply, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"),
		"幫我把昨天的 PR 摘要發到 Slack")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(reply, "外部工具已經回覆") {
		t.Errorf("最終回應不對: %q", reply)
	}
	if len(reqs) != 4 {
		t.Fatalf("LLM 迭代數 = %d, 期望 4（讀描述／讀正文／讀 gh 結果／讀 slack 結果）", len(reqs))
	}

	// 三次 Tool 呼叫的結果依序回填進對話。每一條都取「該次請求裡最後一條 tool 訊息」，
	// 也就是上一輪剛執行完那個 Tool 的結果。
	if got := toolMessageOf(t, reqs, 1); !strings.Contains(got, "SKILL_BODY_SENTINEL") {
		t.Errorf("第 2 次迭代的 tool 訊息不是 Skill 正文: %q", got)
	}
	if got := toolMessageOf(t, reqs, 2); !strings.Contains(got, ghToolResult) {
		t.Errorf("第 3 次迭代的 tool 訊息不是 gh 的結果: %q", got)
	}
	// 這一條同時證明三件事：slack 真的被呼叫到、它是**另一個** server（回應帶的是它
	// 自己的名字，不是 gh）、而且送進它的參數確實是那份帶著 gh 輸出的文字——參數只要
	// 在轉發途中被改掉或送錯 server，這個字串就湊不出來。
	if got := toolMessageOf(t, reqs, 3); !strings.Contains(got, slackToolResult) {
		t.Errorf("第 4 次迭代的 tool 訊息不是 slack 對那份參數的加工: %q", got)
	}

	// 兩個 server 各自只收到一次呼叫——「兩個獨立子進程」在 server 那一側的證據。
	if n := countCallLog(t, ghLog); n != 1 {
		t.Errorf("gh server 收到 %d 次 tools/call, 期望 1", n)
	}
	if n := countCallLog(t, slackLog); n != 1 {
		t.Errorf("slack server 收到 %d 次 tools/call, 期望 1", n)
	}

	// 三次呼叫都落審計，且順序可還原（started_at 是固定寬度的奈秒時間戳，字串排序
	// 即時間排序）。MCP 那兩筆帶各自的 <server>__<tool> 前綴，查得出來是哪個 server
	// 做的（憲法 6.2）。
	st.flush(t)
	rows := queryToolInvocations(t, dbPath)
	var names, statuses []string
	for _, row := range rows {
		names = append(names, row.toolName)
		statuses = append(statuses, row.status)
	}
	wantNames := []string{"load_skill", "gh__list_prs", "slack__post_message"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tool_invocations 的執行順序 = %v, 期望 %v", names, wantNames)
	}
	for i, status := range statuses {
		if status != core.AuditStatusCompleted {
			t.Errorf("%s 的審計狀態 = %q, 期望 %q", names[i], status, core.AuditStatusCompleted)
		}
	}
	// 呼叫參數要**完整**落審計（`RedactArgs` 不截斷，只遮敏感值）：slack 那筆看得到
	// 前一步的輸出，事後查帳的人不必重跑就能把這兩步接起來讀。
	if !strings.Contains(rows[2].parameters, ghToolResult) {
		t.Errorf("slack 那筆的 parameters 沒帶上 gh 的結果: %q", rows[2].parameters)
	}

	// 漸進揭露在跨鏈路下**仍然成立**：四次迭代的 system prompt 都只有描述、沒有正文。
	// 少了這條，這條鏈路就退化成「正文常駐」，省 token 的前提整個沒了。
	for i, raw := range reqs {
		got := systemPrompt(t, raw)
		if !strings.Contains(got, crossSkillDesc) {
			t.Errorf("第 %d 次迭代的 system prompt 遺失 Skill 描述: %q", i+1, got)
		}
		if strings.Contains(got, "SKILL_BODY_SENTINEL") {
			t.Errorf("第 %d 次迭代的 system prompt 含 Skill 正文——正文只該走 tool 訊息", i+1)
		}
	}
}

// TestCrossChainSurvivesSecondServerToolError 驗跨鏈路下的失敗語義與 #22 一致：
// 編排走到一半、第二個 server 的工具自己說失敗，錯誤回填給 LLM，**turn 不中斷**，
// LLM 換一條路收尾。
//
// 這一格分開驗是因為失敗發生的位置很要緊：前面已經做過兩次成功的 Tool 呼叫（含一個
// 外部 server），rollback 語義與「第一步就失敗」完全不同。
func TestCrossChainSurvivesSecondServerToolError(t *testing.T) {
	const toolErr = "slack 頻道不存在或權杖已過期"

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_load_skill.json"),
		readFixture(t, "reply_skill_then_mcp_tool_call.json"),
		readFixture(t, "reply_chained_mcp_tool_call.json"),
		readFixture(t, "reply_after_mcp_tool_error.json"))

	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, crossSkillName, crossSkillDesc, crossSkillBody)
	// 與成功那條完全同一個情境，只有第二個 server 壞：工具跑了但自己說失敗
	// （isError），協議層一切正常——那是外部工具最常見的失敗形態。
	specs, ghLog, _ := crossChainSpecs(t, map[string]string{mcpServerToolErrorEnv: toolErr})

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	st := openStore(t, dbPath)
	agent := newLoadSkillAgentOn(t, srv.URL, root, crossChainProfile(), st, specs...)

	reply, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"),
		"幫我把昨天的 PR 摘要發到 Slack")
	if err != nil {
		t.Fatalf("編排中途的工具失敗不該中斷 turn: %v", err)
	}
	if !strings.Contains(reply, "沒有成功") {
		t.Errorf("最終回應不是 LLM 對失敗的收尾: %q", reply)
	}
	// 迭代數與成功那條一樣是 4：失敗只是換了最後一次回應的內容，不該讓循環多跑或早退。
	if len(reqs) != 4 {
		t.Fatalf("LLM 迭代數 = %d, 期望 4", len(reqs))
	}

	// 失敗原文回填給 LLM——那是它換路用的資訊，不該被改寫成一句籠統的「呼叫失敗」。
	if got := toolMessageOf(t, reqs, 3); !strings.Contains(got, toolErr) {
		t.Errorf("失敗原文沒有回填給 LLM: %q", got)
	}
	// 失敗之前那一步是**真的做過的**：gh 收到了呼叫。這是「編排走到一半才失敗」與
	// 「一開始就失敗」的分野。
	if n := countCallLog(t, ghLog); n != 1 {
		t.Errorf("gh server 收到 %d 次 tools/call, 期望 1", n)
	}

	// 失敗的那次照樣落審計，狀態不是 completed——外部工具做過什麼（含沒做成的）同樣
	// 要可查證。
	// 列數也要釘住：`isError` 是工具自己說失敗，**不可重試**（mcp.go 的 IsError 分支
	// 不設 Retryable）。若哪天被改成可重試，這裡會多出重試列——只取「最後一列 slack」
	// 的寫法會安靜地放過那個改動。
	st.flush(t)
	rows := queryToolInvocations(t, dbPath)
	var names []string
	for _, row := range rows {
		names = append(names, row.toolName)
	}
	wantNames := []string{"load_skill", "gh__list_prs", "slack__post_message"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tool_invocations 的執行順序 = %v, 期望 %v（多出的列代表重試）", names, wantNames)
	}
	slack := rows[2]
	// 斷言**等於 failed**，不是「不等於 completed」：後者對一個把狀態寫成空字串的
	// 回歸照樣會通過。
	if slack.status != core.AuditStatusFailed {
		t.Errorf("失敗的呼叫審計狀態 = %q, 期望 %q", slack.status, core.AuditStatusFailed)
	}
	if !slack.errText.Valid || !strings.Contains(slack.errText.String, toolErr) {
		t.Errorf("失敗原因沒落審計: %+v", slack.errText)
	}
}
