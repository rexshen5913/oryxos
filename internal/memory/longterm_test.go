// 長期記憶檔案層的單元測試。整份鏈路的行為由 internal/core 的整合測試從
// AgentService.Process seam 驅動；這裡補的是**單一 turn 的 seam 觀察不到**的
// 性質：條目邊界的組裝細節、並行追加、以及檔案操作不得越出 Workspace。
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryRelPath 是長期記憶檔在 Workspace 內的相對路徑。
const memoryRelPath = "memory/MEMORY.md"

// newTestMemory 在 t.TempDir() 開一個 Workspace 根，回傳長期記憶讀寫與該檔的
// 絕對路徑（供測試直接讀寫檔案做斷言）。
func newTestMemory(t *testing.T) (*LongTermMemory, string) {
	t.Helper()
	dir := t.TempDir()
	return openMemoryAt(t, dir), filepath.Join(dir, memoryRelPath)
}

// openMemoryAt 在 dir 上開一個**獨立**的 Workspace root 與長期記憶讀寫。對同一個
// dir 呼叫多次會得到彼此無關的實例（各自的 os.Root 與 mutex），用來模擬同一個
// Workspace 上並行的兩個 oryxos 進程。
func openMemoryAt(t *testing.T, dir string) *LongTermMemory {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return NewLongTermMemory(root, memoryRelPath)
}

// TestEntryBlock 是條目邊界的組裝矩陣：同一天共用日期 header、換日另起一段、
// 使用者手改後沒有換行收尾也接得上，且記憶內容裡自帶的 markdown 標題不會被
// 誤認成日期 header。
func TestEntryBlock(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		existing string
		entry    string
		want     string
	}{
		{
			name:     "首次寫入：起日期 header",
			existing: "",
			entry:    "使用者的專案用 Go 開發",
			want:     "## 2026-08-11\n\n- 使用者的專案用 Go 開發\n",
		},
		{
			name:     "同一天：共用既有 header，只補列表項",
			existing: "## 2026-08-11\n\n- 使用者的專案用 Go 開發\n",
			entry:    "部署在 K8s",
			want:     "- 部署在 K8s\n",
		},
		{
			name:     "換日：另起一段新的 header",
			existing: "## 2026-08-10\n\n- 使用者的專案用 Go 開發\n",
			entry:    "部署在 K8s",
			want:     "\n## 2026-08-11\n\n- 部署在 K8s\n",
		},
		{
			name:     "手改後沒有換行收尾：先補換行再接",
			existing: "## 2026-08-11\n\n- 使用者的專案用 Go 開發",
			entry:    "部署在 K8s",
			want:     "\n- 部署在 K8s\n",
		},
		{
			name:     "記憶內容自帶 markdown 標題：不當成日期 header",
			existing: "## 2026-08-11\n\n- 專案筆記\n## Go 服務\n",
			entry:    "部署在 K8s",
			want:     "- 部署在 K8s\n",
		},
		{
			name:     "只有非日期標題：視同沒有 header，另起一段",
			existing: "# 我的長期記憶\n",
			entry:    "部署在 K8s",
			want:     "\n## 2026-08-11\n\n- 部署在 K8s\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entryBlock(tt.existing, tt.entry, now); got != tt.want {
				t.Errorf("entryBlock = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestAppendPreservesExistingEntries 驗證追加不會動到既有內容——長期記憶的全部
// 價值就在於留得住，這條是 read-modify-write 覆寫式實作最容易毀掉的性質。
func TestAppendPreservesExistingEntries(t *testing.T) {
	mem, path := newTestMemory(t)
	entries := []string{"使用者的專案用 Go 開發", "部署在 K8s", "偏好繁體中文回覆"}
	for _, entry := range entries {
		if err := mem.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append(%q): %v", entry, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}
	for _, entry := range entries {
		if !strings.Contains(string(data), entry) {
			t.Errorf("MEMORY.md 遺失條目 %q: %s", entry, data)
		}
	}
	// 同一天寫的三條共用一個日期 header。
	if got := strings.Count(string(data), "## "); got != 1 {
		t.Errorf("日期 header 數 = %d, 期望 1（同一天共用）: %s", got, data)
	}
}

// TestAppendConcurrentAcrossInstances 驗證並行追加不遺失條目——這正是本張改用
// O_APPEND（而非「讀整檔、組新內容、覆寫」）所要換來的性質。
//
// **每個寫入者持有自己的 LongTermMemory**（自己的 os.Root 與 mutex），模擬同一個
// Workspace 上並行的兩個 oryxos 進程。共用一個實例是測不到東西的：實例內的 mutex
// 會把測試完全串行化，即使退回覆寫式寫入也照樣全綠。單一 turn 的 seam 同樣觀察
// 不到這件事——它要兩個以上的寫入者才成立。
func TestAppendConcurrentAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, memoryRelPath)
	const writers = 16

	mems := make([]*LongTermMemory, writers)
	for i := range writers {
		mems[i] = openMemoryAt(t, dir)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			errs[i] = mems[i].Append(context.Background(), fmt.Sprintf("並行條目-%02d", i))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("並行 Append(%d): %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}
	for i := range writers {
		if entry := fmt.Sprintf("並行條目-%02d", i); !strings.Contains(string(data), entry) {
			t.Errorf("並行追加遺失 %q: %s", entry, data)
		}
	}
}

// secretContent 是「不該被長期記憶讀走、也不該被它改寫」的目標檔內容。
const secretContent = "SUPER-SECRET-TOKEN"

// TestRejectsSymlinkedPath 驗證長期記憶只讀寫它自己那個固定目標：路徑上任何一段
// 是符號連結就拒絕跟隨。MEMORY.md 隨 Workspace 進 git，一個惡意 repo 或使用者的
// 一次手滑都可能讓它指向別的檔案——讀取端會把該檔內容注入 prompt 送往 Provider、
// 寫入端會把 markdown 追加進去把它弄壞。
//
// 越界的連結由 os.Root 擋，但 Workspace **內**的連結（如指向 profiles/default.yaml）
// os.Root 是照樣跟隨的，兩種都要測。
func TestRejectsSymlinkedPath(t *testing.T) {
	tests := []struct {
		name string
		// link 建好符號連結，回傳被指到、必須保持原樣的目標檔路徑。
		link func(t *testing.T, ws, memPath string) string
	}{
		{
			name: "最終檔連到 Workspace 外",
			link: func(t *testing.T, _, memPath string) string {
				target := filepath.Join(t.TempDir(), "secret.txt")
				writeTestFile(t, target, secretContent)
				mkdirTest(t, filepath.Dir(memPath))
				symlinkTest(t, target, memPath)
				return target
			},
		},
		{
			// 相對連結是關鍵：os.Root 本來就拒絕絕對的符號連結，但相對且
			// 指向 Workspace 內的連結它會照樣跟隨——這條只有 ensureNoSymlink 擋得住。
			name: "最終檔以相對路徑連到 Workspace 內的其他檔案",
			link: func(t *testing.T, ws, memPath string) string {
				target := filepath.Join(ws, "profiles", "default.yaml")
				mkdirTest(t, filepath.Dir(target))
				writeTestFile(t, target, secretContent)
				mkdirTest(t, filepath.Dir(memPath))
				symlinkTest(t, filepath.Join("..", "profiles", "default.yaml"), memPath)
				return target
			},
		},
		{
			name: "父目錄以相對路徑連到 Workspace 內的其他目錄",
			link: func(t *testing.T, ws, memPath string) string {
				target := filepath.Join(ws, "profiles", "default.yaml")
				mkdirTest(t, filepath.Dir(target))
				writeTestFile(t, target, secretContent)
				symlinkTest(t, "profiles", filepath.Dir(memPath))
				return target
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, memPath := newTestMemory(t)
			target := tt.link(t, mem.root.Name(), memPath)

			got, err := mem.Load(context.Background())
			if err == nil {
				t.Errorf("符號連結應讀取失敗，實際讀到 %q", got)
			}
			if strings.Contains(got, secretContent) {
				t.Errorf("目標檔內容被讀進長期記憶: %q", got)
			}

			if err := mem.Append(context.Background(), "覆寫測試"); err == nil {
				t.Error("符號連結應寫入失敗，實際成功")
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("讀取目標檔 %s: %v", target, err)
			}
			if string(data) != secretContent {
				t.Errorf("目標檔被改寫: %q", data)
			}
			// 連結指向的目錄裡也不該多出一個新檔。
			if _, err := os.Lstat(filepath.Join(filepath.Dir(target), "MEMORY.md")); err == nil {
				t.Errorf("經符號連結在 %s 建出了 MEMORY.md", filepath.Dir(target))
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("建立檔案 %s: %v", path, err)
	}
}

func mkdirTest(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建立目錄 %s: %v", dir, err)
	}
}

func symlinkTest(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
}
