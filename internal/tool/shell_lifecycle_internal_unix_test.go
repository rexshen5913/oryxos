//go:build unix

// 本檔是 shell 生命週期矩陣裡**必須確定性地製造某個交錯**的那幾格：(3) 第三道防線、
// (5) 第零道與被放棄的進程、(7) 移交競態，加上 (6) 的 ESRCH 映射。
//
// 它住在 `package tool`（內部測試）而不是 `package tool_test`，因為那些交錯的同步點是
// 未匯出的欄位（shellTestHooks）——**spec #29 的測試矩陣對這三格明文要求測試替身**，
// 理由是「這一格必須**確定性地製造**那個關鍵交錯，跑一次隨機時序不算通過」。
//
// **這不是新開 seam**：同步點只存在於未匯出欄位，正式路徑一律 nil，對外的介面
// （NewShell → OryxTool.Execute）一個字都沒變。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// hookedShell 組出一個帶測試同步點的 shell，只准跑探針。
func hookedShell(t *testing.T, limiter *ShellLimiter, timeout time.Duration, hooks shellTestHooks) *shellTool {
	t.Helper()
	binDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("此環境取不到測試二進制路徑: %v", err)
	}
	if err := os.Symlink(self, filepath.Join(binDir, argv0ProbeName)); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
	return &shellTool{
		checker: NewSandboxChecker(SandboxConfig{AllowedCommands: []string{argv0ProbeName}}),
		runtime: ShellRuntime{
			Dir:      t.TempDir(),
			PathDirs: append([]string{binDir}, ParentPathDirs()...),
			Timeout:  timeout,
		},
		limiter: limiter,
		hooks:   hooks,
	}
}

// hangInput 組一次「卡住直到 release 出現」的探針呼叫。
func hangInput(startedMarker, releaseFile string) string {
	return `{"command":"` + argv0ProbeName + `","args":["` + probeHangArg + `","` +
		startedMarker + `","` + releaseFile + `"]}`
}

// awaitFile 等一個檔案出現，回傳等到了沒有。不 Fatal——有幾格要在同步點裡呼叫它，
// 而 t.Fatalf 只能在測試自己的 goroutine 用。
func awaitFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// awaitSlotsFree 等 limiter 的未完成 worker 數歸零。
func awaitSlotsFree(t *testing.T, limiter *ShellLimiter, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if limiter.inFlight() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("等不到 slot 歸還：仍有 %d 個未完成的 lifecycle worker", limiter.inFlight())
}

// readProbePid 讀探針寫進 startedMarker 的 PID。
func readProbePid(t *testing.T, marker string) int {
	t.Helper()
	if !awaitFile(marker, 10*time.Second) {
		t.Fatalf("等不到探針的啟動證據 %s", marker)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("讀取 %s: %v", marker, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("啟動證據 %q 不是 PID: %v", raw, err)
	}
	return pid
}

// probeGone 等一個 PID 消失。訊號 0 只做存在性檢查，不真的送出訊號。
func probeGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

// TestShellThirdLineGivesUpWhenReapSignalNeverArrives 是矩陣 (3)：**第三道防線**。
//
// uninterruptible sleep（D state）在自動化測試裡造不出來，所以驗的是**期限邏輯本身**：
// 讓「已回收」的訊號**永不到達**，斷言主路徑仍在期限內放棄等待、回錯誤、訊息含
// 「殘留」。
//
// **這一格不製造 D state，但它證明 bounded return 不依賴子進程真的死掉**——那正是
// 第三道存在的理由：SIGKILL 對 D state 無效，Process.Wait 的 wait4 因此不返回，而
// WaitDelay 只會「再 Kill 一次 ＋ 關 pipe」，**不能讓進行中的 Wait 提前返回**。
//
// 訊息還要說出**代價**（US 63）：留下了一條回收 goroutine。
func TestShellThirdLineGivesUpWhenReapSignalNeverArrives(t *testing.T) {
	const timeout = 200 * time.Millisecond
	ctrl := t.TempDir()
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{swallowReap: true})

	started := time.Now()
	result := shell.Execute(context.Background(),
		hangInput(filepath.Join(ctrl, "started"), filepath.Join(ctrl, "never-released")))
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("回收訊號永不到達，不該回成功: %s", result.Content)
	}
	// 上限是「命令期限 ＋ 第三道的寬限」再留餘裕。**掛到測試框架的 timeout 去等於
	// 沒有 bounded return**，所以這個斷言比「有沒有回錯誤」重要得多。
	if want := timeout + shellKillReapGrace; elapsed > want+5*time.Second {
		t.Errorf("Execute 花了 %v 才返回（上限應約 %v）——第三道沒有放棄等待", elapsed, want)
	}
	// 而且不能提前放棄：太早返回代表期限根本沒有等滿，正常命令會被誤殺。
	if elapsed < timeout {
		t.Errorf("Execute 只花了 %v 就返回，比命令自己的期限 %v 還早", elapsed, timeout)
	}
	for _, want := range []string{"殘留", "goroutine"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("第三道的錯誤 %q 未提到 %q（代價要寫出來，不是被隱藏）", result.Error, want)
		}
	}
	// 第一道仍然生效：放棄等待不代表沒殺。
	if pid := readProbePid(t, filepath.Join(ctrl, "started")); !probeGone(pid, 10*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("探針 %d 仍存活——放棄等待不該連 kill 都省掉", pid)
	}
}

// TestShellZeroLineBoundsBlockingResolve 是矩陣 (5) 前半：**第零道防線**。
//
// 解析（LookPath 的 stat／access）與 Start（fork＋execve）**同步、不吃 context**：
// PATH 某段位在故障的 NFS／FUSE 掛載時它們自己就卡住，而那時 reap goroutine 還沒建立，
// 後三道介入不了。這一格以同步點模擬那個阻塞，斷言 Execute **仍在期限內返回**。
func TestShellZeroLineBoundsBlockingResolve(t *testing.T) {
	const timeout = 200 * time.Millisecond
	release := make(chan struct{})
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{
		beforeResolve: func() { <-release },
	})

	started := time.Now()
	result := shell.Execute(context.Background(),
		hangInput(filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "never")))
	elapsed := time.Since(started)
	close(release)

	if result.OK {
		t.Fatalf("解析卡住，不該回成功: %s", result.Content)
	}
	if elapsed > timeout+5*time.Second {
		t.Errorf("解析卡住時 Execute 花了 %v 才返回——第零道沒有把它關進期限裡", elapsed)
	}
	if !strings.Contains(result.Error, "逾時") {
		t.Errorf("錯誤 %q 未說明是逾時", result.Error)
	}
	// 解析放行之後那條 worker 會走完（命令找得到、Start 成功、發現已被放棄 → 自己
	// kill＋reap），slot 最終歸還。
	awaitSlotsFree(t, limiter, 30*time.Second)
}

// TestShellAbandonedProcessIsKilledAndReaped 是矩陣 (5) 後半：進程在期限之後才被
// worker「認領」時，**不得留在背景繼續跑**。
//
// **關於字面上的「late success」。** Go 的 Start 對一個**已經 done** 的 context 會直接
// 回 ctx.Err() 且不建立進程（Go 1.24 實測），所以「逾時之後才 Start 成功」在「以 runCtx
// 建構」之下**不會發生**——那條路徑上根本沒有進程可以留在背景。真正會發生、也真正需要
// 狀態機的，是它的**競態版本**：Start 在期限**之前**剛好成功，而主路徑的期限緊接著到。
// 這一格以 afterStart 同步點**確定性地**製造那一刻。
//
// 三件事都要斷言（只斷言「回了逾時錯誤」不算通過）：進程**被殺掉**、**沒有留在背景**、
// **slot 最終歸還**。
func TestShellAbandonedProcessIsKilledAndReaped(t *testing.T) {
	const timeout = 200 * time.Millisecond
	ctrl := t.TempDir()
	marker := filepath.Join(ctrl, "started")
	release := make(chan struct{})
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{
		afterStart: func() {
			// 先確定探針已寫下 PID，再卡住——不然這一格可能在它來得及留下證據之前
			// 就把它殺了，我們會失去「它真的死了」的驗證對象。
			awaitFile(marker, 10*time.Second)
			<-release
		},
	})

	started := time.Now()
	result := shell.Execute(context.Background(), hangInput(marker, filepath.Join(ctrl, "never")))
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("期限已到，不該回成功: %s", result.Content)
	}
	if elapsed > timeout+5*time.Second {
		t.Errorf("Execute 花了 %v 才返回，bounded return 不成立", elapsed)
	}

	pid := readProbePid(t, marker)
	// 放行同步點：worker 這時才會發現自己已被放棄，並自行 kill＋reap＋歸還 slot。
	close(release)

	if !probeGone(pid, 30*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("被放棄的進程 %d 仍在背景跑——逾時之後才被認領的進程不得留著跑到底", pid)
	}
	awaitSlotsFree(t, limiter, 30*time.Second)
}

// TestShellHandoffRaceLeavesNoOrphan 是矩陣 (7)：**移交競態**。
//
// 同步點插在 worker 設完 ready **之後、投遞之前**，然後讓期限到。這是被推翻的第八輪
// 修法會漏掉的那一刻——那個版本用 buffered channel ＋ select 當移交，於是 worker 一投遞
// 就認定移交完成，而主路徑同時選中期限分支直接返回，結果**沒有人接管那個進程**，它留在
// 背景跑到底（spec #29 下修表第二十一列）。
//
// 現在的規則是：期限處理者是決定者，`prev == ready` 時主路徑把它轉交 detached reaper。
// 斷言進程**仍被 kill＋reap**、**沒有留在背景**、slot 最終歸還。
func TestShellHandoffRaceLeavesNoOrphan(t *testing.T) {
	const timeout = 200 * time.Millisecond
	ctrl := t.TempDir()
	marker := filepath.Join(ctrl, "started")
	release := make(chan struct{})
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{
		afterReady: func() {
			awaitFile(marker, 10*time.Second)
			<-release
		},
	})

	started := time.Now()
	result := shell.Execute(context.Background(), hangInput(marker, filepath.Join(ctrl, "never")))
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("期限已到，不該回成功: %s", result.Content)
	}
	// **主路徑不得自己等投遞。** 同步點還卡著，若實作寫成 `started := <-delivery`
	// 再轉交，這裡就會一路等到 close(release)——那正是「不自己等」要保住的東西。
	if elapsed > timeout+5*time.Second {
		t.Errorf("Execute 花了 %v 才返回——主路徑在期限分支自己等了投遞，破壞 bounded return", elapsed)
	}

	pid := readProbePid(t, marker)
	close(release)

	if !probeGone(pid, 30*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("移交競態下的進程 %d 留在背景——ready 與期限同時發生時沒有人接管它", pid)
	}
	awaitSlotsFree(t, limiter, 30*time.Second)
}

// TestShellHandoffRaceRepeated 是矩陣 (7) 的第二半：同情境**重複數十次**（配 -race），
// 斷言**既無殘留、也無重複回收**。
//
// 單跑一次只證明那一個交錯被處理對了；重複跑配上 -race 才照得出鎖用錯、或某條路徑
// 重複歸還 slot。**重複歸還不是無害的**：release 會從 channel 取走別人的佔位，上限就此
// 少一格，而且完全沒有錯誤訊息——會隨時間累積成「shell 越用越容易被拒」。
func TestShellHandoffRaceRepeated(t *testing.T) {
	if testing.Short() {
		t.Skip("重複競態測試在 -short 下略過")
	}
	const rounds = 30
	const timeout = 50 * time.Millisecond
	limiter := NewShellLimiter()

	for i := range rounds {
		ctrl := t.TempDir()
		marker := filepath.Join(ctrl, "started")
		release := make(chan struct{})
		var once sync.Once
		shell := hookedShell(t, limiter, timeout, shellTestHooks{
			afterReady: func() {
				awaitFile(marker, 10*time.Second)
				<-release
			},
		})

		result := shell.Execute(context.Background(), hangInput(marker, filepath.Join(ctrl, "never")))
		if result.OK {
			once.Do(func() { close(release) })
			t.Fatalf("第 %d 輪：期限已到，不該回成功", i)
		}
		pid := readProbePid(t, marker)
		once.Do(func() { close(release) })
		if !probeGone(pid, 30*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("第 %d 輪：進程 %d 留在背景", i, pid)
		}
		// 每一輪都要回到零。**不是最後檢查一次**——重複回收造成的短少會被後續輪次的
		// 正常歸還掩蓋掉，只有逐輪檢查照得出來。
		awaitSlotsFree(t, limiter, 30*time.Second)
		if got := limiter.inFlight(); got != 0 {
			t.Fatalf("第 %d 輪結束時仍有 %d 個未完成 worker", i, got)
		}
	}
}

// TestShellCancelMapsESRCHToProcessDone 是矩陣 (6) 的核心斷言：**cmd.Cancel 的
// ESRCH → os.ErrProcessDone 映射**。
//
// 官方文件：「If the command exits with a success status after Cancel is called, and
// Cancel **does not return an error equivalent to os.ErrProcessDone**, then Wait and
// similar methods will return a non-nil error」。取消與正常結束會競態——命令剛好成功
// 退出、process group 已經消失，此時 kill(-pgid) 回 **ESRCH**；把它當成「成功」回 nil
// 就會讓一個**本來成功**的 Wait 被 Go 改報成失敗，使用者看到「命令失敗」而它其實跑完了。
//
// **這一格確定性地製造那個 syscall 情境**：真的起一個進程、真的等它被回收（此時那一組
// 已經空了），然後才呼叫 Cancel——kill(-pgid) 必回 ESRCH。比「跑一個很快結束的命令、
// 希望剛好撞上」可靠得多。
func TestShellCancelMapsESRCHToProcessDone(t *testing.T) {
	shell := hookedShell(t, NewShellLimiter(), time.Minute, shellTestHooks{})
	resolved, err := lookupInPathDirs("true", ParentPathDirs())
	if err != nil {
		t.Skipf("這台機器上找不到 true: %v", err)
	}

	started, err := shell.buildAndStart(context.Background(), shellInput{Command: "true"}, resolved)
	if err != nil {
		t.Fatalf("啟動 true: %v", err)
	}
	if err := started.cmd.Wait(); err != nil {
		t.Fatalf("true 應該成功結束: %v", err)
	}

	// 進程已回收，那一組已空——這正是「取消訊號送出的同時命令剛好結束」那一刻的
	// syscall 狀態。
	cancelErr := shellCancelProcessGroup(started.cmd)
	if !errors.Is(cancelErr, os.ErrProcessDone) {
		t.Errorf("Cancel 對已結束的 process group 回 %v，"+
			"期望一個 errors.Is(err, os.ErrProcessDone) 成立的錯誤——"+
			"否則 Go 會把本來成功的 Wait 改報成失敗", cancelErr)
	}
}

// TestShellFastCommandNotReportedAsFailure 是矩陣 (6) 的行為面：命令**剛好在逾時邊緣
// 正常結束**時，回填的是正常的 exit code，不是逾時錯誤。
//
// 期限刻意取在命令實際耗時的量級上，讓取消與正常結束**真的**去競爭；重複多次提高撞上
// 的機會。**判準不是「每次都成功」**——期限真的先到時回逾時錯誤是正確的——而是
// 「成功的那些回填必須是正常的 exit code 0，不得出現『等待結束』那類 Wait 失敗」。
func TestShellFastCommandNotReportedAsFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("競態重複測試在 -short 下略過")
	}
	shell := &shellTool{
		checker: NewSandboxChecker(SandboxConfig{AllowedCommands: []string{"true"}}),
		runtime: ShellRuntime{Dir: t.TempDir(), PathDirs: ParentPathDirs(), Timeout: 5 * time.Millisecond},
		limiter: NewShellLimiter(),
	}
	for i := range 60 {
		result := shell.Execute(context.Background(), `{"command":"true"}`)
		if result.OK {
			if out := decodeShellOutput(t, result.Content); out.ExitCode != 0 {
				t.Fatalf("第 %d 次：true 的 exit code = %d, 期望 0", i, out.ExitCode)
			}
			continue
		}
		// 逾時是合法結果（期限真的先到）；「等待結束」不是——那就是 ESRCH 沒被映射，
		// 一個本來成功的命令被報成失敗。
		if strings.Contains(result.Error, "等待") {
			t.Fatalf("第 %d 次：成功結束的命令被報成 Wait 失敗（ESRCH 沒有映射成 os.ErrProcessDone）: %s",
				i, result.Error)
		}
	}
}

// decodeShellOutput 把回填內容解回 shellOutput（`package tool` 這一側的版本；外部測試
// 包那邊有一份自己宣告的鏡像結構，兩邊對不上時會直接失敗，不會假綠）。
func decodeShellOutput(t *testing.T, content string) shellOutput {
	t.Helper()
	var out shellOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON（%q）: %v", content, err)
	}
	return out
}

// TestShellBoundedReturnIsNotRecomputedAfterSlowStart 釘住**最壞返回時間就是
// `timeout + grace`，不是「第零道花多久，再加一個完整的 timeout」**。
//
// 被抓的錯法很自然：第零道投遞之後，主路徑再 `awaitShellReap(reaped, Timeout+grace)`。
// 那樣解析與 Start 花掉的時間**沒有算在同一個期限裡**，最壞返回時間變成接近
// `2×timeout + grace`——而 shell_lifecycle.go 對外宣稱的是 `timeout + grace`。
//
// 這一格把投遞刻意延到接近 deadline（afterStart 睡掉四分之三的期限），再讓回收訊號
// **永不到達**（swallowReap）逼出第三道的完整等待。兩者缺一都測不出差距：不延遲投遞
// 的話兩種算法一樣長；不吞掉回收訊號的話命令一被殺就返回，等待期限根本沒被走滿。
func TestShellBoundedReturnIsNotRecomputedAfterSlowStart(t *testing.T) {
	const timeout = 2 * time.Second
	const startDelay = timeout * 3 / 4
	ctrl := t.TempDir()
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{
		afterStart:  func() { time.Sleep(startDelay) },
		swallowReap: true,
	})

	begun := time.Now()
	result := shell.Execute(context.Background(),
		hangInput(filepath.Join(ctrl, "started"), filepath.Join(ctrl, "never")))
	elapsed := time.Since(begun)

	if result.OK {
		t.Fatalf("回收訊號永不到達，不該回成功: %s", result.Content)
	}
	// 上限＝對外宣稱的 timeout + grace，加一點排程餘裕。**重算一份完整 timeout 的
	// 實作會落在 startDelay + timeout + grace，明顯超過這條線。**
	if want := timeout + shellKillReapGrace; elapsed > want+time.Second {
		t.Errorf("Execute 花了 %v，超過對外宣稱的上限 %v——"+
			"第零道花掉的時間沒有算在同一個期限裡（等待期限是不是又重算了一份完整的 Timeout？）",
			elapsed, want)
	}
	if pid := readProbePid(t, filepath.Join(ctrl, "started")); !probeGone(pid, 10*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("探針 %d 仍存活", pid)
	}
	awaitSlotsFree(t, limiter, 30*time.Second)
}

// TestShellTimeoutCauseFixedAtFirstCancellation 釘住**「誰先到期」在第一次取消的當下
// 就決定，不受之後誰又到期影響**。
//
// 被抓的錯法同樣自然：回收結束之後才看 parent `ctx.Err()`。情境是 shell 的上限**先**
// 到（200ms）、命令被殺、而回收訊號遲遲不來，主路徑一路等到 deadline ＋ grace；呼叫端
// 自己的期限（1s）在**那段等待之中**到期。事後比對只會看到「兩邊都過期了」，於是把一次
// **由 shell 上限觸發**的中止報成「呼叫端的期限先到」——使用者跑去調一個沒被觸及的
// 設定，而真正該調的 timeout_seconds 沒人動。
func TestShellTimeoutCauseFixedAtFirstCancellation(t *testing.T) {
	const shellLimit = 200 * time.Millisecond
	const parentLimit = time.Second // 落在 shellLimit 與 shellLimit+grace 之間
	ctrl := t.TempDir()
	shell := hookedShell(t, NewShellLimiter(), shellLimit, shellTestHooks{swallowReap: true})

	ctx, cancel := context.WithTimeout(context.Background(), parentLimit)
	defer cancel()
	result := shell.Execute(ctx, hangInput(filepath.Join(ctrl, "started"), filepath.Join(ctrl, "never")))

	if result.OK {
		t.Fatalf("不該回成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "逾時") {
		t.Errorf("錯誤 %q 未說明是 shell 自己的上限先到——"+
			"cause 是不是在回收結束之後才用 ctx.Err() 事後比對？", result.Error)
	}
	if strings.Contains(result.Error, "呼叫端") {
		t.Errorf("錯誤 %q 報成呼叫端的期限先到，但先到期的是 shell 的上限 %v", result.Error, shellLimit)
	}
	if pid := readProbePid(t, filepath.Join(ctrl, "started")); !probeGone(pid, 10*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// TestShellAbortDisclosureMatchesWhatWasActuallyCleanedUp 釘住**清理揭露必須與該路徑
// 上真正發生過的事相符**。
//
// 三條中止路徑對「同 group 的後代收到什麼程度」的答案**各不相同**，合成一句就會說謊：
//
//   - pending（期限到時 worker 還沒宣告）：連命令有沒有啟動都不確定，worker 可能卡在
//     一個已 fork、尚未從 Start 返回的階段，取消監看都還沒建立。
//   - ready（移交競態）：進程確實存在、已交給 detached reaper，但**本次呼叫沒等它完成**。
//   - reaped（Wait 真的返回了）：**只有這一條**能說「已收掉」。
//
// 前兩條都**不得**出現「已收掉」——三句保證的第三句正是要求這些情形如實說明可能有殘留。
func TestShellAbortDisclosureMatchesWhatWasActuallyCleanedUp(t *testing.T) {
	const timeout = 200 * time.Millisecond
	tests := []struct {
		name      string
		hooks     func(marker string, release chan struct{}) shellTestHooks
		wantSub   string
		forbidden string
	}{
		{
			name: "pending：連啟動都還沒確認，不得宣稱收掉了",
			hooks: func(marker string, release chan struct{}) shellTestHooks {
				return shellTestHooks{beforeResolve: func() { <-release }}
			},
			wantSub:   "尚未確認啟動",
			forbidden: cleanupReaped,
		},
		{
			name: "ready：已交給背景回收，但本次呼叫沒等它完成",
			hooks: func(marker string, release chan struct{}) shellTestHooks {
				return shellTestHooks{afterReady: func() { awaitFile(marker, 10*time.Second); <-release }}
			},
			wantSub:   "尚未確認收乾淨",
			forbidden: cleanupReaped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := t.TempDir()
			marker := filepath.Join(ctrl, "started")
			release := make(chan struct{})
			limiter := NewShellLimiter()
			shell := hookedShell(t, limiter, timeout, tt.hooks(marker, release))

			result := shell.Execute(context.Background(), hangInput(marker, filepath.Join(ctrl, "never")))
			close(release)

			if result.OK {
				t.Fatalf("期限已到，不該回成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantSub) {
				t.Errorf("錯誤 %q 未說明實際的清理程度（期望含 %q）", result.Error, tt.wantSub)
			}
			if strings.Contains(result.Error, tt.forbidden) {
				t.Errorf("錯誤 %q 宣稱 %q，但這條路徑上 Wait 根本還沒返回", result.Error, tt.forbidden)
			}
			// 脫離者的揭露每一條路徑都要帶。
			if !strings.Contains(result.Error, "殺不到") {
				t.Errorf("錯誤 %q 未揭露脫離 process group 的後代殺不到", result.Error)
			}
			awaitSlotsFree(t, limiter, 30*time.Second)
		})
	}

	// 對照組：Wait 真的返回的那條路徑**要**宣稱已收掉——否則上面兩格用「一律不說」
	// 也能通過，而那會讓正常逾時的訊息變得比實際情況更悲觀。
	t.Run("reaped：Wait 返回了，這條路徑要說已收掉", func(t *testing.T) {
		ctrl := t.TempDir()
		shell := hookedShell(t, NewShellLimiter(), timeout, shellTestHooks{})
		result := shell.Execute(context.Background(),
			hangInput(filepath.Join(ctrl, "started"), filepath.Join(ctrl, "never")))
		if result.OK {
			t.Fatalf("不該回成功: %s", result.Content)
		}
		if !strings.Contains(result.Error, cleanupReaped) {
			t.Errorf("錯誤 %q 未說明同 group 的後代已收掉（這條路徑上 Wait 真的返回了）", result.Error)
		}
	})
}

// TestShellCancelDuringRunDoesNotWaitOutTheTimeout 釘住**執行中被取消時，grace 從
// 取消的那一刻起算，不是從原本的期限起算**。
//
// 被抓的錯法：以「到 runCtx 的 deadline 還剩多久 ＋ grace」算等待時間。那個算法對
// 「期限到」是對的，對**取消**是錯的——context 被取消時 `Deadline()` **不動**（實測：
// parent 一取消，子 context 的 Err() 已是 canceled，而距 Deadline 仍有完整的上限）。
//
// 後果是實的：`oryxos chat` 的呼叫端是 `signal.NotifyContext`，使用者按 Ctrl+C 時走的
// 正是這條路。命令的 Wait 若剛好卡住（後代抓著 pipe、或子進程在 D state），CLI 會再等掉
// **整段剩餘的 shell 上限**才退出——預設 30 秒。取消於是變成「等一下下」，違反憲法 5.3。
//
// 這一格把上限刻意開得很大（30 秒），再讓回收訊號**永不到達**（swallowReap）逼出等待
// 路徑；斷言返回時間落在 grace 的量級，而不是上限的量級。**兩者差一個數量級**，所以
// 這條斷言不需要精細的時序也分得出來。
//
// 既有的 TestShellContextCancelled 涵蓋不到這裡：那格是**進 Execute 之前**就已取消，
// 在最前面的 ctx.Err() 檢查就被擋下，根本走不到回收等待。
func TestShellCancelDuringRunDoesNotWaitOutTheTimeout(t *testing.T) {
	// 上限開得比 grace 大一個數量級，讓「等了上限」與「等了 grace」一眼分得出來。
	const timeout = 30 * time.Second
	ctrl := t.TempDir()
	marker := filepath.Join(ctrl, "started")
	limiter := NewShellLimiter()
	shell := hookedShell(t, limiter, timeout, shellTestHooks{swallowReap: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 等命令**真的跑起來**再取消——這樣取消一定落在「執行中」，不是進 Execute 之前。
	go func() {
		awaitFile(marker, 20*time.Second)
		cancel()
	}()

	begun := time.Now()
	result := shell.Execute(ctx, hangInput(marker, filepath.Join(ctrl, "never")))
	elapsed := time.Since(begun)

	if result.OK {
		t.Fatalf("已被取消，不該回成功: %s", result.Content)
	}
	if elapsed > shellKillReapGrace+10*time.Second {
		t.Errorf("取消之後 Execute 花了 %v 才返回（shell 上限是 %v，grace 是 %v）——"+
			"grace 是不是從原本的 deadline 起算？context 被取消時 Deadline() 不會動",
			elapsed, timeout, shellKillReapGrace)
	}
	if !strings.Contains(result.Error, "取消") {
		t.Errorf("錯誤 %q 未說明是被呼叫端取消", result.Error)
	}
	// 取消同樣觸發第一道：命令不該還在跑。
	if pid := readProbePid(t, marker); !probeGone(pid, 10*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("被取消的命令 %d 仍存活——取消沒有觸發 process group kill", pid)
	}
	awaitSlotsFree(t, limiter, 30*time.Second)
}
