// Skill 漸進揭露第一層的整合測試（ticket #19）：沿用既有兩個 seam——從
// AgentService.Process 驅動、斷言送往 LLM 邊界請求的 system prompt——SKILL.md 在
// seam 之下用真實檔案（t.TempDir()，憲法 4.3）。
//
// 這裡驗的是那條省 token 的前提：常駐在 prompt 裡的只有 name 與 description，
// **正文不在**。正文按需回填是第二層的事（#20）。
package core_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// seedSkill 在 Workspace 的 skills/ 底下寫一份 SKILL.md（真實檔案）。
func seedSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	skills := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("建立 skills/: %v", err)
	}
	doc := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(skills, name+".md"), []byte(doc), 0o644); err != nil {
		t.Fatalf("寫入 %s.md: %v", name, err)
	}
}

// writeRaw 原樣寫一個檔案（不套 SKILL.md 的模板），供「檔案被改壞」的案例使用。
//
// 失敗用 **t.Errorf 而不是 t.Fatalf**：這個 helper 會從 newMutatingReplayServer 的
// mutate callback 呼叫，而那個 callback 跑在 **HTTP handler 的 goroutine** 上。
// 在非測試 goroutine 呼叫 t.Fatalf 會對那條 goroutine 執行 runtime.Goexit——handler
// 死在回應中途，測試不會乾淨地失敗，反而變成 Agent 收到半截回應或整個卡住。
func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Errorf("寫入 %s: %v", path, err)
	}
}

// profileWithSkills 回傳一份引用了指定 Skill 的 Profile（帶 identity.prompt，讓
// 人格層有東西、SOUL.md 被互斥排除，聚焦在 Skill 段本身）。
func profileWithSkills(skills ...string) *core.Profile {
	prof := testProfile()
	prof.Identity.Prompt = bootIdentity
	prof.Skills = skills
	return prof
}

// TestSkillNameAndDescriptionInjected 是最小鏈路：引用得到的每份 Skill，其 name 與
// description 出現在 system prompt 裡，而**正文不出現**。
func TestSkillNameAndDescriptionInjected(t *testing.T) {
	const (
		descA = "把昨天的 PR 整理成摘要。需要做每日 PR 摘要時使用。"
		descB = "發訊息到 Slack 頻道。需要通知團隊時使用。"
		bodyA = "BODY_A_SENTINEL 這段正文不該常駐在 system prompt"
		bodyB = "BODY_B_SENTINEL 這段也不該"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "daily-pr-digest", descA, bodyA)
	seedSkill(t, dir, "slack-post", descB, bodyB)

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills("daily-pr-digest", "slack-post"))
	got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

	for _, want := range []string{"daily-pr-digest", descA, "slack-post", descB} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt 遺失 %q: %q", want, got)
		}
	}
	// 這兩條是漸進揭露的**全部價值**：正文常駐的話，裝十份 Skill 就把 prompt 撐爆了。
	for _, notWant := range []string{bodyA, bodyB} {
		if strings.Contains(got, notWant) {
			t.Errorf("system prompt 含 Skill 正文 %q——第一層只該有 name 與 description", notWant)
		}
	}
}

// TestSkillSectionIsLastLayer 釘住 ADR-0003 的第五層位置：Skill 段排在長期記憶
// 之後、是 system prompt 的最後一層。
func TestSkillSectionIsLastLayer(t *testing.T) {
	const (
		agents   = "本專案的慣例是測試先行"
		user     = "使用者偏好繁體中文回覆"
		longTerm = "使用者的後端統一用 Go"
		skillDsc = "把昨天的 PR 整理成摘要。需要做每日 PR 摘要時使用。"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", agents)
	seedBootstrap(t, dir, "USER.md", user)
	seedMemory(t, filepath.Join(dir, memoryRelPath), "## 2026-08-01\n\n- "+longTerm+"\n")
	seedSkill(t, dir, "daily-pr-digest", skillDsc, "正文")

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills("daily-pr-digest"))
	got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

	// 五層都在，且按 ADR-0003 的順序。
	var last int
	for _, want := range []string{bootIdentity, agents, user, longTerm, skillDsc} {
		at := strings.Index(got, want)
		if at < 0 {
			t.Fatalf("system prompt 遺失 %q: %q", want, got)
		}
		if at < last {
			t.Errorf("順序錯誤：%q 出現在前一層之前（Skill 段應是最後一層）\n%s", want, got)
		}
		last = at
	}
}

// TestNoSkillSectionWhenUnreferenced 釘住「沒有引用就不留空標題」：skills 省略或
// 為空清單時，system prompt 不得多出一個 Skill 段的引言讓 LLM 猜。
//
// 這與 bootstrap 欄位**刻意不同**：bootstrap 省略是「載入預設三檔」，skills 省略是
// 「這個 Agent 沒有 Skill」。
func TestNoSkillSectionWhenUnreferenced(t *testing.T) {
	tests := []struct {
		name   string
		skills []string
	}{
		{name: "欄位省略（nil）", skills: nil},
		{name: "空清單", skills: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			// Workspace 裡放著一份沒被引用的 Skill：它不該自動生效。
			seedSkill(t, dir, "unused", "沒有被任何 Profile 引用。", "正文")

			prof := testProfile()
			prof.Identity.Prompt = bootIdentity
			prof.Skills = tt.skills

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, prof)
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			// 所有 Bootstrap 層都空、又沒有 Skill，system prompt 必須**恰好**是
			// identity.prompt——不得留下空標題或多餘的空行。
			if got != bootIdentity {
				t.Errorf("system prompt 應恰好是 identity.prompt，實際 %q", got)
			}
			if strings.Contains(got, "unused") {
				t.Error("未被引用的 Skill 進了 prompt")
			}
		})
	}
}

// TestSkillRereadEveryTurn 釘住 Skill 與 Bootstrap 同一條規則：每個 turn 重讀。
// 使用者改了 description，下一個 turn 立刻生效，不必重啟。
func TestSkillRereadEveryTurn(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "digest", "舊的描述。", "正文")

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills("digest"))
	session := core.NewSession("cli", "local", "default")

	first := promptAfterTurn(t, agent, session, &reqs, "早安")
	if !strings.Contains(first, "舊的描述。") {
		t.Fatalf("第一個 turn 的 prompt 應含舊描述: %q", first)
	}

	seedSkill(t, dir, "digest", "新的描述。", "正文")
	second := promptAfterTurn(t, agent, session, &reqs, "午安")
	if !strings.Contains(second, "新的描述。") {
		t.Errorf("第二個 turn 應讀到新描述（每 turn 重讀、不緩存）: %q", second)
	}
	if strings.Contains(second, "舊的描述。") {
		t.Errorf("第二個 turn 仍含舊描述: %q", second)
	}
}

// captureSink 收集 slog 記錄的 message，供斷言「某個事件有沒有發生」。
//
// 這是 spec #3 允許的例外形狀：一般資訊型日誌的**措辭**不進斷言，但像
// `audit_write_failed` 那種降級事件要看得見，所以斷言的是**事件鍵**不是句子。
//
// 鎖放在 sink、handler 只持有指標：`WithAttrs`／`WithGroup` 都要回傳新的 handler，
// 把 mutex 放進 handler 會被 go vet 的 copylocks 攔下。
type captureSink struct {
	mu       sync.Mutex
	messages []string
}

func (s *captureSink) add(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
}

// count 回傳收到的記錄裡等於 want 的筆數。
func (s *captureSink) count(want string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.messages {
		if m == want {
			n++
		}
	}
	return n
}

type capturingHandler struct{ sink *captureSink }

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h capturingHandler) WithGroup(string) slog.Handler            { return h }
func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.sink.add(r.Message)
	return nil
}

func capturingLogger() (*slog.Logger, *captureSink) {
	sink := &captureSink{}
	return slog.New(capturingHandler{sink: sink}), sink
}

// TestSkillSectionTruncationLoggedEveryTurn 釘住截斷偵測**不是啟動時的一次性快照**。
//
// description 每個 turn 重讀，使用者在對話中途把某份寫長就可能跨過上限——啟動時算的
// 那一次對此一無所知。少了這條，Skill 會在對話中途無聲消失：LLM 那端有標記，使用者
// 與維運端卻什麼都看不到。
//
// 斷言的是事件鍵 `skill_section_truncated` 的**有無**，不綁日誌的措辭。
func TestSkillSectionTruncationLoggedEveryTurn(t *testing.T) {
	// 12 份、每份 700 rune 合計約 8600，未溢出；把其中一份改長就跨過 10000。
	const (
		count    = 12
		fits     = 700
		overflow = 1000
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	refs := make([]string, 0, count)
	for i := range count {
		name := "skill-" + strconv.Itoa(i)
		seedSkill(t, dir, name, strings.Repeat("述", fits), "正文")
		refs = append(refs, name)
	}

	logger, sink := capturingLogger()
	prof := testProfile()
	prof.Identity.Prompt = bootIdentity
	prof.Skills = refs
	agent := newBootstrapAgentWithLogger(t, srv.URL, root, prof, logger)
	session := core.NewSession("cli", "local", "default")

	// 第一個 turn：沒有溢出，不該記。
	if _, err := agent.Process(t.Context(), session, "早安"); err != nil {
		t.Fatalf("第一個 turn: %v", err)
	}
	if n := sink.count("skill_section_truncated"); n != 0 {
		t.Fatalf("未溢出卻記了 %d 筆截斷", n)
	}

	// 對話中途把每份描述改長，總量跨過上限。
	for _, name := range refs {
		seedSkill(t, dir, name, strings.Repeat("述", overflow), "正文")
	}

	// 第二個 turn：溢出了，必須記得到——啟動時那一次快照看不到這件事。
	if _, err := agent.Process(t.Context(), session, "午安"); err != nil {
		t.Fatalf("第二個 turn: %v", err)
	}
	if n := sink.count("skill_section_truncated"); n != 1 {
		t.Errorf("對話中途溢出後應記到 1 筆截斷，實際 %d 筆——截斷偵測還停在啟動時的快照", n)
	}
}

// TestDuplicateSkillRefsFailTurn 釘住手組的 Profile 也擋得住重複引用。
//
// `LoadProfile` 會擋，但**不是每個 Profile 都經過它**——core 自己的測試全是手組的，
// 日後 ProfileRegistry 也是。少了每 turn 的入口校驗，`skills: [a, a]` 會把同一份
// 注入兩次、吃掉預算，甚至把尾端其他 Skill 擠出視野。bootstrap 欄位在 #17 已經補過
// 同一道，這裡補上 skills 那半邊。
func TestDuplicateSkillRefsFailTurn(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "digest", "做摘要。", "正文")

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills("digest", "digest"))
	_, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安")
	if err == nil {
		t.Fatal("重複引用同一份 Skill 時該 turn 應失敗")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("錯誤訊息 %q 未指出是哪一份", err.Error())
	}
}

// TestBrokenSkillFailsTurn 釘住 Skill 載入失敗會 fail 該 turn，不靜默降級成
// 「這個 Agent 沒有技能」——那會讓使用者以為 Skill 沒被觸發，而不是根本沒載入。
func TestBrokenSkillFailsTurn(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedSkill(t, dir, "digest", "做摘要。", "正文")

	// 引用一份不存在的 Skill。
	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills("digest", "ghost"))
	_, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安")
	if err == nil {
		t.Fatal("引用不存在的 Skill 時該 turn 應失敗")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("錯誤訊息 %q 未指出是哪一份 Skill", err.Error())
	}
}
