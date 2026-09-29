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
				Settings:    core.Settings{MaxIterations: 5, MaxHistoryTurns: 8, MaxRepeatedToolFailures: 3, MaxContextRunes: 100000},
			},
		},
		{
			name: "settings 省略時套用預設值",
			yaml: "name: d\nprovider:\n  name: openai\n  model: m\n",
			want: &core.Profile{
				Name:     "d",
				Provider: core.ProviderRef{Name: "openai", Model: "m"},
				Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20, MaxRepeatedToolFailures: 3, MaxContextRunes: 100000},
			},
		},
		{
			// 死循環守衛的門檻走的是與另外兩個相同的「零值回退預設」形狀（ticket #54）：
			// **明設的值不得被預設值蓋掉**。上面兩格證明未設定時回退，這一格證明設定了
			// 就作數——少了它，一個把 effective 寫成無條件回傳預設值的實作也會全綠。
			name: "明設 max_repeated_tool_failures 時保留",
			yaml: "name: d\nprovider:\n  name: openai\n  model: m\nsettings:\n  max_repeated_tool_failures: 5\n",
			want: &core.Profile{
				Name:     "d",
				Provider: core.ProviderRef{Name: "openai", Model: "m"},
				Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20, MaxRepeatedToolFailures: 5, MaxContextRunes: 100000},
			},
		},
		{
			// 上下文預算走的是同一個「零值回退預設」形狀（ticket #48）：明設的值
			// 不得被預設值蓋掉。理由與上一格相同，形狀相同的三個欄位各自要有證據
			// ——共用一格的話，漏接其中一個欄位的正規化不會有人發現。
			name: "明設 max_context_runes 時保留",
			yaml: "name: d\nprovider:\n  name: openai\n  model: m\nsettings:\n  max_context_runes: 2048\n",
			want: &core.Profile{
				Name:     "d",
				Provider: core.ProviderRef{Name: "openai", Model: "m"},
				Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20, MaxRepeatedToolFailures: 3, MaxContextRunes: 2048},
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

// TestLoadProfileBootstrapField 釘住 bootstrap 欄位的三態與名稱校驗（ticket #17）。
//
// **三態必須在反序列化層就分得出來**：省略是 nil、空清單是非 nil 的零長切片。
// 兩者若在這裡就被壓成同一個值，下游再怎麼實作也救不回「省略＝載入預設三檔」與
// 「空清單＝一份都不載入」的差別，所以斷言直接看 nil 與否。
func TestLoadProfileBootstrapField(t *testing.T) {
	const head = "name: d\nprovider:\n  name: openai\n  model: m\n"

	tests := []struct {
		name    string
		yaml    string
		wantNil bool
		wantLen int
		wantErr string
	}{
		{
			name:    "欄位省略：nil（沒意見，載入預設三檔）",
			yaml:    head,
			wantNil: true,
		},
		{
			name:    "裸 key 無值：仍是 nil，歸省略",
			yaml:    head + "bootstrap:\n",
			wantNil: true,
		},
		{
			name:    "空清單：非 nil 的零長切片（明確表達我不要）",
			yaml:    head + "bootstrap: []\n",
			wantNil: false,
			wantLen: 0,
		},
		{
			name:    "列一份",
			yaml:    head + "bootstrap:\n  - AGENTS.md\n",
			wantNil: false,
			wantLen: 1,
		},
		{
			name:    "列滿三份",
			yaml:    head + "bootstrap:\n  - SOUL.md\n  - AGENTS.md\n  - USER.md\n",
			wantNil: false,
			wantLen: 3,
		},
		{
			name:    "未知檔名：報錯（設定筆誤不靜默）",
			yaml:    head + "bootstrap:\n  - NOTES.md\n",
			wantErr: "NOTES.md",
		},
		{
			name:    "大小寫不符也算未知檔名",
			yaml:    head + "bootstrap:\n  - agents.md\n",
			wantErr: "agents.md",
		},
		{
			name:    "重複列出同一份：報錯（沿 tools 的既有語義）",
			yaml:    head + "bootstrap:\n  - AGENTS.md\n  - AGENTS.md\n",
			wantErr: "AGENTS.md",
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
			if (got.Bootstrap == nil) != tt.wantNil {
				t.Fatalf("bootstrap nil = %v, 期望 %v（省略與空清單必須分得出來）", got.Bootstrap == nil, tt.wantNil)
			}
			if !tt.wantNil && len(got.Bootstrap) != tt.wantLen {
				t.Errorf("bootstrap = %v，長度 %d, 期望 %d", got.Bootstrap, len(got.Bootstrap), tt.wantLen)
			}
		})
	}
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

// TestLoadProfileKeepsParsedProfileOnValidationFailure 釘住 LoadProfile 失敗時的回傳值：**解析成功、
// 校驗失敗時交出解析出來的 Profile**，讀檔或解析失敗時才是 nil（ticket #76）。
//
// server 的 profiles 端點靠它分辨「不知道」與「知道但不能用」：一份只拼錯 bootstrap 檔名的 Profile，
// 描述、Agent 名與 Provider 都寫得好好的，運維人員要靠它們認出是哪一個 Agent 壞了。
func TestLoadProfileKeepsParsedProfileOnValidationFailure(t *testing.T) {
	const valid = "name: p\ndescription: 保留下來\nprovider:\n  name: openrouter\n  model: m\n"
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		// wantParsed 為真時，期望錯誤之外還拿得到解析出來的 Profile。
		wantParsed bool
	}{
		{name: "provider.model 缺漏", yaml: "name: p\ndescription: 保留下來\nprovider:\n  name: openrouter\n",
			wantErr: "provider.model 必填", wantParsed: true},
		{name: "bootstrap 列了不是 Bootstrap 檔案的名字", yaml: valid + "bootstrap:\n  - TYPO.md\n",
			wantErr: "TYPO.md", wantParsed: true},
		{name: "skills 引用不合法的 Skill 名稱", yaml: valid + "skills:\n  - Not_Valid\n",
			wantErr: "Not_Valid", wantParsed: true},
		{name: "YAML 解析失敗", yaml: "name: p\nprovider: [unclosed\n", wantErr: "解析 Profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := core.LoadProfile(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("錯誤 = %v, 期望含 %q", err, tt.wantErr)
			}
			if !tt.wantParsed {
				if got != nil {
					t.Errorf("YAML 解析失敗時回傳 %+v, 期望 nil（沒有東西可以交）", got)
				}
				return
			}
			if got == nil {
				t.Fatal("解析成功、校驗失敗時回傳 nil, 期望交出解析出來的 Profile")
			}
			if got.Description != "保留下來" {
				t.Errorf("Description = %q, 期望 %q（解析出來的值要原樣交出）", got.Description, "保留下來")
			}
		})
	}
}
