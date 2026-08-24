package tool

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// 本檔是 shell 子進程的生命週期：**第零道防線 ＋ 所有權移交的四態狀態機 ＋ 回收路徑**。
//
// ── shell 對逾時的保證只有三句，不得寫得更好聽 ──
//
//  1. **Execute 一定在期限內返回**——由**第零道 ＋ 第三道**共同保證，**不是** WaitDelay。
//  2. **同一 process group 內、且可被 SIGKILL 回收的後代一定被收掉。**
//  3. **不保證**脫離 group 者死亡、**不保證**卡在 uninterruptible sleep 的直接子進程被
//     回收、**也不保證**卡在解析或 Start 的 worker 完成——這些情形的回填與審計訊息都要
//     **如實說明可能有殘留**。
//
// 這三句是 spec #29 **九輪**審查逐次**下修**的結果（二十二條更正，其中十二條是「前一輪
// 的修法本身又出問題」）。把它們改回更好聽的版本 ＝ 把已經踩過的坑再踩一次。
//
// ── 第零道 ＋ 三道防線，不是只有三道 ──
//
//   - **第零道**：解析 ＋ 建構 ＋ Start 整段放進一條 goroutine，主路徑以帶期限的 select
//     等它。理由是那些呼叫**同步、不吃 context**：LookPath 要對 PATH 每段做 stat／access、
//     Start 要 fork＋execve，PATH 某段位在故障的 NFS／FUSE 掛載時它們自己就卡住，而那時
//     reap goroutine 還沒建立，後三道介入不了。
//   - **第一道**：對 process group 送 SIGKILL（cmd.Cancel，見 shell_cancel_unix.go）。
//   - **第二道**：關掉我方 pipe（cmd.WaitDelay 到期）。**非 Unix 平台唯一的保障。**
//   - **第三道**：獨立 reap goroutine ＋ 期限 ＋ 放棄等待。**bounded return 的唯一來源**
//     ——WaitDelay 不能讓進行中的 Process.Wait 提前返回。
//
// **卡住的 lifecycle worker 會留下 goroutine**（憲法 5.3 的一個明列例外，理由同既有的
// MCP 實作），但該例外被 admission slot 關在「至多 8 個未完成 worker」的**有界**範圍內，
// 不是無限度的豁免。

// shellHandoffState 是進程所有權的四個狀態。
//
// **三態不夠。** 若讓 worker 自己把 pending 改成 handed 之後才投遞，就會出現一個沒人
// 負責的窗口——已標 handed、**尚未實際交付**，此時期限到，主路徑看到不是 pending、
// 仍走期限分支回錯誤，而 worker 認為自己已移交，**兩邊都不 reap**（spec #29 下修表
// 第二十一列）。因此 handed **只能由主路徑在真正收到進程時提交**。
type shellHandoffState int

const (
	// shellPending：worker 還沒把進程啟動起來（可能卡在解析或 Start）。
	shellPending shellHandoffState = iota
	// shellReady：進程已存在，worker 正要投遞給主路徑。
	shellReady
	// shellHanded：主路徑已真正收到進程並接管。**只有主路徑寫得到這一格。**
	shellHanded
	// shellAbandoned：主路徑的期限先到，已放棄等待。
	shellAbandoned
)

// shellHandoff 是進程所有權的移交，以 mutex 保護。
//
// **buffered channel ＋ select 不構成移交。** 期限與 Start 成功可以**同時** ready：
// 若 worker 一把成功結果送進 channel 就認定移交完成，而主路徑的 select 同時選中期限
// 分支直接返回，結果是**沒有人接管那個進程**——它留在背景跑到底（spec #29 下修表
// 第二十一列）。
//
// 規則一句話：**期限處理者永遠是決定者，而決定的當下誰持有進程，誰就負責 kill＋reap。**
// 任一交錯都恰好落在「主路徑接管」「worker kill＋reap」「detached reaper kill＋reap」
// 三者之一，不會皆非（留下背景進程）也不會皆是（重複回收）。
type shellHandoff struct {
	mu    sync.Mutex
	state shellHandoffState
}

// workerReady 由 worker 在 Start 成功之後呼叫。
//
// 回傳 true 表示「請投遞給主路徑」；false 表示「主路徑已經放棄了，**你自己** kill＋reap
// ＋歸還 slot」。**worker 永遠不寫 handed**（見型別說明）。
func (h *shellHandoff) workerReady() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == shellAbandoned {
		return false
	}
	h.state = shellReady
	return true
}

// mainTake 由主路徑在收到投遞時呼叫，回傳是否接管成功。
//
// 只有 ready 能轉成 handed。已 abandoned 表示期限先到，那個進程已交給別人負責，
// 主路徑只回逾時錯誤。
func (h *shellHandoff) mainTake() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != shellReady {
		return false
	}
	h.state = shellHanded
	return true
}

// mainAbandon 由主路徑在期限到時呼叫，回傳**放棄的當下**是哪一態。
//
// 呼叫端據 prev 決定誰負責善後：
//
//   - prev == shellPending：進程還不存在（或 worker 還沒宣告），交由 worker 之後自行
//     kill＋reap＋歸還 slot。
//   - prev == shellReady：進程已存在且 worker 正在投遞。**主路徑必須把它收下來**——
//     但**不自己等**（那會破壞 bounded return），而是轉交一條 detached reaper 去
//     kill＋reap＋歸還 slot。
//   - prev == shellHanded：主路徑早已接管，善後由 reap goroutine 負責，這裡不做事。
func (h *shellHandoff) mainAbandon() shellHandoffState {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.state
	h.state = shellAbandoned
	return prev
}

// shellStarted 是一個已經 Start 成功的命令，連同它的輸出緩衝。
//
// stdout／stderr 只有在 Wait 返回**之後**才可以讀：os/exec 的複製 goroutine 在那之前
// 還在寫。走到第三道防線（放棄等待）時主路徑因此**一個位元組都不碰**——那不只是
// 「內容不完整」，是資料競爭。
type shellStarted struct {
	cmd    *exec.Cmd
	stdout *boundedBuffer
	stderr *boundedBuffer
}

// shellWorkerResult 是第零道那條 goroutine 投遞給主路徑的東西：不是啟動成功的命令，
// 就是一個早期失敗（解析或 Start）。
type shellWorkerResult struct {
	started *shellStarted
	err     error
}

// shellTestHooks 是**只有 package 內測試設得到**的同步點（欄位未匯出，正式路徑一律 nil）。
//
// spec #29 的測試矩陣對三格明文要求測試替身——「以測試替身讓解析函式阻塞」「以測試替身
// 在 worker 設完 ready 之後、投遞之前插入同步點」——因為那些交錯**必須確定性地製造**，
// 跑一次隨機時序不算通過。這幾個欄位就是那些同步點，沒有別的用途。
type shellTestHooks struct {
	// beforeResolve 在 PATH 解析之前，用來模擬「解析卡在故障掛載上」（矩陣 (5) 前半）。
	beforeResolve func()
	// afterStart 在 Start 成功之後、workerReady 之前，用來確定性地製造「進程已存在，
	// 而主路徑的期限剛好在這一刻到」（矩陣 (5) 後半）。
	afterStart func()
	// afterReady 在 workerReady 回 true 之後、投遞之前，用來確定性地製造移交競態
	// （矩陣 (7)）。
	afterReady func()
	// swallowReap 讓「已回收」的訊號**永不到達**主路徑，用來驗第三道防線本身
	// （矩陣 (3)）——證明 bounded return **不依賴子進程真的死掉**。
	swallowReap bool
}

// call 是 hook 的安全呼叫：nil 就什麼都不做。
func (h *shellTestHooks) call(fn func()) {
	if fn != nil {
		fn()
	}
}

// startShellWorker 起**第零道防線**那條 goroutine，回傳投遞用的 channel。
//
// 容量 1 是刻意的：worker 的投遞**永遠不能阻塞**。若它阻塞，主路徑放棄之後 worker 就
// 卡在 send 上，連自己那條 kill＋reap 都跑不到。
//
// slot 的歸還在這條路徑上遵守**唯一那條規則**：**任何尚未成功把進程所有權移交給 reap
// 路徑的終止路徑，一律歸還**。用「有沒有移交所有權」這個**二分**判準而不是列舉失敗
// 種類——列舉會漏掉 PATH 解析階段（spec #29 下修表第二十列），而「叫一個沒安裝的工具」
// 正是 LLM 最常犯的錯，連續八次就耗盡 slot。
func (t *shellTool) startShellWorker(runCtx context.Context, in shellInput,
	handoff *shellHandoff, slot *shellSlot) <-chan shellWorkerResult {
	delivery := make(chan shellWorkerResult, 1)
	go func() {
		t.hooks.call(t.hooks.beforeResolve)

		resolved, err := lookupInPathDirs(in.Command, t.runtime.PathDirs)
		if err != nil {
			// **此時連進程都還不存在**，所有權當然未移交 → 歸還。
			slot.release()
			delivery <- shellWorkerResult{err: err}
			return
		}

		started, err := t.buildAndStart(runCtx, in, resolved)
		if err != nil {
			// Start 失敗：**不能呼叫 Wait**，正常歸還路徑走不到，所以在這裡歸還。
			slot.release()
			delivery <- shellWorkerResult{err: err}
			return
		}

		// ── 從這裡開始進程真的存在，「誰持有它」由狀態機決定 ──
		t.hooks.call(t.hooks.afterStart)
		if !handoff.workerReady() {
			// 主路徑的期限已經到了、也已經回了錯誤。**這個進程不得留在背景繼續跑**
			// ——worker 自己收，收完才歸還 slot。
			reapAbandonedShell(started, slot)
			return
		}
		t.hooks.call(t.hooks.afterReady)
		delivery <- shellWorkerResult{started: started}
	}()
	return delivery
}

// buildAndStart 建構並啟動命令。**這個函式同步、不吃 context 的取消**——它整個就是
// 第零道防線要包住的東西。
func (t *shellTool) buildAndStart(runCtx context.Context, in shellInput, resolved string) (*shellStarted, error) {
	// **一律 exec.CommandContext 建構，不得手動組 &exec.Cmd{}。** 這不是風格選擇：
	// exec.Cmd 存放 context 的欄位**未匯出**，只有 CommandContext 設得了；而 Cancel
	// 的官方文件明寫「the command must have been created with CommandContext」。
	// 手動建構的 Cmd **完全沒有取消監看**——第一道防線掛不上去，而它同樣能讓 argv[0]、
	// Dir、Env 全都正確，於是安靜地通過大部分驗收。
	cmd := exec.CommandContext(runCtx, resolved, in.Args...)
	// **這一行不能省。** 官方文件：「Args[0] is always name, not the possibly resolved
	// Path」——傳絕對路徑當 name，argv[0] 就是那個絕對路徑，busybox／git 這類依 argv[0]
	// 改變行為的 multicall 程式會走錯分支。
	cmd.Args[0] = in.Command
	cmd.Dir = t.runtime.Dir
	cmd.Env = shellChildEnv(t.runtime.PathDirs)

	// 第一道防線的兩半，缺一不可：Setpgid 讓子進程**自成一組**（必須在 Start 之前），
	// Cancel 才有一整組可以送訊號。沿用 MCP 那個同名函式——語義完全相同，不另立一套。
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return shellCancelProcessGroup(cmd) }

	// 第二道防線：Cancel 觸發之後再過 shellWaitDelay，仍未回收就關掉**我方**的 pipe。
	// 不設的話（Go 的預設）pipe 要讀到 EOF，而官方文件明說那「might not occur until
	// orphaned subprocesses of the command have also closed their descriptors」。
	cmd.WaitDelay = shellWaitDelay

	started := &shellStarted{
		cmd:    cmd,
		stdout: &boundedBuffer{limit: maxShellOutputBytes},
		stderr: &boundedBuffer{limit: maxShellOutputBytes},
	}
	cmd.Stdout = started.stdout
	cmd.Stderr = started.stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return started, nil
}

// reapAbandonedShell 是 worker 自己善後的路徑：主路徑已經放棄，這個進程由 worker
// kill＋reap，**收完才歸還 slot**。
//
// **關於「逾時之後才 Start 成功」（late success）。** Go 的 Start 對一個**已經 done**
// 的 context 會直接回 ctx.Err() 且不建立進程（Go 1.24 實測），所以字面上的 late success
// 在「以 runCtx 建構」之下**不會發生**——那條路徑上根本沒有進程可以留在背景。真正會
// 發生的是它的競態版本：Start 在期限**之前**剛好成功，而主路徑的期限緊接著到。這個
// 函式處理的就是那一格，測試以 afterStart 同步點確定性地製造它。
func reapAbandonedShell(started *shellStarted, slot *shellSlot) {
	defer slot.release()
	// 不倚賴 os/exec 的取消監看在這一刻已經跑過：那是排程時序，這裡要的是確定性。
	// 重複送訊號是安全的——組已經空了就回 ESRCH，我們不看這個回傳值。
	_ = shellCancelProcessGroup(started.cmd)
	_ = started.cmd.Wait()
}

// reapDetachedShell 是主路徑期限到、而 prev == shellReady 時走的路徑：進程已存在且
// worker 正在投遞，**主路徑必須把它收下來**——但收下來的動作交給這條 detached
// goroutine，主路徑立刻回逾時錯誤。
//
// **為什麼收取本身也要離開主路徑。** worker 在投遞之前可能還沒被排程到（矩陣 (7) 的
// 測試就是確定性地卡在那裡），主路徑若自己 `<-delivery` 就會等下去——那正好破壞
// 「不自己等」要保住的 bounded return。所有權的**歸屬**在 mainAbandon 的鎖裡已經確定
// 了，這裡只是去把東西拿到手。
func reapDetachedShell(delivery <-chan shellWorkerResult, slot *shellSlot) {
	go func() {
		defer slot.release()
		// worker 的 workerReady 已回 true，因此投遞必定發生（channel 有緩衝、不阻塞）。
		res := <-delivery
		if res.started == nil {
			return
		}
		_ = shellCancelProcessGroup(res.started.cmd)
		_ = res.started.cmd.Wait()
	}()
}

// reapStartedShell 起**第三道防線**那條獨立的 reap goroutine，回傳等回收結果的 channel。
//
// **slot 由這條 goroutine 歸還，而且是在 Wait 返回之後**——主路徑放棄等待時 slot 不會
// 跟著回來，那正是「卡住的 worker 佔著一格」的意思，也是上限存在的理由。
func reapStartedShell(started *shellStarted, slot *shellSlot, swallow bool) <-chan error {
	reaped := make(chan error, 1)
	go func() {
		defer slot.release()
		err := started.cmd.Wait()
		if swallow {
			// 矩陣 (3)：讓「已回收」的訊號**永不到達**。驗的是期限邏輯本身——
			// bounded return **不依賴子進程真的死掉**。
			return
		}
		reaped <- err
	}()
	return reaped
}

// awaitShellReap 等回收：**先等 runCtx 結束，之後最多再等 grace**。
//
// 分兩段而不是算一個總時長，是因為「該等到什麼時候」有**兩個**觸發源，而它們的性質
// 不同：
//
//   - **期限到**（shell 自己的上限，或呼叫端帶的期限）——第一道與第二道由 os/exec 在
//     這一刻自動觸發，之後給 grace 讓回收完成。
//   - **被取消**（Ctrl+C 走 signal.NotifyContext 那條）——同樣在這一刻觸發前兩道，
//     但它可能發生在期限**之前很久**。
//
// **用「到 deadline 還剩多久 ＋ grace」算總時長會漏掉第二種。** context 被取消時
// `Deadline()` **不動**（實測：parent 一取消，子 context 的 Err() 已是 canceled，而
// 距 Deadline 仍有完整的 30 秒）。於是使用者按下 Ctrl+C、命令的 Wait 又剛好卡住時，
// CLI 會再等掉那整段剩餘的上限才退出——取消變成「等一下下」，違反憲法 5.3。
//
// 兩段式對兩種觸發源都給出同一條規則：**從 runCtx 結束的那一刻起，最多再等 grace**。
// 正常結束的命令在第一個 select 就返回，不受影響；解析與 Start 花掉的時間也自動算在
// 同一個期限裡（它們與這裡共用 runCtx），最壞返回時間仍是「觸發時刻 ＋ grace」。
//
// 第二個回傳值是「等到了沒有」。
func awaitShellReap(runCtx context.Context, reaped <-chan error, grace time.Duration) (error, bool) {
	select {
	case err := <-reaped:
		// 正常結束的命令走這裡，而且是**立刻**——期限與 grace 都沒被觸及。
		return err, true
	case <-runCtx.Done():
	}

	// 期限到或被取消。第一道（cmd.Cancel）與第二道（WaitDelay）已由 os/exec 在這一刻
	// 觸發，給它們 grace 把回收做完；逾期就是第三道：放棄等待。
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-reaped:
		return err, true
	case <-timer.C:
		return nil, false
	}
}
