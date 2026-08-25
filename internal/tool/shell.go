package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// ShellToolName 是內建 Tool shell 的註冊名，也是 Profile 的 tools 欄位要引用的那個
// 字串。
const ShellToolName = "shell"

// ShellRuntime 是 Shell Tool 的執行上下文，由組裝點填好後顯式注入（憲法 5.2）。
//
// 三個欄位都是**收窄**，不是方便設定：
//
//   - Dir 固定為 Workspace 根。不固定的話 `ls`、`git status` 的可觀測行為會隨使用者
//     從哪個目錄啟動 oryxos 而變——而 `oryxos chat` 是在 Workspace 的父目錄執行的。
//     這與 CheckFilePath 的解析基準是**同一個**，論述直接沿用。
//   - PathDirs 是過濾後的絕對 PATH 段（見 EffectivePathDirs）。它有兩個消費者——
//     解析執行檔、以及子進程 Env 的 PATH——而且**必須是同一份**（US 55）。
//   - Timeout 是單次命令的上限，來自 config.yaml 的 shell.timeout_seconds（三態回退
//     在 config 那一層做完，這裡拿到的是已生效的值）。
type ShellRuntime struct {
	Dir      string
	PathDirs []string
	Timeout  time.Duration
}

// shellTool 是內建 Tool shell：執行**單一程式加參數**，不經任何 shell 直譯器。
//
// **執行模型是結構化 exec**（ADR-0005，已推翻本 spec 初版的 `bash -c` 路線）。輸入是
// `command`（程式名）＋ `args`（參數陣列），以 exec.CommandContext 直接執行。管線、
// 重導向、命令替換、glob、變數展開一律不存在——`args` 裡的 `|`、`;`、`$(...)` 只是
// 普通字元，會原樣進 execve 成為參數。
//
// **注入在結構上不成立，因為中間沒有第二個解析器。** `bash -c` 之下交出去的是一段
// 文字，由 bash 決定怎麼切、怎麼展開；ADR 記錄的實測是 `echo "$(rm -f victim.txt)"`
// 通過了逐段校驗**並且真的刪了檔**。結構化 exec 交出去的是一個陣列，白名單檢查因此
// 退化成「argv[0] 是否在清單裡」一次字串比對，沒有切分器可以被騙。
//
// **白名單的保證只到「OryxOS 直接 execve 的程式」，不延伸到那個程式接下來啟動什麼。**
// 被列入的程式可依它自己的參數或配置啟動清單外的程式，而這類工具**遠不只直譯器**：
// `find -exec`、`xargs`、`git -c core.pager=`／hooks、`make` 的 recipe、
// `tar --use-compress-program` 都做得到。這份清單不完整，也不試圖窮舉——「哪些看似
// 無害的工具能拿來執行別的程式」本身是一個持續被擴充的研究題目。
//
// 因此**不得**從這裡反推出兩句話：「只列 echo／cat／ls 就安全」（白名單管的是跑哪個
// 程式，不是那個程式能碰什麼——`cat` 讀得到任何路徑），以及「只要不列直譯器就安全」
// （`find` 與 `git` 都不是直譯器）。
//
// **shell 完全不受 file.allowed_paths 約束。** 兩段白名單並排寫在同一份 config.yaml
// 裡，使用者幾乎必然以為前者也管得住後者。os.Root 對 File Tool 是真邊界（Go 程式
// 自己開檔，openat 管得住），但它**不改變進程的檔案系統視圖**，對子進程沒有任何作用。
// 要真隔離就把 oryxos 跑在容器裡，容器級隔離屬擴展階段。
//
// **逾時的保證只有三句，不得寫得更好聽**（ticket #35，完整論述見 shell_lifecycle.go）：
// Execute **一定在期限內返回**（第零道 ＋ 第三道，**不是** WaitDelay）；**同一 process
// group 內、且可被 SIGKILL 回收的**後代一定被收掉；**不保證**脫離 group 者死亡、不保證
// 卡在 uninterruptible sleep 的直接子進程被回收、也不保證卡在解析或 Start 的 worker
// 完成——這些情形的回填與審計訊息都**如實說明可能有殘留**。
type shellTool struct {
	checker *SandboxChecker
	runtime ShellRuntime
	// limiter 是**整個 OryxOS 進程共用一份**的 admission slot，由 composition root
	// 建立一次再注入（憲法 5.2；不是 package 級全域，也不在 buildToolRegistry 內部
	// 建立——那個函式有兩個呼叫點）。詳見 ShellLimiter 的型別說明。
	limiter *ShellLimiter
	// hooks 是測試替身的同步點，正式路徑一律是零值（全 nil）。見 shellTestHooks。
	hooks shellTestHooks
}

// NewShell 建立內建 Tool shell。依賴顯式注入（憲法 5.2），形狀同 NewReadFile。
//
// limiter 刻意是**獨立參數**而不是 ShellRuntime 的一個欄位：ShellRuntime 是**單次執行
// 的上下文收窄**（工作目錄、PATH、期限），而 limiter 是**跨執行、跨 registry 共用**的
// 進程級資源。放進去會讓「整個進程一份」這個性質在呼叫點上完全看不見——而那正是
// spec #29 下修表第十七列要保護的東西。
func NewShell(checker *SandboxChecker, runtime ShellRuntime, limiter *ShellLimiter) OryxTool {
	return &shellTool{checker: checker, runtime: runtime, limiter: limiter}
}

func (t *shellTool) Name() string { return ShellToolName }

func (t *shellTool) Description() string {
	return "在 Workspace 根目錄下執行一個命令白名單允許的程式，回傳 stdout、stderr 與 exit code。" +
		"執行的是單一程式加參數，不經 shell 直譯器：不支援管線與重導向，參數也不做 shell 展開。"
}

// InputSchema 是**「事前教 LLM」唯一的落點**，所以描述文字要把三件事講到（本票 AC）。
//
// LLM 的訓練分佈裡 shell tool 預設吃 shell 語法，不講清楚它很可能仍然生出
// `ls | wc -l`。這段描述與錯誤訊息是同一件事的兩面：**描述負責事前教，錯誤訊息負責
// 事後教**——兩邊都要，因為描述擋不住全部。
func (t *shellTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "要執行的程式名，不含路徑（例如 git，不是 /usr/bin/git），須在 shell.allowed_commands 白名單內。這裡只能放一個程式名，不能放一整串命令文字"},
			"args": {"type": "array", "items": {"type": "string"}, "description": "傳給該程式的參數陣列，省略等於無參數。每個元素原樣成為一個參數：不支援管線（|）與重導向（> >>），要組合多個命令請分多次呼叫、要把輸出寫進檔案請改用 write_file；參數也不做 shell 展開——*.txt 不會展開成檔名清單、$HOME 不會展開成路徑，它們就是那幾個字元"}
		},
		"required": ["command"]
	}`)
}

// shellInput 是 shell 的輸入參數。
//
// Args 用裸 slice 而不是指標：省略與空陣列在這裡是**同一件事**（無參數），沒有
// write_file 的 content 那種「漏填會靜默清空檔案」的風險。
type shellInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// shellOutput 是回填給 LLM 的結果內容。
//
// stdout 與 stderr **分開回填、不合流**：LLM 要據「錯誤訊息說了什麼」決定下一步，
// 混在一起它分不出哪句是輸出、哪句是錯誤。兩者各自標示截斷。
type shellOutput struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

// Execute 校驗命令名、取得 admission slot，然後把「解析 ＋ 建構 ＋ Start」交給第零道
// 那條 goroutine，自己以帶期限的 select 等它。
//
// 任何失敗都以錯誤 ToolResult 回填給 LLM，不 panic、不中斷 turn。**沒有一條路徑標
// Retryable**：白名單拒絕、找不到程式、逾時、取消、slot 已滿，重跑一次結果都一樣
// ——slot 已滿尤其不能標，那會把「拒絕」變成 ReAct 循環的重試風暴。
func (t *shellTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in shellInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", ShellToolName, err)}
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 被取消: %v", ShellToolName, err)}
	}

	// 白名單在**解析執行檔之前、也在取 slot 之前**：兩個理由。不先擋下來，一個不被
	// 允許的命令名照樣會讓我們去 stat 一輪 PATH，而那本身就是資訊洩漏（哪些程式裝在
	// 這台機器上）；而且被拒的呼叫**不該消耗**一張入場券——它連 worker 都不會起。
	//
	// 判斷的依據是**決策**而不是「有沒有錯誤」，理由見 SandboxDecision。
	if decision, err := t.checker.CheckShellCommand(in.Command); decision != SandboxAllow {
		return core.ToolResult{Error: sandboxRefusal(err)}
	}

	// **slot 在第零道那條 goroutine 開始之前取得**，不只是 Start 之前——現在連 PATH
	// 解析都可能卡住（stat 落在故障的 NFS／FUSE 掛載上），門要開在最外面。
	//
	// **滿了就在啟動進程之前拒絕，不排隊**：排隊會把「拒絕」變成「掛住」，違背
	// bounded return 的初衷。
	if !t.limiter.acquire() {
		return core.ToolResult{Error: t.slotExhaustedMessage(in.Command)}
	}
	slot := &shellSlot{limiter: t.limiter}

	// 逾時是這個 Tool 自己的期限，疊在呼叫端的 ctx 之上：呼叫端更早取消仍然生效。
	//
	// **用 WithTimeoutCause 而不是 WithTimeout，是為了把「誰先到期」固定在第一次取消的
	// 當下。** 事後比對 ctx.Err() 判斷不出順序：shell 的上限先到、命令被殺、而回收在
	// 呼叫端的期限**之後**才完成時，那時 ctx.Err() 已經非 nil，於是一個**由 shell 自己
	// 的上限觸發**的中止會被報成「呼叫端的期限先到」——使用者於是跑去調一個沒被觸及的
	// 設定，而真正該調的 timeout_seconds 反而沒人動。context.Cause 讀的是第一次取消時
	// 記下的原因，與之後誰又到期無關。
	runCtx, cancel := context.WithTimeoutCause(ctx, t.runtime.Timeout, errShellTimeout)
	defer cancel()

	handoff := &shellHandoff{}
	delivery := t.startShellWorker(runCtx, in, handoff, slot)

	var started *shellStarted
	select {
	case res := <-delivery:
		if res.err != nil {
			// 解析或 Start 失敗。slot 已由 worker 歸還（所有權從未移交）。
			return core.ToolResult{Error: res.err.Error()}
		}
		if !handoff.mainTake() {
			// 走不到（只有主路徑寫得到 abandoned，而它在這個分支沒寫過），但保留
			// 這條判斷讓「handed 只能由主路徑提交」是程式自己的性質，不是註解的宣稱。
			return core.ToolResult{Error: t.abortMessage(runCtx, in.Command, cleanupInBackground)}
		}
		started = res.started
	case <-runCtx.Done():
		// **期限處理者永遠是決定者。** 決定的當下誰持有進程，誰就負責 kill＋reap。
		//
		// **兩條路徑的清理揭露不同，這一點不能合成一句。** 兩者都還沒 Wait 成功，所以
		// 都**不得**宣稱「已收掉」——但它們沒有確認的東西不一樣：ready 那條進程確實存在
		// 且已交給 reaper，pending 那條連「命令有沒有啟動」都還不確定。
		if prev := handoff.mainAbandon(); prev == shellReady {
			// 進程已存在、worker 正在投遞：**必須有人把它收下來**，否則它留在背景
			// 跑到底。轉交 detached reaper，自己**不等**（等就破壞 bounded return）。
			reapDetachedShell(delivery, slot)
			return core.ToolResult{Error: t.abortMessage(runCtx, in.Command, cleanupInBackground)}
		}
		// prev == shellPending：worker 可能還卡在解析、或卡在一個**已 fork 但尚未從
		// Start 返回**的階段——那時取消監看還沒建立、進程可能存在也可能不存在。清理
		// 交由 worker 自行完成（見 reapAbandonedShell），而三句保證的第三句明說
		// **卡住的 worker 不保證完成**。
		return core.ToolResult{Error: t.abortMessage(runCtx, in.Command, cleanupUnconfirmed)}
	}

	// ── 進程已接管。第一道（cmd.Cancel）與第二道（WaitDelay）由 runCtx 到期自動觸發 ──
	reaped := reapStartedShell(started, slot, t.hooks.swallowReap)
	// **等法是「等 runCtx 結束，之後最多再等 grace」，不是算一個總時長。** 兩件事都
	// 靠這個形狀成立：解析與 Start 花掉的時間自動算在同一個期限裡（不會變成
	// `2×timeout + grace`）；而**取消**也能立刻起算 grace——`Deadline()` 在 context 被
	// 取消時**不動**，用「還剩多久 ＋ grace」算會讓 Ctrl+C 之後再等掉整段剩餘上限。
	waitErr, ok := awaitShellReap(runCtx, reaped, shellKillReapGrace)
	if !ok {
		// **第三道防線：放棄等待。** 這是 bounded return 的唯一來源——SIGKILL 對卡在
		// uninterruptible sleep 的進程無效，Process.Wait 的 wait4 因此不返回，而
		// WaitDelay 只會「再 Kill 一次 ＋ 關 pipe」，**不能讓進行中的 Wait 提前返回**。
		//
		// **這裡一個位元組都不碰 stdout／stderr**：os/exec 的複製 goroutine 還在寫，
		// 讀它不只是「內容不完整」，是資料競爭。
		return core.ToolResult{Error: t.abandonedMessage(runCtx, in.Command)}
	}

	// ctx 的狀態要先看：逾時／取消時 Wait 也會回一個 ExitError（signal: killed），
	// 照 exit code 那條路走會把「被我們砍掉」報成「命令自己失敗了」。誰的期限先到由
	// abortCause 判斷（順序的理由寫在那裡）。
	if runCtx.Err() != nil {
		// **只有這一條路徑上 Wait 真的返回了**，所以只有這裡能宣稱同 group 內、可被
		// SIGKILL 回收的後代已經收掉。
		return core.ToolResult{Error: t.abortMessage(runCtx, in.Command, cleanupReaped)}
	}

	out := shellOutput{
		Stdout:          started.stdout.text(),
		Stderr:          started.stderr.text(),
		StdoutTruncated: started.stdout.dropped,
		StderrTruncated: started.stderr.dropped,
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		out.ExitCode = 0
	case errors.As(waitErr, &exitErr):
		// **非零 exit code 不算 Tool 失敗**，與 HTTP Tool 對非 2xx 的既有語義同構
		// （http.go 對任何狀態碼都回 OK:true）。「測試失敗了」正是 Agent 最需要知道
		// 的那個事實，把它報成 Tool 壞掉只會讓 ReAct 循環退避重試一件重跑幾次都
		// 一樣的事。
		out.ExitCode = exitErr.ExitCode()
	default:
		// 走到這裡代表 Wait 自己出了問題（不是命令回非零）。**cmd.Cancel 的 ESRCH →
		// os.ErrProcessDone 映射就是為了不讓一個本來成功的命令掉進這一格**：取消與
		// 正常結束競態時 kill(-pgid) 回 ESRCH，若原樣回傳，Go 會把成功的 Wait 改報成
		// 失敗（見 shell_cancel_unix.go）。
		return core.ToolResult{Error: fmt.Sprintf("%s 等待 %s 結束: %v", ShellToolName, in.Command, waitErr)}
	}

	content, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("編碼 %s 結果: %v", ShellToolName, err)}
	}
	return core.ToolResult{OK: true, Content: string(content)}
}

// ── 四種終止訊息。措辭集中在這裡，因為它們共同構成對外的那三句保證 ──

// ── 三句保證在使用者面前的樣子 ──
//
// 揭露拆成**兩半**，因為它們回答的是不同的問題：`cleanup*` 說「同 group 的後代**這次
// 收到什麼程度**」，`residualEscaped` 說「脫離 group 的那些我們**本來就**碰不到」。
//
// **合成一句是不行的。** 只有 Wait 真的返回的那條路徑才能說「已收掉」；期限先到的兩條
// 路徑上回收都還沒完成，宣稱收乾淨就是說謊——而本票的第三句保證正是「這些情形都要
// **如實說明可能有殘留**」。

// residualEscaped 是每一條中止路徑都要帶的那半句。
//
// 脫離 process group 的後代（自己 setsid 的）我們**既殺不到、也數不到**——這與 slot
// 「有上限」是同一件事的兩面，不能讓使用者從後者反推出前者也被管住了。
const residualEscaped = "命令若派生過**脫離 process group** 的後代（自己 setsid 的），" +
	"那些殘留我們殺不到、也數不到"

// cleanupReaped 只用在 **Wait 已經返回**的路徑上。這是唯一能宣稱「已收掉」的地方。
const cleanupReaped = "同一 process group 內、可被 SIGKILL 回收的後代已收掉"

// cleanupInBackground 用在**進程確實存在、但回收交給了背景**的路徑（移交競態下的
// detached reaper，以及主路徑放棄接管時）。
//
// 措辭刻意停在「已交出去」而不是「已收掉」：那條 reaper 還在跑，本次呼叫沒有等它。
const cleanupInBackground = "該進程已交由背景回收（對 process group 送 SIGKILL ＋ Wait），" +
	"但**本次呼叫未等它完成**，因此尚未確認收乾淨"

// cleanupUnconfirmed 用在**連命令有沒有啟動都還不確定**的路徑（期限到時仍是 pending）。
//
// 那時 worker 可能卡在 PATH 解析，也可能卡在一個**已 fork 但尚未從 Start 返回**的
// 階段——後者連取消監看都還沒建立。而三句保證的第三句明說：**卡在解析或 Start 的
// worker 不保證完成**。
const cleanupUnconfirmed = "命令**尚未確認啟動**（解析或啟動本身可能卡住，那時連取消監看都還沒建立），" +
	"清理交由背景的 lifecycle worker 負責，而**卡住的 worker 不保證完成**"

// errShellTimeout 是 shell 自己那個期限的 cause 標記。
//
// 它讓「誰先到期」在**第一次取消的當下**就被固定下來，而不是事後比對 ctx.Err() 去猜
// ——後者在「shell 先到期、回收拖過了呼叫端的期限」時會給出相反的答案。
var errShellTimeout = errors.New("shell 的執行上限到期")

// abortCause 說明是誰的期限先到，讀的是**第一次取消時記下的** cause。
//
// **順序必須在取消的當下決定，不能事後比對。** runCtx 是 ctx 的子節點，context 的錯誤
// 會沿父子傳下來；等回收結束再看 ctx.Err()，只能知道「現在兩邊都過期了」，分不出誰先。
// 呼叫端那兩種措辭**刻意不提 shell 的上限**——提了就是把使用者導向一個沒被觸及的設定。
func (t *shellTool) abortCause(runCtx context.Context) string {
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, errShellTimeout):
		return fmt.Sprintf("逾時（上限 %s）", t.runtime.Timeout)
	case errors.Is(cause, context.Canceled):
		return "被呼叫端取消"
	case cause != nil:
		return fmt.Sprintf("呼叫端的期限先到（%v），不是 shell 自己的上限", cause)
	default:
		// 走不到（只在 runCtx 已 Done 時呼叫），保留一句不說謊的話而不是空字串。
		return "中止"
	}
}

// abortMessage 是中止路徑的統一措辭：誰的期限到了 ＋ **這次收到什麼程度** ＋ 脫離者的揭露。
func (t *shellTool) abortMessage(runCtx context.Context, command, cleanup string) string {
	return fmt.Sprintf("%s 執行 %s %s，已中止；%s；%s",
		ShellToolName, command, t.abortCause(runCtx), cleanup, residualEscaped)
}

// abandonedMessage 是第三道防線：期限與寬限都過了，回收訊號仍未到達。
//
// **代價要寫出來而不是被隱藏**（US 63）：這條路徑上留下了一條回收 goroutine，而那條
// goroutine 佔著的 slot 要等它終於返回才歸還。
func (t *shellTool) abandonedMessage(runCtx context.Context, command string) string {
	return fmt.Sprintf(
		"%s 執行 %s %s，且強制終止後仍未回收，已放棄等待並返回；"+
			"該直接子進程可能卡在無法中斷的狀態（SIGKILL 對它無效），因此**可能有殘留進程**，"+
			"並留下一條回收 goroutine 佔著一個 lifecycle worker 名額直到它終於返回；%s",
		ShellToolName, command, t.abortCause(runCtx), residualEscaped)
}

// slotExhaustedMessage 是 admission slot 已滿。
//
// **措辭不得把上限講過頭**（spec #29 下修表第二十二列）：數的是「未完成的 lifecycle
// worker」，不是「未 reap 的子進程」——worker 可能卡在 PATH 解析或 Start，那時連子進程
// 都還不存在。而且要**明說它不限制脫離的後代**，否則使用者會從「有上限」反推成進程樹
// 層級的資源上限。
func (t *shellTool) slotExhaustedMessage(command string) string {
	return fmt.Sprintf(
		"%s 拒絕執行 %s：目前有 %d 個未完成的 lifecycle worker（上限 %d），已達上限，在啟動進程之前拒絕。"+
			"每個 worker 可能持有 0 或 1 個未回收的直接子進程——它可能卡在 PATH 解析、啟動或等待，"+
			"卡在前兩者時連子進程都還不存在。這些是**回收不掉、需要人介入**的命令："+
			"請查看這台機器上殘留的進程並手動處理，或調整 shell.timeout_seconds。"+
			"注意這個上限**不限制脫離 process group 的後代**（daemonize 的程式會讓名額正常歸還而後代還活著，"+
			"可無界累積）——要限制那個需要 container／cgroup 等進程樹層級的隔離，屬擴展階段。",
		ShellToolName, command, t.limiter.inFlight(), maxShellLifecycleWorkers)
}

// lookupInPathDirs 在過濾後的絕對 PATH 段裡找 name，回傳絕對路徑。
//
// **自己解析、不用 exec.Command 的隱式 LookPath**（ADR-0005）：隱式解析發生在
// Cmd.Dir 被設定**之前**，所以 PATH 裡的相對段與空段是以 oryxos 父進程的工作目錄
// 解讀的，而子進程拿到同一份字串時工作目錄已經是 Workspace 根——同一段在兩邊指向
// 不同的目錄。那些段在 EffectivePathDirs 就被丟掉了，這裡拿到的每一條都是絕對的。
//
// 找不到時的錯誤**與白名單違規分得出來**：兩者使用者的下一步完全不同——前者去裝那個
// 程式，後者去改 config.yaml（US 37）。因此這裡回的是普通錯誤，不是 ErrSandboxViolation。
//
// 可執行的判準是「普通檔 ＋ 帶任一執行位」。這比 access(2) 寬鬆一點（不看擁有者），
// 差異落在「檔案有執行位但我們沒權限執行」那一格，而那一格由 Start 失敗回明確錯誤
// 接住。Windows 的副檔名解析（PATHEXT）不在本切片範圍。
func lookupInPathDirs(name string, dirs []string) (string, error) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	// 訊息不列出 PATH 的內容：它會落日誌與回填 LLM，而那等於交出這台機器的目錄佈局。
	return "", fmt.Errorf("%s 找不到程式 %q：它在 shell.allowed_commands 白名單內，但不在這台機器的 PATH 上（PATH 的相對段與空段一律不採用）",
		ShellToolName, name)
}

// shellChildEnv 組出子進程的環境變數，**白名單式傳遞**——只有三個變數進得去。
//
// 防的是具體的事：Provider 憑證（OPENROUTER_API_KEY 這類）不進子進程，`env`／`printenv`
// 即使被列進白名單也回填不出密鑰（US 22）。
//
// PATH 傳的是**過濾後的絕對段清單**，與解析所用的是同一份——不是父進程的原值。這樣
// 兩邊不只字串相同，**語義也相同**（都與工作目錄無關）。HOME 與 LANG 取父進程同名
// 變數的原值，父進程沒有就省略該筆。
func shellChildEnv(pathDirs []string) []string {
	env := []string{"PATH=" + strings.Join(pathDirs, string(os.PathListSeparator))}
	for _, key := range []string{"HOME", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// boundedBuffer 是一個帶上限的 io.Writer：收下前 limit 個位元組，其餘丟掉並記下
// 「丟過東西」。
//
// **Write 一律回報全部寫完**（不回 short write）：回 short write 會讓 exec 的複製
// goroutine 收到 io.ErrShortWrite 並中止，那會把「輸出太長」變成一次執行失敗——而
// 截斷是正常情況，不是失敗。
type boundedBuffer struct {
	limit   int
	buf     bytes.Buffer
	dropped bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	// **remaining 必須在寫入之前算。** 寫完之後 b.buf.Len() 已經含有這次收下的資料，
	// 再拿它去加 len(p) 等於把同一段位元組數兩次——輸出**剛好等於上限**時就會被誤標
	// 成截斷過。判準是「這次進來的量有沒有超過寫入前還剩的額度」，就這一件事。
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		b.buf.Write(p[:min(remaining, len(p))])
	}
	if len(p) > remaining {
		b.dropped = true
	}
	return len(p), nil
}

// text 回傳收下的內容；截斷過的話把尾端退到一個完整的 UTF-8 字元邊界。
//
// 直接切在第 N 個位元組會把一個多位元組字元切成兩半，json.Marshal 接著把那半個字元
// 換成 U+FFFD——LLM 讀到的最後一個字是壞的。理由與 trimPartialRune 那邊完全相同。
func (b *boundedBuffer) text() string {
	if !b.dropped {
		return b.buf.String()
	}
	return string(trimPartialRune(b.buf.Bytes()))
}
