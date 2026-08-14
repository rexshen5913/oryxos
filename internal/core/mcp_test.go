// Profile mcp_servers 欄位的校驗與宣告解析的單元測試（ticket #21）。這兩者是**純
// 函式**——不碰檔案、不起子進程——所以直接測，不必繞 seam；經 seam 觀察得到的行為由
// agent_mcp_test.go 覆蓋。
package core_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

func TestProfileMcpServerRefs(t *testing.T) {
	tests := []struct {
		name string
		// servers 是 Profile mcp_servers 欄位的字面值。
		servers []string
		wantLen int
		wantErr string // 空字串表示期望成功；否則錯誤訊息須含此子串
	}{
		{name: "省略：不接任何 server", servers: nil},
		{name: "空清單：不接任何 server", servers: []string{}},
		{name: "正常引用", servers: []string{"github", "slack"}, wantLen: 2},
		{
			// 沿 Registry.Subset 對 tools、validateBootstrap 對 bootstrap 的既有語義：
			// 設定筆誤不靜默。靜默去重還會讓同一個 server 被連兩次、工具第二次註冊撞名。
			name:    "重複列出報錯",
			servers: []string{"github", "github"},
			wantErr: "重複列出",
		},
		{
			name:    "空字串的 server 名報錯",
			servers: []string{""},
			wantErr: "空的 server 名",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prof := &core.Profile{McpServers: tt.servers}
			got, err := prof.McpServerRefs()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，實際成功", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("錯誤 = %q, 期望含 %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("McpServerRefs: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("引用數 = %d, 期望 %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestResolveMcpServers(t *testing.T) {
	declared := map[string]core.McpServerSpec{
		"github": {Name: "github", Transport: "stdio", Command: []string{"gh-mcp"}},
		"slack":  {Name: "slack", Transport: "stdio", Command: []string{"slack-mcp"}},
	}

	tests := []struct {
		name string
		refs []string
		// wantOrder 是解析結果預期的 server 名順序。
		wantOrder []string
		wantErr   string
	}{
		{
			// 「只回被引用到的」正是「不 spawn 無關子進程」的實作方式。
			name:      "只回被引用到的",
			refs:      []string{"slack"},
			wantOrder: []string{"slack"},
		},
		{
			// 順序跟著引用順序，不跟著 map（map 的走訪順序是隨機的，會讓註冊順序、
			// 日誌順序每次都不一樣）。
			name:      "順序等於引用順序",
			refs:      []string{"slack", "github"},
			wantOrder: []string{"slack", "github"},
		},
		{name: "沒有引用：不連任何 server", refs: nil},
		{
			// 設定錯誤（打錯字），與「server 連不上」的環境問題不同質：這裡 fail fast。
			name:    "引用未宣告的 server：報錯並指名",
			refs:    []string{"githbu"},
			wantErr: "githbu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := core.ResolveMcpServers(tt.refs, declared)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，實際成功", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("錯誤 = %q, 期望含 %q", err.Error(), tt.wantErr)
				}
				// 錯誤要指得出「該去哪個檔案補宣告」，否則使用者只知道名字錯了。
				if !strings.Contains(err.Error(), core.McpServersFile) {
					t.Errorf("錯誤 %q 未指出宣告檔 %s", err.Error(), core.McpServersFile)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveMcpServers: %v", err)
			}
			if len(got) != len(tt.wantOrder) {
				t.Fatalf("解析出 %d 份 spec, 期望 %d: %v", len(got), len(tt.wantOrder), got)
			}
			for i, want := range tt.wantOrder {
				if got[i].Name != want {
					t.Errorf("spec[%d].Name = %q, 期望 %q", i, got[i].Name, want)
				}
			}
		})
	}
}
