//go:build unix

// 本檔是 MCP 子進程生命週期的 Unix 實作。拆成 build tag 檔是因為 process group 的
// 概念本身不跨平台：SysProcAttr 的欄位在各作業系統上完全不同，寫在一起編不過。
package tool

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup 讓 MCP server 子進程**自成一個 process group**（它自己是 leader，
// 因此 pgid == pid）。必須在 Start 之前呼叫。
//
// **為什麼需要**：`npx -y some-mcp-server`（我們自己的 init 模板就是這樣示範的）不是
// server 本身，是個啟動器——真正的 server 是它的子進程，也就是我們的**孫行程**，而且
// 那個孫行程繼承了同一份 stdout／stderr 寫入端。只殺直接子進程的話，孫行程還活著、
// 還抓著 pipe，我們的讀取端永遠等不到 EOF，關閉流程就此卡死並留下孤兒——正是這張票
// 要消滅的失敗形態。自成一組之後就能一次送訊號給整棵樹。
//
// **已知的副作用**：子進程不再屬於終端機的前景 process group，所以使用者按 Ctrl-C 時
// SIGINT **不會**直接送到 MCP server。這是刻意的取捨且方向正確——這些子進程的生命週期
// 由 OryxOS 負責（組裝點的 defer Close），而不是由訊號廣播決定；否則 server 可能在我們
// 還在跟它講話的時候就被打斷。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 強制終止子進程**連同它衍生的孫行程**。
//
// 送給 -pid 就是送給整個 process group（前提是 setProcessGroup 讓它自成一組）。
// ESRCH 代表那一組已經沒有成員了——那是成功，不是失敗。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	// 送不到整組（例如 setProcessGroup 因故沒生效）就退回只殺直接子進程：
	// 殺掉一個總比一個都沒殺好，剩下的由呼叫端的期限與 pipe 關閉兜底。
	if perr := cmd.Process.Kill(); perr != nil && !errors.Is(perr, os.ErrProcessDone) {
		return perr
	}
	return nil
}
