package memory

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 長期記憶進 LLM 的兩條路徑各有一組截斷規則，但共用兩個前提：上限一律以 rune
// 計，且截斷**只發生在讀取側**——MEMORY.md 檔案本身一個字都不動，被截掉的內容
// 仍可用 recall_memory 檢索回來。這也是「保留開頭／保留最近」這類取捨可以做得
// 這麼直接的原因：沒有任何資料真的被刪掉。

// truncateForInjection 把整份長期記憶裁到 maxInjectRunes 以內供注入 system
// prompt，保留**最近**的內容——舊記憶先被丟掉。截斷點落條目邊界（日期 header
// 或列表項），不落 rune 或條目中間，類比 spec #1 truncateHistory 落 user 訊息
// 邊界的先例。
//
// 這是常態主線：即使 save_memory 把單條限在 maxEntryRunes，累積數十條仍會超標。
// 只有「最新一條自身就超過上限、沒有邊界可切」才走 fallback——保留該條開頭做
// rune 硬切並附自述省略標記，讓截斷從無聲的資料遺失變成可行動的降級。有了單條
// 寫入上限後此分支近乎不可達，但 MEMORY.md 是使用者可直接手改的檔案，仍須守住。
func truncateForInjection(content string) string {
	if utf8.RuneCountInString(content) <= maxInjectRunes {
		return content
	}
	if suffix, ok := suffixFromEntryBoundary(content, maxInjectRunes); ok {
		return suffix
	}

	// 沒有邊界切得下來。兩種情況必須分開，否則會違反「保留最近」這條政策：
	if entry, ok := lastEntry(content); ok {
		// (1) 最新一條**條目**自身就超標：保留它的開頭——條目的開頭是它在講
		// 什麼，掐掉開頭會讓剩下的內容失去主詞（spec #2 定案的 fallback）。
		return hardCutHead(entry, maxInjectRunes, func(omitted int) string {
			return fmt.Sprintf("…（本條記憶過長，已省略 %d 字；完整內容仍在 MEMORY.md，可用 recall_memory 檢索其中片段）", omitted)
		})
	}
	// (2) 整份檔案連一個條目邊界都沒有——使用者把 MEMORY.md 手寫成一段散文。
	// 它不是「一條記憶」，套用一般政策保留**最近**的內容（保留結尾、省略開頭）。
	// 若沿用 (1) 的保留開頭，注入的會是最舊的內容、最近寫的全部消失。
	return hardCutTail(content, maxInjectRunes, func(omitted int) string {
		return fmt.Sprintf("（較舊的長期記憶已省略 %d 字；完整內容仍在 MEMORY.md，可用 recall_memory 檢索其中片段）…\n", omitted)
	})
}

// recallMatches 回傳 content 中命中 query 的行（大小寫不敏感），總量同樣受
// maxInjectRunes 限制——寫入側的單條上限守不住這條路徑，因為 MEMORY.md 是使用者
// 可直接編輯的純文字檔，手改出來的超長行繞得過寫入校驗。
//
// query 切成多個關鍵詞，一行要**全部**命中才算匹配：
//
//   - 拿整串當單一子字串比對是錯的（#14）。LLM 送來的幾乎都是多個詞，而那串連
//     起來不會是任何一行的連續子字串——「Go 語言 Kubernetes」查不到同時寫著
//     「Go 語言」與「Kubernetes」的那行，這個 Tool 於是永遠回「沒有符合的內容」。
//     實際後果比查不到更糟：Agent 會據此推翻自己 system prompt 裡確實有的記憶。
//   - AND 而非 OR：關鍵詞越多，結果應該越窄。OR 會讓「Go Kubernetes 訂單 部署」
//     幾乎把整份記憶倒回去，那不是檢索。
//   - 作用域是**同一行**：關鍵詞分散在不同行不算命中，否則回來的每行各自只沾到
//     一個詞，合起來答非所問。
//
// 分隔符是空白**與標點**，不做中文斷詞（斷詞要引依賴、且離線不可確定化，
// 憲法 4.3 的可確定化精神）。標點必須算分隔：繁中 LLM 送來的關鍵詞幾乎必然帶
// 頓號或逗號，只切空白的話「語言、Kubernetes」會整團拿去比對，#14 就換個形式
// 復發。切得比需要的細會讓匹配略寬（`api_key` 拆成 `api` 與 `key`），但那是
// AND 之下仍然收斂的偏差；切得不夠細則直接回零匹配——後者正是本張要修的失敗。
// 英數字不算標點，所以 `K8s`、`v4` 這種詞不會被拆開。
//
// 超標時保留**最近**的匹配（較新的記憶較可能仍然成立），並附明確的截斷標記，
// 讓 LLM 知道還有更多、可以換更精確的關鍵詞再查。
func recallMatches(content, query string) string {
	// 切完一個詞都不剩時回空，不是「全部匹配」——strings.Contains(x, "") 恆真，
	// 少了這道防線會把整份記憶倒回給 LLM。呼叫端已擋掉空白關鍵詞，這裡是本函式
	// 自己的契約。
	terms := splitTerms(query)
	if len(terms) == 0 {
		return ""
	}

	// 先折疊、去重一次，而不是每行每詞各折一次：掃描成本因此由**相異**詞數決定
	// （重複的詞在 AND 下本來就是多餘的），也省掉 行數×詞數 次的配置。
	folded := foldedTerms(terms)

	var matched []string
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if matchesAll(line, folded) {
			matched = append(matched, line)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	return joinRecentWithinBudget(matched, folded, maxInjectRunes)
}

// splitTerms 把 query 切成關鍵詞：空白與標點都算分隔（見 recallMatches）。
func splitTerms(query string) []string {
	return strings.FieldsFunc(query, isTermSeparator)
}

// isTermSeparator 判斷一個 rune 是不是關鍵詞的分隔符。符號（IsSymbol）也算：
// `+`、`=`、`|` 這類在 Unicode 分類上不是標點，但在關鍵詞裡同樣是分隔而非內容。
func isTermSeparator(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// foldedTerms 把關鍵詞折疊成小寫並去重，保留首次出現的順序（摘錄超長行時的
// 開窗會用到順序無關的涵蓋區間，但穩定順序讓行為可預期、測試好寫）。
func foldedTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		lower := strings.ToLower(term)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, lower)
	}
	return out
}

// matchesAll 判斷一行是否含有全部關鍵詞；terms 必須是已折疊成小寫的形式。
func matchesAll(line string, terms []string) bool {
	folded := strings.ToLower(line)
	for _, term := range terms {
		if !strings.Contains(folded, term) {
			return false
		}
	}
	return true
}

// matchIndex 回傳 needle 在 line 中的 **rune** 位置（大小寫不敏感）；沒有回 -1。
// 摘錄超長匹配行時要靠這個位置決定切哪一段。
//
// 折疊後用 strings.Index 找到 byte offset 再換算成 rune offset。這個換算之所以
// 成立，是因為 strings.ToLower 走的是 strings.Map(unicode.ToLower)——一對一的
// rune 映射：byte 寬度會變（`İ` 2 bytes 變 1 byte、`K` 3 bytes 變 1 byte），
// rune 數不變，所以折疊字串裡的 rune 位置就是原字串裡的 rune 位置。
// 用 strings.Index 而非自己逐位置比對：它有 Rabin-Karp 與 IndexByte 的快路徑，
// 而 MEMORY.md 沒有大小上限、又可手改，自己寫的樸素搜尋在長重複行上是平方級。
func matchIndex(line, needle string) int {
	folded := strings.ToLower(line)
	at := strings.Index(folded, strings.ToLower(needle))
	if at < 0 {
		return -1
	}
	return utf8.RuneCountInString(folded[:at])
}

// hitSpan 回傳涵蓋 line 中全部命中的 rune 區間 [first, last)。全部沒命中時回
// (0, 0)——呼叫端只用它決定開窗位置，退化成從行首開窗是安全的預設。
func hitSpan(line string, terms []string) (first, last int) {
	first, last = -1, -1
	for _, term := range terms {
		at := matchIndex(line, term)
		if at < 0 {
			continue
		}
		if first < 0 || at < first {
			first = at
		}
		if end := at + len([]rune(term)); end > last {
			last = end
		}
	}
	if first < 0 {
		return 0, 0
	}
	return first, last
}

// joinRecentWithinBudget 由新到舊收攏匹配行，總量不超過 budget；有行被丟掉時
// 附截斷標記，標記本身的長度也算進預算。
func joinRecentWithinBudget(matched []string, terms []string, budget int) string {
	if kept := fittingLines(matched, budget); kept == len(matched) {
		return strings.Join(matched, "\n")
	}

	markerFor := func(dropped int) string {
		return fmt.Sprintf("\n（另有 %d 行較舊的匹配未顯示，已達回傳上限；可換更精確的關鍵詞再查）", dropped)
	}
	// 預留的標記長度以「全部被丟掉」估算，確保加上實際標記後不超預算。
	kept := fittingLines(matched, budget-utf8.RuneCountInString(markerFor(len(matched))))
	if kept == 0 {
		// 連最新的一行都放不下——使用者手改出來的超長行。
		return excerptAround(matched[len(matched)-1], terms, budget, len(matched)-1)
	}
	return strings.Join(matched[len(matched)-kept:], "\n") + markerFor(len(matched)-kept)
}

// excerptAround 從超長的匹配行取出**涵蓋命中位置**的片段。固定保留行首是不行的
// ——關鍵詞若落在第 4500 字，回傳的前 4000 字完全不含它，等於回了一段與提問無關
// 的內容，而且換同一個關鍵詞重查還是拿到同一段，使用者無從補救。
//
// 多個關鍵詞時看的是**所有命中的涵蓋區間**，不是單一命中點：區間放得進窗口就
// 整段保留（全部關鍵詞都看得到），放不進才退回最早命中、從它往後展開——後者是
// 有界限的取捨，不是遺漏。以單一命中點開窗是錯的：窗口有一半浪費在命中之前的
// 內容上，會把本來放得下的後段命中裁掉。單一關鍵詞時兩者等價。
//
// 兩端各自標註省略量；dropped 大於 0 時再帶上被丟掉的匹配行數，否則 LLM 只看到
// 一段被切過的內容，不知道還有別的匹配，也就不會想到換更精確的關鍵詞再查。
func excerptAround(line string, terms []string, budget, dropped int) string {
	runes := []rune(line)
	head := func(omitted int) string { return fmt.Sprintf("（前方省略 %d 字）…", omitted) }
	tail := func(omitted int) string {
		if dropped == 0 {
			return fmt.Sprintf("…（後方省略 %d 字）", omitted)
		}
		return fmt.Sprintf("…（後方省略 %d 字；另有 %d 行較舊的匹配未顯示，可換更精確的關鍵詞再查）", omitted, dropped)
	}

	// 兩端標記都以「整行都被省略」估算長度，實際標記必然更短。
	window := budget - utf8.RuneCountInString(head(len(runes))) - utf8.RuneCountInString(tail(len(runes)))
	if window <= 0 {
		return string(runes[:min(len(runes), max(budget, 0))])
	}
	if window >= len(runes) {
		return line
	}

	// 能走到這裡代表這一行全部命中過；沒有命中是理論上到不了的分支，夾到 0 當保險。
	first, last := hitSpan(line, terms)
	start := (first + last) / 2 // 以涵蓋區間為中心
	if last-first > window {
		start = first // 涵蓋不了全部命中：從最早那個往後展開
	} else {
		start -= window / 2
	}
	start = min(max(start, 0), len(runes)-window) // 撞到兩端就往內夾
	end := start + window

	out := string(runes[start:end])
	if start > 0 {
		out = head(start) + out
	}
	if end < len(runes) {
		out += tail(len(runes) - end)
	}
	return out
}

// fittingLines 回傳由新到舊在 budget 內放得下幾行（含行間換行）。
func fittingLines(matched []string, budget int) int {
	total, kept := 0, 0
	for _, line := range slices.Backward(matched) {
		next := utf8.RuneCountInString(line)
		if kept > 0 {
			next++ // 行間換行
		}
		if total+next > budget {
			break
		}
		total += next
		kept++
	}
	return kept
}

// suffixFromEntryBoundary 回傳「起於條目邊界、且不超過 budget」的最長後綴；
// 沒有任何邊界切得下來時回 false（最新一條自身就超標）。
func suffixFromEntryBoundary(content string, budget int) (string, bool) {
	lines := strings.Split(content, "\n")
	total, best := 0, -1
	for i, line := range slices.Backward(lines) {
		total += utf8.RuneCountInString(line)
		if i < len(lines)-1 {
			total++ // 行間換行
		}
		if total > budget {
			break
		}
		if isEntryBoundary(line) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return strings.Join(lines[best:], "\n"), true
}

// lastEntry 回傳最後一個條目邊界到檔尾的內容（＝最新的一條記憶）；整份內容連
// 一個邊界都沒有時回 false——那不是「一條記憶」，呼叫端要用別的策略處理。
func lastEntry(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, line := range slices.Backward(lines) {
		if isEntryBoundary(line) {
			return strings.Join(lines[i:], "\n"), true
		}
	}
	return "", false
}

// isEntryBoundary 判斷一行是不是條目邊界——日期 header 或列表項。截斷點只落在
// 這種行上，一條記憶才不會被攔腰切開。
func isEntryBoundary(line string) bool {
	return isDateHeader(line) || strings.HasPrefix(strings.TrimSpace(line), "- ")
}

// maxEchoRunes 是回填內容裡「引述外部輸入」的長度上限。回填給 LLM 的文字若原樣
// 引述 LLM 自己送來的參數（如檢索關鍵詞），那段長度就不受任何約束了——引述必須
// 自己有上限，否則「進 LLM 的兩條路徑都受限」這個保證會從引述這條縫漏掉。
const maxEchoRunes = 100

// abbreviate 把 s 縮到 max 個 rune 以內，超出時以 … 收尾。省略號本身算進上限
// ——否則回傳長度是 max+1，宣稱的上限就守不住。
func abbreviate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:max(limit-1, 0)]) + "…"
}

// hardCutHead 保留 text 的開頭、硬切到 budget 以內，末尾附上 marker(省略字數)。
// hardCutTail 相反：保留結尾，開頭附標記。兩者預留的標記長度都以「整段都被省略」
// 估算——實際省略量必然更小、標記必然更短，所以結果保證不超 budget。
func hardCutHead(text string, budget int, marker func(omitted int) string) string {
	runes, kept := cutPlan(text, budget, marker)
	if kept >= len(runes) {
		return text
	}
	return string(runes[:kept]) + marker(len(runes)-kept)
}

func hardCutTail(text string, budget int, marker func(omitted int) string) string {
	runes, kept := cutPlan(text, budget, marker)
	if kept >= len(runes) {
		return text
	}
	return marker(len(runes)-kept) + string(runes[len(runes)-kept:])
}

// cutPlan 算出硬切後留得下幾個 rune（已扣掉標記的預留長度）。
func cutPlan(text string, budget int, marker func(omitted int) string) (runes []rune, kept int) {
	runes = []rune(text)
	return runes, max(budget-utf8.RuneCountInString(marker(len(runes))), 0)
}
