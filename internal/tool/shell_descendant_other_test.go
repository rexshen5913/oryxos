//go:build !unix

package tool

import (
	"os"
	"os/exec"
	"strconv"
)

// spawnShellDescendant 是非 Unix 平台的版本：沒有 process group 可以脫離，所以忽略
// detached（見 shell_descendant_unix_test.go 的說明）。用到它的矩陣格本身就以 build
// tag 跳過，這裡只是為了讓非 Unix 平台編得過。
func spawnShellDescendant(pidFile string, _ bool) {
	desc := exec.Command("sleep", "120")
	desc.Stdout = os.Stdout
	desc.Stderr = os.Stderr
	if err := desc.Start(); err != nil {
		return
	}
	if pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(desc.Process.Pid)), 0o644)
	}
}
