// Workspace 設定檔定價段的載入與轉換測試（ticket #49）。
//
// 這裡驗的是**檔案形狀**（YAML 欄位名對得上、缺席時的行為、展開那一步不弄丟它），
// 算術由 internal/core 的 pricing_test.go 驗——兩層分開，改公式不會弄紅這裡。
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
)

// writeConfigFile 把 yaml 寫成一份設定檔並載入。
func writeConfigFile(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("寫入設定檔: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("載入設定檔: %v", err)
	}
	return cfg
}

const pricingYAML = `providers:
  openrouter:
    api_key: ${TEST_ORYX_PRICE_KEY}
    pricing:
      "anthropic/claude-sonnet-4":
        input: 3
        output: 15
        cached_input: 0.3
`

// loadPricingYAML 寫一份設定檔並載入，回傳錯誤供斷言（成功時錯誤為 nil）。
func loadPricingYAML(t *testing.T, yaml string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("寫入設定檔: %v", err)
	}
	return config.Load(path)
}

// pricingYAMLWith 把一段定價內容包成完整的設定檔。縮排固定六格（provider 之下
// 的 pricing 之下），讓每一格只需要寫自己關心的那幾行。
func pricingYAMLWith(body string) string {
	return "providers:\n  openrouter:\n    api_key: k\n    pricing:\n" + body
}

// TestLoadPricingValidation 釘住定價的 fail fast：**一份寫壞的定價必須在載入時就
// 報錯，不能被當成零價放行**。
//
// 這是整張票最要緊的一條防線。漏寫 input 會讓所有輸入 token 免費、負值會算出負
// 成本、.inf 會讓取整溢位——三者都會在審計表裡留下一個**具體但錯誤**的數字，
// 而錯誤精度比缺值更害（這正是 tool_invocations.token_cost 定案留空的同一條理由）。
// 缺值至少看得出「還沒配置」，一個錯的數字看起來跟對的一模一樣。
func TestLoadPricingValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string // 空字串表示期望載入成功
	}{
		{
			name:    "漏寫 input",
			body:    "      \"m\":\n        output: 15\n",
			wantErr: "input",
		},
		{
			name:    "漏寫 output",
			body:    "      \"m\":\n        input: 3\n",
			wantErr: "output",
		},
		{
			name:    "模型項整個空白",
			body:    "      \"m\":\n",
			wantErr: "input",
		},
		{
			name:    "輸入單價為負",
			body:    "      \"m\":\n        input: -3\n        output: 15\n",
			wantErr: "不得為負",
		},
		{
			name:    "快取單價為負",
			body:    "      \"m\":\n        input: 3\n        output: 15\n        cached_input: -0.3\n",
			wantErr: "不得為負",
		},
		{
			name:    "單價是無限大",
			body:    "      \"m\":\n        input: .inf\n        output: 15\n",
			wantErr: "有限",
		},
		{
			name:    "單價不是數字",
			body:    "      \"m\":\n        input: .nan\n        output: 15\n",
			wantErr: "有限",
		},
		{
			// 上一輪只堵住了大小寫（cachedinput），沒堵住更普通的手滑。未知欄位被
			// yaml 靜默忽略之後 CachedInput 是 nil，看起來與「刻意省略」一模一樣，
			// 快取於是按全價計費——而設定檔上明明寫著 0。
			name:    "選填欄位拼錯一個字母",
			body:    "      \"m\":\n        input: 3\n        output: 15\n        cached_inpt: 0\n",
			wantErr: "cached_inpt",
		},
		{
			// 必填欄位拼錯時，「未知欄位」比「input 必填」更接近使用者犯的錯，
			// 訊息也更好修。
			name:    "必填欄位拼錯一個字母",
			body:    "      \"m\":\n        inputt: 3\n        output: 15\n",
			wantErr: "inputt",
		},
		{
			// 免費模型是真的存在的（OpenRouter 的 :free 系列），所以 0 是合法價格。
			// 這格釘住「必填」不能用「大於零」來實作。
			name: "免費模型：輸入與輸出都是零",
			body: "      \"m\":\n        input: 0\n        output: 0\n",
		},
		{
			name: "明確配置零價的快取",
			body: "      \"m\":\n        input: 3\n        output: 15\n        cached_input: 0\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadPricingYAML(t, pricingYAMLWith(tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("載入失敗: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望載入報錯（含 %q），實際成功: %+v", tt.wantErr, cfg.Providers)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("錯誤訊息 = %q, 期望含 %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestLoadPricingDistinguishesAbsentFromZero 釘住「沒寫」與「寫 0」在型別上分得開。
//
// 兩者若都用零值表達，明確配置免費快取的使用者會被按全價收費（外部審查抓到的）。
func TestLoadPricingDistinguishesAbsentFromZero(t *testing.T) {
	cfg, err := loadPricingYAML(t, pricingYAMLWith(
		"      \"absent\":\n        input: 3\n        output: 15\n"+
			"      \"explicit-zero\":\n        input: 3\n        output: 15\n        cached_input: 0\n"))
	if err != nil {
		t.Fatalf("載入失敗: %v", err)
	}
	pricing := cfg.Providers["openrouter"].Pricing
	if got := pricing["absent"].CachedInput; got != nil {
		t.Errorf("沒寫 cached_input 的模型 CachedInput = %v, 期望 nil", *got)
	}
	got := pricing["explicit-zero"].CachedInput
	if got == nil {
		t.Fatal("明確寫 cached_input: 0 的模型 CachedInput = nil，與「沒寫」分不開")
	}
	if *got != 0 {
		t.Errorf("CachedInput = %v, 期望 0", *got)
	}
}

// TestLoadPricingSection 釘住三個 YAML 欄位名都對得上。
//
// cached_input 這一格不能省：core.ModelPricing 沒有 yaml tag，若定價型別誤用了它，
// yaml.v3 會把 CachedInput 對應成 cachedinput，使用者寫的 cached_input 被靜默忽略
// ——快取 token 於是全額計價，而帳面上完全看不出算錯了。
func TestLoadPricingSection(t *testing.T) {
	cfg := writeConfigFile(t, pricingYAML)

	price, ok := cfg.Providers["openrouter"].Pricing["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatalf("沒有解析出定價: %+v", cfg.Providers["openrouter"])
	}
	if price.Input == nil || *price.Input != 3 {
		t.Errorf("input = %v, 期望 3", price.Input)
	}
	if price.Output == nil || *price.Output != 15 {
		t.Errorf("output = %v, 期望 15", price.Output)
	}
	if price.CachedInput == nil || *price.CachedInput != 0.3 {
		t.Errorf("cached_input = %v, 期望 0.3——欄位名對不上時這裡會是 nil", price.CachedInput)
	}
}

// TestLoadWithoutPricingSection 驗缺定價段的既有 Workspace 照常載入，定價為空。
// 「省略即不計價」這條 AC 的載入側。
func TestLoadWithoutPricingSection(t *testing.T) {
	cfg := writeConfigFile(t, "providers:\n  openrouter:\n    api_key: k\n")
	if got := cfg.Providers["openrouter"].Pricing; len(got) != 0 {
		t.Errorf("Pricing = %v, 期望空——沒寫這段的既有 Workspace 不該憑空長出定價", got)
	}
}

// TestExpandProviderEnvKeepsPricing 釘住憑證展開那一步不會弄丟定價。
//
// ExpandProviderEnv 回傳的是**新的 map**，若它改成逐欄位重建 ProviderConfig 而漏了
// Pricing，定價會在展開後靜默消失：設定檔明明寫了，成本欄位卻永遠是空的。同時驗
// 定價本身不經 resolveEnv——它不是憑證，那條路徑目前只服務 api_key 與 base_url。
func TestExpandProviderEnvKeepsPricing(t *testing.T) {
	t.Setenv("TEST_ORYX_PRICE_KEY", "sk-test-123")
	cfg := writeConfigFile(t, pricingYAML)

	expanded, err := config.ExpandProviderEnv(cfg.Providers)
	if err != nil {
		t.Fatalf("展開 Provider 憑證: %v", err)
	}
	if got := expanded["openrouter"].APIKey; got != "sk-test-123" {
		t.Fatalf("api_key = %q, 期望已展開——這格的前提是展開真的發生了", got)
	}
	price, ok := expanded["openrouter"].Pricing["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatalf("展開之後定價不見了: %+v", expanded["openrouter"])
	}
	if price.Input == nil || *price.Input != 3 || price.CachedInput == nil || *price.CachedInput != 0.3 {
		t.Errorf("展開後定價 = %+v, 期望原樣保留", price)
	}
}

// TestPriceList 驗設定檔的定價段攤平成 core 的定價表之後，算得出成本。
//
// 斷言終點刻意放在 CostMicroUSD 的結果而不是中間那張 map：轉換的價值就是「core 拿
// 得到正確的單價」，比對 map 內容會變成把同一份資料抄兩遍。
func TestPriceList(t *testing.T) {
	cfg := writeConfigFile(t, pricingYAML)
	prices := config.PriceListOf(cfg.Providers)

	got, why := prices.CostMicroUSD("openrouter", "anthropic/claude-sonnet-4",
		core.TokenUsage{PromptTokens: 1000, CachedPromptTokens: 200, CompletionTokens: 500})
	if why != "" {
		t.Fatalf("算不出成本，原因 %q——定價沒有傳達到 core", why)
	}
	if got == nil {
		t.Fatal("CostMicroUSD = 空值, 期望算得出來——定價沒有傳達到 core")
	}
	// 未快取輸入 800×3 + 快取 200×0.3 + 輸出 500×15 = 2400+60+7500
	if *got != 9960 {
		t.Errorf("CostMicroUSD = %d, 期望 9960", *got)
	}
}

// TestPriceListWithoutPricing 驗沒有定價段時攤平出空表，而空表算不出成本。
func TestPriceListWithoutPricing(t *testing.T) {
	cfg := writeConfigFile(t, "providers:\n  openrouter:\n    api_key: k\n")
	prices := config.PriceListOf(cfg.Providers)

	got, why := prices.CostMicroUSD("openrouter", "any-model", core.TokenUsage{PromptTokens: 1000})
	if got != nil {
		t.Errorf("CostMicroUSD = %d, 期望空值", *got)
	}
	if why != core.CostUnavailableNoPricing {
		t.Errorf("原因 = %q, 期望 %q", why, core.CostUnavailableNoPricing)
	}
}
