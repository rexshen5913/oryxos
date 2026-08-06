package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是 Workspace 設定檔的內容：Provider 憑證與 HTTP Tool 域名白名單。
type Config struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
	HTTP      HTTPConfig                `yaml:"http"`
}

// ProviderConfig 是單一 Provider 的連線配置；BaseURL 為空時用 OpenAI 官方端點。
type ProviderConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// HTTPConfig 是 HTTP Tool 的域名白名單（SandboxChecker 於後續 ticket 使用）。
type HTTPConfig struct {
	AllowedDomains []string `yaml:"allowed_domains"`
}

// envPlaceholder 匹配 ${ENV_VAR} 佔位符。
var envPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load 讀取並解析 Workspace 設定檔，將 Provider 憑證中的 ${ENV_VAR} 佔位符
// 以環境變數展開；引用的環境變數未設定時報清晰錯誤。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取 Workspace 設定檔 %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 Workspace 設定檔 %s: %w", path, err)
	}

	for name, pc := range cfg.Providers {
		if pc.APIKey, err = resolveEnv(pc.APIKey); err != nil {
			return nil, fmt.Errorf("providers.%s.api_key: %w", name, err)
		}
		if pc.BaseURL, err = resolveEnv(pc.BaseURL); err != nil {
			return nil, fmt.Errorf("providers.%s.base_url: %w", name, err)
		}
		cfg.Providers[name] = pc
	}
	return &cfg, nil
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
