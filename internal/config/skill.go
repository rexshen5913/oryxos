package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/rexshen5913/oryxos/internal/core"
)

// skillsDir 是 Skill 在 Workspace 內的目錄（需求文檔 §5.6）。`skills: [x]` 解析為
// `skills/x.md`——**扁平單檔**，不是標準的目錄形態（附帶檔案屬擴展階段，見 spec #3
// 的 Further Notes）。
const skillsDir = "skills"

// skillFrontmatter 是 SKILL.md 的 YAML frontmatter。
//
// **刻意只宣告 name 與 description 兩個欄位。** 標準另有 license、compatibility、
// metadata、allowed-tools，本結構一個都不宣告——yaml.v3 預設忽略未知欄位，所以它們
// 「照解析」（不報錯）但不進入 OryxOS 的任何判斷。
//
// 不宣告是為了**兼容**，不是偷懶：一旦宣告了型別，一份在別處跑得動、但把
// `allowed-tools` 寫成 YAML 清單（標準寫的是空白分隔字串）的 Skill，就會在
// Unmarshal 階段被擋掉。spec #3 明文警告過這件事——自創的強制語義會讓在別處能跑的
// Skill 在 OryxOS 被拒絕，與宣稱的兼容相衝突。核心階段對這些欄位沒有著力點
// （`allowed-tools` 的標準語義是 pre-approved，前提是宿主有權限詢問機制可供免除，
// OryxOS 沒有），所以最相容的做法就是不去碰它們。
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Skills 讀回 names 引用的每份 Skill 的 name 與 description，順序等於宣告順序。
// names 為空（欄位省略或空清單）時回 nil、不算錯誤。
//
// **與 bootstrap 欄位刻意不同**：bootstrap 省略是「載入預設三檔」，skills 省略是
// 「這個 Agent 沒有 Skill」——Skill 沒有「預設全部」這種語義，Workspace 的 skills/
// 底下放著的檔案不該因為 Profile 沒提就自動生效。
//
// 任何一份出問題都回錯誤讓呼叫端 fail（引用不存在、frontmatter 不合法、name 與引用
// 名不一致都是設定錯誤）；錯誤訊息一律指出是**哪一份** Skill——一個 Profile 可以引用
// 十份，不說是哪份等於沒說。
//
// 回傳的只有 name 與 description，**正文不在其中**：那是漸進揭露的第一層，正文按需
// 回填是第二層的事（#20）。
func (l *ContextLoader) Skills(ctx context.Context, names []string) ([]core.SkillMeta, error) {
	if len(names) == 0 {
		return nil, nil
	}
	metas := make([]core.SkillMeta, 0, len(names))
	for _, ref := range names {
		// 每讀一份前檢查一次：取消要能在中途生效（憲法 5.3）。
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("載入 Skill: %w", err)
		}
		meta, err := l.readSkill(ref)
		if err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// readSkill 讀回並解析一份 SKILL.md。
func (l *ContextLoader) readSkill(ref string) (core.SkillMeta, error) {
	// 引用值本身必須是合法的 Skill 名稱。這道校驗擋在讀檔**之前**，順帶讓路徑逃逸
	// 結構上不可能——`../`、斜線、絕對路徑都構不出一個只有小寫英數與連字號的字串
	// （見 core.ValidateSkillName）。os.Root 是第二道防線，兩者不互相取代。
	if err := core.ValidateSkillName(ref); err != nil {
		return core.SkillMeta{}, fmt.Errorf("Profile 的 skills 引用了 %q: %w", ref, err)
	}
	name := path.Join(skillsDir, ref+".md")

	info, err := l.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return core.SkillMeta{}, fmt.Errorf("Profile 的 skills 引用的 Skill %q 不存在（找不到 %s）: %w", ref, name, err)
	}
	if err != nil {
		return core.SkillMeta{}, fmt.Errorf("檢查 Skill %q: %w", ref, err)
	}
	// 與 Bootstrap 同一條規則：符號連結一律拒絕、不跟隨。Skill 的內容會被送往
	// Provider，而 skills/ 隨 Workspace 進 git。
	if info.Mode()&os.ModeSymlink != 0 {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 是符號連結，拒絕跟隨（它只能是 Workspace 內的實體檔案）", ref)
	}
	if !info.Mode().IsRegular() {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 不是普通檔（實際為 %s）", ref, info.Mode().Type())
	}

	data, err := l.root.ReadFile(name)
	if err != nil {
		return core.SkillMeta{}, fmt.Errorf("讀取 Skill %q: %w", ref, err)
	}
	return parseSkill(ref, string(data))
}

// parseSkill 解析一份 SKILL.md 的內容，校驗 frontmatter 並確認 name 與引用名一致。
//
// 比對前把 CRLF 正規化成 LF：SKILL.md 隨 Workspace 進 git，而 Git for Windows 安裝時
// 預設 `core.autocrlf=true`——不正規化的話 frontmatter 的 `---` 分隔線在 Windows
// checkout 出來是 `---\r`，整份 Skill 會被判成「沒有 frontmatter」（#16 已為 Bootstrap
// 的舊模板比對付過同一筆學費）。
func parseSkill(ref, content string) (core.SkillMeta, error) {
	front, body, err := splitFrontmatter(normalizeNewlines(content))
	if err != nil {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 frontmatter 不合法: %w", ref, err)
	}

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 frontmatter 不是合法 YAML: %w", ref, err)
	}

	if fm.Name == "" {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 frontmatter 缺少必填欄位 name", ref)
	}
	if fm.Description == "" {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 frontmatter 缺少必填欄位 description", ref)
	}
	if err := core.ValidateSkillName(fm.Name); err != nil {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 frontmatter name 不合法: %w", ref, err)
	}
	// 標準要求 name 與所在目錄名一致；單檔模式下以「與引用名一致」承接同一條約束。
	// 錯誤要同時指出兩者，否則使用者不知道該改哪一邊。
	if fm.Name != ref {
		return core.SkillMeta{}, fmt.Errorf(
			"Skill %q 的 frontmatter name 是 %q，與引用名不一致（請讓兩者相同：改 Profile 的 skills 或改 %s 的 name）",
			ref, fm.Name, path.Join(skillsDir, ref+".md"))
	}
	if n := utf8.RuneCountInString(fm.Description); n > core.MaxSkillDescriptionRunes {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的 description 長 %d 字，超過上限 %d",
			ref, n, core.MaxSkillDescriptionRunes)
	}
	// 正文為空的 Skill 是一個空承諾：LLM 會看到描述、以為有這個技能可用，取回正文
	// 時卻什麼都拿不到（#20 的 load_skill 走的正是這條路）。
	if strings.TrimSpace(body) == "" {
		return core.SkillMeta{}, fmt.Errorf("Skill %q 的正文為空（frontmatter 之後要寫這份 Skill 實際怎麼做）", ref)
	}

	return core.SkillMeta{Name: fm.Name, Description: fm.Description}, nil
}

// frontmatterDelim 是 frontmatter 的分隔線。
const frontmatterDelim = "---"

// splitFrontmatter 把已正規化換行的文件切成 frontmatter 與正文。文件必須以一行
// `---` 開頭、並有一條 `---` 結束行；兩者之間是 YAML，之後是正文。
func splitFrontmatter(doc string) (front, body string, err error) {
	open := frontmatterDelim + "\n"
	if !strings.HasPrefix(doc, open) {
		return "", "", errors.New("文件必須以一行 --- 開頭")
	}
	rest := doc[len(open):]

	close := "\n" + frontmatterDelim
	idx := strings.Index(rest, close)
	if idx < 0 {
		return "", "", errors.New("找不到結束的 --- 分隔線")
	}
	front = rest[:idx+1] // 保留 YAML 最後一行的換行
	after := rest[idx+len(close):]
	// 結束分隔線必須自成一行：`----` 之類的不算數。
	if after != "" && !strings.HasPrefix(after, "\n") {
		return "", "", errors.New("結束的 --- 分隔線必須自成一行")
	}
	return front, strings.TrimPrefix(after, "\n"), nil
}
