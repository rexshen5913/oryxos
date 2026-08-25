package core

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// SkillMeta 是漸進揭露**第一層**要注入 system prompt 的東西：只有 name 與
// description，**正文不在其中**。
//
// 這個型別的欄位就是那條省 token 的前提：常駐在 prompt 裡的體積與 Skill 的**數量**
// 成正比、與 Skill 的**內容長度**無關，所以裝十份 Skill 也不會把 prompt 撐爆。
// 正文由 Agent 判斷相關時經 load_skill 取回、以 tool 訊息回填（第二層，#20）——
// 那條路徑不經過這裡，日後也不該有人往這個結構加 Body 欄位。
type SkillMeta struct {
	Name        string
	Description string
}

const (
	// maxSkillNameRunes 是 name 的長度上限（agentskills.io 標準）。標準原文寫
	// 「字元」，而 name 只允許小寫英數與連字號——那個字集裡 rune 與 byte 等價，
	// 所以這個數字沒有解讀歧義。
	maxSkillNameRunes = 64
	// MaxSkillDescriptionRunes 是 description 的長度上限（agentskills.io 標準）。
	// 標準原文同樣寫「字元」但未定義是 byte 還是 code point；本專案一律以 rune 計
	// （沿 spec #2 的判準），對 ASCII 等價、對中文較寬鬆，不會低估。
	MaxSkillDescriptionRunes = 1024
	// MaxSkillBodyRunes 是 load_skill 單次回填的正文上限（漸進揭露第二層）。
	//
	// 回填走的是 tool 訊息，同樣留在對話歷史裡、同樣每個 turn 隨歷史重送，所以一樣
	// 要有上限——「按需載入」省掉的是**沒用到的** Skill，不是用到的那份的長度。
	// 與 Skill 段取同一個數字：一份寫得完整的任務說明與一份二十來個 Skill 的清單，
	// 量級相當。
	MaxSkillBodyRunes = 10000
	// MaxSkillSectionRunes 是 Skill 段（全部 name ＋ description 合計）的上限。
	//
	// 取 10000 是因為這一層裝的是 N 份 Skill 的描述：以典型描述約 500 rune 計容得下
	// 二十份，即使每份都寫到標準上限（1024）也還有九份。與 Bootstrap 每份 4000 的
	// 判準不同是刻意的——那是「一份文件」的預算，這是「一份清單」的預算。
	MaxSkillSectionRunes = 10000
)

// LoadSkillToolName 是漸進揭露第二層那個內建 Tool 的名字。
//
// 定義在 core 而不是 internal/tool：core 需要它才能檢查「Skill 段承諾的那個 Tool
// 真的在這個 Agent 的工具清單裡」（見 ReActLoop.Run），而 core 不能反向 import
// internal/tool。與 Bootstrap 檔名常數、Skill 名稱規則同一個位置、同一個理由——
// 跨 package 共用的**詞彙**放 core，避免兩邊各寫一份字串。
const LoadSkillToolName = "load_skill"

// skillSectionIntro 是 Skill 段注入時標明來源的引言。措辭不進測試斷言。
//
// 引言承諾「可以取回正文」是有前提的：load_skill 必須真的在這個 Agent 的可用工具
// 裡。組裝點以「skills 非空 → 自動加入 load_skill」保證了這一點（見
// cmd/oryxos/chat.go 的 autoIncludedTools）——沒有那條推導的話，這段話會叫 LLM 去找
// 一個工具清單裡不存在的 Tool，然後要嘛告訴使用者載不到、要嘛拿描述硬編出步驟。
//
// 這個前提由 ReActLoop.Run 每個 turn 檢查一次並在違反時落警示日誌：承諾寫在這裡、
// 前提卻由**別的 package 的組裝點**維護，中間沒有東西盯著的話，日後多一個組裝點
// （例如 Web Service）忘了那條推導就會安靜地重現同一個失敗形態。
const skillSectionIntro = "以下是你可以使用的 Skill（可複用的任務說明，由使用者手寫）。" +
	"這裡只列出名稱與用途；判斷某份與當前任務相關時，用 " + LoadSkillToolName + " 取回它的正文再照著做："

// newlineCollapser 把描述裡的換行折成空白。
//
// Skill 段是「一行一份」的清單，描述若帶換行就能**偽造出不存在的條目**——一份寫成
// YAML block scalar 的描述裡放一行 `- ghost-skill：…`，LLM 就會看到一份根本載不到的
// Skill。折疊而不是拒絕：標準沒有禁止多行 description（block scalar 是合法寫法），
// 拒絕會擋掉在別處跑得動的 Skill，與本專案對 frontmatter 選填欄位的判準一致。
//
// 折疊在**組裝端**而不是解析端：一行一份是這個段落的格式契約，由定義格式的人負責
// 守住，這樣不論 SkillMeta 從哪裡來都成立。
var newlineCollapser = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// ValidateSkillName 校驗一個 Skill 名稱是否符合 agentskills.io 的格式約束：
// 不得為空、至多 64 字元、只允許小寫英數與連字號、不得以連字號開頭或結尾、
// 不得連續兩個連字號。
//
// 這組規則在本專案有**兩個**用途：驗 SKILL.md frontmatter 的 name，以及驗 Profile
// skills 欄位的值。後者順帶讓路徑逃逸**結構上不可能**——一個只允許小寫英數與連字號
// 的字串構不出 `../`、斜線或絕對路徑，不必再立一份「危險字元」黑名單（黑名單總有
// 漏的，白名單沒有）。os.Root 仍是第二道防線，兩者不互相取代。
//
// 定義在 core：這是**使用者寫在 Profile 裡的欄位值**的詞彙，與 Bootstrap 檔名常數
// 同一個位置、同一個理由（單一來源，避免兩個 package 各寫一份規則）。
func ValidateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("Skill 名稱不得為空")
	}
	if n := utf8.RuneCountInString(name); n > maxSkillNameRunes {
		return fmt.Errorf("Skill 名稱 %q 長 %d 字元，超過上限 %d", name, n, maxSkillNameRunes)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("Skill 名稱 %q 不得以連字號開頭或結尾", name)
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("Skill 名稱 %q 不得連續兩個連字號", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return fmt.Errorf("Skill 名稱 %q 含不合法字元 %q（只允許小寫英數與連字號）", name, string(r))
	}
	return nil
}

// SkillRefs 回傳這個 Profile 引用的 Skill 名單，並校驗每個值合法且沒有重複。
//
// 校驗在**每個 turn** 的入口重做一次，不是只在 LoadProfile：不是每個 Profile 都經過
// LoadProfile——core 自己的測試全是手組的，日後 ProfileRegistry 也是。少了這道，
// `skills: [a, a]` 會把同一份 Skill 注入兩次、吃掉預算，甚至把尾端其他 Skill 擠出
// 視野。與 BootstrapSelection 對 bootstrap 欄位做的是同一件事，兩個欄位的繞過路徑
// 因此一致（#17 為 bootstrap 補過同一道，本張補上 skills 這半邊）。
func (p *Profile) SkillRefs() ([]string, error) {
	if err := validateSkills(p.Skills); err != nil {
		return nil, err
	}
	return p.Skills, nil
}

// validateSkills 校驗 Profile skills 欄位的每個值都是合法的 Skill 名稱、且沒有重複。
// 兩者都是設定筆誤，一律報錯不靜默（沿 Registry.Subset 對 tools、validateBootstrap
// 對 bootstrap 的既有語義）。
//
// 名稱合法性擋在讀檔之前，路徑逃逸因此構不出來（見 ValidateSkillName）。
func validateSkills(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := ValidateSkillName(name); err != nil {
			return fmt.Errorf("skills 引用的 %q 不是合法的 Skill 名稱: %w", name, err)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("skills 重複列出 %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// ComposeSkillSection 把每份 Skill 的 name 與 description 拼成 system prompt 的
// Skill 段，回傳段落與**被丟棄的份數**。沒有 Skill 時回空字串、不留空標題。
//
// 段落順序等於 Profile 的宣告順序：使用者列在前面的先進，被丟掉的一定是尾端那幾份。
// 順序若不穩定，「哪幾份會消失」就變成碰運氣。
//
// **兩種破法要分開看，後果不同。** 改用 `map` 迭代——Go 規格對 map 的迭代順序只說
// **未指定、且不保證兩次迭代之間相同**（gc 實際會打亂，但那是實作細節，不是規格承諾）
// ——除了上面那個問題，還連帶違反 composeSystemPrompt 那條**整條鏈路**的位元組級確定
// 不變式：來源沒變、前綴卻可能變，在有前綴快取的 Provider 上會靜默失效。注意違反的是
// 鏈路層那條，不是該函式自己的純函式不變式——skillSection 變了，它收到的就是不同的
// 輸入，局部那條攔不住上游。理由與守它的測試都寫在該函式的註解裡。
//
// 改成按名字**排序**則仍然是確定的、**不影響快取**，壞掉的只有「宣告順序」這條契約
// 本身：使用者列在前面的不再先進，被丟掉的也不再是尾端那幾份。前者是兩個問題，
// 後者是一個，別混為一談。
//
// 超過 MaxSkillSectionRunes 時截斷點落**單份 Skill 邊界**，不切半份描述——切一半的
// 描述比整份不出現更糟：LLM 會以為自己知道那份 Skill 何時該用，實際上拿到的是殘缺的
// 觸發條件。
//
// dropped 大於 0 時段落末尾附自述丟棄份數的標記，且標記長度算進預算（沿
// internal/config 對 Bootstrap 的同一判準：以最壞情況估算，保證結果不超上限）。
// 但標記**不是**唯一的告知管道——被丟掉的是整份 Skill 從 LLM 視野消失，組裝點
// 另在啟動時發一次可見警示（見 cmd/oryxos/chat.go）。
func ComposeSkillSection(skills []SkillMeta) (string, int) {
	if len(skills) == 0 {
		return "", 0
	}

	entries := make([]string, len(skills))
	full := utf8.RuneCountInString(skillSectionIntro)
	for i, s := range skills {
		entries[i] = "\n\n- " + s.Name + "：" + newlineCollapser.Replace(s.Description)
		full += utf8.RuneCountInString(entries[i])
	}

	// **先看塞不塞得下，再決定要不要預留標記。** 無條件預留會讓宣告的上限縮水
	// （標記約 83 rune，10000 就變成 9917），一組總長 9985 的合法設定會被判成溢出、
	// 丟掉一份 Skill，還對使用者發出「請減少 skills」的假警示——假警報加上真實的
	// 資料遺失。標記只在真的要截斷時才存在，預算就該照這個事實算。
	//
	// 這是 internal/config 對 Bootstrap 截斷的同一個形狀（先 `len <= 上限` 就原樣
	// 回傳，再進截斷路徑）。
	if full <= MaxSkillSectionRunes {
		return skillSectionIntro + strings.Join(entries, ""), 0
	}

	marker := func(dropped int) string {
		return fmt.Sprintf("\n\n…（另有 %d 份 Skill 因 Skill 段超過 %d 字上限而未列出；"+
			"請減少 Profile 的 skills 或精簡各份 description）", dropped, MaxSkillSectionRunes)
	}
	// 確實溢出了才預留。長度以「全部都被丟棄」估算——實際丟棄數必然更小、位數不多於
	// 這個估計，所以結果保證不超上限。
	budget := max(MaxSkillSectionRunes-utf8.RuneCountInString(marker(len(skills))), 0)

	var b strings.Builder
	b.WriteString(skillSectionIntro)
	used := utf8.RuneCountInString(skillSectionIntro)

	kept := 0
	for i, entry := range entries {
		n := utf8.RuneCountInString(entry)
		if used+n > budget {
			break // 落單份邊界：塞不下就整份不進，不切半份描述
		}
		b.WriteString(entry)
		used += n
		kept = i + 1
	}

	// 一份都塞不下時整段略過：只有引言的 Skill 段是一個空承諾，LLM 會以為自己有
	// 技能可用卻看不到任何一份。這個路徑要走到，得是單份描述就吃光整個預算。
	if kept == 0 {
		return "", len(skills)
	}
	// 走到這裡 dropped 必然大於 0，所以標記無條件寫：full 已經超過上限，而 budget
	// 又比上限更小，全部塞得下與這個前提矛盾。不加 `if dropped > 0` 的守衛是刻意的
	// ——那條分支在早退存在的前提下不可達，而不可達的守衛既測不到、又會讓讀者以為
	// 這裡真的可能不截斷。守住它的是上面那個早退，不是這裡的第二道判斷。
	dropped := len(skills) - kept
	b.WriteString(marker(dropped))
	return b.String(), dropped
}
