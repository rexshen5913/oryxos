package config

import (
	"fmt"
	"unicode/utf8"
)

// maxBootstrapRunes 是**每一份** Bootstrap 檔案進 system prompt 的長度上限。
//
// 三檔各自獨立計算、不共用預算：它們是三份語義不同的文件（專案怎麼做事／這個人
// 偏好什麼／Agent 是誰），讓其中一份把另外兩份擠掉會讓注入結果取決於讀取順序。
//
// 數字沿用長期記憶的 maxInjectRunes（spec #2）：好記、有先例、與既有的注入上限
// 共用判準。以 **rune** 計而非 byte——中文一字一 rune，用 byte 會讓中文體感縮水到
// 三分之一，也可能切壞 UTF-8。取常數、不開配置欄位（YAGNI）。
//
// 這條上限存在的理由是成本：Bootstrap 每個 turn 都重新送一次，一份幾萬字的
// AGENTS.md 會在使用者毫無察覺的情況下每句話都燒一次錢。
const maxBootstrapRunes = 4000

// truncateForInjection 把單份 Bootstrap 檔案裁到 maxBootstrapRunes 以內供注入
// system prompt，保留**開頭**、末尾附自述省略量的標記。
//
// 保留開頭而不是結尾（與長期記憶相反，那邊保留最近的記憶）：Bootstrap 是使用者
// 手寫的靜態文件，沒有「新舊」這個維度，重點在開頭——標題與主要慣例都寫在前面，
// 掐掉開頭會讓剩下的內容失去主詞。
//
// 截斷只發生在**讀取側**：磁碟上的檔案一個 byte 都不動，使用者寫的東西不會因為
// OryxOS 讀了它而被裁掉。標記讓截斷從無聲的資料遺失變成看得見的降級——LLM 與
// 使用者都該知道自己看到的是一份被裁過的文件。
func truncateForInjection(name, content string) string {
	return truncateHead(content, maxBootstrapRunes, func(omitted int) string {
		return fmt.Sprintf("\n\n…（%s 超過 %d 字上限，已省略結尾 %d 字；此處只注入開頭，完整內容仍在 Workspace 的 %s）",
			name, maxBootstrapRunes, omitted, name)
	})
}

// truncateHead 把 content 裁到 limit 以內，保留**開頭**、末尾附 marker(省略量)；
// 塞得下就原樣回傳。
//
// 本 package 有兩個保留開頭、切行邊界的截斷來源——Bootstrap 三檔（每份 4000 rune）
// 與 load_skill 回填的 Skill 正文（10000 rune）。政策相同、只有上限與措辭不同，
// 所以共用這段機制；各自的上限與標記由呼叫端給。
//
// （`internal/memory` 那份**不**共用：它切條目邊界、保留結尾，是不同的政策。
// 真正相同的只有下面這幾行預算算術，把它從 spec #2 已測過的程式碼裡搬出來，
// 風險大於收益。）
//
// 兩個要點：
//
//   - **先看塞不塞得下，再決定要不要預留標記。** 無條件預留會讓宣告的上限縮水，
//     一組本來裝得下的內容會被判成溢出、白白裁掉一截還發出假警示。
//   - **標記本身算進預算**，預留長度以「整份都被省略」估算——實際省略量必然更小、
//     數字位數必然不多於這個估計，所以結果保證不超上限（沿 internal/memory 的
//     cutPlan 判準）。不這樣做的話回傳長度會是「上限＋標記」，宣稱的上限守不住。
func truncateHead(content string, limit int, marker func(omitted int) string) string {
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	budget := max(limit-utf8.RuneCountInString(marker(len(runes))), 0)
	kept := keepWholeLines(runes, budget)
	return string(runes[:kept]) + marker(len(runes)-kept)
}

// keepWholeLines 回傳「不超過 budget、且落在行邊界」的保留長度：從 budget 往回找
// 最近的換行，讓截斷點不會把一行從中間切開（類比 spec #1 truncateHistory 落 user
// 訊息邊界的先例）。
//
// 兩種情況退回 budget 硬切：
//
//   - budget 之前完全沒有換行——使用者把整份文件寫成不換行的一大段，沒有邊界可切。
//   - 最近的換行退得太遠，保留量不到預算的一半。跨過 budget 的那一行既然長到超過
//     預算的一半，它本身就是一份**超長的行**；照 internal/memory 對超長條目的既有
//     取捨（truncate.go 的分支 (1)），寧可切進它，也不要讓這一層近乎空白。
//
// 第二條是審查揪出來的：軟換行的 Markdown（短標題後接一個沒有硬換行的長段落，中文
// 文件的常見形態）會讓「退回最近的行邊界」退到標題那一行，一份四萬字的 AGENTS.md
// 只注入八十幾個 rune——幾乎只剩標記本身。那比超量更糟，因為使用者看到的是「檔案
// 有被讀、標記也在」，內容卻沒到 LLM 手上，是無聲的失敗。
func keepWholeLines(runes []rune, budget int) int {
	if budget >= len(runes) {
		return len(runes)
	}
	for i := budget; i > 0; i-- {
		if runes[i-1] != '\n' {
			continue
		}
		// 往回掃到的第一個換行就是最大的可用邊界；它都不夠格的話，更早的更不夠。
		if i < budget/2 {
			break
		}
		return i
	}
	return budget
}
