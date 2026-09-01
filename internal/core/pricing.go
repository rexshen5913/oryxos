package core

import "math"

// ModelPricing 是單一模型的單價，三個數字都是**每百萬 token 幾美元**——與各家
// Provider 定價頁上的寫法一致，使用者照抄即可，不必自己換算。
//
// 型別放在 core 而不是 config：它是 config（解析）與 core（計價）共用的詞彙，位置
// 與理由同 McpServerSpec。**刻意不帶 yaml tag**，檔案形狀的知識留在 internal/config。
type ModelPricing struct {
	// Input 是未命中快取的輸入 token 單價。
	Input float64
	// Output 是輸出 token 單價。
	Output float64
	// CachedInput 是命中快取的輸入 token 單價。**指標，因為 nil 與 0 是兩件事**：
	// nil 代表設定檔沒寫這個欄位，那些 token 於是按 Input 計價（沒有快取折扣的
	// Provider 就是這個情況，算成免費會低估成本）；明確寫 0 則真的是 0——有些
	// Provider 的快取讀取免費，那是合法的價格，不該被當成「沒寫」而按全價收費。
	//
	// 用零值表達「沒寫」是第一版的錯：float64 的零值與使用者寫的 0 解析結果相同，
	// 型別上分不開，於是明確配置的免費快取被按全價計費（外部審查抓到，見下）。
	CachedInput *float64
}

// PriceList 是 Workspace 的定價表：provider name → 模型名 → 單價。
//
// 兩層而不是一層是必要的：CONTEXT.md 定義 Provider 是「服務的抽象，不是模型本身」，
// 同一個模型名在不同 Provider 可以有不同價格（同一顆模型經 OpenRouter 與經原廠的
// 報價就不同），攤平成單層會讓後配置的那筆覆蓋前一筆。
type PriceList map[string]map[string]ModelPricing

// CostUnavailable 說明成本為什麼算不出來；空字串代表算出來了。
//
// **三種原因不能收斂成一個空值**：它們對管理員的處置完全不同——沒定價要去補設定檔、
// 用量矛盾要去看 Provider 回報了什麼、總額超範圍要去檢查單價是不是多打了幾個零。
// 呼叫端據此決定日誌怎麼寫（見 ReActLoop.recordLLMCall）；全部記成「缺定價」會把
// 後兩種情況的管理員送去改一份本來就正確的設定檔。
type CostUnavailable string

const (
	// CostUnavailableNoPricing 是定價表裡沒有這個 Provider 或模型。
	CostUnavailableNoPricing CostUnavailable = "no_pricing"
	// CostUnavailableInvalidUsage 是用量本身不可信：任一項為負，或命中快取的
	// token 多於輸入 token（cached 是 prompt 的子集，這是協議的不變式）。
	CostUnavailableInvalidUsage CostUnavailable = "invalid_usage"
	// CostUnavailableOutOfRange 是總額不是有限數、為負、或超出 int64 可表示的範圍。
	// 定價與用量都在，只是乘積落到了整數表達不了的地方。
	CostUnavailableOutOfRange CostUnavailable = "amount_out_of_range"
)

// CostMicroUSD 回傳這次呼叫的成本，單位是**百萬分之一美元**；算不出來時回 nil，
// 並以第二個回傳值說明原因。
//
// 回指標而不是 (int64, bool) 或裸 int64：呼叫端要原樣把它交給審計落庫，而 NULL 與 0
// 在那裡是兩件不同的事——0 是「這次沒花錢」，NULL 是「沒算」。指標直接對應 SQL 的
// 可空整數，中間不必再翻譯一次。
func (p PriceList) CostMicroUSD(provider, model string, usage TokenUsage) (*int64, CostUnavailable) {
	price, ok := p[provider][model]
	if !ok {
		// 未配置定價、或配置了但沒有這個模型，都走這裡。**不回 0**：那會讓報表
		// 看起來很省，而真相是沒算——「沒算」與「不用錢」必須在資料上分得開。
		return nil, CostUnavailableNoPricing
	}

	// 用量的不變式：三項都非負，且 CachedPromptTokens 是 PromptTokens 的**子集**
	// （協議如此定義）。違反其中任一條代表這筆用量本身不可信，此時**算不出來**。
	//
	// 第一版在這裡把相減夾成 0 當護欄，防的是負成本——防對了症狀，選錯了處置：
	// prompt=100、cached=500 之下夾完仍會拿那個不可信的 500 算出一個成本，那是
	// 用壞掉的資料編一個數字出來。與「沒有用量就落 NULL」是同一條原則，只是這裡
	// 的用量不是缺席而是矛盾（外部審查第二輪抓到）。
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 ||
		usage.CachedPromptTokens < 0 || usage.CachedPromptTokens > usage.PromptTokens {
		return nil, CostUnavailableInvalidUsage
	}

	// PromptTokens 已經包含 CachedPromptTokens，先扣掉才不會把同一批 token 算兩遍
	// （而且是用比較貴的那個單價算第二遍）。上面的不變式保證這裡不會是負數。
	uncachedPrompt := usage.PromptTokens - usage.CachedPromptTokens

	// 沒寫 cached_input 才回退成 Input；明確寫 0 就是 0。判準是**欄位在不在**，
	// 不是它的值是不是零。
	cachedPrice := price.Input
	if price.CachedInput != nil {
		cachedPrice = *price.CachedInput
	}

	// **單位在這裡相消**：單價是「每百萬 token 幾美元」，成本要「百萬分之一美元」，
	// tokens ÷ 1e6 × 單價 × 1e6 = tokens × 單價。所以計價是一次單純的乘法——沒有
	// 除法、沒有中間的縮放，也就沒有那一步的精度損失。這正是選這個單位的理由，
	// 不只是為了避免低於一美分時歸零。
	usd := float64(uncachedPrompt)*price.Input +
		float64(usage.CachedPromptTokens)*cachedPrice +
		float64(usage.CompletionTokens)*price.Output

	// 四捨五入而不是截斷：截斷會系統性低估，每一筆都往下偏、累積起來是單向的誤差。
	rounded := math.Round(usd)

	// **轉型前驗總額。** 單價有限不保證乘積有限：1e308 是合法的有限數，乘上一千個
	// token 就溢位成 +Inf，而 int64(+Inf) 在 Go 裡是未定義行為——實測會把 MaxInt64
	// 寫進審計表，一個徹底編造的數字。寫成 !(在範圍內) 而不是 (超出範圍) 是為了
	// 一併擋掉 NaN：NaN 的任何比較都是 false，正向條件會自然把它判成不可轉。
	//
	// 上界用 float64(math.MaxInt64)，它剛好是 2^63；小於它就保證轉得進 int64。
	if !(rounded >= 0 && rounded < float64(math.MaxInt64)) {
		return nil, CostUnavailableOutOfRange
	}
	cost := int64(rounded)
	return &cost, ""
}
