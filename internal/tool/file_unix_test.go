//go:build unix

// 非普通檔的拒絕，獨立成 build tag 檔的理由與 mcp_process_unix_test.go 相同：
// 具名管道與裝置檔是 Unix 才有的東西，`syscall.Mkfifo` 在 Windows 上根本不存在。
package tool_test

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReadFileRejectsFifoBeforeOpening 是這條鏈路上唯一會**卡死**的失敗形態：
// open(2) 一個沒有寫入端的 FIFO 會阻塞到有人來寫為止，而 os.Root.Open 不吃
// context——不在開檔**之前**用 Lstat 擋下它，「所有阻塞路徑都吃 context」在這條
// 路上直接失效（憲法 5.3）。
//
// 因此斷言兩件事：回 SandboxViolation，**而且準時返回**。只斷言前者的話，一個先
// 開檔再檢查型別的實作會讓測試掛到 go test 自己的 timeout 才失敗。
func TestReadFileRejectsFifoBeforeOpening(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(dir+"/pipes", 0o755); err != nil {
		t.Fatalf("建立 pipes/: %v", err)
	}
	fifo := dir + "/pipes/stream"
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("此環境無法建立具名管道: %v", err)
	}

	errText := executeWithin(t, 5*time.Second, newReadFile(root, []string{"pipes"}), `{"path":"pipes/stream"}`)
	if errText == "" {
		t.Fatal("具名管道不該被讀成檔案")
	}
	if !strings.Contains(errText, "SandboxViolation") || !strings.Contains(errText, "普通檔") {
		t.Errorf("錯誤 %q 未說明只支援普通檔", errText)
	}
}

// TestReadFileRejectsDeviceFile 驗證裝置檔一律拒絕。os.Root 的文件自己寫明它
// 「do not prohibit … access to Unix device files」，所以這一格只有應用層的
// Lstat 型別檢查擋得住。
//
// 裝置檔沒有特權就建不出來，所以這裡把 Workspace 直接開在 /dev 上——那是一個
// **真的**裝置檔，比在測試裡假造一個誠實。
func TestReadFileRejectsDeviceFile(t *testing.T) {
	info, err := os.Lstat("/dev/null")
	if err != nil || info.Mode()&os.ModeDevice == 0 {
		t.Skipf("此環境的 /dev/null 不是裝置檔（%v）", err)
	}
	root, err := os.OpenRoot("/dev")
	if err != nil {
		t.Skipf("此環境開不了 /dev: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 /dev root: %v", err)
		}
	})

	errText := executeWithin(t, 5*time.Second, newReadFile(root, []string{"."}), `{"path":"null"}`)
	if errText == "" {
		t.Fatal("裝置檔不該被讀成檔案")
	}
	if !strings.Contains(errText, "SandboxViolation") || !strings.Contains(errText, "普通檔") {
		t.Errorf("錯誤 %q 未說明只支援普通檔", errText)
	}
}
