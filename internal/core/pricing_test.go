// 成本計價純函式的表格驅動測試（ticket #49）。
//
// 這一層與整合層分工明確：**這裡驗算術與缺值語義**（三種 token 各自怎麼計價、沒有
// 定價時回什麼），整合層驗「算出來的數字真的落進了 llm_calls」。
//
// **斷言的期望值在測試裡獨立算出來**，不呼叫生產程式碼的公式：那會變成拿實作驗
// 自己，公式寫錯時兩邊一起錯、斷言照樣成立（理由同 compact_internal_test.go 的
// sentRunes）。所以下面每一格的 want 都是手算的常數，並在註解裡寫出算式。
package core

import (
	"math"
	"testing"
)

// 測試用單價。取這三個數字是為了讓手算的期望值一眼看得出來源：輸入 3、輸出 15
// 是常見的五倍關係，快取輸入 0.3 是輸入的十分之一，三者互不整除因此加總後分得開
// ——若公式把某一項乘到另一項的單價上，總額會落在不同的數。
var testPricing = ModelPricing{Input: 3, Output: 15, CachedInput: float64Ptr(0.3)}

// testPrices 是一份最小定價表，只認得一個 Provider 的一個模型。未知 Provider 與
// 未知模型兩條負向路徑都靠它驗。
var testPrices = PriceList{
	"openrouter": {
		"anthropic/claude-sonnet-4": testPricing,
		// 沒有快取單價的模型，驗零值回退那一格。
		"no-cache-price": {Input: 3, Output: 15},
		// 單價帶半個單位，讓「四捨五入而非截斷」驗得出來。其餘各格的期望值都是
		// 整數，兩種取整方式結果相同——突變測試揭露了這個缺口。
		"half-unit-price": {Input: 0.5, Output: 0.5},
		// **明確**配置成零的快取單價（有些 Provider 的快取讀取免費）。與上面那個
		// 沒寫 cached_input 的模型構成對照：兩者的 CachedInput 若都用零值表達就
		// 分不開，明確配置的免費快取會被按全價收費。
		"free-cache": {Input: 3, Output: 15, CachedInput: float64Ptr(0)},
		// 有限但大到會讓乘積溢位的單價。config 的驗證擋得掉 .inf 與負值，擋不掉
		// 這個——它每一項都是合法的有限數。
		"astronomical": {Input: 1e308, Output: 1e308},
		// 輸入免費、快取要錢的（人為）組合。它讓「cached 多於 prompt」算出來的是
		// **正的**成本，因而繞得過總額的下界檢查——這樣子集不變式才有獨立的證據。
		"free-input": {Input: 0, Output: 15, CachedInput: float64Ptr(0.3)},
		// 單價為無限大。config 的驗證擋得掉它，但 CostMicroUSD 是 exported，手組的
		// PriceList 送得進來；0 個 token 乘上它會得到 NaN。
		"infinite-price": {Input: math.Inf(1), Output: 15},
	},
}

// float64Ptr 讓定價表寫得出「明確配置這個價格」與「沒寫這個欄位」的差別。
func float64Ptr(f float64) *float64 { return &f }

func TestPriceListCostMicroUSD(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		usage    TokenUsage
		want     *int64 // nil 代表期望「算不出來」
	}{
		{
			name:     "三種 token 各自計價後加總",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// 未快取輸入 1000-200=800，800×3 + 200×0.3 + 500×15 = 2400+60+7500
			usage: TokenUsage{PromptTokens: 1000, CachedPromptTokens: 200, CompletionTokens: 500},
			want:  int64Ptr(9960),
		},
		{
			name:     "沒有快取命中時輸入全額計價",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// 1000×3 + 500×15 = 3000+7500
			usage: TokenUsage{PromptTokens: 1000, CompletionTokens: 500},
			want:  int64Ptr(10500),
		},
		{
			name:     "快取 token 不重複計價",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// prompt_tokens 已經包含 cached_tokens，全額計價再加快取那筆會變成 3240。
			// 未快取輸入 1000-800=200，200×3 + 800×0.3 = 600+240
			usage: TokenUsage{PromptTokens: 1000, CachedPromptTokens: 800},
			want:  int64Ptr(840),
		},
		{
			name:     "低於一美分的呼叫不歸零",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// 1000×3 = 3000 微美元 = 0.003 美元。這格直接釘住單位選擇：改回存
			// 美元的整數會得到 0，改回存美分也是 0。
			usage: TokenUsage{PromptTokens: 1000},
			want:  int64Ptr(3000),
		},
		{
			name:     "有定價但零用量算得出來，是 0 不是空值",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// 「沒花錢」與「沒算」是兩件事：這格是前者，資料上必須是 0。
			usage: TokenUsage{},
			want:  int64Ptr(0),
		},
		{
			name:     "省略快取單價時快取 token 以輸入單價計價",
			provider: "openrouter",
			model:    "no-cache-price",
			// 沒有快取折扣的 Provider 就是這個情況。算成免費會低估成本，而
			// 「看起來很省但其實沒算對」正是這張票要消滅的失真。
			// 1000×3 = 3000（800 個快取 token 也按 3 計）
			usage: TokenUsage{PromptTokens: 1000, CachedPromptTokens: 800},
			want:  int64Ptr(3000),
		},
		{
			name:     "明確配置的零價快取真的免費，不回退成輸入單價",
			provider: "openrouter",
			model:    "free-cache",
			// 未快取輸入 1000-800=200，200×3 + 800×0 = 600。
			// 若把「明確寫 0」誤判成「沒寫」而回退，會得到 3000——使用者配置了
			// 免費快取卻被按全價收費。
			usage: TokenUsage{PromptTokens: 1000, CachedPromptTokens: 800},
			want:  int64Ptr(600),
		},
		{
			name:     "快取多於輸入是不可信的用量，算不出來",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// cached 是 prompt 的**子集**，這是協議的不變式；反過來代表這筆用量
			// 本身就不可信。第一版在這裡把未快取輸入夾成 0、拿那個 500 算出 150
			// 微美元——那是**用不可信的資料編一個成本出來**，與「把不知道寫成數字」
			// 是同一個錯（外部審查第二輪抓到）。
			usage: TokenUsage{PromptTokens: 100, CachedPromptTokens: 500},
			want:  nil,
		},
		{
			name:     "負的輸出 token 算不出來",
			provider: "openrouter",
			model:    "anthropic/claude-sonnet-4",
			// 這格刻意讓 cached(0) 不大於 prompt(100)，子集那條檢查因此不觸發，
			// 只有「用量不得為負」擋得住它。多道檢查互相覆蓋時，要這樣設計輸入才
			// 分得出是哪一道在起作用（突變測試揭露的）。
			usage: TokenUsage{PromptTokens: 100, CompletionTokens: -10},
			want:  nil,
		},
		{
			name:     "快取多於輸入且成本為正時仍算不出來",
			provider: "openrouter",
			model:    "free-input",
			// 上一格的鏡像：輸入免費，所以就算 uncached 是負的（100-500），乘出來
			// 仍是正的 150——繞過了總額的下界檢查。只有子集不變式擋得住這格。
			usage: TokenUsage{PromptTokens: 100, CachedPromptTokens: 500},
			want:  nil,
		},
		{
			name:     "單價大到讓乘積溢位時算不出來",
			provider: "openrouter",
			model:    "astronomical",
			// 1e308 通過了「有限」的檢查，乘上 1000 個 token 卻溢位成 +Inf——而
			// int64(math.Round(+Inf)) 在 Go 裡是未定義行為，會把一個任意整數寫進
			// 審計表。單價有限不保證乘積有限，要在轉型前驗總額。
			usage: TokenUsage{PromptTokens: 1000},
			want:  nil,
		},
		{
			name:     "小數成本四捨五入而不是截斷",
			provider: "openrouter",
			model:    "half-unit-price",
			// 3×0.5 = 1.5。截斷會得到 1——每一筆都往下偏，累積起來是單向的誤差，
			// 而成本報表的低估比高估更難察覺。
			usage: TokenUsage{PromptTokens: 3},
			want:  int64Ptr(2),
		},
		{
			name:     "總額是 NaN 時算不出來",
			provider: "openrouter",
			model:    "infinite-price",
			// 0 × Inf = NaN。NaN 的任何比較都是 false，所以檢查必須寫成「不在合法
			// 範圍內就拒絕」；寫成「超出上界才拒絕」會讓 NaN 一路走到 int64() 那個
			// 未定義行為。這格釘住那個正向條件的寫法。
			usage: TokenUsage{},
			want:  nil,
		},
		{
			name:     "未知模型算不出來",
			provider: "openrouter",
			model:    "openai/gpt-4o",
			usage:    TokenUsage{PromptTokens: 1000, CompletionTokens: 500},
			want:     nil,
		},
		{
			name:     "未知 Provider 算不出來",
			provider: "openai",
			model:    "anthropic/claude-sonnet-4",
			usage:    TokenUsage{PromptTokens: 1000, CompletionTokens: 500},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 這張表驗的是算術；「為什麼算不出來」由 TestPriceListCostUnavailableWhy
			// 專門驗，兩者分開才不必在每一格重複填同一個原因。
			got, _ := testPrices.CostMicroUSD(tt.provider, tt.model, tt.usage)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("CostMicroUSD = %d, 期望空值（沒有定價就不該算出數字）", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("CostMicroUSD = 空值, 期望 %d", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("CostMicroUSD = %d, 期望 %d", *got, *tt.want)
			}
		})
	}
}

// TestPriceListCostMicroUSDEmptyList 釘住「整份定價表缺席」與「表裡沒有這個模型」
// 走同一條路：都回空值。缺定價段的既有 Workspace 走的正是這一格。
func TestPriceListCostMicroUSDEmptyList(t *testing.T) {
	for _, prices := range []PriceList{nil, {}, {"openrouter": nil}, {"openrouter": {}}} {
		if got, _ := prices.CostMicroUSD("openrouter", "anthropic/claude-sonnet-4",
			TokenUsage{PromptTokens: 1000}); got != nil {
			t.Errorf("PriceList(%v).CostMicroUSD = %d, 期望空值", prices, *got)
		}
	}
}

// int64Ptr 讓表格裡寫得出「期望這個數字」與「期望空值」的差別。
func int64Ptr(n int64) *int64 { return &n }

// TestPriceListCostUnavailableWhy 釘住「算不出來」要說得出**為什麼**。
//
// 三種原因對管理員的處置完全不同：沒定價要去補設定檔、用量矛盾要去看 Provider、
// 總額超範圍要去檢查單價是不是多打了幾個零。全部回同一個空值、由呼叫端一律記成
// 「缺定價」，會把後兩種情況的管理員送去改一份本來就正確的設定檔（外部審查第三輪
// 抓到——第一輪修過「缺用量不落缺定價警告」，當時只修了缺席那條，沒修矛盾那條）。
func TestPriceListCostUnavailableWhy(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		usage   TokenUsage
		wantWhy CostUnavailable
	}{
		{
			name:    "算得出來時沒有原因",
			model:   "anthropic/claude-sonnet-4",
			usage:   TokenUsage{PromptTokens: 1000},
			wantWhy: "",
		},
		{
			name:    "定價表裡沒有這個模型",
			model:   "openai/gpt-4o",
			usage:   TokenUsage{PromptTokens: 1000},
			wantWhy: CostUnavailableNoPricing,
		},
		{
			name:    "快取多於輸入",
			model:   "anthropic/claude-sonnet-4",
			usage:   TokenUsage{PromptTokens: 100, CachedPromptTokens: 500},
			wantWhy: CostUnavailableInvalidUsage,
		},
		{
			name:    "用量為負",
			model:   "anthropic/claude-sonnet-4",
			usage:   TokenUsage{PromptTokens: 100, CompletionTokens: -10},
			wantWhy: CostUnavailableInvalidUsage,
		},
		{
			name:    "總額溢位",
			model:   "astronomical",
			usage:   TokenUsage{PromptTokens: 1000},
			wantWhy: CostUnavailableOutOfRange,
		},
		{
			name:    "總額是 NaN",
			model:   "infinite-price",
			usage:   TokenUsage{},
			wantWhy: CostUnavailableOutOfRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost, why := testPrices.CostMicroUSD("openrouter", tt.model, tt.usage)
			if why != tt.wantWhy {
				t.Errorf("原因 = %q, 期望 %q", why, tt.wantWhy)
			}
			// 原因與結果必須一致：有原因就沒有數字，沒原因就有數字。
			if (why == "") != (cost != nil) {
				t.Errorf("原因 %q 與結果 %v 不一致", why, cost)
			}
		})
	}
}
