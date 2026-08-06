package core

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Profile 是 Agent 的完整配置。channels、bootstrap 等欄位隨其模組於後續 ticket 加入。
type Profile struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Identity    Identity    `yaml:"identity"`
	Provider    ProviderRef `yaml:"provider"`
	Tools       []string    `yaml:"tools"`
	Settings    Settings    `yaml:"settings"`
}

// Identity 是 Profile 的身份段。Prompt 為 system prompt 的唯一來源
// （Bootstrap 載入屬後續 ticket，落地時依 ADR-0003 與 SOUL.md 互斥）。
type Identity struct {
	AgentName string `yaml:"agent_name"`
	Prompt    string `yaml:"prompt"`
}

// ProviderRef 以 provider name 引用 Provider 並指定模型與參數。
type ProviderRef struct {
	Name        string  `yaml:"name"`
	Model       string  `yaml:"model"`
	Temperature float32 `yaml:"temperature"`
}

// Settings 是 ReAct 循環的執行參數。
type Settings struct {
	MaxIterations   int `yaml:"max_iterations"`
	MaxHistoryTurns int `yaml:"max_history_turns"`
}

const (
	defaultMaxIterations   = 10
	defaultMaxHistoryTurns = 20
)

// LoadProfile 從 path 讀取並解析 Profile YAML，套用 Settings 預設值並做基礎校驗
// （provider.name、provider.model 必填）。
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取 Profile %s: %w", path, err)
	}

	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("解析 Profile %s: %w", path, err)
	}

	if p.Provider.Name == "" {
		return nil, fmt.Errorf("Profile %s 校驗失敗: provider.name 必填", path)
	}
	if p.Provider.Model == "" {
		return nil, fmt.Errorf("Profile %s 校驗失敗: provider.model 必填", path)
	}

	if p.Settings.MaxIterations == 0 {
		p.Settings.MaxIterations = defaultMaxIterations
	}
	if p.Settings.MaxHistoryTurns == 0 {
		p.Settings.MaxHistoryTurns = defaultMaxHistoryTurns
	}
	return &p, nil
}
