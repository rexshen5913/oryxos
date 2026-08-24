package tool_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/tool"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newHTTPRegistry 建一個已顯式註冊全部內建 Tool（http_get、http_post、read_file）
// 的 Registry。Workspace 是一個空的 t.TempDir()：這裡驗的是註冊與過濾，不碰檔案。
func newHTTPRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	root, _ := newWorkspace(t)
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(tool.SandboxConfig{}), root, testShellRuntime(t)); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	return r
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := newHTTPRegistry(t)
	err := r.Register(tool.NewHTTPGet(tool.NewSandboxChecker(tool.SandboxConfig{})))
	if err == nil || !strings.Contains(err.Error(), "http_get") {
		t.Errorf("重複註冊 http_get 應報名稱衝突錯誤, 實際 %v", err)
	}
}

func TestRegistrySubset(t *testing.T) {
	tests := []struct {
		name string
		// tools 是使用者手寫的 tools 欄位；autoIncluded 是由其他配置推導出來的
		// （目前唯一來源是「skills 非空 → load_skill」）。兩者的重複語義刻意相反。
		tools        []string
		autoIncluded []string
		wantNames    []string
		wantErr      string // 非空時期望錯誤含此子串
	}{
		{
			name:      "過濾出列出的子集（未列出的不進定義列表）",
			tools:     []string{"http_get"},
			wantNames: []string{"http_get"},
		},
		{
			name:      "全列出時保持 Profile 順序",
			tools:     []string{"http_post", "http_get"},
			wantNames: []string{"http_post", "http_get"},
		},
		{
			name:      "空列表得到空定義（無 Tool 的 Agent）",
			tools:     nil,
			wantNames: nil,
		},
		{
			name:    "引用未註冊的 Tool 報清晰錯誤",
			tools:   []string{"http_get", "no_such_tool"},
			wantErr: "no_such_tool",
		},
		{
			name:    "重複列出同一 Tool 報清晰錯誤（配置筆誤不靜默）",
			tools:   []string{"http_get", "http_get"},
			wantErr: "重複",
		},
		// 以下是 ticket #20 的自動加入語義。上面五列同時是它的**迴歸保護**：
		// 自動加入不得把「未註冊報錯」與「同一份 tools 重複列出報錯」弄壞。
		{
			name:         "自動加入：tools 沒列也進得來",
			tools:        []string{"http_get"},
			autoIncluded: []string{"http_post"},
			wantNames:    []string{"http_get", "http_post"},
		},
		{
			name:         "自動加入：tools 為空時仍進得來",
			tools:        nil,
			autoIncluded: []string{"http_post"},
			wantNames:    []string{"http_post"},
		},
		{
			// 使用者把設定寫清楚不該被懲罰——「顯式列出」與「依配置推導」撞在
			// 一起是必然會發生的正常情況，冪等合併而非報錯。
			name:         "自動加入：使用者也顯式列了，冪等合併不報錯也不重複",
			tools:        []string{"http_get", "http_post"},
			autoIncluded: []string{"http_post"},
			wantNames:    []string{"http_get", "http_post"},
		},
		{
			name:         "自動加入的 Tool 未註冊：錯誤指向組裝點而不是使用者設定",
			tools:        []string{"http_get"},
			autoIncluded: []string{"no_such_tool"},
			wantErr:      "組裝點",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec, err := newHTTPRegistry(t).Subset(tt.tools, tt.autoIncluded, discardLogger())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Subset(%v) err = %v, 期望含 %q", tt.tools, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Subset(%v): %v", tt.tools, err)
			}

			defs := exec.Definitions()
			var got []string
			for _, d := range defs {
				got = append(got, d.Name)
				if d.Description == "" {
					t.Errorf("Tool %s 缺描述", d.Name)
				}
				if len(d.InputSchema) == 0 {
					t.Errorf("Tool %s 缺輸入 JSON Schema", d.Name)
				}
			}
			if len(got) != len(tt.wantNames) {
				t.Fatalf("Definitions 名稱 = %v, 期望 %v", got, tt.wantNames)
			}
			for i := range got {
				if got[i] != tt.wantNames[i] {
					t.Fatalf("Definitions 名稱 = %v, 期望 %v", got, tt.wantNames)
				}
			}
		})
	}
}

// TestRegisterBuiltinsRejectsNilWorkspaceRoot 把「組裝點漏傳 Workspace root」從一次
// **對話中途的 panic** 變成一次啟動錯誤。
//
// File Tool 一律經 *os.Root 開檔，而 RegisterBuiltins 新收的這個參數很容易漏傳。
// nil 的話註冊會照樣成功，然後第一次 read_file 呼叫在 root.Lstat 裡解參考 nil
// ——repo 裡沒有任何 recover()，那會直接殺掉 CLI。訊息指向組裝點，理由同 Subset
// 對「自動加入的 Tool 未註冊」的既有措辭：那不是使用者的設定錯誤。
// TestRegisterBuiltinsRejectsIncompleteShellRuntime 把兩種 shell 接線失誤從
// 「安靜跑錯」變成一次啟動錯誤，形狀同下面那格的 nil wsRoot。
//
// 兩者的失敗形態不同，而 **Dir 那個更安靜**：
//
//   - Timeout 為零 → 每一次呼叫在啟動的瞬間就逾時，錯誤訊息長得像「命令跑太久」，
//     使用者去調 config.yaml 的 timeout_seconds，那裡卻早已回退好預設值。
//   - Dir 為空 → exec.Cmd 的文件寫明那代表「在呼叫進程的工作目錄執行」，於是命令
//     **照跑、什麼錯都不報**，只是 shell 落在 Workspace 的父目錄動作，而 File Tool
//     仍在 Workspace 之內。「Dir 固定為 Workspace 根」是 shell.go 寫明的載重不變式。
func TestRegisterBuiltinsRejectsIncompleteShellRuntime(t *testing.T) {
	root, dir := newWorkspace(t)
	tests := []struct {
		name    string
		shell   tool.ShellRuntime
		wantSub string
	}{
		{name: "缺工作目錄", shell: tool.ShellRuntime{Timeout: time.Minute}, wantSub: "工作目錄"},
		{name: "超時為零", shell: tool.ShellRuntime{Dir: dir}, wantSub: "超時"},
		{name: "超時為負", shell: tool.ShellRuntime{Dir: dir, Timeout: -time.Second}, wantSub: "超時"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tool.RegisterBuiltins(tool.NewRegistry(), tool.NewSandboxChecker(tool.SandboxConfig{}), root, tt.shell)
			if err == nil {
				t.Fatal("接線不完整時應報錯，實際註冊成功——那會變成執行期才發現的安靜錯誤")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤 %q 未含 %q", err, tt.wantSub)
			}
		})
	}
}

func TestRegisterBuiltinsRejectsNilWorkspaceRoot(t *testing.T) {
	err := tool.RegisterBuiltins(tool.NewRegistry(), tool.NewSandboxChecker(tool.SandboxConfig{}), nil, testShellRuntime(t))
	if err == nil {
		t.Fatal("wsRoot 為 nil 時應報錯，實際成功註冊——第一次 read_file 呼叫才會 panic")
	}
	if !strings.Contains(err.Error(), "組裝點") {
		t.Errorf("錯誤訊息 %q 未指向組裝點", err.Error())
	}
}
