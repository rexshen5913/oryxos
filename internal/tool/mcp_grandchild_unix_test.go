//go:build unix

package tool

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// spawnPipeHoldingGrandchild 生一個**繼承本進程 stdout／stderr** 的孫行程，並把它的
// PID 寫進 toolMcpGrandchildPidEnv 指的檔案。
//
// 那一句 `grand.Stdout = os.Stdout` 就是整個問題的來源：那個 fd 是 OryxOS 那一端 pipe
// 的寫入端，孫行程拿著它不放，OryxOS 的讀取端就收不到 EOF。
//
// detached 為真時讓孫行程自成一個 process group——那它就**逃出了**我們殺整棵樹的範圍，
// 用來逼出 close 的第二道防線（自己關掉讀取端）。
//
// 用 `sleep` 而不是永久阻塞：萬一收尾完全失效，它仍會自己退出，不留給後續測試。
func spawnPipeHoldingGrandchild(detached bool) {
	grand := exec.Command("sleep", "120")
	grand.Stdout = os.Stdout
	grand.Stderr = os.Stderr
	if detached {
		grand.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := grand.Start(); err != nil {
		return
	}
	if path := os.Getenv(toolMcpGrandchildPidEnv); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(grand.Process.Pid)), 0o644)
	}
}
