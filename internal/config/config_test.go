package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		check   func(t *testing.T, cfg *config.Config)
		wantErr string // 空字串表示期望成功；否則錯誤訊息須含此子串
	}{
		{
			// **Load 刻意不展開佔位符**，展開是 ExpandProviderEnv 的事（issue #27）。
			// 缺一個環境變數只代表「這台機器上還沒設定」，不該擋下與 Provider 無關的
			// 事情——`oryxos tools` 只是列出可用的 Tool，卻會在剛 init、key 還沒設好的
			// Workspace 上失敗，而那正是最可能跑它的時刻。
			name: "佔位符原樣留著，不在載入時展開",
			yaml: "providers:\n  openai:\n    api_key: ${TEST_ORYX_KEY}\n",
			env:  map[string]string{"TEST_ORYX_KEY": "sk-test-123"},
			check: func(t *testing.T, cfg *config.Config) {
				if got := cfg.Providers["openai"].APIKey; got != "${TEST_ORYX_KEY}" {
					t.Errorf("api_key = %q, 期望佔位符原樣留著", got)
				}
			},
		},
		{
			// 上一格的對照：環境變數**沒設**時同樣照常載入，不報錯。這一格才是
			// `oryxos tools` 真正依賴的性質。
			name: "引用的環境變數未設定也照常載入",
			yaml: "providers:\n  openai:\n    api_key: ${TEST_ORYX_ABSENT}\n",
			check: func(t *testing.T, cfg *config.Config) {
				if got := cfg.Providers["openai"].APIKey; got != "${TEST_ORYX_ABSENT}" {
					t.Errorf("api_key = %q, 期望佔位符原樣留著", got)
				}
			},
		},
		{
			name: "base_url 與 allowed_domains 原樣載入",
			yaml: "providers:\n  deepseek:\n    api_key: k\n    base_url: https://api.deepseek.com\nhttp:\n  allowed_domains:\n    - api.example.com\n",
			check: func(t *testing.T, cfg *config.Config) {
				if got := cfg.Providers["deepseek"].BaseURL; got != "https://api.deepseek.com" {
					t.Errorf("base_url = %q", got)
				}
				if len(cfg.HTTP.AllowedDomains) != 1 || cfg.HTTP.AllowedDomains[0] != "api.example.com" {
					t.Errorf("allowed_domains = %v", cfg.HTTP.AllowedDomains)
				}
			},
		},
		{
			name:    "非法 YAML",
			yaml:    "providers: [未閉合",
			wantErr: "解析 Workspace 設定檔",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.Load(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，實際成功", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，實際錯誤: %v", err)
			}
			tt.check(t, cfg)
		})
	}

	t.Run("檔案不存在時錯誤鏈含 os.ErrNotExist", func(t *testing.T) {
		_, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("期望錯誤鏈含 os.ErrNotExist，實際: %v", err)
		}
	})
}

// TestExpandProviderEnv 驗 Provider 憑證的 ${ENV_VAR} 展開。
//
// 這一段原本住在 Load 裡，issue #27 把它拆成顯式的一步——理由與 MCP 憑證那條
// （ExpandMcpServerEnv）相同：缺一個環境變數只代表「這台機器上還沒設定」，不該擋下
// 與 Provider 無關的命令。行為本身一條沒少，只是換了入口，所以斷言原樣搬過來。
func TestExpandProviderEnv(t *testing.T) {
	tests := []struct {
		name      string
		providers map[string]config.ProviderConfig
		env       map[string]string
		check     func(t *testing.T, got map[string]config.ProviderConfig)
		wantErr   string // 空字串表示期望成功；否則錯誤訊息須含此子串
	}{
		{
			name:      "api_key 環境變數佔位解析",
			providers: map[string]config.ProviderConfig{"openai": {APIKey: "${TEST_ORYX_KEY}"}},
			env:       map[string]string{"TEST_ORYX_KEY": "sk-test-123"},
			check: func(t *testing.T, got map[string]config.ProviderConfig) {
				if k := got["openai"].APIKey; k != "sk-test-123" {
					t.Errorf("api_key = %q, 期望 sk-test-123", k)
				}
			},
		},
		{
			name:      "base_url 也展開",
			providers: map[string]config.ProviderConfig{"openai": {APIKey: "k", BaseURL: "${TEST_ORYX_URL}/v1"}},
			env:       map[string]string{"TEST_ORYX_URL": "https://proxy.example.com"},
			check: func(t *testing.T, got map[string]config.ProviderConfig) {
				if u := got["openai"].BaseURL; u != "https://proxy.example.com/v1" {
					t.Errorf("base_url = %q", u)
				}
			},
		},
		{
			name:      "api_key 引用的環境變數未設定",
			providers: map[string]config.ProviderConfig{"openai": {APIKey: "${TEST_ORYX_ABSENT}"}},
			wantErr:   "TEST_ORYX_ABSENT",
		},
		{
			name:      "多個缺失的環境變數全部回報",
			providers: map[string]config.ProviderConfig{"openai": {APIKey: "${TEST_ORYX_A}${TEST_ORYX_B}"}},
			wantErr:   "TEST_ORYX_A、TEST_ORYX_B",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := config.ExpandProviderEnv(tt.providers)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，實際成功", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，實際錯誤: %v", err)
			}
			tt.check(t, got)
		})
	}

	t.Run("回新 map，不就地改動傳進來的那份", func(t *testing.T) {
		t.Setenv("TEST_ORYX_KEY", "sk-test-123")
		original := map[string]config.ProviderConfig{"openai": {APIKey: "${TEST_ORYX_KEY}"}}
		if _, err := config.ExpandProviderEnv(original); err != nil {
			t.Fatalf("ExpandProviderEnv: %v", err)
		}
		if got := original["openai"].APIKey; got != "${TEST_ORYX_KEY}" {
			t.Errorf("傳進去的那份被改成了 %q——「檔案裡寫了什麼」與「展開後是什麼」該分得開", got)
		}
	})
}
