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
// **逾時在本票只做到 exec.CommandContext 的原生行為**（只 Kill 直接子進程）：沒有
// process group 終止、沒有 bounded wait。後代若持有 stdout／stderr，Wait 可能不返回。
// 這是**已知缺口，由 ticket #35 關閉**——在那之前，逾時的回填訊息一律如實說明可能有
// 殘留進程，不說任何「已清乾淨」的話。
type shellTool struct {
	checker *SandboxChecker
	runtime ShellRuntime
}

// NewShell 建立內建 Tool shell。依賴顯式注入（憲法 5.2），形狀同 NewReadFile。
func NewShell(checker *SandboxChecker, runtime ShellRuntime) OryxTool {
	return &shellTool{checker: checker, runtime: runtime}
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

// Execute 校驗命令名、在過濾後的 PATH 絕對段裡解析出絕對路徑，然後執行。
//
// 任何失敗都以錯誤 ToolResult 回填給 LLM，不 panic、不中斷 turn。**沒有一條路徑標
// Retryable**：白名單拒絕、找不到程式、逾時、取消，重跑一次結果都一樣。
func (t *shellTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in shellInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", ShellToolName, err)}
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 被取消: %v", ShellToolName, err)}
	}

	// 白名單在**解析執行檔之前**：不先擋下來，一個不被允許的命令名照樣會讓我們去
	// stat 一輪 PATH，而那本身就是資訊洩漏（哪些程式裝在這台機器上）。
	if err := t.checker.CheckShellCommand(in.Command); err != nil {
		return core.ToolResult{Error: err.Error()}
	}

	resolved, err := lookupInPathDirs(in.Command, t.runtime.PathDirs)
	if err != nil {
		return core.ToolResult{Error: err.Error()}
	}

	// 逾時是這個 Tool 自己的期限，疊在呼叫端的 ctx 之上：呼叫端更早取消仍然生效。
	runCtx, cancel := context.WithTimeout(ctx, t.runtime.Timeout)
	defer cancel()

	// **一律 exec.CommandContext 建構，不得手動組 &exec.Cmd{}。** 這不是風格選擇：
	// exec.Cmd 存放 context 的欄位**未匯出**，只有 CommandContext 設得了；而 Cancel
	// 的官方文件明寫「the command must have been created with CommandContext」。
	// 手動建構的 Cmd **完全沒有取消監看**——ticket #35 的第一道防線（覆寫 cmd.Cancel
	// 做 process-group kill）會從一開始就掛不上去，而它同樣能讓 argv[0]、Dir、Env
	// 全都正確、逾時也「會中止」，於是完全通過本票的驗收，等到 #35 開工才炸。
	cmd := exec.CommandContext(runCtx, resolved, in.Args...)
	// **這一行不能省。** 官方文件：「Args[0] is always name, not the possibly resolved
	// Path」——傳絕對路徑當 name，argv[0] 就是那個絕對路徑。後果有二：「白名單決定
	// argv[0]」在字面上不再成立；busybox／git 這類依 argv[0] 改變行為的 multicall
	// 程式會看到不同的名字。傳絕對路徑同時讓 Go 不觸發隱式 LookPath，ErrDot 分支
	// 一併消失。
	cmd.Args[0] = in.Command
	cmd.Dir = t.runtime.Dir
	cmd.Env = shellChildEnv(t.runtime.PathDirs)

	stdout := &boundedBuffer{limit: maxShellOutputBytes}
	stderr := &boundedBuffer{limit: maxShellOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	// ctx 的狀態要先看：逾時／取消時 Run 也會回一個 ExitError（signal: killed），
	// 照 exit code 那條路走會把「被我們砍掉」報成「命令自己失敗了」。
	//
	// **順序是先看呼叫端的 ctx，再看自己的期限，這一點不能反過來。** runCtx 是 ctx 的
	// 子節點，而 context 的錯誤會沿著父子傳下來——呼叫端的 ctx 若**自己帶期限**且先
	// 到期，runCtx.Err() 同樣是 DeadlineExceeded。先比對 runCtx 的話，那種情形會被報成
	// 「shell 的上限到了」並附上一個根本沒被觸及的秒數，使用者於是跑去調
	// timeout_seconds，怎麼調都沒用。
	//
	// 目前的呼叫端（main.go 的 signal.NotifyContext）只帶取消不帶期限，所以這條現在
	// 走不到——但那是**呼叫端當下的性質**，不是這段程式的性質。哪天有人加一個 per-turn
	// 的期限，這裡就會開始說謊；先把順序排對，讓那句話結構上成立。
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return core.ToolResult{Error: fmt.Sprintf(
			"%s 執行 %s 被取消，已中止；命令若派生過子進程，那些後代可能仍有殘留",
			ShellToolName, in.Command)}
	case ctx.Err() != nil:
		// 呼叫端自己的期限先到。**訊息刻意不提 shell 的上限**——提了就是把使用者
		// 導向一個沒被觸及的設定。
		return core.ToolResult{Error: fmt.Sprintf(
			"%s 執行 %s 中止：呼叫端的期限先到（%v），不是 shell 自己的上限；命令若派生過子進程，那些後代可能仍有殘留",
			ShellToolName, in.Command, ctx.Err())}
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// **訊息如實說明可能有殘留，不說任何「已清乾淨」的話。** 本票的逾時只是
		// exec.CommandContext 的原生行為（只 Kill 直接子進程），後代若自己派生就
		// 收不到——ticket #35 會把保證補上去並替換這段措辭。
		return core.ToolResult{Error: fmt.Sprintf(
			"%s 執行 %s 逾時（上限 %s），已中止；命令若派生過子進程，那些後代可能仍有殘留",
			ShellToolName, in.Command, t.runtime.Timeout)}
	}

	out := shellOutput{
		Stdout:          stdout.text(),
		Stderr:          stderr.text(),
		StdoutTruncated: stdout.dropped,
		StderrTruncated: stderr.dropped,
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		out.ExitCode = 0
	case errors.As(runErr, &exitErr):
		// **非零 exit code 不算 Tool 失敗**，與 HTTP Tool 對非 2xx 的既有語義同構
		// （http.go 對任何狀態碼都回 OK:true）。「測試失敗了」正是 Agent 最需要知道
		// 的那個事實，把它報成 Tool 壞掉只會讓 ReAct 循環退避重試一件重跑幾次都
		// 一樣的事。
		out.ExitCode = exitErr.ExitCode()
	default:
		// 走到這裡代表命令**沒跑起來**（解析後檔案被刪、權限被改、fork 資源不足）。
		// 這與「跑起來但失敗了」是兩件事，不能混成同一種回填。
		return core.ToolResult{Error: fmt.Sprintf("%s 啟動 %s 失敗: %v", ShellToolName, in.Command, runErr)}
	}

	content, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("編碼 %s 結果: %v", ShellToolName, err)}
	}
	return core.ToolResult{OK: true, Content: string(content)}
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
