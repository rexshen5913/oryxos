// 本檔驗**「有哪些工具可以寫進 Profile 的 tools」查得出來**（issue #27 方向一）：
// Subset 擋下未註冊的名字時，把可用的選項一併交出去。
//
// 場景取自 #24 的真實遭遇：接上 `@modelcontextprotocol/server-github` 之後，使用者得
// 先知道它那 26 個工具叫什麼名字才寫得出 tools 欄位，而當時唯一的辦法是自己寫腳本探。
// 所以這裡的 server 起的是**真實的本地 stdio 子進程**、工具名經真實的 tools/list 取回，
// 不捏假 Tool（憲法 4.3、ADR-0002）。
package tool

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestSubsetErrorListsAvailableTools 釘住**錯誤訊息把死路變成走得出去的路**。
//
// 白名單本身不變（那是 #24 驗到的安全性質）：這裡改的只是「擋下來的時候有沒有給線索」。
// 原本的訊息只說某個名字未註冊，使用者要嘛翻該 server 的文件、要嘛自己寫腳本探——
// 試錯的迴圈走不出去，因為錯誤不告訴你正確答案長什麼樣。
//
// **範圍跟著前綴走**：打錯的是工具名時只列同一台 server 的（接兩台 26 工具的 server
// 就是 50 多個名字，全倒出來等於沒說）；前綴本身無匹配時才列全部——那時使用者多半是
// server 名打錯或記錯，需要看到的正是「有哪些 server」。
func TestSubsetErrorListsAvailableTools(t *testing.T) {
	registry := NewRegistry()
	wsRoot, rootErr := os.OpenRoot(t.TempDir())
	if rootErr != nil {
		t.Fatalf("OpenRoot: %v", rootErr)
	}
	t.Cleanup(func() {
		if cerr := wsRoot.Close(); cerr != nil {
			t.Errorf("關閉 Workspace root: %v", cerr)
		}
	})
	if err := RegisterBuiltins(registry, NewSandboxChecker(SandboxConfig{}), wsRoot); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	svc, err := ConnectMcpServers(context.Background(), registry, []core.McpServerSpec{
		toolMcpSpec(t, "github", "", "list_pull_requests", "get_pull_request", "merge_pull_request"),
		toolMcpSpec(t, "slack", "", "post_message"),
	}, nil, discardLogger())
	t.Cleanup(func() {
		if cerr := svc.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})
	if err != nil {
		t.Fatalf("連線 MCP server: %v", err)
	}

	tests := []struct {
		name string
		// ref 是使用者在 Profile 的 tools 裡寫錯的那個名字。
		ref string
		// wantContains 是錯誤訊息裡必須出現的可用名字。
		wantContains []string
		// wantAbsent 是**不該**出現的名字：列太多等於沒列。
		wantAbsent []string
	}{
		{
			name:         "工具名打錯：只列同一台 server 的工具",
			ref:          "github__list_prs",
			wantContains: []string{"github__list_pull_requests", "github__merge_pull_request"},
			wantAbsent:   []string{"slack__post_message", "http_get"},
		},
		{
			name: "server 名打錯：前綴無匹配，列出全部讓使用者看得到有哪些 server",
			ref:  "gihub__list_pull_requests",
			wantContains: []string{
				"github__list_pull_requests", "slack__post_message", "http_get",
			},
		},
		{
			name:         "內建 Tool 打錯字：沒有前綴可依，列出全部",
			ref:          "http_gett",
			wantContains: []string{"http_get", "http_post"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Subset([]string{tt.ref}, nil, discardLogger())
			if err == nil {
				t.Fatalf("Subset(%q) 沒有報錯——白名單被放寬了", tt.ref)
			}
			msg := err.Error()
			// 打錯的那個名字仍要在訊息裡：使用者得知道是哪一行寫錯了。
			if !strings.Contains(msg, tt.ref) {
				t.Errorf("錯誤訊息沒有指出是哪個名字寫錯了 %q:\n%s", tt.ref, msg)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("錯誤訊息沒有列出可用的 %q——使用者仍然無從得知該寫什麼:\n%s", want, msg)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(msg, absent) {
					t.Errorf("錯誤訊息把不相干的 %q 也倒出來了——列太多等於沒列:\n%s", absent, msg)
				}
			}
		})
	}
}

// TestSuggestDistinguishesOverlappingServerPrefixes 釘住**前綴重疊時建議不混台**。
//
// server 名沒有任何字元限制（見 config.validateMcpServerEntry：只擋 transport 與
// command），所以 `foo` 與 `foo__bar` 可以同時宣告。此時 `foo__bar__echo` 這個註冊名
// 以第一個雙底線去切會得到 `foo`——於是建議清單裡混進另一台 server 的工具。
//
// 混台的後果不只是雜訊：使用者會照著抄一個**別台**的工具名，然後在下一次啟動繼續失敗，
// 而錯誤訊息看起來還很篤定。
func TestSuggestDistinguishesOverlappingServerPrefixes(t *testing.T) {
	registry := NewRegistry()
	svc, err := ConnectMcpServers(context.Background(), registry, []core.McpServerSpec{
		toolMcpSpec(t, "foo", "", "alpha"),
		toolMcpSpec(t, "foo__bar", "", "echo"),
	}, nil, discardLogger())
	t.Cleanup(func() {
		if cerr := svc.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})
	if err != nil {
		t.Fatalf("連線 MCP server: %v", err)
	}

	// 打錯的是 foo__bar 這台的工具名。
	_, err = registry.Subset([]string{"foo__bar__ech"}, nil, discardLogger())
	if err == nil {
		t.Fatal("Subset 沒有報錯——白名單被放寬了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "foo__bar__echo") {
		t.Errorf("錯誤沒有列出 foo__bar 這台真正提供的工具:\n%s", msg)
	}
	if strings.Contains(msg, "foo__alpha") {
		t.Errorf("錯誤把另一台 server（foo）的工具混了進來——使用者會照抄一個錯的名字:\n%s", msg)
	}
}
