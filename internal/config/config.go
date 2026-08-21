package config

import (
	"fmt"
	"os"
	"regexp"
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
	return &cfg, nil
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
