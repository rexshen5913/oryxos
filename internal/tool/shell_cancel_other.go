//go:build !unix

// 本檔是 shell 第一道防線在非 Unix 平台的實作（見 shell_cancel_unix.go 的說明）。
//
// 這裡沒有 process group 的對應物，所以只殺得到直接子進程。後代抓著 pipe 的情形由
// **第二道防線**兜底——關掉我方的讀取端讓複製 goroutine 收工，那道在這些平台上是
// 唯一的保障；bounded return 仍由第三道（放棄等待）給出。
package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// shellCancelProcessGroup 終止直接子進程。
//
// **ESRCH → os.ErrProcessDone 的映射在這裡對應的是 os.ErrProcessDone 本身**：Go 在
// 這些平台上把「進程已結束」直接表述成那個哨兵錯誤。回傳契約與 Unix 版一致——Cancel
// 若在命令成功結束之後未回傳等價於 os.ErrProcessDone 的錯誤，成功的 Wait 會被 Go
// 改報成失敗（見 Unix 版的完整說明）。
func shellCancelProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("%s 的子進程已結束: %w", ShellToolName, os.ErrProcessDone)
		}
		return fmt.Errorf("%s 強制終止子進程: %w", ShellToolName, err)
	}
	return nil
}
