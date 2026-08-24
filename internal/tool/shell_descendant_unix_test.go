//go:build unix

package tool

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// spawnShellDescendant 生一個**繼承本進程 stdout／stderr** 的後代，並把它的 PID 寫進
// pidFile。形狀照抄 spawnPipeHoldingGrandchild（mcp_grandchild_unix_test.go），差別只有
// 一處：參數走 argv 而不是環境變數，因為 shell 的 Env 是白名單式的、傳不進自訂變數。
//
// **那兩行 `desc.Stdout = os.Stdout` 就是整個問題的來源**：那個 fd 是 OryxOS 那一端
// pipe 的寫入端，後代抓著它不放，我們的讀取端就收不到 EOF——`Wait` 因此可能永遠不返回。
// 這正是本票要關上的缺口。
//
// detached 為真時讓後代自成一個 process group，它就**逃出**我們殺整棵樹的射程——用來
// 逼出矩陣 (2) 那格「殺不到就是殺不到」的誠實邊界。
//
// 用 `sleep` 而不是永久阻塞：萬一回收完全失效，它仍會自己退出，不留給後續測試。
func spawnShellDescendant(pidFile string, detached bool) {
	desc := exec.Command("sleep", "120")
	desc.Stdout = os.Stdout
	desc.Stderr = os.Stderr
	if detached {
		desc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := desc.Start(); err != nil {
		return
	}
	if pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(desc.Process.Pid)), 0o644)
	}
}
