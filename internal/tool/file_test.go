// File Tool 的行為矩陣。檔案系統**用真的**（憲法 4.3）：Workspace 是 t.TempDir()、
// 檔案真的建、符號連結真的 os.Symlink、開檔真的走 os.Root——這條鏈路要防的東西
// （連結跟隨、路徑穿越、非普通檔）沒有一個在假的檔案系統上驗得出來。
//
// 這裡涵蓋的是**開檔層**那一道防線；應用層白名單（CheckFilePath）的純字串矩陣在
// sandbox_test.go。兩道防線分工明確、不互相取代，所以兩邊各有自己的矩陣，而白名單
// 那幾格在這裡再驗一次「經 Tool 這條路徑也成立」。
package tool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// readFileOutput 是 read_file 回填給 LLM 的結果形狀，測試據此解回來斷言。
type readFileOutput struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// newWorkspace 在 t.TempDir() 開一個 Workspace 根並回傳 root 與它的絕對路徑
// （後者供測試直接建檔、建連結）。
func newWorkspace(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return root, dir
}

// writeWorkspaceFile 在 Workspace 內建一個真實檔案（父目錄一併建出來）。
func writeWorkspaceFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建立 %s 的父目錄: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %s: %v", rel, err)
	}
	return path
}

// symlink 建一條真的符號連結；建不起來的環境（Windows 未開開發者模式）整條測試跳過，
// 形狀沿用 internal/memory 的 symlinkTest。
func symlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
}

// newReadFile 以指定白名單組出 read_file Tool，Workspace 是傳進來的那個 root。
func newReadFile(root *os.Root, allowedPaths []string) tool.OryxTool {
	return tool.NewReadFile(tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: allowedPaths}), root)
}

// TestReadFileReadsRealFile 是 happy path：白名單內的相對路徑真的把檔案內容讀回來。
func TestReadFileReadsRealFile(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "買牛奶\n寫 spec\n")

	result := newReadFile(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/todo.md"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	var out readFileOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON（%q）: %v", result.Content, err)
	}
	if out.Content != "買牛奶\n寫 spec\n" {
		t.Errorf("content = %q, 期望讀回真實檔案內容", out.Content)
	}
	if out.Truncated {
		t.Errorf("小檔不該標示截斷: %+v", out)
	}
}

// TestReadFileTruncatesOversizeContent 驗證超過上限時**截斷並標示**——讓 LLM 知道
// 自己只看到一部分，不會據殘缺內容下結論。上限沿用既有的 maxResponseBytes（1 MiB），
// 這裡以字面值釘住那個對外契約。
func TestReadFileTruncatesOversizeContent(t *testing.T) {
	const limit = 1 << 20
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("logs", "big.log"), strings.Repeat("a", limit+512))

	result := newReadFile(root, []string{"logs"}).Execute(context.Background(), `{"path":"logs/big.log"}`)
	if !result.OK {
		t.Fatalf("截斷不算失敗，實際錯誤: %s", result.Error)
	}

	var out readFileOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON: %v", err)
	}
	if !out.Truncated {
		t.Errorf("超過上限卻沒標示截斷: %d bytes", len(out.Content))
	}
	if len(out.Content) != limit {
		t.Errorf("content = %d bytes, 期望截到 %d bytes", len(out.Content), limit)
	}
}

// TestReadFileTruncatesOnRuneBoundary 驗證截斷切在**完整字元**的邊界上。
//
// 直接切第 1048576 個位元組會把一個中文字切成兩半，json.Marshal 接著把那半個字換成
// U+FFFD——LLM 讀到的最後一個字是壞的。中日韓文字每個 3 個位元組，切中的機率是
// 三分之二，對這個專案不是邊角案例。
//
// 用「好」填到剛好跨過上限：1048576 不是 3 的倍數，所以裸切一定切在字元中間。
func TestReadFileTruncatesOnRuneBoundary(t *testing.T) {
	const limit = 1 << 20
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "cjk.md"), strings.Repeat("好", limit/3+16))

	result := newReadFile(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/cjk.md"}`)
	if !result.OK {
		t.Fatalf("截斷不算失敗，實際錯誤: %s", result.Error)
	}
	var out readFileOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON: %v", err)
	}
	if !out.Truncated {
		t.Fatalf("超過上限卻沒標示截斷: %d bytes", len(out.Content))
	}
	if !utf8.ValidString(out.Content) {
		t.Errorf("截斷後的內容不是合法 UTF-8——結尾被切成半個字元")
	}
	if strings.ContainsRune(out.Content, utf8.RuneError) {
		t.Errorf("截斷後的內容含 U+FFFD，LLM 讀到的最後一個字是壞的")
	}
	if len(out.Content) > limit {
		t.Errorf("content = %d bytes, 不得超過上限 %d", len(out.Content), limit)
	}
}

// TestReadFileResolvesAgainstWorkspaceNotProcessCwd 釘住**解析基準是 Workspace 根，
// 不是進程當下的工作目錄**。
//
// 這條非測不可：兩個目錄裡都有一份 notes/todo.md，只有基準錯了的實作會讀到 cwd 那份。
// 基準若隨啟動目錄浮動，同一份 config.yaml 在不同目錄下跑就有不同的允許範圍——那是
// 白名單最不該有的性質。
func TestReadFileResolvesAgainstWorkspaceNotProcessCwd(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "WORKSPACE 這一份")

	// 進程換到另一個目錄，那裡也有一份同名檔案（內容不同）。
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "notes"), 0o755); err != nil {
		t.Fatalf("建立 cwd 的 notes/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "notes", "todo.md"), []byte("CWD 這一份"), 0o644); err != nil {
		t.Fatalf("寫入 cwd 的 todo.md: %v", err)
	}
	t.Chdir(elsewhere)

	result := newReadFile(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/todo.md"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}
	var out readFileOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON: %v", err)
	}
	if out.Content != "WORKSPACE 這一份" {
		t.Errorf("content = %q, 期望讀到 Workspace 根底下那一份（基準不是進程 cwd）", out.Content)
	}
}

// TestReadFileRejectionMatrix 是開檔層的拒絕矩陣。每一格都斷言三件事：呼叫失敗、
// 錯誤訊息可辨識、**Retryable 一律 false**——不會因為重跑而改變結果的事情不該被
// ReAct 循環重跑（沿用 internal/tool/sandbox.go 已宣告的不可重試語義）。
func TestReadFileRejectionMatrix(t *testing.T) {
	root, dir := newWorkspace(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("Workspace 之外的機密"), 0o644); err != nil {
		t.Fatalf("建立 Workspace 外的檔案: %v", err)
	}

	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")
	writeWorkspaceFile(t, dir, filepath.Join("secrets", "api.txt"), "白名單外的機密")
	writeWorkspaceFile(t, dir, filepath.Join("notes", "real", "b.txt"), "中間元件測試的目標")

	// 三種符號連結，各自對應一個真的漏洞形態。
	symlink(t, filepath.Join(dir, "secrets", "api.txt"), filepath.Join(dir, "notes", "inside-link.md"))
	symlink(t, outsideFile, filepath.Join(dir, "notes", "outside-link.md"))
	// 中間元件的連結：只檢查最終目標的實作會**整個漏掉**這一格，而 os.Root 補不上
	// ——它只擋指到 Workspace 之外的連結，Workspace 之內的照樣跟隨。
	symlink(t, filepath.Join(dir, "notes", "real"), filepath.Join(dir, "notes", "link-dir"))

	tests := []struct {
		name       string
		allowed    []string
		input      string
		wantErrSub string
	}{
		{
			name:       "白名單外的路徑回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"secrets/api.txt"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "../ 穿越出白名單回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/../secrets/api.txt"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "絕對路徑回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"/etc/passwd"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "空白名單全部拒絕",
			allowed:    nil,
			input:      `{"path":"notes/todo.md"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "符號連結指向 Workspace 內也拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/inside-link.md"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結指向 Workspace 外拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/outside-link.md"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結在中間路徑元件一樣拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/link-dir/b.txt"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "目標是目錄回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes"}`,
			wantErrSub: "目錄",
		},
		{
			name:       "檔案不存在回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/absent.md"}`,
			wantErrSub: "不存在",
		},
		{
			name:       "輸入非 JSON",
			allowed:    []string{"notes"},
			input:      `not-json`,
			wantErrSub: "解析",
		},
		{
			name:       "缺 path 參數",
			allowed:    []string{"notes"},
			input:      `{}`,
			wantErrSub: "path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newReadFile(root, tt.allowed).Execute(context.Background(), tt.input)
			if result.OK {
				t.Fatalf("期望失敗，實際成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q", result.Error, tt.wantErrSub)
			}
			// Sandbox 拒絕與其他確定性失敗都不可重試。
			if result.Retryable {
				t.Errorf("Retryable = true, 期望 false（重跑不會改變結果）: %q", result.Error)
			}
		})
	}
}

// TestReadFileSymlinkDoesNotLeakOutsideContent 是上面矩陣那兩格的實質斷言：
// 拒絕不只要「回錯誤」，還要**真的沒把目標內容讀出來**。少了這條，一個先讀檔
// 再回錯誤的實作也會讓矩陣全綠。
func TestReadFileSymlinkDoesNotLeakOutsideContent(t *testing.T) {
	const secret = "OUTSIDE-SECRET-VALUE"
	root, dir := newWorkspace(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte(secret), 0o644); err != nil {
		t.Fatalf("建立 Workspace 外的檔案: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	symlink(t, outside, filepath.Join(dir, "notes", "link.md"))

	result := newReadFile(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/link.md"}`)
	if result.OK {
		t.Fatalf("符號連結不該被跟隨，實際回填: %s", result.Content)
	}
	if strings.Contains(result.Content, secret) || strings.Contains(result.Error, secret) {
		t.Errorf("回填內容洩漏了連結目標: content=%q error=%q", result.Content, result.Error)
	}
}

// TestReadFileContextCancelled 驗證取消當下就中止（憲法 5.3）。os.Root 的開檔與讀取
// 都不吃 context，所以取消要在進到那些呼叫**之前**收下來。
func TestReadFileContextCancelled(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := newReadFile(root, []string{"notes"}).Execute(ctx, `{"path":"notes/todo.md"}`)
	if result.OK {
		t.Fatalf("context 已取消，不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "取消") {
		t.Errorf("錯誤 %q 未說明是取消", result.Error)
	}
}

// TestReadFilePermissionDenied 驗證權限不足回明確錯誤（與「不存在」分得出來），
// 不是 panic、也不是空內容。root 身分讀得到任何檔案，那時這一格驗不到東西。
func TestReadFilePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時權限位不生效，這一格驗不到東西")
	}
	root, dir := newWorkspace(t)
	path := writeWorkspaceFile(t, dir, filepath.Join("notes", "locked.md"), "不給讀")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("拿掉讀取權限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	result := newReadFile(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/locked.md"}`)
	if result.OK {
		t.Fatalf("沒有讀取權限不該成功: %s", result.Content)
	}
	if strings.Contains(result.Error, "不存在") {
		t.Errorf("「沒權限」被報成「不存在」，兩者要分得出來: %q", result.Error)
	}
	if !strings.Contains(result.Error, "權限") {
		t.Errorf("錯誤 %q 未說明是權限問題", result.Error)
	}
}

// executeWithin 在期限內跑一次 Execute；逾時視為測試失敗（用於「不得阻塞」的斷言）。
func executeWithin(t *testing.T, d time.Duration, tl tool.OryxTool, input string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		result := tl.Execute(context.Background(), input)
		if result.OK {
			done <- ""
			return
		}
		done <- result.Error
	}()
	select {
	case errText := <-done:
		return errText
	case <-time.After(d):
		t.Fatalf("Execute 在 %v 內未返回——它阻塞住了", d)
		return ""
	}
}
