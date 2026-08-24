//go:build unix

// 本檔是 shell 第一道防線的 Unix 實作。拆成 build tag 檔的理由同 mcp_process_unix.go：
// process group 的概念本身不跨平台。
package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// shellCancelProcessGroup 是掛給 cmd.Cancel 的第一道防線：對**整個 process group**
// 送 SIGKILL。
//
// **為什麼要覆寫 cmd.Cancel。** CommandContext 的預設值只呼叫 Process.Kill，也就是
// 只殺**直接**子進程（官方文件：「sets the command's Cancel function to invoke the
// Kill method on its Process」）。後代若繼承了 stdout／stderr 的寫入端，它還活著、
// 還抓著 pipe，我們的讀取端就永遠等不到 EOF——`Wait` 因此可能不返回。送給 -pid 就是
// 送給整組（前提是 Start 之前設過 Setpgid）。
//
// **保證範圍是「仍在同一 process group 內的後代」**——後代自己 setsid()／setpgid()
// 就脫離射程。這條邊界既有的 MCP 測試已經釘死，本票沿用同一個誠實程度：殺不到就是
// 殺不到，回填訊息如實揭露，不假裝清乾淨了。
//
// ── 回傳值有一條必須做的錯誤映射，這才是本函式與 killProcessTree 不共用的原因 ──
//
// 官方文件：「If the command exits with a success status after Cancel is called, and
// Cancel **does not return an error equivalent to os.ErrProcessDone**, then Wait and
// similar methods will return a non-nil error」。
//
// 取消與正常結束**會競態**：命令剛好成功退出、process group 已經消失，此時
// kill(-pgid, SIGKILL) 回 **ESRCH**。若把它當成「成功」回 nil，Go 會把一個**本來成功**
// 的 Wait 改報成錯誤——使用者看到「命令失敗」而它其實跑完了。因此 ESRCH 必須映射成
// **包裝 os.ErrProcessDone 的錯誤**，只有真正的失敗（如 EPERM）才照實回傳。
//
// 既有的 killProcessTree 把 ESRCH 吞成 nil，那對 MCP 是對的（它的呼叫端把非 nil 當成
// 「強制終止失敗」記進日誌），但當成 Cancel 用就正好踩中上面那條。兩者的**回傳契約
// 不同**，所以不共用——共用的是 setProcessGroup，那個語義完全相同。
func shellCancelProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		// 還沒 Start 就被取消：沒有進程可殺，而「沒有進程」正是 ErrProcessDone 的語義。
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		// 整組已經沒有成員了——命令自己結束了。**這是成功，而且必須以
		// os.ErrProcessDone 的形式說出來**，否則成功的 Wait 會被改報成失敗。
		return fmt.Errorf("%s 的 process group 已不存在: %w", ShellToolName, os.ErrProcessDone)
	}
	// 送不到整組（例如 Setpgid 因故沒生效）就退回只殺直接子進程：殺掉一個總比一個都
	// 沒殺好，剩下的由第二道（關 pipe）與第三道（放棄等待）兜底。
	if perr := cmd.Process.Kill(); perr != nil {
		if errors.Is(perr, os.ErrProcessDone) {
			return fmt.Errorf("%s 的子進程已結束: %w", ShellToolName, os.ErrProcessDone)
		}
		return fmt.Errorf("%s 強制終止 process group: %w", ShellToolName, errors.Join(err, perr))
	}
	return nil
}
