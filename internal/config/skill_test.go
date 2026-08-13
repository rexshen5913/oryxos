// SKILL.md 載入的單元測試（ticket #19）。檔案一律用真的（t.TempDir()，憲法 4.3）。
//
// frontmatter 照 agentskills.io 開放標準：name 與 description 必填，license／
// compatibility／metadata／allowed-tools 選填，**標準未定義的欄位一律相容通過**
// ——標準預留 metadata 給各實作放額外屬性，嚴格拒絕會擋掉別處寫好的 Skill。
package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// skipIfNoSymlink 在建立符號連結不可行的平台上回傳跳過理由。Windows 建立符號連結
// 需要開發者模式或管理員權限，一般 CI 使用者做不到——逐案例跳過而不是整個矩陣
// 消失，平台依賴才看得見（#16 的教訓）。
func skipIfNoSymlink() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir, err := os.MkdirTemp("", "symlink-probe")
	if err != nil {
		return "無法建立探測目錄: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		return "本平台無法建立符號連結（Windows 需開發者模式或管理員權限）"
	}
	return ""
}

// writeSkill 在 Workspace 的 skills/ 底下寫一份 SKILL.md（扁平單檔，
// `skills: [daily-pr-digest]` 解析為 `skills/daily-pr-digest.md`）。
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	skills := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("建立 skills/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skills, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %s.md: %v", name, err)
	}
}

// skillDoc 組一份合法的 SKILL.md。
func skillDoc(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// TestSkillsLoadsNameAndDescription 是最小鏈路：引用得到的每份 Skill 回傳其 name
// 與 description，順序等於宣告順序。
func TestSkillsLoadsNameAndDescription(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "daily-pr-digest", skillDoc("daily-pr-digest", "把昨天的 PR 整理成摘要。需要做每日 PR 摘要時使用。", "## 步驟\n\n1. 拉 PR"))
	writeSkill(t, dir, "slack-post", skillDoc("slack-post", "發訊息到 Slack 頻道。需要通知團隊時使用。", "## 步驟\n\n1. 發送"))

	got, err := loader.Skills(context.Background(), []string{"daily-pr-digest", "slack-post"})
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("回傳 %d 份，期望 2", len(got))
	}
	if got[0].Name != "daily-pr-digest" || got[1].Name != "slack-post" {
		t.Errorf("順序應等於宣告順序，實際 %q, %q", got[0].Name, got[1].Name)
	}
	if !strings.Contains(got[0].Description, "每日 PR 摘要") {
		t.Errorf("description 不對: %q", got[0].Description)
	}
}

// TestSkillsReturnsNothingWhenUnreferenced 釘住「沒有引用就沒有 Skill」：`skills`
// 省略（nil）與空清單都回空、不報錯。
//
// 注意這與 bootstrap 欄位**刻意不同**：bootstrap 省略是「載入預設三檔」，skills
// 省略是「沒有 Skill」——Skill 沒有「預設全部」這種語義，Workspace 裡放著的
// SKILL.md 不該因為 Profile 沒提就自動生效。
func TestSkillsReturnsNothingWhenUnreferenced(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "unused", skillDoc("unused", "沒有被任何 Profile 引用。", "正文"))

	for _, names := range [][]string{nil, {}} {
		got, err := loader.Skills(context.Background(), names)
		if err != nil {
			t.Fatalf("Skills(%v): %v", names, err)
		}
		if len(got) != 0 {
			t.Errorf("Skills(%v) 回傳 %d 份，期望 0——未引用的 Skill 不該自動生效", names, len(got))
		}
	}
}

// TestSkillFrontmatterMatrix 是 frontmatter 的解析矩陣。錯誤訊息一律要指出**是哪
// 一份檔案**——一個 Profile 可以引用十份 Skill，不說是哪份等於沒說。
func TestSkillFrontmatterMatrix(t *testing.T) {
	const long1025 = 1025

	tests := []struct {
		name string
		// doc 為該 Skill 檔案的完整內容。
		doc string
		// wantErrSub 為空表示期望載入成功。
		wantErrSub string
	}{
		{
			name: "必填欄位齊全",
			doc:  skillDoc("digest", "做摘要。需要摘要時使用。", "正文"),
		},
		{
			name: "帶全部選填欄位",
			doc: "---\nname: digest\ndescription: 做摘要。需要摘要時使用。\n" +
				"license: MIT\ncompatibility: 需要 Go 1.24\n" +
				"metadata:\n  author: rex\n  version: \"1\"\n" +
				"allowed-tools: http_get http_post\n---\n\n正文\n",
		},
		{
			// 標準預留 metadata 給各實作放額外屬性；遇到不認得的欄位應相容而非拒絕，
			// 否則別處寫好的 Skill 拿到 OryxOS 就跑不動，與宣稱的「兼容」相衝突。
			name: "帶標準未定義的欄位：相容通過",
			doc: "---\nname: digest\ndescription: 做摘要。需要摘要時使用。\n" +
				"trigger: 每天早上\nrequired_tools: [http_get]\nfuture_field: 42\n---\n\n正文\n",
		},
		{
			name:       "缺 name",
			doc:        "---\ndescription: 做摘要。需要摘要時使用。\n---\n\n正文\n",
			wantErrSub: "name",
		},
		{
			name:       "缺 description",
			doc:        "---\nname: digest\n---\n\n正文\n",
			wantErrSub: "description",
		},
		{
			name:       "name 有大寫",
			doc:        skillDoc("Digest", "做摘要。需要摘要時使用。", "正文"),
			wantErrSub: "Digest",
		},
		{
			name:       "name 連字號開頭",
			doc:        "---\nname: \"-digest\"\ndescription: 做摘要。\n---\n\n正文\n",
			wantErrSub: "-digest",
		},
		{
			name:       "name 連字號結尾",
			doc:        skillDoc("digest-", "做摘要。", "正文"),
			wantErrSub: "digest-",
		},
		{
			name:       "name 連續兩個連字號",
			doc:        skillDoc("di--gest", "做摘要。", "正文"),
			wantErrSub: "di--gest",
		},
		{
			name:       "name 超過 64 字元",
			doc:        skillDoc(strings.Repeat("a", 65), "做摘要。", "正文"),
			wantErrSub: "64",
		},
		{
			name:       "description 超過 1024 rune",
			doc:        skillDoc("digest", strings.Repeat("述", long1025), "正文"),
			wantErrSub: "1024",
		},
		{
			// 中文 description：以 rune 計恰好 1024 應通過。用 byte 計的話
			// 3072 byte 會被誤判成超標。
			name: "description 中文恰好 1024 rune：以 rune 計通過",
			doc:  skillDoc("digest", strings.Repeat("述", 1024), "正文"),
		},
		{
			name:       "沒有 frontmatter",
			doc:        "# 只有正文\n\n沒有 frontmatter。\n",
			wantErrSub: "frontmatter",
		},
		{
			name:       "frontmatter 沒有結束分隔線",
			doc:        "---\nname: digest\ndescription: 做摘要。\n\n正文\n",
			wantErrSub: "frontmatter",
		},
		{
			name:       "frontmatter 不是合法 YAML",
			doc:        "---\nname: [未閉合\n---\n\n正文\n",
			wantErrSub: "frontmatter",
		},
		{
			name:       "正文為空",
			doc:        "---\nname: digest\ndescription: 做摘要。\n---\n\n   \n",
			wantErrSub: "正文",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader, dir := newLoader(t)
			writeSkill(t, dir, "digest", tt.doc)

			_, err := loader.Skills(context.Background(), []string{"digest"})
			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("期望載入成功，實際錯誤: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望錯誤含 %q，實際成功", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantErrSub)
			}
			// 一個 Profile 可以引用十份 Skill，錯誤不指名是哪份等於沒說。
			if !strings.Contains(err.Error(), "digest") {
				t.Errorf("錯誤訊息 %q 未指出是哪一份 Skill", err.Error())
			}
		})
	}
}

// TestSkillNameMustMatchReference 釘住「欄位值與 frontmatter name 必須一致」：
// 標準要求 name 與所在目錄名一致，單檔模式下以「與引用名一致」承接同一條約束。
// 錯誤要同時指出兩者，否則使用者不知道該改哪一邊。
func TestSkillNameMustMatchReference(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "daily-digest", skillDoc("weekly-digest", "做摘要。", "正文"))

	_, err := loader.Skills(context.Background(), []string{"daily-digest"})
	if err == nil {
		t.Fatal("引用名與 frontmatter name 不一致時應報錯")
	}
	for _, want := range []string{"daily-digest", "weekly-digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("錯誤訊息 %q 未指出 %q——使用者得知道該改哪一邊", err.Error(), want)
		}
	}
}

// TestSkillsRejectsUnreadablePaths 釘住路徑安全。三種形態各自獨立驗：
//
//   - 引用值本身不合法（路徑逃逸）——由名稱規則擋在讀檔之前
//   - 檔案不存在——設定錯誤，fail fast
//   - 路徑不是普通檔（目錄、符號連結）——Skill 是使用者手寫的實體檔案
func TestSkillsRejectsUnreadablePaths(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		setup   func(t *testing.T, dir string)
		skip    func() string
		wantSub string
		// wantNameError 為真時，錯誤必須指出這是**名稱**不合法，而不是「檔案不存在」。
		//
		// 這個區分不是措辭潔癖：`os.Root` 本來就會擋下逃逸的路徑，所以少了名稱校驗
		// 也「安全」——但使用者拿到的會是「找不到 skills/../../etc/passwd.md」，被
		// 指去找一個本來就不該存在的檔案。真正該說的是「你的 skills 欄位寫錯了」。
		// 少了這條斷言，把名稱校驗整個拿掉測試也不會紅（實測確認過）。
		wantNameError bool
	}{
		{
			name:          "引用值含 ../：拒絕",
			ref:           "../../etc/passwd",
			wantSub:       "../../etc/passwd",
			wantNameError: true,
		},
		{
			name:          "引用值含斜線：拒絕",
			ref:           "sub/skill",
			wantSub:       "sub/skill",
			wantNameError: true,
		},
		{
			name:          "引用值帶副檔名：拒絕（欄位值不帶副檔名）",
			ref:           "digest.md",
			wantSub:       "digest.md",
			wantNameError: true,
		},
		{
			name:    "Skill 不存在",
			ref:     "ghost",
			wantSub: "ghost",
		},
		{
			name: "路徑是目錄",
			ref:  "digest",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "skills", "digest.md"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "digest",
		},
		{
			name: "路徑是符號連結：拒絕跟隨",
			ref:  "digest",
			setup: func(t *testing.T, dir string) {
				writeSkill(t, dir, "real", skillDoc("real", "真的。", "正文"))
				if err := os.Symlink("real.md", filepath.Join(dir, "skills", "digest.md")); err != nil {
					t.Fatal(err)
				}
			},
			skip:    skipIfNoSymlink,
			wantSub: "符號連結",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip != nil {
				if reason := tt.skip(); reason != "" {
					t.Skip(reason)
				}
			}
			loader, dir := newLoader(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}

			_, err := loader.Skills(context.Background(), []string{tt.ref})
			if err == nil {
				t.Fatalf("引用 %q 應報錯", tt.ref)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
			}
			if tt.wantNameError && !strings.Contains(err.Error(), "Skill 名稱") {
				t.Errorf("錯誤訊息 %q 應指出這是名稱不合法，而不是把使用者指去找一個不該存在的檔案", err.Error())
			}
		})
	}
}

// TestSkillsHonoursContextCancellation 釘住阻塞路徑吃 ctx（憲法 5.3）。
func TestSkillsHonoursContextCancellation(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "digest", skillDoc("digest", "做摘要。", "正文"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := loader.Skills(ctx, []string{"digest"}); err == nil {
		t.Fatal("已取消的 ctx 應回錯誤")
	} else if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("錯誤鏈應帶出 context.Canceled: %v", err)
	}
}

// TestSkillsRereadsEveryCall 釘住「不緩存」：改檔後下一次呼叫要讀到新內容，與
// Bootstrap 同一條規則（每個 turn 重讀）。
func TestSkillsRereadsEveryCall(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "digest", skillDoc("digest", "舊的描述。", "正文"))

	first, err := loader.Skills(context.Background(), []string{"digest"})
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	writeSkill(t, dir, "digest", skillDoc("digest", "新的描述。", "正文"))
	second, err := loader.Skills(context.Background(), []string{"digest"})
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}

	if !strings.Contains(first[0].Description, "舊的") {
		t.Errorf("第一次讀到 %q", first[0].Description)
	}
	if !strings.Contains(second[0].Description, "新的") {
		t.Errorf("第二次讀到 %q——載入器不該緩存", second[0].Description)
	}
}

// TestSkillBodyGoesThroughSameChecks 釘住第二層**不另立一條寬鬆的讀取路徑**：
// SkillBody 與 Skills 走同一組校驗。
//
// 這比第一層更要緊——第二層的入口是 **LLM 送來的參數**，第一層是使用者手寫的
// Profile 欄位。少了任何一道，一個被誘導的模型就能拿它讀 Workspace 外的檔案。
func TestSkillBodyGoesThroughSameChecks(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		setup   func(t *testing.T, dir string)
		skip    func() string
		wantSub string
	}{
		{name: "路徑逃逸：名稱規則擋在讀檔之前", ref: "../../etc/passwd", wantSub: "Skill 名稱"},
		{name: "帶副檔名", ref: "digest.md", wantSub: "Skill 名稱"},
		{name: "不存在", ref: "ghost", wantSub: "ghost"},
		{
			name: "符號連結：拒絕跟隨",
			ref:  "digest",
			setup: func(t *testing.T, dir string) {
				writeSkill(t, dir, "real", skillDoc("real", "真的。", "正文"))
				if err := os.Symlink("real.md", filepath.Join(dir, "skills", "digest.md")); err != nil {
					t.Fatal(err)
				}
			},
			skip:    skipIfNoSymlink,
			wantSub: "符號連結",
		},
		{
			name:    "frontmatter name 與引用名不一致",
			ref:     "digest",
			setup:   func(t *testing.T, dir string) { writeSkill(t, dir, "digest", skillDoc("other", "描述。", "正文")) },
			wantSub: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip != nil {
				if reason := tt.skip(); reason != "" {
					t.Skip(reason)
				}
			}
			loader, dir := newLoader(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}

			_, err := loader.SkillBody(context.Background(), tt.ref)
			if err == nil {
				t.Fatalf("SkillBody(%q) 應報錯", tt.ref)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestSkillBodyReturnsBodyOnly 釘住 SkillBody 回的是**正文**、不含 frontmatter：
// 回填給 LLM 的應該是任務說明，不是那幾行 YAML metadata（那已經在 system prompt
// 的第一層了，重複送等於白花 token）。
func TestSkillBodyReturnsBodyOnly(t *testing.T) {
	loader, dir := newLoader(t)
	writeSkill(t, dir, "digest", skillDoc("digest", "做摘要。需要摘要時使用。", "## 步驟\n\n1. 拉 PR\n2. 寫摘要"))

	body, err := loader.SkillBody(context.Background(), "digest")
	if err != nil {
		t.Fatalf("SkillBody: %v", err)
	}
	if !strings.Contains(body, "拉 PR") {
		t.Errorf("回傳未含正文: %q", body)
	}
	for _, notWant := range []string{"---", "name: digest", "description:"} {
		if strings.Contains(body, notWant) {
			t.Errorf("回傳含 frontmatter 片段 %q，那已經在第一層了: %q", notWant, body)
		}
	}
}

// TestSkillBodyNotReturned 釘住漸進揭露的第一層：載入器回傳的只有 name 與
// description，**正文不在其中**。正文按需回填是第二層的事（#20）。
func TestSkillBodyNotReturned(t *testing.T) {
	const body = "BODY_SENTINEL_這段正文不該出現在第一層"
	loader, dir := newLoader(t)
	writeSkill(t, dir, "digest", skillDoc("digest", "做摘要。", body))

	got, err := loader.Skills(context.Background(), []string{"digest"})
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	if got[0].Name+got[0].Description == "" {
		t.Fatal("回傳空的 SkillMeta")
	}
	if strings.Contains(got[0].Name+got[0].Description, body) {
		t.Error("正文洩漏進第一層——漸進揭露的省 token 前提就沒了")
	}
}
