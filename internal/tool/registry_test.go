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
		name      string
		tools     []string
		wantNames []string
		wantErr   string // 非空時期望錯誤含此子串
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec, err := newHTTPRegistry(t).Subset(tt.tools, discardLogger())
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
