package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

func TestLoadProfile(t *testing.T) {
	const fullProfile = `name: default
description: OryxOS 預設 Agent
identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openai
  model: gpt-4o-mini
  temperature: 0.7
tools:
  - http_get
  - http_post
settings:
  max_iterations: 5
  max_history_turns: 8
`

	tests := []struct {
		name    string
		yaml    string
		want    *core.Profile
		wantErr string // 空字串表示期望成功；否則錯誤訊息須含此子串
	}{
		{
			name: "完整合法 Profile",
			yaml: fullProfile,
			want: &core.Profile{
				Name:        "default",
				Description: "OryxOS 預設 Agent",
				Identity:    core.Identity{AgentName: "Oryx", Prompt: "你是 Oryx。"},
				Provider:    core.ProviderRef{Name: "openai", Model: "gpt-4o-mini", Temperature: 0.7},
				Tools:       []string{"http_get", "http_post"},
				Settings:    core.Settings{MaxIterations: 5, MaxHistoryTurns: 8},
			},
		},
		{
			name: "settings 省略時套用預設值",
			yaml: "name: d\nprovider:\n  name: openai\n  model: m\n",
			want: &core.Profile{
				Name:     "d",
				Provider: core.ProviderRef{Name: "openai", Model: "m"},
				Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20},
			},
		},
		{
			name:    "非法 YAML",
			yaml:    "name: [未閉合",
			wantErr: "解析 Profile",
		},
		{
			name:    "provider.name 缺失",
			yaml:    "name: d\nprovider:\n  model: m\n",
			wantErr: "provider.name",
		},
		{
			name:    "provider.model 缺失",
			yaml:    "name: d\nprovider:\n  name: openai\n",
			wantErr: "provider.model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := core.LoadProfile(path)
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
			assertProfileEqual(t, got, tt.want)
		})
	}

	t.Run("檔案不存在時錯誤鏈含 os.ErrNotExist", func(t *testing.T) {
		_, err := core.LoadProfile(filepath.Join(t.TempDir(), "absent.yaml"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("期望錯誤鏈含 os.ErrNotExist，實際: %v", err)
		}
	})
}

func assertProfileEqual(t *testing.T, got, want *core.Profile) {
	t.Helper()
	if got.Name != want.Name || got.Description != want.Description {
		t.Errorf("name/description = %q/%q, 期望 %q/%q", got.Name, got.Description, want.Name, want.Description)
	}
	if got.Identity != want.Identity {
		t.Errorf("identity = %+v, 期望 %+v", got.Identity, want.Identity)
	}
	if got.Provider != want.Provider {
		t.Errorf("provider = %+v, 期望 %+v", got.Provider, want.Provider)
	}
	if len(got.Tools) != len(want.Tools) {
		t.Fatalf("tools = %v, 期望 %v", got.Tools, want.Tools)
	}
	for i := range got.Tools {
		if got.Tools[i] != want.Tools[i] {
			t.Errorf("tools[%d] = %q, 期望 %q", i, got.Tools[i], want.Tools[i])
		}
	}
	if got.Settings != want.Settings {
		t.Errorf("settings = %+v, 期望 %+v", got.Settings, want.Settings)
	}
}
