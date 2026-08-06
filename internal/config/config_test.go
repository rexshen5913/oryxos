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
			name: "api_key 環境變數佔位解析",
			yaml: "providers:\n  openai:\n    api_key: ${TEST_ORYX_KEY}\n",
			env:  map[string]string{"TEST_ORYX_KEY": "sk-test-123"},
			check: func(t *testing.T, cfg *config.Config) {
				if got := cfg.Providers["openai"].APIKey; got != "sk-test-123" {
					t.Errorf("api_key = %q, 期望 sk-test-123", got)
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
			name:    "api_key 引用的環境變數未設定",
			yaml:    "providers:\n  openai:\n    api_key: ${TEST_ORYX_ABSENT}\n",
			wantErr: "TEST_ORYX_ABSENT",
		},
		{
			name:    "多個缺失的環境變數全部回報",
			yaml:    "providers:\n  openai:\n    api_key: ${TEST_ORYX_A}${TEST_ORYX_B}\n",
			wantErr: "TEST_ORYX_A、TEST_ORYX_B",
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
