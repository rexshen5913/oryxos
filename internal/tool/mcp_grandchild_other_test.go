//go:build !unix

package tool

import (
	"os"
	"os/exec"
	"strconv"
)

// spawnPipeHoldingGrandchild 是非 Unix 平台的版本：沒有 process group 可以脫離，
// 所以 detached 沒有作用（用到它的測試本來就是 Unix 專屬的）。
func spawnPipeHoldingGrandchild(_ bool) {
	grand := exec.Command("sleep", "120")
	grand.Stdout = os.Stdout
	grand.Stderr = os.Stderr
	if err := grand.Start(); err != nil {
		return
	}
	if path := os.Getenv(toolMcpGrandchildPidEnv); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(grand.Process.Pid)), 0o644)
	}
}
