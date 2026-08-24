//go:build unix

// Shell 進程生命週期的行為矩陣（ticket #35）。**真的跑程式、真的卡住、真的殺**——
// 這條鏈路要驗的東西沒有一個在假的 exec 上驗得出來。
//
// 本檔涵蓋矩陣 (1)(2)(4a)(4c)(4d) 與 (6) 的行為面。需要測試替身在**進程生命週期內部**
// 插同步點的三格——(3) 第三道防線、(5) 第零道與 late success、(7) 移交競態——住在
// shell_lifecycle_internal_unix_test.go（`package tool`），因為那些同步點是未匯出的。
package tool_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// 探針參數名是 `package tool` 那邊同名常數的副本（理由同 shell_test.go 的 argv0ProbeName：
// 這裡是外部測試包，看不到那邊的未匯出常數）。兩邊對不上時測試會直接失敗，不會假綠。
const (
	probeHangArg            = "hang"
	probeSpawnDescendantArg = "spawn-descendant"
)

// maxShellWorkers 以字面值釘住 spec #4 定案的容量。
//
// **它數的是「未完成的 lifecycle worker」，不是「未回收的直接子進程」**——一個 worker
// 可能卡在 PATH 解析或 Start，那時連子進程都還不存在（spec #29 下修表第二十二列）。
const maxShellWorkers = 8

// shellProbeBinDir 把測試二進制以探針名字連結進一個臨時目錄，回傳那個目錄。
// 它就是要交給 ShellRuntime.PathDirs 的那一份 PATH。
func shellProbeBinDir(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("此環境取不到測試二進制路徑: %v", err)
	}
	if err := os.Symlink(self, filepath.Join(binDir, argv0ProbeName)); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
	return binDir
}

// newLifecycleShell 組出一個只准跑探針的 shell。
//
// PATH 是「探針目錄 ＋ 真實的父進程 PATH」：探針排在前面確保白名單那個名字解析到它，
// 而後半段是**必要的**——spawn-descendant 模式的探針自己要去跑 `sleep`，而它拿到的
// PATH 就是我們傳下去的這一份（Env 白名單式傳遞，見 shellChildEnv）。只給探針目錄的話
// 後代根本生不出來，那格會驗到一個空殼。
//
// 順帶一提，`sleep` **不必**列進白名單：白名單只約束 OryxOS 直接 execve 的程式，不約束
// 被列入的程式再啟動什麼——這一格正是那條邊界的實例。
func newLifecycleShell(t *testing.T, limiter *tool.ShellLimiter, timeout time.Duration) tool.OryxTool {
	t.Helper()
	return tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{argv0ProbeName}}),
		tool.ShellRuntime{
			Dir:      t.TempDir(),
			PathDirs: append([]string{shellProbeBinDir(t)}, tool.ParentPathDirs()...),
			Timeout:  timeout,
		},
		limiter,
	)
}

// waitForFile 等一個檔案出現，逾時就讓測試失敗。用來等「探針真的啟動了」。
func waitForFile(t *testing.T, path string, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到 %s（%s）", what, path)
}

// countFiles 數 dir 底下有幾個檔名以 prefix 開頭的檔案。
func countFiles(t *testing.T, dir, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("讀取 %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			n++
		}
	}
	return n
}

// TestShellAdmissionSlotRejectsBeforeStartingProcess 是矩陣 (4a)：**並發** N+1 個卡死
// 呼叫，第 N+1 個必須在**啟動進程之前**被拒絕。
//
// **必須是並發，循序驗不到**（spec #29 下修表第十四列）：「走到第三道時才佔 slot」那個
// 被推翻的版本在循序測試下完全正常——多個呼叫可以**同時**啟動命令並一起走進第三道，
// 那些不可回收的進程在 slot 滿之前就已經產生，事後拒絕收不回它們。
//
// 斷言不只是「回了錯誤」：第 N+1 個呼叫的探針**不得留下啟動證據**。少了這一條，一個
// 「先 Start 再檢查」的實作照樣全綠，而那正是這一格要擋的東西。
func TestShellAdmissionSlotRejectsBeforeStartingProcess(t *testing.T) {
	ctrl := t.TempDir()
	shell := newLifecycleShell(t, tool.NewShellLimiter(), 60*time.Second)

	// 收尾一律放行所有探針：測試失敗時也不留殘留進程給後續測試。
	t.Cleanup(func() {
		for i := range maxShellWorkers + 1 {
			_ = os.WriteFile(filepath.Join(ctrl, "release-"+strconv.Itoa(i)), nil, 0o644)
		}
	})

	var wg sync.WaitGroup
	for i := range maxShellWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shell.Execute(context.Background(), shellInputJSON(t, argv0ProbeName, probeHangArg,
				filepath.Join(ctrl, "started-"+strconv.Itoa(i)),
				filepath.Join(ctrl, "release-"+strconv.Itoa(i))))
		}()
	}
	for i := range maxShellWorkers {
		waitForFile(t, filepath.Join(ctrl, "started-"+strconv.Itoa(i)), 30*time.Second, "第 %d 個探針啟動")
	}

	// 第 N+1 個：slot 已滿。
	extraStarted := filepath.Join(ctrl, "extra-started")
	extra := shell.Execute(context.Background(), shellInputJSON(t, argv0ProbeName, probeHangArg,
		extraStarted, filepath.Join(ctrl, "release-"+strconv.Itoa(maxShellWorkers))))

	if extra.OK {
		t.Fatalf("slot 已滿仍回成功: %s", extra.Content)
	}
	if _, err := os.Stat(extraStarted); err == nil {
		t.Error("第 N+1 個呼叫**啟動了進程**——admission slot 必須在啟動之前就擋下來，" +
			"事後拒絕收不回已經產生的進程與 goroutine")
	}
	if got := countFiles(t, ctrl, "started-"); got != maxShellWorkers {
		t.Errorf("實際啟動的探針數 = %d, 期望不超過 %d", got, maxShellWorkers)
	}
	if extra.Retryable {
		t.Errorf("slot 已滿的錯誤標了 Retryable，但排隊重試正是這條路徑要避免的: %q", extra.Error)
	}

	// 訊息要**說得出目前有幾個未完成、需人介入**，且**不得把上限講過頭**（下修表第
	// 二十二列）：數的是 lifecycle worker，不是「未 reap 的子進程」；而且要**明說它
	// 不限制脫離的後代**，否則使用者會從「有上限」反推成進程樹層級的資源上限——
	// 第八輪的原話是「我在第二輪就承認了『殺不到』，卻在第四輪引入 slot 時以為自己
	// 『數得到』」。
	for _, want := range []string{strconv.Itoa(maxShellWorkers), "worker", "介入", "脫離", "cgroup"} {
		if !strings.Contains(extra.Error, want) {
			t.Errorf("slot 已滿的錯誤 %q 未提到 %q", extra.Error, want)
		}
	}
	// 反向：**不得**把上限描述成「未 reap 的子進程數」。worker 可能卡在 PATH 解析或
	// Start，那時連子進程都還不存在，卻同樣佔著一格。
	for _, forbidden := range []string{"個未回收的子進程（上限", "個未 reap 的子進程"} {
		if strings.Contains(extra.Error, forbidden) {
			t.Errorf("slot 已滿的錯誤把上限描述成 %q，但它數的是 lifecycle worker: %q", forbidden, extra.Error)
		}
	}

	// 讓一條完成 → slot 歸還 → 呼叫恢復。
	if err := os.WriteFile(filepath.Join(ctrl, "release-0"), nil, 0o644); err != nil {
		t.Fatalf("放行第一個探針: %v", err)
	}
	recovered := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		result := shell.Execute(context.Background(), shellInputJSON(t, argv0ProbeName, probeHangArg,
			filepath.Join(ctrl, "retry-started"), filepath.Join(ctrl, "release-0")))
		if result.OK {
			recovered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recovered {
		t.Error("一條 reap 完成之後 slot 沒有歸還——呼叫始終被拒")
	}

	// 放行其餘探針再收工。**不這樣做的話這一格要等滿 shell 的期限**（那些 Execute
	// 全都還卡著），而那個期限刻意取得很長——本格驗的是 slot 的行為，不是逾時。
	for i := range maxShellWorkers {
		if err := os.WriteFile(filepath.Join(ctrl, "release-"+strconv.Itoa(i)), nil, 0o644); err != nil {
			t.Fatalf("放行第 %d 個探針: %v", i, err)
		}
	}
	wg.Wait()
}

// TestShellSlotReturnedAfterRepeatedStartFailures 是矩陣 (4c)：連續超過 8 次 Start
// 失敗之後，正常呼叫**仍然可用**。
//
// 這一格與 (4d) 一起驗的是歸還的**二分判準**（spec #29 下修表第二十列）。被推翻的
// 版本以**列舉**定義歸還路徑（Wait 返回／走到第三道），漏掉了 Start 失敗——連續八次
// 就永久耗盡 slot，`shell` 完全不可用。
//
// 製造 Start 失敗的手法是「有執行位、但不是合法的執行檔」：lookupInPathDirs 找得到它
// （普通檔 ＋ 執行位），execve 卻回 ENOEXEC。這樣不必跟權限檢查搶時序。
func TestShellSlotReturnedAfterRepeatedStartFailures(t *testing.T) {
	binDir := t.TempDir()
	const brokenName = "oryxos-shell-not-an-executable"
	if err := os.WriteFile(filepath.Join(binDir, brokenName), []byte("\x00not an executable"), 0o755); err != nil {
		t.Fatalf("建立壞掉的執行檔: %v", err)
	}
	// 真的 echo 也要找得到，最後那次正常呼叫才有東西可跑。
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{brokenName, "echo"}}),
		tool.ShellRuntime{
			Dir:      t.TempDir(),
			PathDirs: append([]string{binDir}, tool.ParentPathDirs()...),
			Timeout:  30 * time.Second,
		},
		tool.NewShellLimiter(),
	)

	for i := range maxShellWorkers + 2 {
		result := shell.Execute(context.Background(), shellInputJSON(t, brokenName))
		if result.OK {
			t.Fatalf("第 %d 次：壞掉的執行檔不該啟動成功: %s", i, result.Content)
		}
	}
	assertShellStillUsable(t, shell, "連續 %d 次 Start 失敗", maxShellWorkers+2)
}

// TestShellSlotReturnedAfterRepeatedResolveFailures 是矩陣 (4d)：連續超過 8 次
// **PATH 解析失敗**之後，正常呼叫仍然可用。
//
// **這一格走的路徑上連進程都還沒有**，被推翻的三條列舉走不到它——而「叫一個沒安裝的
// 工具」正是 LLM 最常犯的錯，比 Start 失敗更容易踩到（spec #29 下修表第二十列）。
func TestShellSlotReturnedAfterRepeatedResolveFailures(t *testing.T) {
	const missingName = "oryxos-shell-definitely-not-installed"
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{missingName, "echo"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: 30 * time.Second},
		tool.NewShellLimiter(),
	)

	for i := range maxShellWorkers + 2 {
		result := shell.Execute(context.Background(), shellInputJSON(t, missingName))
		if result.OK {
			t.Fatalf("第 %d 次：機器上沒有的程式不該執行成功: %s", i, result.Content)
		}
	}
	assertShellStillUsable(t, shell, "連續 %d 次 PATH 解析失敗", maxShellWorkers+2)
}

// assertShellStillUsable 斷言 shell 仍跑得動 echo——也就是 slot 沒有被前面那批失敗吃光。
func assertShellStillUsable(t *testing.T, shell tool.OryxTool, format string, args ...any) {
	t.Helper()
	result := shell.Execute(context.Background(), shellInputJSON(t, "echo", "still-alive"))
	if !result.OK {
		t.Fatalf("%s 之後正常呼叫失敗（slot 沒有歸還？）: %s", fmt.Sprintf(format, args...), result.Error)
	}
	if got := strings.TrimSpace(decodeShell(t, result.Content).Stdout); got != "still-alive" {
		t.Errorf("stdout = %q, 期望 still-alive", got)
	}
}

// TestShellTimeoutKillsSameProcessGroupDescendant 是矩陣 (1)：後代留在同一 process
// group 且持有 stdout／stderr。
//
// 兩件事都要斷言，缺一不可：`Execute` **在期限內返回**，**且那個後代不再存活**。
// 只斷言前者的話，一個「殺不到後代但靠 WaitDelay 關 pipe 脫身」的實作也會全綠——
// 而第一道防線的存在理由正是收掉這種後代。
func TestShellTimeoutKillsSameProcessGroupDescendant(t *testing.T) {
	ctrl := t.TempDir()
	pidFile := filepath.Join(ctrl, "descendant.pid")
	shell := newLifecycleShell(t, tool.NewShellLimiter(), 500*time.Millisecond)

	started := time.Now()
	result := shell.Execute(context.Background(),
		shellInputJSON(t, argv0ProbeName, probeSpawnDescendantArg, pidFile, "0"))
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("卡住的命令不該回成功: %s", result.Content)
	}
	// 上限是 shell 自己的期限加上第三道的寬限，再留一點餘裕。**不是**測試框架的
	// timeout——掛到那裡去等於沒有 bounded return。
	if elapsed > 20*time.Second {
		t.Fatalf("Execute 花了 %v 才返回，bounded return 不成立", elapsed)
	}
	pid := readPid(t, pidFile)
	if !waitForProcessGone(pid, 10*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("同一 process group 內的後代 %d 仍存活——第一道防線（kill -pgid）沒有生效", pid)
	}
}

// TestShellTimeoutReturnsWhenDescendantEscapesProcessGroup 是矩陣 (2)：後代 setsid
// 脫離 process group。
//
// **只斷言在期限內返回，不得斷言它死了。** 沿用既有 MCP 那格的誠實程度——殺不到就是
// 殺不到，假裝驗得到只會讓測試說謊（mcp_process_unix_test.go 的同一句註解）。
// 測試自己 t.Cleanup 收掉它。
//
// 訊息**必須如實揭露可能有殘留**：使用者不該以為每次逾時都清乾淨了（US 58）。
func TestShellTimeoutReturnsWhenDescendantEscapesProcessGroup(t *testing.T) {
	ctrl := t.TempDir()
	pidFile := filepath.Join(ctrl, "descendant.pid")
	shell := newLifecycleShell(t, tool.NewShellLimiter(), 500*time.Millisecond)

	started := time.Now()
	result := shell.Execute(context.Background(),
		shellInputJSON(t, argv0ProbeName, probeSpawnDescendantArg, pidFile, "1"))
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("卡住的命令不該回成功: %s", result.Content)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Execute 花了 %v 才返回，bounded return 不成立", elapsed)
	}
	if !strings.Contains(result.Error, "殘留") {
		t.Errorf("逾時錯誤 %q 未如實說明脫離 process group 的後代可能殘留", result.Error)
	}
	// 這個後代我們殺不到，所以測試自己負責收——不是被測行為的一部分。
	if pid := readPid(t, pidFile); pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
}

// readPid 讀探針寫下的 PID。等它出現，因為探針是非同步寫的。
func readPid(t *testing.T, path string) int {
	t.Helper()
	waitForFile(t, path, 10*time.Second, "後代的 PID 檔")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 PID 檔: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("PID 檔內容 %q 不是數字: %v", raw, err)
	}
	return pid
}

// TestShellNormalCommandUnaffectedByLifecycle 釘住一件容易被生命週期改動弄壞的事：
// **正常結束的命令照常回填 exit code 與輸出**，而且不佔著 slot 不放。
//
// 這一格與矩陣 (6) 是同一個關切的兩半——那格驗的是取消與正常結束**競態**時不被誤報，
// 這格驗的是沒有競態時的基準行為沒有被三道防線改壞。
func TestShellNormalCommandUnaffectedByLifecycle(t *testing.T) {
	limiter := tool.NewShellLimiter()
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"echo"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: 30 * time.Second},
		limiter,
	)
	// 跑滿 slot 數的兩倍：每一次都必須歸還，否則後面幾次會開始被拒。
	for i := range maxShellWorkers * 2 {
		result := shell.Execute(context.Background(), shellInputJSON(t, "echo", "ok"))
		if !result.OK {
			t.Fatalf("第 %d 次正常呼叫失敗（slot 沒歸還？）: %s", i, result.Error)
		}
		if out := decodeShell(t, result.Content); strings.TrimSpace(out.Stdout) != "ok" || out.ExitCode != 0 {
			t.Fatalf("第 %d 次回填不對: %+v", i, out)
		}
	}
}

// processAlive 與 waitForProcessGone 是 mcp_process_unix_test.go 那兩個同名函式的
// 副本，理由同 argv0ProbeName：那邊住在 `package tool`（內部測試），這裡是
// `package tool_test`，看不到它們。行為刻意一致——訊號 0 只做存在性檢查，不真的送出。
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

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
