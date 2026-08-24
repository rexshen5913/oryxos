package tool

import "time"

// 回填給 LLM 的結果上限集中在這一個常數區塊。
//
// **放同一處是刻意的**：這些數字彼此有關係（它們共同決定一次 turn 最多能塞多少
// 內容進 prompt），散在各 Tool 的檔案裡，下一個要加上限的人看不到別人給了多少，
// 於是每張 ticket 各給一個數字。spec #4 為此把三個上限一次定案（issue #29
// 「回填給 LLM 的結果形狀」）。
//
// spec #4 定案的三個上限現在全數住在這裡（read_file 沿用 HTTP Tool 已有的常數、
// list_dir 的條目上限、shell 的輸出上限），沒有一個是由 ticket 自己挑的。
const (
	// maxResponseBytes 限制單次回填給 LLM 的內容大小（資源佔用限制，需求 §5.6）。
	// HTTP Tool 的回應內文與 read_file 的檔案內容共用同一個上限——兩者都是「一段
	// 從外部拿回來、要塞進 prompt 的位元組」，沒有理由給兩個數字。
	maxResponseBytes = 1 << 20 // 1 MiB

	// maxListDirEntries 限制 list_dir 單次回填的條目數（spec #4 定案的確數）。
	// 它與上面那個上限量的是不同東西——那個數位元組，這個數條目——所以不能共用：
	// 一萬個短檔名離 1 MiB 還很遠，但塞進 prompt 一樣是把 token 燒光。
	maxListDirEntries = 1000

	// maxShellOutputBytes 限制 shell 單次回填的 stdout 與 stderr，**兩者各自**這個
	// 額度（不是合計）。分開算是刻意的：一個把診斷訊息全寫進 stderr 的命令，不該
	// 因為 stdout 很長就看不到自己的錯誤訊息——而 LLM 的下一步常常只靠 stderr。
	maxShellOutputBytes = 256 << 10 // 256 KiB

	// maxShellLifecycleWorkers 是 shell 的 admission slot 容量（spec #4 定案的確數）。
	//
	// **它數的是「未完成的 lifecycle worker」，不是「未回收的直接子進程」。** 一個
	// worker 可能卡在**任何**階段：PATH 解析（stat 卡在故障掛載）、Start（fork／execve
	// 卡住，**此時連直接子進程都還不存在**）、Wait（子進程在 uninterruptible sleep）。
	// 準確的說法是「至多 8 個未完成的 lifecycle worker，其中每一個可能持有 0 或 1 個
	// 未回收的直接子進程」——把它描述成「8 個未 reap 的子進程 ＋ 8 條 goroutine」是
	// 不準確的（spec #29 下修表第二十二列）。
	//
	// **它不限制脫離的後代，這一點不能含糊。** 一個 daemonize 的程式（子進程 setsid
	// 後自己正常退場）會讓 Wait **正常返回**、slot **隨即歸還**，而那個脫離的後代
	// **還活著**；反覆呼叫因此仍可累積**無界的 detached descendants**。slot 數的是
	// 「我們還在等的直接子進程」，不是「這台機器上因我們而存在的進程」。要限制後者需要
	// container／cgroup 等進程樹層級的隔離，屬擴展階段——**使用者不得從「有上限」反推
	// 成進程樹層級的資源上限**。這與第一道防線的邊界是同一件事的兩面：**脫離 process
	// group 的東西，我們既殺不到、也數不到。**
	//
	// 為什麼需要上限而 MCP 不需要：MCP server 的數量由 config.yaml 限定，洩漏有天然
	// 上限；shell 由 LLM 觸發、一個 turn 內可反覆呼叫、也可跨 session 重複，同一個
	// 「留下一條 goroutine 是刻意取捨」原樣套用就變成可被反覆踩的資源洩漏路徑。
	//
	// 8 的來源：核心階段單節點、性能目標是 10 個 Agent 等級，8 個並發足夠正常使用。
	maxShellLifecycleWorkers = 8
)

// shell 生命週期的三個期限。與上面的回填上限分開，因為它們量的是時間不是大小；形狀
// 與命名沿用 MCP 那組（mcp.go 的 mcpCloseTimeout／mcpKillReapGrace）。
const (
	// shellKillReapGrace 是**期限到、第一道與第二道都已觸發之後**，等待回收的寬限。
	// 逾期就走第三道：放棄等待、回錯誤。
	//
	// **第三道是 bounded return 的唯一來源。** SIGKILL 對卡在 uninterruptible sleep
	// 的進程無效，該進程不被 OS 回收、Process.Wait 的 wait4 就不返回；而 WaitDelay
	// 只會「再 Kill 一次 ＋ 關 pipe」，**它不能讓進行中的 Process.Wait 提前返回**。
	//
	// 取 2 秒、與 mcpKillReapGrace 同值：SIGKILL 之後的回收是毫秒級的，這個值不是
	// 效能參數，是「等不到就別再等」的界線。Execute 的最壞返回時間因此是
	// shell.timeout_seconds ＋ shellKillReapGrace。
	shellKillReapGrace = 2 * time.Second

	// shellWaitDelay 是第二道防線的期限：Cancel 觸發之後再過這麼久，仍未回收就由
	// os/exec 關掉**我方**的 pipe，讓複製 goroutine 收工。
	//
	// 脫離 process group 的後代照樣抓著輸出 fd（第一道殺不到它），唯一能做的就是關掉
	// 我們這一側。**這道同時是非 Unix 平台唯一的保障**（那裡沒有 process group）。
	//
	// **必須明顯小於 shellKillReapGrace**：第二道要在第三道放棄之前有機會生效，不然
	// 它形同不存在。
	shellWaitDelay = 500 * time.Millisecond
)

// newFilePerm 是 write_file 新建檔案的權限：擁有者可讀寫、其他人唯讀，**不含執行位**。
//
// 這不是風格選擇，是 spec #4 定案的一條緩解（issue #29「PATH 與可寫路徑重疊矩陣」）：
// 若父進程的 PATH 含有一個落在 file.allowed_paths 之內的目錄，write_file 就能在那裡
// 放一個與 shell 白名單命令同名的檔案，**把檔案寫入權限升級成命令執行權限**。不給
// 執行位讓新放的檔案不會被 LookPath 選中——但**不宣稱足夠**：覆寫該目錄下既有的
// 可執行檔照樣達到目的，主要緩解是啟動時的重疊警告與部署要求（ticket #33）。
//
// 只在**建立**檔案時生效：open(2) 的 mode 參數對既有檔案沒有作用，覆寫因此不改變
// 目標原有的權限（那是定案的另一半，見 write_file 的說明）。
const newFilePerm = 0o644
