//go:build unix

// 非普通檔的拒絕，獨立成 build tag 檔的理由與 mcp_process_unix_test.go 相同：
// 具名管道與裝置檔是 Unix 才有的東西，`syscall.Mkfifo` 在 Windows 上根本不存在。
package tool_test

import (
	"context"
	"os"
	"os/signal"
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

// TestWriteFileCreatesFileWithoutExecuteBit 是 spec「PATH 與可寫路徑重疊矩陣」第四格
// 的前半：**新建檔案一律 0644、不含執行位**。
//
// 這不是風格斷言。若父進程的 PATH 含有一個落在 file.allowed_paths 之內的目錄，
// write_file 就能在那裡放一個與 shell 白名單命令同名的檔案，把「檔案寫入權限」升級成
// 「命令執行權限」——而 LookPath 只挑得出可執行檔。這一格是那條路徑的緩解之一，
// **不宣稱足夠**（下面那格演示為什麼）。
//
// umask 暫時設 0 是刻意的：不動 umask 的話，這一格在 umask 077 的機器上會看到 0600
// 而變紅，而它要驗的是「**程式要求的是** 0644」，不是「這台機器的 umask 剛好是多少」。
// 進程級的設定，所以窗口壓到只包住那一次 Execute（這個 package 沒有 t.Parallel）。
func TestWriteFileCreatesFileWithoutExecuteBit(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(dir+"/notes", 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}

	prev := syscall.Umask(0)
	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/new.md","content":"x"}`)
	syscall.Umask(prev)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	info, err := os.Stat(dir + "/notes/new.md")
	if err != nil {
		t.Fatalf("查新建檔案的權限: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("新建檔案權限 = %04o, 期望 0644", got)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("新建檔案含執行位（%04o）——write_file 因此能在 PATH 目錄裡造出一個跑得起來的程式",
			info.Mode().Perm())
	}
}

// TestWriteFileOverwriteKeepsExistingMode 是第四格的後半：**覆寫既有檔案時不改變其
// 原有權限**。open(2) 的 mode 參數只在建立時生效，這一格把那個性質釘成對外契約。
//
// 0755 那一列同時是誠實的那一半：既有的可執行檔被覆寫之後**仍然可執行**，所以上面
// 那格的緩解不足以擋住「覆寫既有可執行檔」這條路——主要緩解是啟動時的重疊警告與
// 「PATH 目錄不要與 file.allowed_paths 重疊」的部署要求（ticket #33）。
func TestWriteFileOverwriteKeepsExistingMode(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{name: "比 0644 嚴的權限原樣保留", mode: 0o600},
		{name: "既有的可執行檔覆寫後仍可執行（緩解不足的誠實斷言）", mode: 0o755},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, dir := newWorkspace(t)
			path := writeWorkspaceFile(t, dir, "notes/existing.sh", "舊內容")
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatalf("設定既有權限 %04o: %v", tt.mode, err)
			}

			result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
				`{"path":"notes/existing.sh","content":"新內容"}`)
			if !result.OK {
				t.Fatalf("期望成功，實際錯誤: %s", result.Error)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("查覆寫後的權限: %v", err)
			}
			if got := info.Mode().Perm(); got != tt.mode {
				t.Errorf("覆寫後權限 = %04o, 期望維持原有的 %04o", got, tt.mode)
			}
		})
	}
}

// TestWriteFilePermissionDeniedIsNotRetryable 是 Retryable 契約的一半：**權限不足
// 不標可重試**。檔案的權限位不會因為多等三秒而改變，標了只是讓 ReAct 循環白白多燒
// 兩輪再回同一個錯誤。
//
// 另一半（暫時性 I/O 失敗**要**標）在下面那格，兩者分開斷言。
func TestWriteFilePermissionDeniedIsNotRetryable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時權限位不生效，這一格驗不到東西")
	}
	root, dir := newWorkspace(t)
	path := writeWorkspaceFile(t, dir, "notes/locked.md", "不給寫")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("拿掉寫入權限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(),
		`{"path":"notes/locked.md","content":"x"}`)
	if result.OK {
		t.Fatalf("沒有寫入權限不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "權限") {
		t.Errorf("錯誤 %q 未說明是權限問題", result.Error)
	}
	if result.Retryable {
		t.Errorf("Retryable = true, 期望 false——權限不足重試幾次都一樣: %q", result.Error)
	}
}

// TestWriteFileTransientIOFailureIsRetryable 是 Retryable 契約的另一半：**暫時性的
// I/O 失敗要標 Retryable**（形狀沿用 HTTP Tool 對讀取失敗的做法）。
//
// 真的讓寫入失敗，不 mock 檔案系統（憲法 4.3）：把 RLIMIT_FSIZE 壓到比內容還小，
// 核心就會讓 write(2) 回 EFBIG——與「磁碟滿」「配額用盡」同一個家族的資源耗盡錯誤，
// 而且是**部分寫入**（前面幾個位元組真的落了盤），正是這條契約要涵蓋的形態。
//
// SIGXFSZ 要先設成忽略：它的預設動作是終止進程，不忽略的話整個測試二進位會直接死掉
// 而不是拿到一個錯誤。rlimit 與訊號都是進程級的設定，所以窗口壓到只包住那一次
// Execute，跑完立刻還原（這個 package 沒有 t.Parallel）。
func TestWriteFileTransientIOFailureIsRetryable(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(dir+"/notes", 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}

	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Skipf("此環境取不到 RLIMIT_FSIZE: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 512, Max: orig.Max}); err != nil {
		signal.Reset(syscall.SIGXFSZ)
		t.Skipf("此環境改不了 RLIMIT_FSIZE: %v", err)
	}
	input := `{"path":"notes/big.md","content":"` + strings.Repeat("a", 4096) + `"}`
	result := newWriteFile(root, []string{"notes"}).Execute(context.Background(), input)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatalf("還原 RLIMIT_FSIZE: %v", err)
	}
	signal.Reset(syscall.SIGXFSZ)

	if result.OK {
		t.Fatalf("寫入超過 RLIMIT_FSIZE 不該成功: %s", result.Content)
	}
	if strings.Contains(result.Error, "權限") {
		t.Errorf("資源耗盡被報成權限問題，兩者要分得出來: %q", result.Error)
	}
	if !result.Retryable {
		t.Errorf("Retryable = false, 期望 true——暫時性的 I/O 失敗換個時間點可能就成功了: %q", result.Error)
	}
}

// TestWriteFileRejectsFifoBeforeOpening 是寫入路徑上唯一會**卡死**的失敗形態：
// O_WRONLY 開一個沒有讀取端的具名管道會阻塞到有人來讀為止，而 os.Root.OpenFile
// 不吃 context——不在開檔**之前**用 Lstat 擋下它，憲法 5.3 在這條路上直接失效。
//
// 與 read_file 那一格同形，但這一格證明的是**寫入路徑也有那道檢查**：只有讀路徑
// 擋得住的實作會讓這裡掛到 go test 自己的 timeout。
func TestWriteFileRejectsFifoBeforeOpening(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(dir+"/pipes", 0o755); err != nil {
		t.Fatalf("建立 pipes/: %v", err)
	}
	if err := syscall.Mkfifo(dir+"/pipes/stream", 0o644); err != nil {
		t.Skipf("此環境無法建立具名管道: %v", err)
	}

	errText := executeWithin(t, 5*time.Second, newWriteFile(root, []string{"pipes"}),
		`{"path":"pipes/stream","content":"x"}`)
	if errText == "" {
		t.Fatal("具名管道不該被寫成檔案")
	}
	if !strings.Contains(errText, "SandboxViolation") || !strings.Contains(errText, "普通檔") {
		t.Errorf("錯誤 %q 未說明只支援普通檔", errText)
	}
}

// TestListDirRejectsFifoBeforeOpening 是 read_file 那一格在列目錄路徑上的鏡像。
//
// 理由完全相同：open(2) 一個沒有寫入端的 FIFO 會阻塞到有人來寫為止，而 os.Root.Open
// 不吃 context。型別檢查要求「目標是目錄」，因此這裡的 FIFO 在**開檔之前**就被擋下，
// 而斷言同樣是兩件事：回錯誤，**而且準時返回**。
func TestListDirRejectsFifoBeforeOpening(t *testing.T) {
	root, dir := newWorkspace(t)
	if err := os.MkdirAll(dir+"/pipes", 0o755); err != nil {
		t.Fatalf("建立 pipes/: %v", err)
	}
	fifo := dir + "/pipes/stream"
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("此環境無法建立具名管道: %v", err)
	}

	errText := executeWithin(t, 5*time.Second, newListDir(root, []string{"pipes"}), `{"path":"pipes/stream"}`)
	if errText == "" {
		t.Fatal("具名管道不該被當成目錄列出來")
	}
	if !strings.Contains(errText, "SandboxViolation") || !strings.Contains(errText, "目錄") {
		t.Errorf("錯誤 %q 未說明只支援目錄", errText)
	}
}
