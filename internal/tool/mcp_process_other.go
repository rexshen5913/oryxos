//go:build !unix

// 本檔是 MCP 子進程生命週期在非 Unix 平台的實作（見 mcp_process_unix.go 的說明）。
//
// 這裡沒有 process group 的對應物，所以只殺得到直接子進程。孫行程抓著 pipe 的情形由
// close() 的第三道防線兜底：關掉讀取端讓讀取 goroutine 收工，關閉流程因此仍有終點。
package tool

import (
	"errors"
	"os"
	"os/exec"
)

// setProcessGroup 在這些平台上不做任何事。
func setProcessGroup(_ *exec.Cmd) {}

// killProcessTree 終止直接子進程。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
