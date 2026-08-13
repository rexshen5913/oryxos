package tool_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newHTTPRegistry 建一個已顯式註冊全部內建 Tool（http_get、http_post）的 Registry。
func newHTTPRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(nil)); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	return r
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := newHTTPRegistry(t)
	err := r.Register(tool.NewHTTPGet(tool.NewSandboxChecker(nil)))
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
