//go:build unix

// 本檔驗**孫行程**的收尾（ticket #22 的審查發現）。獨立成 build tag 檔是因為 process
// group 與 syscall.Kill 都不跨平台，寫在一般測試檔裡會讓 Windows 編不過。
package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMcpCloseKillsGrandchildHoldingPipes 驗關閉會收掉**整棵進程樹**。
//
// **這一格防的是本票最容易漏掉、後果卻最嚴重的形態**，而且它就是我們自己 init 模板
// 示範的用法：`npx -y some-mcp-server` 不是 server 本身而是啟動器，真正的 server 是
// 它生的子進程（OryxOS 的孫行程），並且繼承了同一份 stdout／stderr 寫入端。
//
// 只殺直接子進程的話：啟動器死了，孫行程還活著、還抓著 pipe，於是讀取端**永遠等不到
// EOF**，close 就卡在回收上——`oryxos chat` 再也退不出來，使用者只能 Ctrl-C，而那正好
// 留下這張票要消滅的孤兒。
//
// 兩條斷言缺一不可：close 準時返回（CLI 不卡在退出上）＋ 孫行程真的死了（沒有孤兒）。
// 只驗前者的話，一個「放棄等待就返回」的實作也會通過，而它留下的正是孤兒進程。
func TestMcpCloseKillsGrandchildHoldingPipes(t *testing.T) {
	const closeTimeout = 300 * time.Millisecond
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := toolMcpSpecWithEnv(t, "launcher", modeLeakPipeToGrandchild,
		map[string]string{toolMcpGrandchildPidEnv: pidFile}, "echo")
	conn, err := dialMcpStdio(ctx, spec, discardLogger())
	if err != nil {
		t.Fatalf("連上測試 MCP server: %v", err)
	}
	// 觸發那個模式：server 回完 tools/list 才生孫行程。
	if _, err := conn.listTools(ctx); err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	pid := waitForPid(t, pidFile)
	t.Cleanup(func() {
		// 萬一收尾失敗，別把孫行程留給後面的測試（sleep 120 會撐很久）。
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	if !processAlive(pid) {
		t.Fatalf("孫行程 %d 還沒起來就不見了，這一格測不到東西", pid)
	}

	start := time.Now()
	if err := conn.close(closeTimeout); err != nil {
		t.Errorf("close 回了錯誤: %v（強制終止是預期路徑，不該當成失敗上拋）", err)
	}
	elapsed := time.Since(start)

	// 上限算進 forceClose 的兩段寬限：期限 ＋ 2×mcpKillReapGrace，再放寬一點給 CI。
	limit := closeTimeout + 2*mcpKillReapGrace + 3*time.Second
	if elapsed > limit {
		t.Errorf("close 等了 %v 才返回（上限 %v）——孫行程抓著 pipe 時關閉卡住了", elapsed, limit)
	}
	if conn.cmd.ProcessState == nil {
		t.Error("close 返回時直接子進程還沒被回收")
	}

	// 孫行程也必須死。SIGKILL 不是同步的，給它一點時間落地。
	if waitForProcessGone(pid, 5*time.Second) {
		return
	}
	t.Errorf("孫行程 %d 在 close 之後仍然活著——只殺了直接子進程，留下孤兒", pid)
}

// TestMcpCloseReturnsWhenGrandchildEscapesProcessGroup 驗 close 的**第二道防線**。
//
// 殺整棵樹解決得了絕大多數情形，但解決不了「孫行程自己 daemonize」——它自成一個
// process group，我們的訊號送不到它，而它照樣抓著 pipe 不放。這時唯一還能做的是把
// **我們這一側**的讀取端關掉，讀取 goroutine 才收得了工。
//
// 這一格因此只斷言「close 準時返回」，**不**斷言那個孫行程死了：殺不到就是殺不到，
// 假裝驗得到只會讓測試說謊。這也正是本票對這種情形的誠實立場——CLI 不卡在退出上是
// 硬要求，而一個刻意脫離的進程本來就不歸我們管。
//
// 沒有這一格的話，那道防線（同時也是非 Unix 平台唯一的保障）就是死碼。
func TestMcpCloseReturnsWhenGrandchildEscapesProcessGroup(t *testing.T) {
	const closeTimeout = 300 * time.Millisecond
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := toolMcpSpecWithEnv(t, "daemonizer", modeLeakPipeToDetachedGrandchild,
		map[string]string{toolMcpGrandchildPidEnv: pidFile}, "echo")
	conn, err := dialMcpStdio(ctx, spec, discardLogger())
	if err != nil {
		t.Fatalf("連上測試 MCP server: %v", err)
	}
	if _, err := conn.listTools(ctx); err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	pid := waitForPid(t, pidFile)
	// 這個孫行程逃出了我們的射程，測試自己負責收掉它。
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	start := time.Now()
	if err := conn.close(closeTimeout); err != nil {
		t.Errorf("close 回了錯誤: %v", err)
	}
	elapsed := time.Since(start)

	limit := closeTimeout + 2*mcpKillReapGrace + 3*time.Second
	if elapsed > limit {
		t.Errorf("close 等了 %v 才返回（上限 %v）——孫行程逃出 process group 時關閉卡住了",
			elapsed, limit)
	}
	if conn.cmd.ProcessState == nil {
		t.Error("close 返回時直接子進程還沒被回收")
	}
}

// waitForPid 等 server 把孫行程的 PID 寫出來。
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到孫行程的 PID 檔 %s", path)
	return 0
}

// processAlive 問一個 PID 還在不在。訊號 0 只做權限與存在性檢查，不真的送出訊號。
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitForProcessGone 等一個 PID 消失，最多等 d。
func waitForProcessGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}
