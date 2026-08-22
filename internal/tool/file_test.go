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
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// writeFileOutput 是 write_file 回填給 LLM 的結果形狀，測試據此解回來斷言。
type writeFileOutput struct {
	BytesWritten int `json:"bytes_written"`
}

// newWriteFile 以指定白名單組出 write_file Tool，Workspace 是傳進來的那個 root。
func newWriteFile(root *os.Root, allowedPaths []string) tool.OryxTool {
	return tool.NewWriteFile(tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: allowedPaths}), root)
}

// TestWriteFileWritesRealFile 是 happy path：白名單內的相對路徑真的把內容寫上磁碟。
//
// **寫完真的讀回來比對**——只斷言 result.OK 的話，一個什麼都沒做卻回成功的實作照樣
// 全綠，而這條 Tool 存在的理由就是「關掉 chat 之後那個檔案還在」。
func TestWriteFileWritesRealFile(t *testing.T) {
	const content = "第一版摘要\n"
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/summary.md","content":"第一版摘要\n"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	var out writeFileOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON（%q）: %v", result.Content, err)
	}
	if out.BytesWritten != len(content) {
		t.Errorf("bytes_written = %d, 期望 %d", out.BytesWritten, len(content))
	}

	got, err := os.ReadFile(filepath.Join(dir, "notes", "summary.md"))
	if err != nil {
		t.Fatalf("讀回寫入的檔案: %v", err)
	}
	if string(got) != content {
		t.Errorf("磁碟上的內容 = %q, 期望 %q", got, content)
	}
}

// TestWriteFileOverwritesInsteadOfAppending 釘住**覆寫**的語義。
//
// 追加與覆寫從 result.OK 看起來一模一樣，差別只在磁碟上——所以斷言必須落在讀回來的
// 完整內容上，而且新內容要**比舊的短**：用等長或更長的內容，一個 O_APPEND 的實作也
// 可能矇混過去。追加的需求由 save_memory 那條專用鏈路承擔，不是這裡。
func TestWriteFileOverwritesInsteadOfAppending(t *testing.T) {
	root, dir := newWorkspace(t)
	path := writeWorkspaceFile(t, dir, filepath.Join("notes", "summary.md"), "很長的第一版內容，應該整段被蓋掉\n")

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/summary.md","content":"第二版\n"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀回被覆寫的檔案: %v", err)
	}
	if string(got) != "第二版\n" {
		t.Errorf("磁碟上的內容 = %q, 期望只剩新內容（覆寫不是追加）", got)
	}
}

// TestWriteFileWritesEmptyContent 驗證 content 給空字串是合法的「寫一個空檔」，
// 與下面矩陣裡「漏給 content」那格分得出來——後者是 LLM 漏填參數，不能靜靜地把
// 既有檔案清空。
func TestWriteFileWritesEmptyContent(t *testing.T) {
	root, dir := newWorkspace(t)
	path := writeWorkspaceFile(t, dir, filepath.Join("notes", "draft.md"), "舊內容")

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/draft.md","content":""}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀回檔案: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("磁碟上的內容 = %q, 期望空檔", got)
	}
}

// treeSnapshot 列出 dir 底下所有目錄的相對路徑（排序後），供「不自動建目錄」那格
// 比對前後差異。只收目錄：那一格要證明的是**沒有長出資料夾**。
func treeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var dirs []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return rerr
			}
			dirs = append(dirs, rel)
		}
		return nil
	}); err != nil {
		t.Fatalf("走訪 %s: %v", dir, err)
	}
	slices.Sort(dirs)
	return dirs
}

// TestWriteFileDoesNotCreateMissingParentDirs 是「父目錄不存在」那格的實質斷言。
//
// **只斷言「回了錯誤」不夠**：一個先 MkdirAll 再因為別的原因失敗的實作也會回錯誤，
// 卻已經在工作區裡長出一整串空資料夾——而使用者要的正是「路徑打錯不會長出一堆空
// 資料夾」。所以這裡比對呼叫前後磁碟上的目錄清單完全相同。
func TestWriteFileDoesNotCreateMissingParentDirs(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	before := treeSnapshot(t, dir)

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/2026/q3/report.md","content":"x"}`)
	if result.OK {
		t.Fatalf("父目錄不存在不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "不存在") {
		t.Errorf("錯誤 %q 未說明父目錄不存在", result.Error)
	}
	if result.Retryable {
		t.Errorf("Retryable = true, 期望 false（父目錄不會因為重跑而出現）: %q", result.Error)
	}

	after := treeSnapshot(t, dir)
	if !slices.Equal(before, after) {
		t.Errorf("磁碟上多出了目錄：呼叫前 %v，呼叫後 %v——write_file 不得自動建目錄", before, after)
	}
}

// TestWriteFileSymlinkDoesNotClobberTarget 是符號連結那格的實質斷言：拒絕不只要
// 「回錯誤」，還要**真的沒把連結目標寫壞**。
//
// 連結刻意用**相對**寫法。os.Root 對絕對連結一律拒絕（「Symbolic links must not be
// absolute」），拿絕對連結來測，過的是 os.Root 那一關，驗不到我們自己的檢查有沒有
// 接上寫入路徑；相對連結它**會跟隨**，此時擋下來的只可能是應用層那道。而 O_TRUNC
// 在開檔當下就把目標清空，錯誤訊息回填得再漂亮，檔案都已經沒了。
func TestWriteFileSymlinkDoesNotClobberTarget(t *testing.T) {
	const original = "白名單外、不該被動到的內容"
	root, dir := newWorkspace(t)
	target := writeWorkspaceFile(t, dir, filepath.Join("secrets", "api.txt"), original)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	symlink(t, filepath.Join("..", "secrets", "api.txt"), filepath.Join(dir, "notes", "link.md"))

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/link.md","content":"被覆寫了"}`)
	if result.OK {
		t.Fatalf("符號連結不該被跟隨，實際回填: %s", result.Content)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("讀回連結目標: %v", err)
	}
	if string(got) != original {
		t.Errorf("連結目標被寫壞了：內容 = %q, 期望維持 %q", got, original)
	}
}

// TestWriteFileRejectionMatrix 是寫入路徑的拒絕矩陣。
//
// 前四格是 #30 的 CheckFilePath **被套用在寫入路徑上**的斷言——校驗邏輯本票不重寫，
// 但「它有沒有被接上去」是本票的事：一個漏呼叫校驗器的 write_file 會讓 read_file 的
// 矩陣照樣全綠。符號連結與非普通檔同理。
//
// 每一格都斷言三件事：呼叫失敗、錯誤訊息可辨識、**Retryable 一律 false**——這些失敗
// 重跑一次結果都一樣。
func TestWriteFileRejectionMatrix(t *testing.T) {
	root, dir := newWorkspace(t)
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("Workspace 之外"), 0o644); err != nil {
		t.Fatalf("建立 Workspace 外的檔案: %v", err)
	}

	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")
	writeWorkspaceFile(t, dir, filepath.Join("secrets", "api.txt"), "白名單外的機密")
	writeWorkspaceFile(t, dir, filepath.Join("notes", "real", "b.txt"), "中間元件測試的目標")
	symlink(t, filepath.Join(dir, "secrets", "api.txt"), filepath.Join(dir, "notes", "inside-link.md"))
	symlink(t, outsideFile, filepath.Join(dir, "notes", "outside-link.md"))
	symlink(t, filepath.Join(dir, "notes", "real"), filepath.Join(dir, "notes", "link-dir"))
	// 相對連結是 os.Root **會跟隨**的那一種，因此也是只有應用層檢查擋得住的那一種。
	symlink(t, filepath.Join("..", "secrets", "api.txt"), filepath.Join(dir, "notes", "rel-link.md"))

	tests := []struct {
		name       string
		allowed    []string
		input      string
		wantErrSub string
	}{
		{
			name:       "白名單外的路徑回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"secrets/api.txt","content":"x"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "../ 穿越出白名單回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/../secrets/api.txt","content":"x"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "絕對路徑回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"/tmp/anywhere.txt","content":"x"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "空白名單全部拒絕",
			allowed:    nil,
			input:      `{"path":"notes/todo.md","content":"x"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "符號連結指向 Workspace 內也拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/inside-link.md","content":"x"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結指向 Workspace 外拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/outside-link.md","content":"x"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結在中間路徑元件一樣拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/link-dir/b.txt","content":"x"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "相對符號連結（os.Root 會跟隨的那一種）同樣拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/rel-link.md","content":"x"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "目標是目錄回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/real","content":"x"}`,
			wantErrSub: "目錄",
		},
		{
			name:       "父目錄不存在回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/missing/f.md","content":"x"}`,
			wantErrSub: "不存在",
		},
		{
			name:       "父路徑上是一個檔案而不是目錄",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/todo.md/child.md","content":"x"}`,
			wantErrSub: "目錄",
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
			input:      `{"content":"x"}`,
			wantErrSub: "path",
		},
		{
			name:       "缺 content 參數不得靜靜把既有檔案清空",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/todo.md"}`,
			wantErrSub: "content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newWriteFile(root, tt.allowed).Execute(context.Background(), tt.input)
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

	// 矩陣跑完，白名單外那份檔案的內容必須一個位元組都沒變。
	got, err := os.ReadFile(filepath.Join(dir, "secrets", "api.txt"))
	if err != nil {
		t.Fatalf("讀回白名單外的檔案: %v", err)
	}
	if string(got) != "白名單外的機密" {
		t.Errorf("白名單外的檔案被寫壞了: %q", got)
	}
}

// TestWriteFileRejectsOversizeContent 驗證超過上限時**明確拒絕而不是靜默截斷**。
//
// 這條與 read_file 的截斷刻意不對稱，理由寫在斷言裡：讀截斷是安全的（少看到一段，
// 而且有 truncated 標記），寫截斷會在磁碟上留下一個**內容不完整卻回報成功**的檔案，
// 而覆寫的語義讓原內容同時也沒了。所以除了「回錯誤」，還要斷言**既有檔案一個位元組
// 都沒被動到**。
func TestWriteFileRejectsOversizeContent(t *testing.T) {
	const limit = 1 << 20
	const original = "原本的內容，不該被動到"
	root, dir := newWorkspace(t)
	path := writeWorkspaceFile(t, dir, filepath.Join("notes", "big.md"), original)

	oversize, err := json.Marshal(map[string]string{
		"path":    "notes/big.md",
		"content": strings.Repeat("a", limit+1),
	})
	if err != nil {
		t.Fatalf("組輸入參數: %v", err)
	}

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(), string(oversize))
	if result.OK {
		t.Fatalf("超過上限不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "上限") {
		t.Errorf("錯誤 %q 未說明是超過上限", result.Error)
	}
	if result.Retryable {
		t.Errorf("Retryable = true, 期望 false（內容不會因為重跑而變短）: %q", result.Error)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀回既有檔案: %v", err)
	}
	if string(got) != original {
		t.Errorf("既有檔案被動到了：內容 = %q, 期望維持 %q（不得靜默截斷）", got, original)
	}
}

// TestWriteFileContextCancelled 驗證取消當下就中止（憲法 5.3）。os.Root 的開檔與寫入
// 都不吃 context，所以取消要在進到那些呼叫**之前**收下來——而且要斷言**檔案沒被建
// 出來**，否則一個「先寫再檢查取消」的實作照樣通過。
func TestWriteFileContextCancelled(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := newWriteFile(root, []string{"notes"}).Execute(ctx, `{"path":"notes/x.md","content":"x"}`)
	if result.OK {
		t.Fatalf("context 已取消，不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "取消") {
		t.Errorf("錯誤 %q 未說明是取消", result.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes", "x.md")); !os.IsNotExist(err) {
		t.Errorf("取消之後檔案仍被建了出來（stat err = %v）", err)
	}
}

// listDirEntry／listDirOutput 是 list_dir 回填給 LLM 的結果形狀，測試據此解回來斷言。
type listDirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type listDirOutput struct {
	Entries   []listDirEntry `json:"entries"`
	Truncated bool           `json:"truncated"`
}

// newListDir 以指定白名單組出 list_dir Tool，Workspace 是傳進來的那個 root。
func newListDir(root *os.Root, allowedPaths []string) tool.OryxTool {
	return tool.NewListDir(tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: allowedPaths}), root)
}

// decodeListDir 把回填內容解成 listDirOutput。
func decodeListDir(t *testing.T, content string) listDirOutput {
	t.Helper()
	var out listDirOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON（%q）: %v", content, err)
	}
	return out
}

// TestListDirListsRealDirectory 是 happy path，跑在一棵**真的**目錄樹上：有子目錄、
// 有檔案、有符號連結（憲法 4.3）。
//
// 三個欄位（名稱、是否為目錄、大小）都要斷言得到，因為它們是 Agent 決定「下一步讀
// 哪一個」的全部依據——少了 is_dir 它會拿一個目錄去餵 read_file，少了 size 它挑不出
// 該不該讀那份 800 MB 的 log。
//
// 符號連結那一格順帶釘住型別判定走的是 **Lstat**：連結指向一個目錄，而回填的
// is_dir 必須是 false。跟隨連結去判型別等於幫 LLM 把一條它不該跨過的界線跨過去。
func TestListDirListsRealDirectory(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("project", "README.md"), "# oryxos\n")
	if err := os.MkdirAll(filepath.Join(dir, "project", "src"), 0o755); err != nil {
		t.Fatalf("建立 project/src/: %v", err)
	}
	symlink(t, filepath.Join(dir, "project", "src"), filepath.Join(dir, "project", "link-to-src"))

	result := newListDir(root, []string{"project"}).Execute(context.Background(), `{"path":"project"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	out := decodeListDir(t, result.Content)
	if out.Truncated {
		t.Errorf("三個條目不該標示截斷: %+v", out)
	}
	if len(out.Entries) != 3 {
		t.Fatalf("條目數 = %d, 期望 3（README.md、link-to-src、src）: %+v", len(out.Entries), out.Entries)
	}
	// 回填順序按名稱排序，讓同一個目錄每次列出來都一樣——LLM 據清單挑檔案時，
	// 順序飄動會讓同一句話得到不同的下一步。
	if got := []string{out.Entries[0].Name, out.Entries[1].Name, out.Entries[2].Name}; !slices.Equal(
		got, []string{"README.md", "link-to-src", "src"}) {
		t.Errorf("條目名稱 = %v, 期望按名稱排序的 [README.md link-to-src src]", got)
	}
	if out.Entries[0].IsDir {
		t.Errorf("README.md 的 is_dir = true, 期望 false: %+v", out.Entries[0])
	}
	if want := int64(len("# oryxos\n")); out.Entries[0].Size != want {
		t.Errorf("README.md 的 size = %d, 期望磁碟上的真實大小 %d", out.Entries[0].Size, want)
	}
	if out.Entries[1].IsDir {
		t.Errorf("指向目錄的符號連結 is_dir = true, 期望 false（型別判定不跟隨連結）: %+v", out.Entries[1])
	}
	if !out.Entries[2].IsDir {
		t.Errorf("src 的 is_dir = false, 期望 true: %+v", out.Entries[2])
	}
}

// TestListDirListsWorkspaceRoot 驗證白名單條目是 Workspace 根本身（`.`）時列得出東西。
// rel 在這條路徑上是 "."，而 statNoSymlink 是逐段切路徑的——這一格防的是它在單一
// 「.」元件上失手。
func TestListDirListsWorkspaceRoot(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, "top.txt", "x")

	result := newListDir(root, []string{"."}).Execute(context.Background(), `{"path":"."}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}
	out := decodeListDir(t, result.Content)
	if len(out.Entries) != 1 || out.Entries[0].Name != "top.txt" {
		t.Errorf("條目 = %+v, 期望只有 top.txt", out.Entries)
	}
}

// TestListDirTruncatesAtEntryLimit 驗證**條目上限 1000**：超出時截斷並明確標示，
// 讓一個上萬檔的目錄不會把整份清單塞進 prompt。
//
// 上限以字面值 1000 釘住——那是 spec #4 定案的對外契約（issue #29「回填給 LLM 的
// 結果形狀」），不是可以隨手調的實作細節。
func TestListDirTruncatesAtEntryLimit(t *testing.T) {
	const limit = 1000
	root, dir := newWorkspace(t)
	big := filepath.Join(dir, "many")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatalf("建立 many/: %v", err)
	}
	for i := range limit + 1 {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("建立第 %d 個檔案: %v", i, err)
		}
	}

	result := newListDir(root, []string{"many"}).Execute(context.Background(), `{"path":"many"}`)
	if !result.OK {
		t.Fatalf("截斷不算失敗，實際錯誤: %s", result.Error)
	}
	out := decodeListDir(t, result.Content)
	if len(out.Entries) != limit {
		t.Errorf("條目數 = %d, 期望截到上限 %d", len(out.Entries), limit)
	}
	if !out.Truncated {
		t.Error("truncated = false, 期望 true——LLM 必須知道自己只看到一部分")
	}
}

// TestListDirDoesNotTruncateExactlyAtLimit 是上一格的對照：**剛好等於上限**不算截斷。
// 少了這一格，一個「>= limit 就標截斷」的差一錯誤不會被測出來。
func TestListDirDoesNotTruncateExactlyAtLimit(t *testing.T) {
	const limit = 1000
	root, dir := newWorkspace(t)
	exact := filepath.Join(dir, "exact")
	if err := os.MkdirAll(exact, 0o755); err != nil {
		t.Fatalf("建立 exact/: %v", err)
	}
	for i := range limit {
		if err := os.WriteFile(filepath.Join(exact, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("建立第 %d 個檔案: %v", i, err)
		}
	}

	result := newListDir(root, []string{"exact"}).Execute(context.Background(), `{"path":"exact"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}
	out := decodeListDir(t, result.Content)
	if len(out.Entries) != limit || out.Truncated {
		t.Errorf("條目數 = %d truncated = %v, 期望 %d 且不標截斷", len(out.Entries), out.Truncated, limit)
	}
}

// TestListDirRejectionMatrix 是列目錄路徑上的拒絕矩陣。
//
// 前四格的作用是**斷言 CheckFilePath 確實被套用在列目錄這條路徑上**——白名單校驗
// 加在 read_file 上不代表 list_dir 也走了同一關，而少了這關 Agent 能靠列目錄把整個
// Workspace 的檔名清單掃出來（那正是「哪裡有東西可讀」的地圖）。
//
// 符號連結三格則釘住「每一段元件」的檢查在這條路徑上同樣生效，不是只看最終目標。
func TestListDirRejectionMatrix(t *testing.T) {
	root, dir := newWorkspace(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("Workspace 之外"), 0o644); err != nil {
		t.Fatalf("建立 Workspace 外的檔案: %v", err)
	}

	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")
	writeWorkspaceFile(t, dir, filepath.Join("secrets", "api.txt"), "白名單外的機密")
	if err := os.MkdirAll(filepath.Join(dir, "notes", "real"), 0o755); err != nil {
		t.Fatalf("建立 notes/real/: %v", err)
	}

	// 三種符號連結，各自對應一個真的漏洞形態（形狀沿用 read_file 的同一組）。
	symlink(t, filepath.Join(dir, "secrets"), filepath.Join(dir, "notes", "inside-link"))
	symlink(t, outsideDir, filepath.Join(dir, "notes", "outside-link"))
	symlink(t, filepath.Join(dir, "notes", "real"), filepath.Join(dir, "notes", "link-dir"))

	tests := []struct {
		name       string
		allowed    []string
		input      string
		wantErrSub string
	}{
		{
			name:       "白名單外的目錄回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"secrets"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "../ 穿越出白名單回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/../secrets"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "絕對路徑回 SandboxViolation",
			allowed:    []string{"notes"},
			input:      `{"path":"/etc"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "空白名單全部拒絕",
			allowed:    nil,
			input:      `{"path":"notes"}`,
			wantErrSub: "SandboxViolation",
		},
		{
			name:       "符號連結指向 Workspace 內的目錄也拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/inside-link"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結指向 Workspace 外的目錄拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/outside-link"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "符號連結在中間路徑元件一樣拒絕",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/link-dir/sub"}`,
			wantErrSub: "符號連結",
		},
		{
			name:       "目標是普通檔回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/todo.md"}`,
			wantErrSub: "普通檔",
		},
		{
			name:       "目錄不存在回明確錯誤",
			allowed:    []string{"notes"},
			input:      `{"path":"notes/absent"}`,
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
			result := newListDir(root, tt.allowed).Execute(context.Background(), tt.input)
			if result.OK {
				t.Fatalf("期望失敗，實際成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q", result.Error, tt.wantErrSub)
			}
			// Sandbox 拒絕與其他確定性失敗都不可重試——重跑一次結果都一樣，
			// 讓 ReAct 循環退避重試只是白白多燒兩輪。
			if result.Retryable {
				t.Errorf("Retryable = true, 期望 false（重跑不會改變結果）: %q", result.Error)
			}
		})
	}
}

// TestListDirSandboxRejectionLeaksNoNames 是上面矩陣那幾格的實質斷言：拒絕不只要
// 「回錯誤」，還要**真的沒把目錄內容列出來**。少了這條，一個先列目錄再回錯誤的實作
// 也會讓矩陣全綠——而 list_dir 洩漏的正是檔名，也就是「哪裡有東西可讀」的地圖。
func TestListDirSandboxRejectionLeaksNoNames(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("secrets", "prod-api-key.txt"), "值不重要，檔名才是這格要防的")

	result := newListDir(root, []string{"notes"}).Execute(context.Background(), `{"path":"secrets"}`)
	if result.OK {
		t.Fatalf("白名單外的目錄不該列得出來: %s", result.Content)
	}
	if strings.Contains(result.Content, "prod-api-key") || strings.Contains(result.Error, "prod-api-key") {
		t.Errorf("回填洩漏了白名單外的檔名: content=%q error=%q", result.Content, result.Error)
	}
}

// TestFileToolTypeErrorsAreMirrored 驗證兩個 Tool 的型別錯誤**互為鏡像**：兩邊都說得出
// 實際型別，也都指向另一個 Tool。
//
// 這一格存在的理由是回填鏈路，不是措辭潔癖：LLM 拿目錄去餵 read_file 是常見的一步，
// 而它的下一步全靠這句錯誤訊息。「不是檔案」只說了否定的一半，LLM 得自己猜要改用
// 哪個 Tool——多繞一輪就是多一次 LLM 呼叫。
func TestFileToolTypeErrorsAreMirrored(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")

	tests := []struct {
		name      string
		tl        tool.OryxTool
		input     string
		wantType  string // 訊息要說得出目標的**實際**型別
		wantOther string // 也要指向另一個 Tool
	}{
		{
			name:      "read_file 指向目錄時提示改用 list_dir",
			tl:        newReadFile(root, []string{"notes"}),
			input:     `{"path":"notes"}`,
			wantType:  "目錄",
			wantOther: tool.ListDirToolName,
		},
		{
			name:      "list_dir 指向普通檔時提示改用 read_file",
			tl:        newListDir(root, []string{"notes"}),
			input:     `{"path":"notes/todo.md"}`,
			wantType:  "普通檔",
			wantOther: tool.ReadFileToolName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.tl.Execute(context.Background(), tt.input)
			if result.OK {
				t.Fatalf("型別不符不該成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantType) {
				t.Errorf("錯誤 %q 未說出實際型別 %q", result.Error, tt.wantType)
			}
			if !strings.Contains(result.Error, tt.wantOther) {
				t.Errorf("錯誤 %q 未指向該改用的 %s", result.Error, tt.wantOther)
			}
			if result.Retryable {
				t.Errorf("Retryable = true, 期望 false: %q", result.Error)
			}
		})
	}
}

// TestListDirPermissionDenied 驗證權限不足回明確錯誤（與「不存在」分得出來）。
// 目錄要列得出內容需要 r 權限，拿掉它就是這一格的情境。
func TestListDirPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時權限位不生效，這一格驗不到東西")
	}
	root, dir := newWorkspace(t)
	locked := filepath.Join(dir, "notes", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("建立 notes/locked/: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("拿掉目錄權限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	result := newListDir(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/locked"}`)
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

// TestListDirContextCancelled 驗證取消當下就中止（憲法 5.3）。os.Root 的開檔與
// ReadDir 都不吃 context，所以取消要在進到那些呼叫**之前**收下來。
func TestListDirContextCancelled(t *testing.T) {
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "todo.md"), "ok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := newListDir(root, []string{"notes"}).Execute(ctx, `{"path":"notes"}`)
	if result.OK {
		t.Fatalf("context 已取消，不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "取消") {
		t.Errorf("錯誤 %q 未說明是取消", result.Error)
	}
}

// TestListDirListsEmptyDirectory 是邊界格：空目錄回**空清單**，不是錯誤。
//
// 這條路徑上 ReadDir 會回 io.EOF，而 io.EOF 在這裡不是失敗——把它當錯誤處理的實作會
// 讓「這個資料夾是空的」變成一則錯誤訊息，LLM 收到後只會去猜是不是路徑打錯了。
func TestListDirListsEmptyDirectory(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes", "empty"), 0o755); err != nil {
		t.Fatalf("建立 notes/empty/: %v", err)
	}

	result := newListDir(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/empty"}`)
	if !result.OK {
		t.Fatalf("空目錄不算失敗，實際錯誤: %s", result.Error)
	}
	out := decodeListDir(t, result.Content)
	if len(out.Entries) != 0 || out.Truncated {
		t.Errorf("條目 = %+v truncated = %v, 期望空清單且不標截斷", out.Entries, out.Truncated)
	}
	// 空清單要編成 `[]` 而不是 `null`：LLM 那一側 null 讀起來像「這個欄位沒有資料」，
	// 與「這個資料夾是空的」是兩件事。
	if !strings.Contains(result.Content, `"entries":[]`) {
		t.Errorf("回填 = %s, 期望空清單編成 []（不是 null）", result.Content)
	}
}

// TestListDirPermissionDeniedOnEntries 是權限不足的**第二種形態**，與上一格不同因：
// 目錄可讀（r）但不可搜尋（x）。這時 ReadDir 讀得到全部檔名，但每個項目的 Lstat 都會
// 回 EACCES——「拿得到名字、拿不到 metadata」。
//
// 兩件事一起釘住：
//
//  1. 回**乾淨的權限錯誤**，不是把 Lstat 的原始錯誤原樣拋出去。少了這條，回填給 LLM
//     並落進 tool_invocations 的會是一句帶**絕對路徑**的 lstat 錯誤，而這個檔案其餘每
//     一條訊息都只提相對路徑。
//  2. 不回一份 size 全是 0 的假清單。這個目錄底下的檔案**一個都讀不到**（開檔同樣要
//     搜尋權限），列出來只會讓 LLM 拿去 read_file 然後撞同一道牆。
func TestListDirPermissionDeniedOnEntries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時權限位不生效，這一格驗不到東西")
	}
	root, dir := newWorkspace(t)
	writeWorkspaceFile(t, dir, filepath.Join("notes", "unsearchable", "a.txt"), "hi")
	target := filepath.Join(dir, "notes", "unsearchable")
	if err := os.Chmod(target, 0o444); err != nil { // 可讀、不可搜尋
		t.Fatalf("拿掉目錄的搜尋權限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })

	result := newListDir(root, []string{"notes"}).Execute(context.Background(), `{"path":"notes/unsearchable"}`)
	if result.OK {
		t.Fatalf("沒有搜尋權限不該回填一份清單: %s", result.Content)
	}
	if !strings.Contains(result.Error, "權限") {
		t.Errorf("錯誤 %q 未說明是權限問題", result.Error)
	}
	if strings.Contains(result.Error, dir) {
		t.Errorf("錯誤訊息洩漏了 Workspace 的絕對路徑（其餘每條訊息都只提相對路徑）: %q", result.Error)
	}
	if result.Retryable {
		t.Errorf("Retryable = true, 期望 false（權限位不會因為多等三秒而改變）: %q", result.Error)
	}
}
