// PATH 解析與「PATH ／ file.allowed_paths 重疊」兩組檢查的行為矩陣。
//
// 兩者都用**真的**檔案系統（`t.TempDir()`、真的 `os.Symlink`）：重疊檢查要防的正是
// 「PATH 寫 /opt/tools、而它是指向 Workspace 內某目錄的符號連結」這種形態，那在假的
// 檔案系統上驗不出來。
package tool_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestEffectivePathDirs 是 PATH 切段的矩陣。
//
// **相對段與空段一律丟掉，這是拒絕不是忽略**（ADR-0005）：它們的語義依賴當下的工作
// 目錄，而 oryxos 進程與 shell 子進程的工作目錄**必然不同**（後者固定在 Workspace
// 根）——同一段字串在兩邊指向不同的目錄。留著它就是留一個無法自圓其說的分支。
//
// 「拒絕不是忽略」的具體後果由 TestShellCommandOnlyInDroppedSegmentIsNotFound 驗：
// 命令只在被丟掉的段裡找得到時，結果是明確的「找不到該程式」，不是悄悄從別處執行。
func TestEffectivePathDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	abs := func(parts ...string) string {
		return filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
	}

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "純絕對段原樣保留且保持順序",
			raw:  abs("usr", "bin") + sep + abs("bin"),
			want: []string{abs("usr", "bin"), abs("bin")},
		},
		{
			name: "相對段丟掉",
			raw:  "./bin" + sep + abs("usr", "bin") + sep + "node_modules/.bin",
			want: []string{abs("usr", "bin")},
		},
		{
			// POSIX 上空段等於當前目錄——頭、尾、中間三個位置都要丟。
			name: "空段丟掉（頭尾與中間）",
			raw:  sep + abs("usr", "bin") + sep + sep + abs("bin") + sep,
			want: []string{abs("usr", "bin"), abs("bin")},
		},
		{
			name: "純空白段丟掉",
			raw:  "   " + sep + abs("usr", "bin"),
			want: []string{abs("usr", "bin")},
		},
		{
			name: "PATH 為空時是空清單",
			raw:  "",
			want: []string{},
		},
		{
			name: "整份 PATH 都是相對段時是空清單",
			raw:  "./bin" + sep + "bin",
			want: []string{},
		},
		{
			name: "段內的 .. 標準化後保留",
			raw:  abs("usr", "local", "..", "bin"),
			want: []string{abs("usr", "bin")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tool.EffectivePathDirs(tt.raw)
			if !slices.Equal(got, tt.want) {
				t.Errorf("EffectivePathDirs(%q) = %v, 期望 %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestPathDirsOverlappingAllowedPaths 是**內部提權路徑**的偵測矩陣（ADR-0005 第一條）。
//
// 要防的是這件事：父進程的 PATH 若含有一個落在 `file.allowed_paths` 之內的目錄，
// Agent 光靠**已被授權的 `write_file`** 就能在那裡放一個與白名單命令同名的檔案、或
// 覆寫該目錄下既有的可執行檔——**把「寫檔權限」升級成「執行白名單內程式的權限」**。
// 兩個能力都是使用者自己開的，所以「攻擊者本來就能寫檔了」那套論證在此不成立。
//
// 三格各自對應一種會被做錯的比對方式：
//
//   - 絕對目錄落在白名單內 → 最基本的一格。
//   - **符號連結**：PATH 寫 /opt/tools、它連到 Workspace 內某目錄 → 純字串比對會漏。
//   - **子樹前綴假匹配**：PATH 有 /tmp/foobar、白名單有 /tmp/foo → **不得**警告。
//     用 strings.HasPrefix 實作的版本會在這一格誤報。
func TestPathDirsOverlappingAllowedPaths(t *testing.T) {
	// evalWs 是 Workspace 根化到真實路徑後的樣子。macOS 的 t.TempDir() 在
	// /var/folders/... 而 /var 本身是指向 /private/var 的連結，不化開的話**每一格**
	// 都會假性重疊。
	newWs := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		real, err := filepath.EvalSymlinks(ws)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", ws, err)
		}
		return real
	}

	t.Run("PATH 含落在白名單內的絕對目錄要警告", func(t *testing.T) {
		ws := newWs(t)
		binDir := filepath.Join(ws, "scripts", "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("建立 scripts/bin: %v", err)
		}

		got := tool.PathDirsOverlappingAllowedPaths([]string{binDir}, []string{"scripts"}, ws)
		if len(got) != 1 || got[0] != binDir {
			t.Errorf("重疊清單 = %v, 期望 [%s]", got, binDir)
		}
	})

	t.Run("PATH 是指向白名單內目錄的符號連結也要警告", func(t *testing.T) {
		ws := newWs(t)
		binDir := filepath.Join(ws, "scripts", "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("建立 scripts/bin: %v", err)
		}
		// 連結本身放在 Workspace **之外**：字面上完全看不出重疊。
		link := filepath.Join(newWs(t), "tools")
		if err := os.Symlink(binDir, link); err != nil {
			t.Skipf("此環境不支援建立符號連結: %v", err)
		}

		got := tool.PathDirsOverlappingAllowedPaths([]string{link}, []string{"scripts"}, ws)
		if len(got) != 1 || got[0] != link {
			t.Errorf("重疊清單 = %v, 期望 [%s]（比對前兩邊都要 EvalSymlinks）", got, link)
		}
	})

	t.Run("子樹前綴假匹配不得警告", func(t *testing.T) {
		ws := newWs(t)
		for _, rel := range []string{"foo", "foobar"} {
			if err := os.MkdirAll(filepath.Join(ws, rel), 0o755); err != nil {
				t.Fatalf("建立 %s: %v", rel, err)
			}
		}

		got := tool.PathDirsOverlappingAllowedPaths([]string{filepath.Join(ws, "foobar")}, []string{"foo"}, ws)
		if len(got) != 0 {
			t.Errorf("重疊清單 = %v, 期望空（foobar 不在 foo 這棵子樹內，判準是子樹包含不是字串前綴）", got)
		}
	})

	t.Run("白名單為空時沒有重疊", func(t *testing.T) {
		ws := newWs(t)
		got := tool.PathDirsOverlappingAllowedPaths([]string{filepath.Join(ws, "bin")}, nil, ws)
		if len(got) != 0 {
			t.Errorf("重疊清單 = %v, 期望空", got)
		}
	})

	t.Run("Workspace 之外的 PATH 目錄不算重疊", func(t *testing.T) {
		ws := newWs(t)
		if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
			t.Fatalf("建立 scripts: %v", err)
		}
		outside := newWs(t)

		got := tool.PathDirsOverlappingAllowedPaths([]string{outside}, []string{"scripts"}, ws)
		if len(got) != 0 {
			t.Errorf("重疊清單 = %v, 期望空", got)
		}
	})

	t.Run("白名單條目是 Workspace 根本身時整個 Workspace 都算重疊", func(t *testing.T) {
		ws := newWs(t)
		binDir := filepath.Join(ws, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("建立 bin: %v", err)
		}

		got := tool.PathDirsOverlappingAllowedPaths([]string{binDir}, []string{"."}, ws)
		if len(got) != 1 {
			t.Errorf("重疊清單 = %v, 期望偵測到 %s", got, binDir)
		}
	})
}

// TestPathDirsOverlappingIgnoresUnresolvableEntries 釘住一條刻意的降級：路徑化不開
// （不存在、沒權限）時**不當成錯誤，改用標準化後的絕對路徑比對**。
//
// 理由是這個檢查只是一行啟動警告，不該因為 PATH 上有一個不存在的目錄就整個失效——
// 而字面重疊仍然值得警告。
func TestPathDirsOverlappingIgnoresUnresolvableEntries(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	absent := filepath.Join(ws, "scripts", "not-created-yet")

	got := tool.PathDirsOverlappingAllowedPaths([]string{absent}, []string{"scripts"}, ws)
	if len(got) != 1 || !strings.Contains(got[0], "not-created-yet") {
		t.Errorf("重疊清單 = %v, 期望仍偵測到字面重疊的 %s", got, absent)
	}
}
