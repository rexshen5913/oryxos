// 內建 Tool 失敗時回報的**錯誤類型**的行為矩陣（ticket #51）。
//
// 斷言對象是 Execute 回傳的 ToolResult 這個外部產物的一個欄位，不是內部呼叫序列：
// 換掉 Tool 內部的分支結構而回報的類型不變時，這張表應該保持綠色。
//
// 依賴一律用真的（憲法 4.3）：Workspace 是 t.TempDir()、檔案真的建、SandboxChecker
// 是真的。HTTP 那一格**不打網路**——白名單校驗在送出請求之前就拒絕了，這正是它的設計。
package tool_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// denyAllShell 組一個白名單為空（因此全部拒絕）的 shell Tool。
func denyAllShell(t *testing.T) tool.OryxTool {
	t.Helper()
	return tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: 5 * time.Second},
		tool.NewShellLimiter(),
	)
}

// loadSkillIn 組一個 load_skill，上下文載入器指向一個真的（空的）Workspace。
func loadSkillIn(t *testing.T, root *os.Root) tool.OryxTool {
	t.Helper()
	return tool.NewLoadSkillTool(config.NewContextLoader(root), []string{"daily-pr-digest"})
}

// TestBuiltinToolErrorKinds 是本票遷移的驗收矩陣：三類（sandbox、not_found、
// invalid_args）在內建 Tool 上各自報得出來。
//
// **未分類（零值）也要有格子。** 本票只遷三類，其餘失敗刻意留在零值——沒有這幾格，
// 「還沒遷的維持零值」就只是一句宣稱；有了它們，日後有人順手把某一條改成別的類型，
// 這裡會轉紅、逼他去想那是不是他真的要的。
func TestBuiltinToolErrorKinds(t *testing.T) {
	tests := []struct {
		name  string
		run   func(t *testing.T) core.ToolResult
		want  core.ToolErrorKind
		allow bool // true 代表這一格期望成功；其餘都是失敗路徑
	}{
		// ---- sandbox：白名單拒絕 ----
		{
			name: "read_file 路徑不在白名單",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				writeWorkspaceFile(t, dir, filepath.Join("secrets", "k.txt"), "x")
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"secrets/k.txt"}`)
			},
			want: core.ToolErrorSandbox,
		},
		{
			name: "read_file 絕對路徑（CheckFilePath 的另一條拒絕）",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"/etc/passwd"}`)
			},
			want: core.ToolErrorSandbox,
		},
		{
			name: "write_file 路徑不在白名單",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newWriteFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"secrets/k.txt","content":"x"}`)
			},
			want: core.ToolErrorSandbox,
		},
		{
			name: "list_dir 路徑不在白名單",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newListDir(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"secrets"}`)
			},
			want: core.ToolErrorSandbox,
		},
		{
			name: "http_get 的 host 不在白名單（不會送出請求）",
			run: func(t *testing.T) core.ToolResult {
				return tool.NewHTTPGet(tool.NewSandboxChecker(tool.SandboxConfig{})).
					Execute(context.Background(), `{"url":"https://example.com/x"}`)
			},
			want: core.ToolErrorSandbox,
		},
		{
			name: "shell 命令不在白名單",
			run: func(t *testing.T) core.ToolResult {
				return denyAllShell(t).Execute(context.Background(), `{"command":"rm","args":["-rf","notes"]}`)
			},
			want: core.ToolErrorSandbox,
		},

		// ---- not_found：目標不存在 ----
		{
			name: "read_file 找不到檔案（白名單內）",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
					t.Fatalf("建立 notes/: %v", err)
				}
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes/nope.md"}`)
			},
			want: core.ToolErrorNotFound,
		},
		{
			name: "list_dir 找不到目錄（白名單內）",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newListDir(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes"}`)
			},
			want: core.ToolErrorNotFound,
		},

		// ---- invalid_args：參數 JSON 或欄位錯誤 ----
		{
			name: "read_file 的參數不是合法 JSON",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newReadFile(root, []string{"notes"}).Execute(context.Background(), `{`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "read_file 缺必填 path",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newReadFile(root, []string{"notes"}).Execute(context.Background(), `{}`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "write_file 缺必填 content",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newWriteFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes/a.md"}`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "list_dir 缺必填 path",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newListDir(root, []string{"notes"}).Execute(context.Background(), `{}`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "http_get 缺必填 url",
			run: func(t *testing.T) core.ToolResult {
				return tool.NewHTTPGet(tool.NewSandboxChecker(tool.SandboxConfig{})).
					Execute(context.Background(), `{}`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			// 這一格是第 2 輪 Codex 抓到的漏網：`{}` 原本一路走到 CheckShellCommand("")，
			// 被包成 SandboxViolation 而錯分成 sandbox。schema 宣告了 required 卻沒給，
			// 那是 invalid_args——與 read_file 缺 path 是同一件事，不該因為 Tool 不同
			// 就分到不同類。
			name: "shell 缺必填 command",
			run: func(t *testing.T) core.ToolResult {
				return denyAllShell(t).Execute(context.Background(), `{}`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "shell 的參數不是合法 JSON",
			run: func(t *testing.T) core.ToolResult {
				return denyAllShell(t).Execute(context.Background(), `{`)
			},
			want: core.ToolErrorInvalidArgs,
		},
		{
			name: "load_skill 缺必填 name",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return loadSkillIn(t, root).Execute(context.Background(), `{}`)
			},
			want: core.ToolErrorInvalidArgs,
		},

		{
			// turn 進行中 Skill 被刪掉：Skill 段每個 turn 重讀、列了它，取正文時卻已經
			// 不在。這裡用「先建再刪」重現同一個形狀——不必製造競爭，是確定性的。
			// 既有的 agent_load_skill_test.go 有同名場景（經 seam、mid-turn 刪檔），
			// 這一格補的是「它報得出 not_found」這半。
			name: "load_skill 的 SKILL.md 已被刪掉",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				rel := filepath.Join("skills", "daily-pr-digest.md")
				writeWorkspaceFile(t, dir, rel,
					"---\nname: daily-pr-digest\ndescription: 做摘要。\n---\n\n正文\n")
				if err := os.Remove(filepath.Join(dir, rel)); err != nil {
					t.Fatalf("刪除 SKILL.md: %v", err)
				}
				return loadSkillIn(t, root).Execute(context.Background(), `{"name":"daily-pr-digest"}`)
			},
			want: core.ToolErrorNotFound,
		},

		// ---- 未分類（零值）：本票不遷的失敗維持現況 ----
		{
			name: "read_file 的目標是目錄——不屬本票遷移的三類",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
					t.Fatalf("建立 notes/: %v", err)
				}
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes"}`)
			},
			want: core.ToolErrorUnclassified,
		},
		{
			// **刻意不分類，不是漏掉。** 這不是 schema 錯誤（name 欄位在、型別也對），
			// 而 invalid_args 的通用指引會叫 LLM「對照 input schema 修正欄位名稱與
			// 型別」——對這個失敗是**錯的**建議。它既有的訊息已經把可用清單列出來了，
			// 比任何通用句子有用。
			name: "load_skill 引用範圍外的 Skill——刻意不分類",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return loadSkillIn(t, root).Execute(context.Background(), `{"name":"slack-post"}`)
			},
			want: core.ToolErrorUnclassified,
		},

		// ---- 成功路徑：類型欄位必須留在零值 ----
		{
			name: "read_file 成功時不帶錯誤類型",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "hi\n")
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes/todo.md"}`)
			},
			want:  core.ToolErrorUnclassified,
			allow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.run(t)
			if got.OK != tt.allow {
				t.Fatalf("OK = %v, 期望 %v；錯誤: %q", got.OK, tt.allow, got.Error)
			}
			if got.ErrorKind != tt.want {
				t.Errorf("ErrorKind = %d, 期望 %d；錯誤: %q", got.ErrorKind, tt.want, got.Error)
			}
		})
	}
}

// TestToolErrorKindDoesNotDisturbRetryable 釘住「錯誤類型與 Retryable 正交」。
//
// 兩者是不同的問題——一個問「這是哪一類失敗」，一個問「要不要重試」——而本票**完全
// 沒有動** Retryable 的判定。分類遷移最容易順手弄壞的就是它：改一行 ToolResult 字面
// 的時候把 Retryable 一起改掉，既有的重試測試未必抓得到每一條路徑。
func TestToolErrorKindDoesNotDisturbRetryable(t *testing.T) {
	tests := []struct {
		name          string
		run           func(t *testing.T) core.ToolResult
		wantRetryable bool
	}{
		{
			// Sandbox 拒絕永遠不可重試：白名單的答案重跑一次不會變。
			name: "sandbox 拒絕不可重試",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"secrets/k.txt"}`)
			},
			wantRetryable: false,
		},
		{
			name: "參數錯誤不可重試",
			run: func(t *testing.T) core.ToolResult {
				root, _ := newWorkspace(t)
				return newReadFile(root, []string{"notes"}).Execute(context.Background(), `{`)
			},
			wantRetryable: false,
		},
		{
			name: "找不到檔案不可重試",
			run: func(t *testing.T) core.ToolResult {
				root, dir := newWorkspace(t)
				if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
					t.Fatalf("建立 notes/: %v", err)
				}
				return newReadFile(root, []string{"notes"}).
					Execute(context.Background(), `{"path":"notes/nope.md"}`)
			},
			wantRetryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.run(t); got.Retryable != tt.wantRetryable {
				t.Errorf("Retryable = %v, 期望 %v；錯誤: %q", got.Retryable, tt.wantRetryable, got.Error)
			}
		})
	}
}
