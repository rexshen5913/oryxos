package core

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// 上下文壓縮（ticket #48）：讓讀過的大檔案不在後續每個 iteration 重送一次。
//
// 它與 truncateHistory 是**兩層**，排在它之後：先決定保留哪幾個 turn，再決定每條
// 保留多少內容。兩層互不知情，各自可測——turn 級截斷回答「哪幾輪還算數」，這一層
// 回答「還算數的那幾輪裡，每條留多長」。
//
// 名字用 compact 而不是 compress 是刻意的：這裡只做**遮罩與截斷**，不呼叫 LLM 做
// 語義摘要（語義摘要屬擴展階段，票面明文）。叫 compress 會讓下一個讀的人以為內容
// 被無損地縮起來了，而它其實是被丟掉的。

const (
	// defaultMaxContextRunes 是上下文預算的預設值，以 **rune** 計。
	//
	// 按 rune 不按 byte 是本票的核心約束：中文一字三個位元組，用 byte 會讓中文對話
	// 比英文提早三倍觸發（沿 internal/config 的 maxBootstrapRunes 與 internal/memory
	// 的 maxInjectRunes 同一套計量）。
	//
	// 取 100000 的判準是它與 system prompt 那一整套的比例：Bootstrap 三份各 4000、
	// Skill 段 10000、長期記憶 4000，上界合計約 26000——約佔四分之一，剩下四分之三
	// 留給對話歷史。而票面點名的場景（三個各 200 KB 的檔案）離它有六倍距離，會確實
	// 觸發，不是一個聊備一格的數字。
	defaultMaxContextRunes = 100000

	// contextPerMessageDivisor 決定**單條** Tool 結果的上限：預算除以它。
	//
	// 取 4 沿 internal/memory 的 maxEntryRunes = maxInjectRunes / 4，那裡寫的理由
	// 這裡原樣成立：預算至少要容得下三四條近期結果。給得更大（例如除以 2）會讓
	// 一條超長結果吃掉半個預算，Agent 於是忘記它前兩步做了什麼；給得更小則每條都
	// 被削得只剩梗概，報錯訊息與結論一起消失。
	contextPerMessageDivisor = 4
)

// contextDroppedFormat 是**超出保護區**的 Tool 結果被整條替換後的內容。
//
// 帶上原長度而不只說「已省略」：LLM 據此知道自己錯過的是一段長內容而不是一句話，
// 要用得上就得重新呼叫一次 Tool。自述省略量是本專案既有截斷標記的一貫作法
// （internal/config 的 truncateForInjection、ComposeSkillSection 的 marker）。
const contextDroppedFormat = "[較早的 Tool 結果已省略以控制上下文長度，原長 %d 字；需要的話請重新呼叫該 Tool]"

// contextElisionFormat 是**保護區內**單條過長者中間被挖掉那一段的標記。
const contextElisionFormat = "\n…（中間已省略 %d 字）…\n"

// messageRunes 是一條訊息**送出去時**佔的 rune 數：內容欄位、tool 訊息回應的
// ToolCallID，加上每個 Tool 呼叫的 ID、名稱與參數。
//
// 這幾個欄位就是 internal/provider 的 toOpenAIMessages 會從一條 Message 搬上請求的
// 全部。**判準是「Provider 會不會把這個欄位送出去」，不是「它看起來重不重要」**——
// 憑重要性挑欄位就是漏算的來源（第一版漏了 arguments，第二版又漏了 ToolCallID）。
//
// **為什麼把改不動的欄位也算進來。** 預算要回答的是「這次請求有多大」，不是「我能
// 壓掉多少」。Tool 呼叫的 arguments 由 LLM 產生、長度沒有上界——write_file 要寫的
// 檔案內容就住在裡面——不記帳的話，一次大寫入配上幾條中等長度的結果，帳面完全看不
// 出請求已經超標，壓縮於是整個不啟動。
//
// 這與非 Tool 訊息「自己不動、但佔掉的空間算數」是**同一條規則**：不可壓縮不等於
// 不佔位置。第一版只算 Content，等於把這條規則套在訊息角色上卻沒套在欄位上。
//
// **範圍到訊息為止。** ChatRequest.Tools（Tool 定義）也會被送出去，但它不在訊息序列
// 裡：每個 turn 固定、不隨對話歷史增長，壓縮碰不到它。協議本身的 JSON 結構開銷同理。
// 預算因此是**訊息序列**的預算，不是整個請求的精確大小——這是刻意畫的界，不是遺漏。
func messageRunes(m Message) int {
	n := utf8.RuneCountInString(m.Content) + utf8.RuneCountInString(m.ToolCallID)
	for _, c := range m.ToolCalls {
		n += utf8.RuneCountInString(c.ID) + utf8.RuneCountInString(c.Name) + utf8.RuneCountInString(c.Arguments)
	}
	return n
}

// shorterOf 回傳兩者中較短的那個（以 rune 計），等長時回 original。
//
// 壓縮的每一次替換都要過這道關：**換上去的東西必須真的更短**。佔位說明有四十幾個
// rune，拿它換掉一條很短的舊結果會讓請求變長——那與壓縮的目的正好相反，而且它是
// 合法輸入就到得了的狀態（一條一個字的 Tool 結果落在保護區外）。
func shorterOf(original, replacement string) string {
	if utf8.RuneCountInString(replacement) < utf8.RuneCountInString(original) {
		return replacement
	}
	return original
}

// compactToolResults 把 msgs 壓到 budget（以 rune 計）之內，回傳壓縮後的序列與被壓
// 的條數。總量未超出預算時原樣回傳同一個 slice，一個 byte 都不動。
//
// **走訪方向由新到舊**，這是「久遠的 Tool 結果先被犧牲、最近的保留」的直接翻譯：
// 邊走邊花預算，走到預算用完為止；還沒走到的那些就是「久遠的」。
//
// 每條訊息三種命運：
//
//   - **不是 tool 角色**（system、user、assistant）——一律不動，但長度算進已用預算。
//     它們確實佔掉空間讓更久遠的 Tool 結果活不下來，這是事實，不該裝作沒發生。
//     壓縮只授權動 Tool 結果：使用者說過的話與模型的推理都是不可替代的原文。
//   - **tool 且預算未用完**——在保護區內。單條超過上限則頭尾保留、中間標省略量，
//     否則原樣留著。
//   - **tool 且預算已用完**——整條換成佔位說明。
//
// 系統提示詞排在最舊端，因此是最後才被算到的，**不會害任何一條 Tool 結果被犧牲**。
// 這是刻意的：系統提示詞壓不掉，為它多犧牲幾條 Tool 結果換不到任何空間。
//
// **不就地改動 msgs。** buildMessages 組出來的序列共用 Session 對話歷史的字串，
// 就地改會讓壓縮結果污染持久化的歷史——恢復的 Session 就再也重放不出原樣了。
func compactToolResults(msgs []Message, budget int) ([]Message, int) {
	// 先看塞不塞得下（沿 internal/config 的 truncateHead 判準）。塞得下就回同一個
	// slice、不 clone——「未超出預算時完全不改動訊息」要的是這個，不是「改動後剛好
	// 等於原值」。單條過長也不在這裡處理：整份請求都裝得下時，切它換不到任何空間。
	total := 0
	for _, m := range msgs {
		total += messageRunes(m)
	}
	if total <= budget {
		return msgs, 0
	}

	perMessage := budget / contextPerMessageDivisor
	out := slices.Clone(msgs)
	used, compacted := 0, 0
	for i, m := range slices.Backward(out) {
		if m.Role != RoleTool {
			used += messageRunes(m)
			continue
		}
		n := utf8.RuneCountInString(m.Content)
		switch {
		case used >= budget:
			// 保護區外：整條換掉。**這一分支排在單條上限之前**——一條落在保護區外的
			// 結果就算長度合格也留不住，先問「還在不在保護區」才是「久遠的先被犧牲」。
			out[i].Content = shorterOf(m.Content, fmt.Sprintf(contextDroppedFormat, n))
		case n > perMessage:
			out[i].Content = elideMiddle(m.Content, perMessage)
		default:
			used += messageRunes(m)
			continue
		}
		// 兩個分支都可能因為「換上去的並沒有比較短」而原封不動（見 shorterOf）。
		// **compacted 數的是真的被改過的條數**，不是走進壓縮分支的條數——它會落進
		// 警告日誌，報一個沒發生的降級比不報更糟。
		if out[i].Content != m.Content {
			compacted++
		}
		// 換上去的佔位／標記本身也佔位置，照它的實際長度記帳。低估的話保護區會比
		// 宣告的預算寬，壓縮就達不到它存在的目的。
		used += messageRunes(out[i])
	}
	return out, compacted
}

// elideMiddle 把 s 裁到 limit 個 rune 以內，**保留開頭與結尾**、中間放自述省略量的
// 標記；塞得下就原樣回傳。
//
// 保留頭尾而不是只留開頭（與 internal/config 的 truncateHead 相反）：那邊裁的是
// 使用者手寫的文件，重點都在前面；這邊裁的是 Tool 結果，開頭是結構（JSON 的欄位
// 名、命令的第一行輸出），**結尾常常才是結論或報錯訊息**。只留開頭會把 LLM 最需要
// 的那一段掐掉。
//
// 兩個要點沿 truncateHead 的既有判準，理由那裡已經寫過：先看塞不塞得下再決定要不要
// 預留標記；標記本身算進預算、預留長度以「整份都被省略」估算——實際省略量必然更小、
// 數字位數必然不多於這個估計。
//
// **limit 小到連標記都裝不下時，兩條性質無法同時成立**：要嘛超過上限，要嘛把一段
// 短內容換成更長的標記。選擇不放大——上限是為了省空間才存在的，為了守住它而把請求
// 撐大是本末倒置。那種時候原樣回傳（見 shorterOf），呼叫端記到的長度因此仍是實情。
func elideMiddle(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	marker := func(omitted int) string { return fmt.Sprintf(contextElisionFormat, omitted) }
	kept := max(limit-utf8.RuneCountInString(marker(len(runes))), 0)
	// 對半分，**餘數給開頭**：Tool 結果的開頭帶著結構（JSON 的欄位名、命令輸出的
	// 第一行），少一個 rune 就可能連形狀都認不出來；結尾少一個字通常還讀得懂。
	head := kept - kept/2
	tail := kept / 2
	return shorterOf(s, string(runes[:head])+marker(len(runes)-kept)+string(runes[len(runes)-tail:]))
}
