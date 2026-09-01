// 漸進揭露第二層的整合測試（ticket #20）：LLM 讀到 Skill 描述後呼叫 load_skill
// 取回正文，正文以 **tool 訊息**回填進對話——**不是**塞回 system prompt。
//
// 沿用既有兩個 seam：從 AgentService.Process 驅動、LLM 以 httptest 回放（ADR-0002），
// SKILL.md 在 seam 之下用真實檔案（t.TempDir()，憲法 4.3）。
package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// newLoadSkillAgent 組出一個帶 load_skill 的 AgentService：Skill 從 root 底下的
// 真實檔案載入，load_skill 的可用範圍就是 Profile 的 skills 欄位。
func newLoadSkillAgent(t *testing.T, baseURL string, root *os.Root, prof *core.Profile) *core.AgentService {
	t.Helper()
	return newLoadSkillAgentOn(t, baseURL, root, prof, newStore(t))
}

// newLoadSkillAgentOn 同上，但用指定的 Session／審計儲存——要查 tool_invocations 的
// 測試得自己持有那個 db 檔的路徑。
//
// specs 是要一併連上的真實 MCP server，跨鏈路編排的測試走這條
// （agent_skill_mcp_test.go）。它們與 load_skill 進**同一個** Registry，因為那正是
// 「零程式碼跨工具編排」的前提：ReAct 循環不感知工具來自 Skill 那條線還是 MCP 那條線。
// 空 specs 時 ConnectMcpServers 是 no-op，既有呼叫端不受影響。
func newLoadSkillAgentOn(t *testing.T, baseURL string, root *os.Root, prof *core.Profile,
	st *testStore, specs ...core.McpServerSpec) *core.AgentService {
	t.Helper()
	loader := config.NewContextLoader(root)

	registry := tool.NewRegistry()
	if err := registry.Register(tool.NewLoadSkillTool(loader, prof.Skills)); err != nil {
		t.Fatalf("註冊 load_skill: %v", err)
	}
	connectMcp(t, registry, specs)
	exec, err := registry.Subset(prof.Tools, []string{"load_skill"}, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}

	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(prof, svc, exec,
		memory.NewService(st.sessions(), memory.NewLongTermMemory(root, memoryRelPath)),
		st.audit, loader, core.NopEventSink{}, nil, discardLogger())
}

// toolMessageOf 回傳第 n 次 LLM 邊界請求裡的 tool 訊息內容（沒有則回空字串）。
//
// 先檢查長度再索引：請求數不如預期時，`reqs[n]` 會以 index out of range panic 蓋掉
// 真正的失敗原因，讀 log 的人只看得到堆疊而不是「少了一次 LLM 呼叫」。
func toolMessageOf(t *testing.T, reqs [][]byte, n int) string {
	t.Helper()
	if len(reqs) <= n {
		t.Fatalf("只收到 %d 次 LLM 請求，取不到第 %d 次", len(reqs), n+1)
	}
	var last string
	for _, m := range parseLLMRequest(t, reqs[n]).Messages {
		if m.Role == "tool" {
			last = m.Content
		}
	}
	return last
}

// TestLoadSkillReturnsBodyAsToolMessage 是本票主場景：LLM 讀到描述 → 呼叫
// load_skill → 正文以 tool 訊息回填 → 據此回覆。
//
// 兩條斷言撐起整個漸進揭露：正文**在 tool 訊息裡**，而且**不在 system prompt 裡**。
// 少了後者，這條鏈路就退化成「正文常駐」——省 token 的前提整個沒了。
func TestLoadSkillReturnsBodyAsToolMessage(t *testing.T) {
	const (
		desc = "把昨天的 PR 整理成摘要。需要做每日 PR 摘要時使用。"
		body = "## 步驟\n\n1. 拉昨天合併的 PR\n2. 依模組分組\n3. 每組寫一句摘要"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_load_skill.json"),
		readFixture(t, "reply_after_skill_body.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "daily-pr-digest", desc, body)

	agent := newLoadSkillAgent(t, srv.URL, root, profileWithSkills("daily-pr-digest"))
	reply, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "幫我做昨天的 PR 摘要")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(reply, "讀完這份 Skill") {
		t.Errorf("最終回應不對: %q", reply)
	}

	// 第二次 LLM 呼叫（tool 結果回填之後）的訊息序列。
	toolMsg := toolMessageOf(t, reqs, 1)
	if toolMsg == "" {
		t.Fatal("對話裡沒有 tool 訊息——正文沒有經 tool 結果回填")
	}
	if !strings.Contains(toolMsg, "拉昨天合併的 PR") {
		t.Errorf("tool 訊息未帶 Skill 正文: %q", toolMsg)
	}

	// system prompt 有描述、**沒有正文**——兩次呼叫都一樣。
	for i, raw := range reqs {
		got := systemPrompt(t, raw)
		if !strings.Contains(got, desc) {
			t.Errorf("第 %d 次呼叫的 system prompt 遺失 Skill 描述: %q", i+1, got)
		}
		if strings.Contains(got, "拉昨天合併的 PR") {
			t.Errorf("第 %d 次呼叫的 system prompt 含 Skill 正文——正文只該走 tool 訊息", i+1)
		}
	}
	// 兩次的 system prompt 必須**完全一樣**：取回正文不得改動常駐的那一層。
	if systemPrompt(t, reqs[0]) != systemPrompt(t, reqs[1]) {
		t.Error("取回正文後 system prompt 變了——第二層不該回頭動第一層")
	}
}

// TestSkillSectionPromiseWithoutToolFailsBeforeSending 釘住「引言承諾的 Tool 必須
// 真的在」這條跨 package 的不變式，且**在送出請求之前**就擋下來。
//
// Skill 段叫 LLM 用 load_skill 取回正文（core 的 skillSectionIntro），但那個 Tool 進
// 不進可用集合是**組裝點**（cmd/oryxos/chat.go）決定的。兩邊分屬不同 package，中間
// 沒有檢查的話，日後多一個組裝點忘了那條推導就會安靜地重現這條鏈路最該避免的失敗
// 形態：LLM 被叫去呼叫一個工具清單裡不存在的 Tool，然後拿描述硬編出步驟。
//
// **關鍵斷言是「Provider 一個請求都沒收到」**，不只是「有警示日誌」——偵測到之後仍
// 把那份自相矛盾的 prompt 送出去，等於明知會壞還讓它壞。
func TestSkillSectionPromiseWithoutToolFailsBeforeSending(t *testing.T) {
	tests := []struct {
		name string
		// withTool 為真時把 load_skill 放進可用集合（＝組裝點做對了）。
		withTool bool
		skills   []string
		wantErr  bool
	}{
		{name: "有 Skill、load_skill 也在：照常", withTool: true, skills: []string{"digest"}},
		{name: "有 Skill、load_skill 卻不在：turn 失敗且不送請求", skills: []string{"digest"}, wantErr: true},
		{name: "沒有 Skill：引言不出現，照常", withTool: false, skills: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedSkill(t, dir, "digest", "做摘要。", "正文")

			st := newStore(t)
			loader := config.NewContextLoader(root)
			registry := tool.NewRegistry()
			if err := registry.Register(tool.NewLoadSkillTool(loader, tt.skills)); err != nil {
				t.Fatalf("註冊 load_skill: %v", err)
			}
			var auto []string
			if tt.withTool {
				auto = []string{tool.LoadSkillToolName}
			}
			exec, err := registry.Subset(nil, auto, discardLogger())
			if err != nil {
				t.Fatalf("Subset: %v", err)
			}

			logger, sink := capturingLogger()
			prof := profileWithSkills(tt.skills...)
			svc := provider.NewService(map[string]provider.Config{
				"openai": {APIKey: "test-key", BaseURL: srv.URL},
			}, discardLogger())
			agent := core.NewAgentService(prof, svc, exec,
				memory.NewService(st.sessions(), memory.NewLongTermMemory(root, memoryRelPath)),
				st.audit, loader, core.NopEventSink{}, nil, logger)

			_, err = agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安")
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("不該失敗: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatal("組裝點漏了 load_skill 時該 turn 應失敗")
			}
			if !strings.Contains(err.Error(), core.LoadSkillToolName) {
				t.Errorf("錯誤訊息 %q 未指出缺的是哪個 Tool", err.Error())
			}
			// 這條才是重點：矛盾的 prompt **一個字都沒送出去**。
			if len(reqs) != 0 {
				t.Errorf("已送出 %d 次 LLM 請求——偵測到組裝錯誤後仍把矛盾的 prompt 送給 Provider", len(reqs))
			}
			// 日誌是給維運端的旁路信號，錯誤才是主線；兩者都要在。
			if sink.count("skill_section_promises_missing_tool") == 0 {
				t.Error("未落結構化警示")
			}
		})
	}
}

// TestLoadSkillCallIsAudited 釘住 load_skill 的呼叫與其他 Tool **一視同仁**地落
// tool_invocations：外部工具與內建工具做過什麼都要可查證（憲法 6.2）。
//
// 這條看起來像是在重測審計那條既有鏈路，但它驗的是另一件事：load_skill 沒有被特殊
// 對待、沒有繞過 ReActLoop.execute 那條記錄路徑。
func TestLoadSkillCallIsAudited(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	st := openStore(t, dbPath)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_load_skill.json"),
		readFixture(t, "reply_after_skill_body.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "daily-pr-digest", "做摘要。", "## 步驟\n\n1. 拉 PR")

	agent := newLoadSkillAgentOn(t, srv.URL, root, profileWithSkills("daily-pr-digest"), st)
	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "做摘要"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	st.flush(t)

	var found bool
	for _, row := range queryToolInvocations(t, dbPath) {
		if row.toolName == "load_skill" {
			found = true
			if row.status != core.AuditStatusCompleted {
				t.Errorf("load_skill 的審計狀態 = %q, 期望 %q", row.status, core.AuditStatusCompleted)
			}
		}
	}
	if !found {
		t.Error("tool_invocations 沒有 load_skill 的資料列——它繞過了審計路徑")
	}
}

// TestLoadSkillErrorsFillBackWithoutBreakingTurn 釘住 load_skill 的失敗一律以
// ToolResult.Error 回填、**不中斷 turn**——沿 spec #1 既有的 Tool 失敗語義，讓 LLM
// 看到錯誤後換一條路回覆使用者。
func TestLoadSkillErrorsFillBackWithoutBreakingTurn(t *testing.T) {
	tests := []struct {
		name string
		// skills 是 Profile 引用的 Skill；setup 在 turn 開始前佈置 Workspace。
		skills []string
		setup  func(t *testing.T, dir string)
		// mutate 在**第一次 LLM 回應之後**動檔案——那正是第一層讀完、load_skill
		// 尚未執行的窗口。檔案在 turn 開始前就壞掉的話，第一層（每個 turn 重讀
		// 全部引用的 Skill）會先 fail 掉整個 turn，根本走不到這個 Tool；那是 #19
		// 的既定行為，不是這裡要驗的東西。
		mutate  func(t *testing.T, dir string)
		wantSub string
	}{
		{
			name:    "指定的 Skill 不存在",
			skills:  []string{"other-skill"},
			setup:   func(t *testing.T, dir string) { seedSkill(t, dir, "other-skill", "別的。", "正文") },
			wantSub: "daily-pr-digest",
		},
		{
			// 檔案在，但這個 Profile 沒引用它——不能讓 LLM 讀到別的 Agent 的 Skill。
			name:   "Skill 存在但未被本 Profile 引用",
			skills: []string{"other-skill"},
			setup: func(t *testing.T, dir string) {
				seedSkill(t, dir, "other-skill", "別的。", "正文")
				seedSkill(t, dir, "daily-pr-digest", "沒被引用。", "正文")
			},
			wantSub: "daily-pr-digest",
		},
		{
			name:   "Profile 沒有宣告任何 Skill",
			skills: nil,
			setup:  func(t *testing.T, dir string) {},
			// 錯誤要說清楚是「這個 Profile 沒有 Skill」，而不是「找不到檔案」。
			wantSub: "沒有宣告任何 Skill",
		},
		{
			name:   "turn 進行中檔案被刪掉",
			skills: []string{"daily-pr-digest"},
			setup:  func(t *testing.T, dir string) { seedSkill(t, dir, "daily-pr-digest", "做摘要。", "正文") },
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "skills", "daily-pr-digest.md")); err != nil {
					t.Errorf("刪除 Skill: %v", err)
				}
			},
			wantSub: "daily-pr-digest",
		},
		{
			name:   "turn 進行中檔案被改壞（frontmatter 不合法）",
			skills: []string{"daily-pr-digest"},
			setup:  func(t *testing.T, dir string) { seedSkill(t, dir, "daily-pr-digest", "做摘要。", "正文") },
			mutate: func(t *testing.T, dir string) {
				writeRaw(t, filepath.Join(dir, "skills", "daily-pr-digest.md"), "沒有 frontmatter 的檔案\n")
			},
			wantSub: "daily-pr-digest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, dir := bootstrapWorkspace(t)
			tt.setup(t, dir)

			var reqs [][]byte
			mutate := func() {
				if tt.mutate != nil {
					tt.mutate(t, dir)
				}
			}
			srv := newMutatingReplayServer(t, &reqs, mutate,
				readFixture(t, "reply_load_skill.json"),
				readFixture(t, "reply_after_tool_error.json"))

			prof := profileWithSkills(tt.skills...)
			agent := newLoadSkillAgent(t, srv.URL, root, prof)
			// turn 必須正常完成——Tool 失敗不是硬錯誤。
			if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "做摘要"); err != nil {
				t.Fatalf("Tool 失敗不該中斷 turn: %v", err)
			}

			toolMsg := toolMessageOf(t, reqs, 1)
			if !strings.Contains(toolMsg, tt.wantSub) {
				t.Errorf("回填的錯誤 %q 未含 %q", toolMsg, tt.wantSub)
			}
		})
	}
}

// TestLoadSkillBodyTruncated 釘住單份正文的回填上限：超過時截斷並附自述省略量的
// 標記。回填走的是 tool 訊息，同樣每個 turn 都在對話裡佔位置，所以一樣要有上限。
func TestLoadSkillBodyTruncated(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_load_skill.json"),
		readFixture(t, "reply_after_skill_body.json"))
	root, dir := bootstrapWorkspace(t)

	const tailSentinel = "TAIL_SENTINEL_這段正文必須被截掉"
	body := strings.Repeat("這是一行填充內容。\n", 2000) + tailSentinel
	seedSkill(t, dir, "daily-pr-digest", "做摘要。", body)

	agent := newLoadSkillAgent(t, srv.URL, root, profileWithSkills("daily-pr-digest"))
	if _, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "做摘要"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	toolMsg := toolMessageOf(t, reqs, 1)
	if n := utf8.RuneCountInString(toolMsg); n > core.MaxSkillBodyRunes {
		t.Errorf("回填 %d rune，超過上限 %d", n, core.MaxSkillBodyRunes)
	}
	if strings.Contains(toolMsg, tailSentinel) {
		t.Error("超過上限卻沒截斷")
	}
	if !strings.Contains(toolMsg, "這是一行填充內容。") {
		t.Error("截斷後應保留開頭")
	}
}
