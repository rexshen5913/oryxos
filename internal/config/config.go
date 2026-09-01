package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 Workspace 設定檔的內容：Provider 憑證，加上 Sandbox 三組白名單
// （HTTP 域名、File 路徑、Shell 命令）。
type Config struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
	HTTP      HTTPConfig                `yaml:"http"`
	File      FileConfig                `yaml:"file"`
	Shell     ShellConfig               `yaml:"shell"`
}

// ProviderConfig 是單一 Provider 的連線配置；BaseURL 為空時用 OpenAI 官方端點。
type ProviderConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	// Pricing 是這個 Provider 各模型的單價，鍵為模型名（ticket #49）。
	//
	// **省略即不計價**：沒有這一段的既有 Workspace 照常啟動，審計的成本欄位維持
	// 空值。放在 Provider 段之下而不是頂層，是因為同一個模型名經不同 Provider 可以
	// 有不同報價——CONTEXT.md 的 Provider 是「服務的抽象，不是模型本身」。
	Pricing map[string]ModelPricing `yaml:"pricing"`
}

// ModelPricing 是單一模型的單價在 config.yaml 裡的形狀，三個數字都是**每百萬 token
// 幾美元**，與各家 Provider 定價頁的寫法一致。
//
// 與 core.ModelPricing 分開存在，理由同 mcpServerEntry 與 core.McpServerSpec：檔案
// 形狀的知識（yaml 欄位名）留在本 package，core 只持有共用詞彙。這裡不能省——沒有
// tag 時 yaml.v3 會把 CachedInput 對應成 cachedinput，使用者寫的 cached_input 會被
// 靜默忽略、快取 token 於是全額計價。
//
// **三個欄位都是指標**，因為「沒寫」與「寫 0」必須分得開：免費模型（OpenRouter 的
// :free 系列）的單價真的是 0，快取讀取免費的 Provider 也真的存在。用 float64 的零值
// 表達「沒寫」，會讓漏寫欄位靜默變成免費、也會讓明確配置的免費被當成漏寫。
// 兩者都會在審計表留下一個具體但錯誤的數字，而錯誤精度比缺值更害。
//
// input 與 output 必填、cached_input 選填（省略時按 input 計價），由 validatePricing
// 在載入時檢查——**壞掉的定價必須擋在啟動，不能一路寫成錯誤的成本**。
type ModelPricing struct {
	Input       *float64 `yaml:"input"`
	Output      *float64 `yaml:"output"`
	CachedInput *float64 `yaml:"cached_input"`
}

// pricingFields 是定價段允許出現的欄位，UnmarshalYAML 據此拒絕未知鍵。
var pricingFields = []string{"input", "output", "cached_input"}

// UnmarshalYAML 解析單一模型的定價，並**拒絕未知欄位**。
//
// 為什麼只有這個型別要這麼嚴：yaml 預設靜默忽略認不得的鍵，而定價的三個欄位都
// 是選填形狀（指標），於是 `cached_inpt: 0` 這種手滑會解析成功、CachedInput 是
// nil——與「刻意省略」一模一樣，快取於是按全價計費，而設定檔上明明寫著 0。上一輪
// 只堵住了 yaml 大小寫轉換那個坑（cachedinput），沒堵住更普通的拼錯。
//
// **不對整份設定檔開啟嚴格模式**：那會讓既有 Workspace 因為一個無害的多餘欄位就
// 啟動失敗，而其餘各段沒有這個「拼錯等於省略」的性質——它們的欄位漏了就是零值，
// 效果看得出來。
func (m *ModelPricing) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("定價必須是 %s 三個欄位組成的映射", strings.Join(pricingFields, "／"))
	}
	// yaml 的映射節點把鍵與值交錯放在 Content 裡，所以一次跨兩格取鍵。
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !slices.Contains(pricingFields, key) {
			return fmt.Errorf("未知的欄位 %q（可用：%s）", key, strings.Join(pricingFields, "、"))
		}
	}
	// 換一個沒有 UnmarshalYAML 方法的同形型別來解，否則 Decode 會再呼叫本方法、
	// 無限遞迴。這是 yaml.v3 自訂解碼的標準寫法。
	type plain ModelPricing
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*m = ModelPricing(decoded)
	return nil
}

// HTTPConfig 是 HTTP Tool 的域名白名單，SandboxChecker 據此做執行前校驗。
type HTTPConfig struct {
	AllowedDomains []string `yaml:"allowed_domains"`
}

// FileConfig 是 File Tool 的路徑白名單，SandboxChecker 據此做執行前校驗。
//
// 每條都是**相對 Workspace 根**的路徑：標準化後必須落在其中一條的子樹內。基準固定
// 在 Workspace 根而不是進程當下的工作目錄——否則同一份 config.yaml 在不同目錄下跑
// 會有不同的允許範圍，那是白名單最不該有的性質。
//
// 空清單（含整段省略）即**全部拒絕**，既有 Workspace 因此免遷移。
type FileConfig struct {
	AllowedPaths []string `yaml:"allowed_paths"`
}

// ShellConfig 是 Shell Tool 的命令白名單與超時上限。
//
// 白名單比對的是程式名的**字面完全匹配**，不做萬用字元、不做 basename 正規化；
// 空清單即全部拒絕，語義與上面兩段一致。
//
// 多出來的 timeout_seconds 與 HTTPConfig 不對稱是**刻意的**：命令執行時間的離散度
// 遠大於 HTTP 請求（`go test` 對 `curl`），使用者不該為了跑一個慢命令去改程式碼重編。
type ShellConfig struct {
	AllowedCommands []string `yaml:"allowed_commands"`
	TimeoutSeconds  int      `yaml:"timeout_seconds"`
}

// defaultShellTimeout 是 shell.timeout_seconds 省略時的超時上限，與 HTTP Tool 的
// 請求超時（internal/tool 的 httpRequestTimeout）對齊。
const defaultShellTimeout = 30 * time.Second

// EffectiveTimeout 回傳單次命令的超時上限，零值（含負值）回退預設。形狀沿用
// core.Settings.effectiveMaxIterations：既有 Workspace 的 config.yaml 根本沒有這一欄，
// 要免遷移就不能讓「沒寫」變成啟動失敗。
func (c ShellConfig) EffectiveTimeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return defaultShellTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// envPlaceholder 匹配 ${ENV_VAR} 佔位符。
var envPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load 讀取並解析 Workspace 設定檔。**憑證裡的 ${ENV_VAR} 佔位符不在這裡展開**——
// 需要用到 Provider 的呼叫端自行呼叫 ExpandProviderEnv。
//
// 展開之所以是獨立的一步，理由與 MCP 憑證那條（見 ExpandMcpServerEnv）完全相同：
// 缺一個環境變數只代表「這台機器上還沒設定」，不該擋下與 Provider 無關的事情。
// `oryxos tools` 只是列出可用的 Tool，卻在剛 `oryxos init`、API key 還沒設好的
// Workspace 上失敗——而那正是最可能跑它的時刻（issue #27）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取 Workspace 設定檔 %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 Workspace 設定檔 %s: %w", path, err)
	}
	// 定價在載入時就驗證，與憑證展開刻意分開的理由不同：憑證要看環境變數，一台還沒
	// 設定好的機器不該被擋下（issue #27）；定價是檔案裡的靜態數字，壞了就是壞了，
	// 現在不擋就會一路變成審計表裡一個錯誤的成本。
	if err := validatePricing(cfg.Providers); err != nil {
		return nil, fmt.Errorf("Workspace 設定檔 %s: %w", path, err)
	}
	return &cfg, nil
}

// validatePricing 檢查所有已宣告的定價段：input 與 output 必填，三個值都必須是非負
// 的有限數。**對已宣告的模型 fail fast**——整段 pricing 省略是明確的「不計價」，
// 但寫了一半的定價是錯誤，靜默放行會讓成本欄位有一個看起來正確的數字。
func validatePricing(providers map[string]ProviderConfig) error {
	for name, pc := range providers {
		for model, price := range pc.Pricing {
			where := fmt.Sprintf("providers.%s.pricing[%q]", name, model)
			for _, f := range []struct {
				field    string
				value    *float64
				required bool
			}{
				{"input", price.Input, true},
				{"output", price.Output, true},
				// cached_input 選填：省略代表按 input 計價，不是零。
				{"cached_input", price.CachedInput, false},
			} {
				if f.value == nil {
					if f.required {
						return fmt.Errorf("%s.%s: 必填（省略整段 pricing 才是不計價）", where, f.field)
					}
					continue
				}
				// 非有限值先擋：NaN 與 Inf 過不了下面的比較，而它們取整之後會溢位成
				// 一個任意的整數落進審計表。YAML 的 .inf／.nan 是合法字面量，寫得出來。
				if math.IsNaN(*f.value) || math.IsInf(*f.value, 0) {
					return fmt.Errorf("%s.%s: 必須是有限的數，實際為 %v", where, f.field, *f.value)
				}
				if *f.value < 0 {
					return fmt.Errorf("%s.%s: 不得為負，實際為 %v", where, f.field, *f.value)
				}
			}
		}
	}
	return nil
}

// ExpandProviderEnv 把 Provider 憑證裡的 ${ENV_VAR} 佔位符以環境變數展開，回傳展開後的
// 新 map；引用的環境變數未設定時報清晰錯誤。
//
// 回新 map 而不是就地改：傳進來的那份來自 Load，呼叫端可能還要拿它做別的事（例如校驗
// Profile 引用的 Provider 有沒有配置），就地改會讓「檔案裡寫了什麼」與「展開後是什麼」
// 分不開。形狀沿用 ExpandMcpServerEnv。
func ExpandProviderEnv(providers map[string]ProviderConfig) (map[string]ProviderConfig, error) {
	expanded := make(map[string]ProviderConfig, len(providers))
	for name, pc := range providers {
		var err error
		if pc.APIKey, err = resolveEnv(pc.APIKey); err != nil {
			return nil, fmt.Errorf("providers.%s.api_key: %w", name, err)
		}
		if pc.BaseURL, err = resolveEnv(pc.BaseURL); err != nil {
			return nil, fmt.Errorf("providers.%s.base_url: %w", name, err)
		}
		expanded[name] = pc
	}
	return expanded, nil
}

// resolveEnv 展開 s 中所有 ${ENV_VAR} 佔位符；引用的環境變數未設定時全部列出。
func resolveEnv(s string) (string, error) {
	var missing []string
	resolved := envPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		key := envPlaceholder.FindStringSubmatch(m)[1]
		val, ok := os.LookupEnv(key)
		if !ok {
			missing = append(missing, key)
			return m
		}
		return val
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("環境變數 %s 未設定", strings.Join(missing, "、"))
	}
	return resolved, nil
}
