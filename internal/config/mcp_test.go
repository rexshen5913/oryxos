// MCP server 宣告檔（.oryxos/mcp_servers.yaml）的載入測試：schema 形狀、env 的
// ${ENV_VAR} 佔位展開、檔案不存在時的免遷移語義。檔案系統一律用真的（t.TempDir()，
// 憲法 4.3）。
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
)

func TestLoadMcpServers(t *testing.T) {
	tests := []struct {
		name string
		// yaml 為 "" 時**不建立檔案**——那是「這個 Workspace 不用 MCP」的情形，
		// 與「檔案存在但沒有宣告任何 server」是不同的輸入，兩者都要有案例。
		yaml    string
		omit    bool
		env     map[string]string
		check   func(t *testing.T, got map[string]core.McpServerSpec)
		wantErr string // 空字串表示期望成功；否則錯誤訊息須含此子串
	}{
		{
			// 既有 Workspace 免遷移：init 自本票起才建這個模板，舊的 Workspace 沒有它。
			name: "檔案不存在：視為沒有任何 MCP server，不算錯誤",
			omit: true,
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				if len(got) != 0 {
					t.Errorf("宣告數 = %d, 期望 0", len(got))
				}
			},
		},
		{
			name: "只有註解的空模板：0 個 server，不算錯誤",
			yaml: "# 這裡宣告外部 MCP server\nmcp_servers: {}\n",
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				if len(got) != 0 {
					t.Errorf("宣告數 = %d, 期望 0", len(got))
				}
			},
		},
		{
			name: "完整宣告：name 取自 key，transport、command、env 原樣載入",
			yaml: "mcp_servers:\n  github:\n    transport: stdio\n" +
				"    command: [node, ./github-server.js, --verbose]\n" +
				"    env:\n      GITHUB_TOKEN: ghp_literal\n",
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				spec, ok := got["github"]
				if !ok {
					t.Fatalf("沒有 github 的宣告: %v", got)
				}
				if spec.Name != "github" {
					t.Errorf("Name = %q, 期望 github（取自 YAML 的 key）", spec.Name)
				}
				if spec.Transport != "stdio" {
					t.Errorf("Transport = %q", spec.Transport)
				}
				want := []string{"node", "./github-server.js", "--verbose"}
				if len(spec.Command) != len(want) {
					t.Fatalf("Command = %v, 期望 %v", spec.Command, want)
				}
				for i, arg := range want {
					if spec.Command[i] != arg {
						t.Errorf("Command[%d] = %q, 期望 %q", i, spec.Command[i], arg)
					}
				}
				if spec.Env["GITHUB_TOKEN"] != "ghp_literal" {
					t.Errorf("env GITHUB_TOKEN = %q", spec.Env["GITHUB_TOKEN"])
				}
			},
		},
		{
			// **載入階段不展開佔位**：展開歸 ExpandMcpServerEnv，在 Profile 過濾之後做。
			// 這裡若順手展開了，一個沒被引用的 server 缺憑證就會擋下整個啟動。
			name: "env 的 ${ENV_VAR} 佔位原樣保留、不在載入時展開",
			yaml: "mcp_servers:\n  slack:\n    transport: stdio\n    command: [slack-mcp]\n" +
				"    env:\n      SLACK_TOKEN: ${TEST_ORYX_SLACK_TOKEN}\n",
			env: map[string]string{"TEST_ORYX_SLACK_TOKEN": "xoxb-123"},
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				if v := got["slack"].Env["SLACK_TOKEN"]; v != "${TEST_ORYX_SLACK_TOKEN}" {
					t.Errorf("SLACK_TOKEN = %q, 期望原樣保留佔位", v)
				}
			},
		},
		{
			// 連環境變數根本沒設定也不該在這一步報錯——這是「載入宣告」不是「取憑證」。
			name: "env 引用的環境變數未設定：載入階段不報錯",
			yaml: "mcp_servers:\n  slack:\n    transport: stdio\n    command: [slack-mcp]\n" +
				"    env:\n      SLACK_TOKEN: ${TEST_ORYX_MISSING_TOKEN}\n",
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				if len(got) != 1 {
					t.Errorf("宣告數 = %d, 期望 1", len(got))
				}
			},
		},
		{
			name: "多個 server 各自獨立",
			yaml: "mcp_servers:\n  github:\n    transport: stdio\n    command: [gh-mcp]\n" +
				"  slack:\n    transport: stdio\n    command: [slack-mcp]\n",
			check: func(t *testing.T, got map[string]core.McpServerSpec) {
				if len(got) != 2 {
					t.Fatalf("宣告數 = %d, 期望 2: %v", len(got), got)
				}
				if got["github"].Command[0] != "gh-mcp" || got["slack"].Command[0] != "slack-mcp" {
					t.Errorf("兩份宣告互相污染: %v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), core.McpServersFile)
			if !tt.omit {
				if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
					t.Fatalf("寫入宣告檔: %v", err)
				}
			}

			got, err := config.LoadMcpServers(path)
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
				t.Fatalf("LoadMcpServers: %v", err)
			}
			tt.check(t, got)
		})
	}
}

// TestExpandMcpServerEnv 驗憑證展開這一步：**只展開傳進來的那幾份**。
//
// 「只展開傳進來的」不是效率考量，是隔離語義：呼叫端已經用 Profile 的 mcp_servers
// 過濾過，一個沒被引用的 server 缺憑證不該影響這個 Agent 能不能啟動。
func TestExpandMcpServerEnv(t *testing.T) {
	tests := []struct {
		name  string
		specs []core.McpServerSpec
		env   map[string]string
		// wantValues 是展開後期望的 <server>/<KEY> → 值。
		wantValues map[string]string
		wantErr    string
	}{
		{
			name: "佔位以環境變數展開",
			specs: []core.McpServerSpec{
				{Name: "slack", Env: map[string]string{"SLACK_TOKEN": "${TEST_ORYX_SLACK_TOKEN}"}},
			},
			env:        map[string]string{"TEST_ORYX_SLACK_TOKEN": "xoxb-123"},
			wantValues: map[string]string{"slack/SLACK_TOKEN": "xoxb-123"},
		},
		{
			name: "沒有佔位的值原樣保留",
			specs: []core.McpServerSpec{
				{Name: "demo", Env: map[string]string{"MODE": "verbose"}},
			},
			wantValues: map[string]string{"demo/MODE": "verbose"},
		},
		{
			// 錯誤要同時指出**哪個 server**與**哪個變數**，否則使用者得自己去猜是哪一行。
			name: "環境變數未設定：錯誤指名 server 與變數",
			specs: []core.McpServerSpec{
				{Name: "slack", Env: map[string]string{"SLACK_TOKEN": "${TEST_ORYX_MISSING_TOKEN}"}},
			},
			wantErr: "mcp_servers.slack.env.SLACK_TOKEN",
		},
		{name: "沒有 spec：沒事", specs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := config.ExpandMcpServerEnv(tt.specs)
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
				t.Fatalf("ExpandMcpServerEnv: %v", err)
			}
			for ref, want := range tt.wantValues {
				server, key, _ := strings.Cut(ref, "/")
				var found bool
				for _, spec := range got {
					if spec.Name != server {
						continue
					}
					found = true
					if spec.Env[key] != want {
						t.Errorf("%s = %q, 期望 %q", ref, spec.Env[key], want)
					}
				}
				if !found {
					t.Errorf("展開結果沒有 server %q", server)
				}
			}
		})
	}
}

// TestExpandMcpServerEnvDoesNotMutateSource 釘住展開不回寫來源。
//
// spec 是值複製，但 Env 是 map、底層共用。原地展開會讓「原始宣告」與「展開結果」變成
// 同一份東西，日後任何想重讀原始佔位的人都會拿到已展開的憑證。
func TestExpandMcpServerEnvDoesNotMutateSource(t *testing.T) {
	t.Setenv("TEST_ORYX_SRC_TOKEN", "secret")
	source := map[string]string{"TOKEN": "${TEST_ORYX_SRC_TOKEN}"}
	specs := []core.McpServerSpec{{Name: "demo", Env: source}}

	got, err := config.ExpandMcpServerEnv(specs)
	if err != nil {
		t.Fatalf("ExpandMcpServerEnv: %v", err)
	}
	if got[0].Env["TOKEN"] != "secret" {
		t.Errorf("展開結果 = %q, 期望 secret", got[0].Env["TOKEN"])
	}
	if source["TOKEN"] != "${TEST_ORYX_SRC_TOKEN}" {
		t.Errorf("來源被改成 %q——展開回寫了原始宣告", source["TOKEN"])
	}
}
