# Shell Tool 走結構化 exec，不經 shell 直譯器

2026-08-17 spec #4 的動工前定案選了「`shell` 走 `bash -c` 保留管線、命令白名單逐段校驗、白名單誠實定位為防呆」。同日的獨立 spec 審查以本機實測推翻了這條定案的可實作性：bash 的**雙引號不抑制** `$( )` 與反引號展開，因此 spec 自己寫的規則（「命令替換在引號之外出現時一律拒絕」＋「引號內的任何字元放行」）會讓 `echo "$(rm -rf x)"` **通過校驗並真的執行 `rm`**——

```
bash -c 'echo "result: $(rm -f victim.txt && echo DELETED)"'
→ result: DELETED ; victim.txt exists? NO
```

首個 token 是 `echo`，逐段校驗全數通過。這與 spec 繞過手法表宣稱的「命令替換一律拒絕」直接矛盾，而 `echo "Found: $(ls)"` 這種句型是 LLM **會自然產生**的寫法，落在「防呆」該擋的範圍之內。

第二個發現是切分規則寫不到可實作精度：規則說按 `|`、`||`、`;`、`&&`、`&`、換行切段，又說 `2>&1` 放行——`2>&1` 裡就有一個 `&`，照規則切會把 `ls 2>&1 | wc -l` 裂成 `ls 2>` 與 `1 | wc -l`，第二段首 token 是 `1`，**合法命令被誤擋**。同一類未定義的還有 `&>`／`>&`／`|&`、`&&` 與 `&` 的比對優先序、反斜線跳脫的分隔符、`#` 註解、背景執行造成的空段，以及 here-doc——`cat <<EOF` 的 body 會被「按換行切段」逐行當成命令名校驗，正常的 here-doc 幾乎必然被誤擋。要補完，等於在 Go 裡寫一份含引號狀態機與最長匹配的 shell 掃描器規格。

**現決定把執行模型改為結構化 exec**：`shell` Tool 的輸入是 `command`（程式名）＋ `args`（參數陣列），以 `exec.CommandContext` 直接執行，**不經任何 shell 直譯器**。放棄管線與輸出重導向。

決定性的理由不是「exec 比較安全」——兩案都不是進程隔離，這點下面說明——而是**白名單的保證性質不同**。`bash -c` 之下，交出去的是一段**文字**，由 bash 決定怎麼切、怎麼展開；Go 這邊的任何檢查都是在猜 bash 會怎麼解讀那串字，是一場 parser 對 parser 的競賽，上面兩個發現就是輸掉的證據。結構化 exec 交出去的是一個**陣列**，直接進 `execve`，**中間沒有第二個解析器**：白名單檢查退化成「`argv[0]` 是否在清單裡」一次字串比對，沒有切分器可以被騙；`;`、`&&`、`$(...)` 傳進去只是普通字元，會原樣成為參數，注入在結構上不成立。

## 與規劃文檔的關係

**本 ADR 刻意偏離需求文檔 §5.6 的字面。** 該節寫「Shell Tool（執行 bash 命令，有超時和命令白名單限制）」。核心階段的 `shell` 自此**不執行 bash 命令**，執行的是單一程式加參數。偏離的理由是同一句話裡的另一半——「有命令白名單限制」——在 `bash -c` 之下無法成立，而白名單是需求文檔自己列為 Sandbox 核心手段的東西。兩者衝突時保白名單。

**本 ADR 讓技術方案 §6.7 的字面回歸成立。** 該節寫「`checkShellCommand`（拆出命令**首個** token 比對白名單）」（單數）。原定案為了讓白名單擋得住 `echo hi | rm -rf x` 必須改成逐段校驗，因此 spec #4 帶了一整段「刻意偏離 §6.7 字面」的說明。結構化 exec 之下 `argv[0]` **就是**首個 token，§6.7 的字面規則不需要修訂，那段偏離說明連同它一起消失。

## Considered Options

考慮過另外兩個方向，都拒絕了：

- **保留 `bash -c`，把切分器補到可實作精度**（引號狀態機、分隔符最長匹配、fd 重導向 token 整體識別、空段略過、here-doc 與 `#` 註解明確不支援）。這條路技術上做得到，但它要求 OryxOS 在 Go 裡維護一份 shell 語法的子集解析器，而正確性的判準是「與 bash 的實際行為一致」——一個沒有規格、只有實作的移動標的。原定案自己寫著「不為了堵繞過手法把切分器越做越複雜」，補完切分器正是在做那件事。憲法 3.2（反過度工程）指向拒絕。
- **exec ＋ 結構化 pipeline 陣列**（`pipeline: [{command, args}, ...]`，由 Go 用 `StdoutPipe` 串接）。這條保住了管線且白名單仍然結構成立，但要另外定義中間段失敗的 exit code 語義、stderr 如何合流、以及部分段成功時的回填形狀。沒有任何 spec 要求管線必須在一次 Tool 呼叫內完成——ReAct 循環本來就能多次呼叫。憲法 3.1（YAGNI）指向拒絕；日後若實際使用中真的痛，這是一個明確的擴展點。

## 連帶確立的四條

**一、執行上下文收窄，不只換執行方式。** `exec.Cmd.Dir` 固定為 **Workspace 根**（與 `checkFilePath` 的解析基準同一個，論述直接沿用）；`Env` 是**白名單式傳遞**——只放 `PATH`、`HOME`、`LANG` 三個，其餘一律不傳。`HOME`／`LANG` 取父進程同名變數的原值（沒有就省略該筆）；**`PATH` 傳的是過濾後的絕對段清單，不是父進程的原值**（理由與過濾規則見下面第三步）。防的是具體的事：Provider 憑證（`OPENROUTER_API_KEY` 這類）不進子進程，`env`／`printenv` 即使被列進白名單也回填不出密鑰。

**`PATH` 的信任邊界必須明說，因為 Go 的解析時機在收窄之前。** `exec.Command` 在**建立 `Cmd` 的當下**就對不含路徑分隔符的名字呼叫 `LookPath`（官方文件：「If name contains no path separators, Command uses LookPath to resolve name to a complete path」），而 `LookPath` 查的是「the directories named by **the PATH environment variable**」——即 **oryxos 父進程的 `PATH`**。事後設定 `Cmd.Env` 不會回頭改變已解析的 `Cmd.Path`。若 `Env` 裡放一個與父進程不同的 `PATH`，就會出現「子進程環境已收窄，但實際執行的檔案仍由繼承的 `PATH` 決定」的落差。

**而且「`PATH` 字串相同」推不出「解析語義相同」。** 隱式 `LookPath` 發生在 `Cmd.Dir` 被設定**之前**，因此 `PATH` 裡的**相對段**（`./bin`、`bin`）與**空段**（`::`、開頭或結尾的 `:`，POSIX 上等於當前目錄）是以 **oryxos 父進程的工作目錄**解讀的；而子進程拿到同一份 `PATH` 字串時，它的工作目錄已經是 **Workspace 根**——同一段字串在兩邊指向**不同的目錄**。此外 Go 1.19 起 `LookPath` 對解析出的相對路徑會回一個滿足 `errors.Is(err, exec.ErrDot)` 的錯誤，行為又多一個分支。

因此定案：**不使用 `exec.Command` 的隱式 `LookPath`，自己解析**，三步：

1. **取父進程的 `PATH` 切段，丟掉所有相對段與空段。** 它們的語義依賴 cwd，而 cwd 在父子之間必然不同——保留它們就等於保留一個無法自圓其說的分支。**這是拒絕，不是忽略**：若某個白名單命令只在被丟掉的段裡找得到，結果是「找不到該程式」的明確錯誤，而不是悄悄從別處執行。
2. **只在剩下的絕對段裡查找，得到絕對路徑。** 然後**仍然用 `exec.CommandContext` 建構，建構後再改 `argv[0]`**，三行：

   ```
   cmd := exec.CommandContext(ctx, resolvedAbsPath, args...)  // 絕對路徑 → 不觸發 LookPath
   cmd.Args[0] = originalCommandName                          // argv[0] 回到白名單名字
   cmd.Cancel = func() error { /* process-group kill */ }      // 覆寫預設的 Process.Kill
   ```

   **不得手動建構 `&exec.Cmd{Path: ..., Args: ...}`。** `exec.Cmd` 存放 context 的欄位**未匯出**，只有 `CommandContext` 能設；而 `Cancel` 的文件明寫「the command **must have been created with CommandContext** and Cancel will be called when the command's Context is done」。手動建構等於**沒有任何取消監看**——第四條那整套逾時契約會從第一道就斷掉，比 `argv[0]` 的問題嚴重得多。

   **`argv[0]` 這一行不能省。** 官方文件寫明「**Args[0] is always name**, not the possibly resolved Path」——傳絕對路徑當 `name`，`argv[0]` 就是那個**絕對路徑**。後果有二：第二條「白名單決定 `argv[0]`」在字面上不再成立；busybox／git 這類**依 `argv[0]` 改變行為的 multicall 程式**會看到不同的名字。建構後改 `cmd.Args[0]` 同時解決：執行的檔案是我們自己解析的絕對路徑（`Cmd.Path` 由建構時的 `name` 決定），而 `argv[0]` 是白名單裡那個名字。傳絕對路徑也讓 Go 不觸發隱式 `LookPath`，`ErrDot` 分支一併消失。

   **`cmd.Cancel` 是 process-group kill 的正確掛點**（第四條第一道）。`CommandContext` 的預設值只呼叫 `Process.Kill`（殺直接子進程），覆寫它才能對 `-pgid` 送訊號。

   **但 `Cancel` 的回傳值有一條必須做的錯誤映射。** 官方文件：「If the command exits with a success status after Cancel is called, and Cancel **does not return an error equivalent to `os.ErrProcessDone`**, then Wait and similar methods will return a non-nil error」。取消與正常結束會競態——命令剛好成功退出、process group 已經消失，此時 `kill(-pgid, SIGKILL)` 回 **`ESRCH`**。若把它原樣回傳，Go 會把一個**本來成功**的 `Wait` 改報成取消函式的錯誤，使用者看到「命令失敗」而它其實跑完了。因此 `Cancel` 必須把 `ESRCH`（以及 `os.ErrProcessDone`）映射成**包裝 `os.ErrProcessDone` 的錯誤**，只有真正的失敗（如 `EPERM`）才照實回傳。**要有「正常退出與取消競態」的測試**：讓命令在取消訊號送出的同時成功結束，斷言回填結果是**正常的 exit code 0**、不是逾時錯誤。
3. **`Env` 的 `PATH` 傳過濾後的絕對段清單**——與解析所用的是同一份。這樣兩邊不只字串相同，**語義也相同**（都與 cwd 無關）。

考慮過改走「自行捏造一份固定可信 `PATH`」，**不採納**：那會讓 Homebrew（`/opt/homebrew/bin`）、`go install`（`~/go/bin`）這類**正常安裝位置**的工具找不到。上面的方案仍然只用父進程給的目錄，只是**濾掉語義不可攜的段**。**這條要有測試釘住**——`PATH` 含相對段／空段時，斷言解析結果與子進程 `Env` 的 `PATH` 都不含那些段。

**接受父進程 `PATH` 為信任邊界，但不得把它排除出威脅模型——因為有一條內部提權路徑。** 若父進程的 `PATH` 含有一個**落在 `file.allowed_paths` 之內的絕對目錄**，那麼 Agent 只靠**已被授權的 `write_file`** 就能在那裡放一個與白名單命令同名的檔案，或覆寫該目錄下既有的可執行檔——**把「檔案寫入權限」升級成「`shell` 白名單內的程式執行權限」**。這條不需要任何外部攻擊者，兩個能力都是使用者自己開的，所以「攻擊者已經能寫檔了」那套論證在此不成立。

**注意這裡只算絕對目錄。** 相對段（`./bin`、`node_modules/.bin`）與空段在解析的第一步就被丟掉了，它們**不構成**這條路徑；而 `direnv`／`mise`／`nvm` 這類工具展開後放進 `PATH` 的**是絕對路徑**（`/home/u/proj/node_modules/.bin`），那才是真正要防的形態。另一種是**符號連結**：`PATH` 寫 `/opt/tools`，而它連到 Workspace 內某目錄。因此定三件事：

1. **要求部署者確保 `PATH` 上的目錄與 `file.allowed_paths` 不重疊。** 這是部署契約的一部分，要寫進 `config.yaml` 的模板註解與文檔，不能只存在於 ADR 裡。
2. **啟動時檢查重疊並警告——依實際解析出的目錄比對，且 symlink-aware。** **字串比對或單純的 Workspace-relative 比對會漏掉三種情形**：相對段、空段、以及**指向可寫目錄的符號連結**（`PATH` 裡是 `/opt/tools`，而它是一個指向 Workspace 內某目錄的連結）。因此比對的兩邊都必須先化到真實路徑：`PATH` 用的是**上面第三步過濾後的絕對段**，`file.allowed_paths` 相對 Workspace 根絕對化，**兩邊都再走 `filepath.EvalSymlinks`**，然後做**子樹包含**判斷（不是字串前綴——`/tmp/foo` 不得匹配 `/tmp/foobar`，與 `checkFilePath` 第 4 條同一個判準）。相對段與空段在第一步就被丟掉，因此不會漏進來。比照既有的空白名單提醒（`cmd/oryxos/chat.go:372-374`）印一行——**警告而非 fail fast**，因為重疊可能是使用者刻意的。
3. **`write_file` 一律以非可執行權限建檔，且不修改既有檔案的權限。** 這縮小了面（新放的檔案不會被 `LookPath` 選中——它只找可執行檔），但**不宣稱足夠**：覆寫該目錄下**既有**可執行檔的內容照樣達到目的，所以第 1、2 條才是主要的緩解。

**二、白名單只約束 OryxOS 直接啟動的子進程的 `argv[0]`。** 定位從「防呆」改為「**直接子進程的程式名這一層結構上成立的邊界**」：`argv[0]` 由白名單確定地決定，沒有字串技巧能改變 OryxOS 自己 `execve` 的是哪個程式。

**這個保證不延伸到後續的進程。** 被列入的程式可以**依它自己的參數或配置**啟動清單外的程式，而這類工具遠不只直譯器：

- **參數驅動**：`find -exec`、`xargs`、`tar --use-compress-program`、`rsync -e`、`sed -e 'e cmd'`（GNU）、`ssh`／`scp` 的 `ProxyCommand`。
- **配置驅動**：`git` 的 `-c core.pager=`／`-c alias.x=!cmd`／repo 內的 hooks 與 credential helper、`make` 的 Makefile recipe。
- **直譯器**：`bash`、`sh`、`python`、`perl`、`node`、`awk`⋯⋯

這份清單**不完整，也不試圖窮舉**——「哪些看似無害的工具能拿來執行別的程式」本身是一個持續被擴充的研究題目（GTFOBins 一類的資料庫就是在編它）。因此**不得**把「只要不列直譯器就安全」寫進任何文檔或模板註解：那個反推是錯的，`find` 與 `git` 都不是直譯器。

仍然成立的差別在於**白名單能不能決定第一個 `execve`**：`bash -c` 之下它**決定不了**——使用者只列 `echo`，`echo "$(rm -rf x)"` 就讓 `rm` 跑起來，出事不需要使用者列進任何危險程式（本 ADR 開頭的實測）。結構化 exec 之下第一個 `execve` 被確定決定，清單外的程式若被執行，必定是**經由某個被列入程式自身的能力**。攻擊面因此從「整個 shell 語法」收斂到「你列的那幾個程式各自能做什麼」——這是收斂，不是消除，而且要求列白名單的人**理解他列的每個程式的能力**。這不是一個能靠文檔完全轉移的責任，所以第三條的容器建議才是核心階段真正的答案。

**三、`shell` 完全不受 `file.allowed_paths` 約束，這句話必須寫出來。** 兩段白名單並排寫在同一份 `config.yaml` 裡，使用者幾乎必然以為前者也管得住後者。`os.Root` 對 File Tool 是真邊界（Go 程式自己開檔，`openat` 管得住），但它**不改變進程的檔案系統視圖**，對子進程沒有任何作用。`shell` 能碰的範圍是 oryxos 進程本身的權限。要真隔離，可執行的建議是把 oryxos 跑在容器裡——零實作成本，比再堵三個繞過手法有效。容器級隔離本身仍屬擴展階段（技術方案 §6.7）。

**四、逾時的後代進程與 bounded wait 必須自己實作，`CommandContext` 不會給。** 這條是實作要求，不是本 ADR 的所得——結構化 exec 只減少了**一層**（不必再處理 bash 自己），「子進程派生的後代」這個問題**完全沒有消失**。Go 的預設行為有兩個缺口，官方文件都寫明：

- `CommandContext`「sets the command's Cancel function to invoke the **Kill** method on its **Process**」——只殺**直接**子進程，後代成為孤兒。
- 同一份文件：`CommandContext`「leaves its WaitDelay unset」，而 `WaitDelay` 為零時「I/O pipes will be read until EOF, **which might not occur until orphaned subprocesses of the command have also closed their descriptors for the pipes**」——後代若繼承了 stdout／stderr 的 write end，`Wait` 會在 context 到期後**繼續阻塞**。

兩者合起來直接違反憲法 5.3（取消要貫穿到底、不得洩漏 goroutine）。**專案已經解決過完全相同的問題**——`internal/tool/mcp_process_unix.go` 對 MCP server 的子進程做的就是這件事，而它的測試已經把保證的**邊界**釘死。因此本切片沿用那套形狀與那個誠實程度，不自己另編一套：

**三道防線，每道都有期限——不是兩道。** `internal/tool/mcp.go` 的 `forceClose` 已經把這件事做完，其註解逐段寫明，本切片照抄它的分段與誠實程度：

1. **殺整棵樹（process group）→ 等一段 grace。** `SysProcAttr.Setpgid = true` 建立獨立的 process group，逾時／取消時對 `-pgid` 送 `SIGKILL`。**保證範圍是「仍在同一 process group 內的後代」**——後代只要自己呼叫 `setsid()`／`setpgid()` 就脫離射程。既有測試把兩種情形分成兩格（`mcp_process_unix_test.go`）：同 group 的孫進程那一格**斷言它死了**；`TestMcpCloseReturnsWhenGrandchildEscapesProcessGroup` **只斷言準時返回、不斷言它死了**，註解寫著「殺不到就是殺不到，假裝驗得到只會讓測試說謊」。
2. **仍卡住 → 關掉我方讀取端 → 再等一次。** 脫離者照樣抓著 stdout／stderr，此時唯一能做的是關掉**我們這一側**的 pipe，讓讀取 goroutine 拿到「檔案已關閉」收工。`Cmd.WaitDelay` 的非零上限承擔這件事。這道同時是**非 Unix 平台唯一的保障**（比照既有的 `mcp_process_other.go` 分檔）。
3. **還是卡住 → 放棄等待，回錯誤並回報可能殘留。** **前兩道給不出 bounded return**：`SIGKILL` 對卡在 uninterruptible sleep（D state）的進程無效，該進程不會被 OS 回收，`Process.Wait` 的 `wait4` 就不返回——而 `Cmd.WaitDelay` 只會「再 Kill 一次 ＋ 關 pipe」，**它不能讓一個進行中的 `Process.Wait` 提前返回**。因此必須像 `forceClose` 那樣：把 `cmd.Wait()` 放進**獨立的 reap goroutine**，主路徑以 `select` 帶期限等它（既有的 `awaitReap` 就是這個形狀），期限到就放棄等待、回錯誤。第三道幾乎到不了，但它是 bounded return 的**唯一**來源。

**第三道必須有上限——這一點不能照抄 MCP。** 既有註解對代價的交代是「那條回收 goroutine 會留著，這是刻意的取捨：一條卡在已經無法回收的進程上的 goroutine，好過一個永遠回不來的 `oryxos chat`」。那個取捨在 MCP 成立，**因為 MCP server 的數量由 `config.yaml` 限定**，洩漏有天然上限。`shell` 沒有這個性質：**它由 LLM 觸發、可以在一個 turn 內反覆呼叫、也可以跨 session 重複**。原樣套用等於「每一次不可回收的命令都永久留下一個進程 ＋ 一條 goroutine」，而觸發次數無上限——這是一條可被反覆踩的資源洩漏路徑，不是一次性的取捨。

因此加一道**固定容量的 admission slot，在啟動進程之前取得**：

- **時機是啟動前，不是走到第三道時。** 「第三道才佔 slot」擋不住並發——多個 session（或同一 turn 的多次呼叫）可以**同時**啟動命令並一起走進第三道，那些不可回收的進程與 goroutine **在 slot 滿之前就已經產生了**，事後拒絕「後續」呼叫收不回它們，實際佔用仍會超過上限。要真的有上限，門必須開在 `Start` 之前。
- **歸還規則只有一條，用「進程所有權」表述，不列舉失敗種類**：**任何尚未成功把進程所有權移交給 reap 路徑的終止路徑，一律歸還 slot**；一旦移交成功，slot 由 reap 路徑負責（正常 `Wait` 返回時歸還，走到第三道則延遲到那條 goroutine 終於返回才歸還）。

  **為什麼要這樣表述，而不是列舉。** 先前的版本寫「(a) `Wait` 返回 (b) `Start` 失敗 (c) 第三道」——那是一份**會漏的清單**：它沒涵蓋 `Start` **之前**的失敗，也就是 **`PATH` 解析階段**（命令不在任何絕對段裡、`stat` 回 I/O 錯誤、解析被期限打斷）。那些路徑上根本沒有進程、也不能呼叫 `Wait`，於是 slot 永遠不歸還——**連續八次「找不到程式」就耗盡 slot**，而那是最常見的錯誤（LLM 呼叫一個沒安裝的工具就會發生），比 `Start` 失敗更容易踩到。改用「有沒有移交所有權」這個**二分**判準，任何現在或未來新增的早期失敗都自動被涵蓋。實作上就是取得 slot 後立刻 `defer` 一個「若尚未移交則歸還」的釋放。

  **驗收要涵蓋兩種早期失敗**：連續超過 8 次**解析失敗**（呼叫一個白名單內、但不存在於任何 `PATH` 絕對段的程式）後，斷言正常呼叫**仍然可用**；連續超過 8 次 **`Start` 失敗**同樣。

- **容量定為 8，作用域是「單一 OryxOS 進程共用一個 limiter」。** 這一點必須寫明：若每個 Profile、每個 session、或每個 `ShellTool` 實例各自建立一個 limiter，跨 session 的總量又變回無界，這一整段的威脅模型就自我作廢。因此 limiter 是**一個實例**，**由 composition root 建立一次，再注入每一個 registry／`ShellTool`**——**不是 package 級全域變數**（憲法 5.2：絕不允許全域變數傳遞狀態，依賴必須顯式注入）。這與 File Tool 拿 `*os.Root`、`shell` 拿超時值是同一個注入形狀，不新增機制。

  **建立點必須在 `buildToolRegistry` 之上，不能在它裡面。** `buildToolRegistry` 有**兩個呼叫點**（`cmd/oryxos/chat.go:330` 與 `tools.go:102`），且未來可能更多——若 limiter 在它內部建立，每次呼叫就是一份新的，「整個進程一份」當場失效。因此 limiter 由**呼叫 `buildToolRegistry` 的那一層**建立並當參數傳進去。

- **容量的意義**：與其他上限放同一個常數區塊；核心階段是單節點、需求文檔的性能目標是 10 個 Agent 等級，8 個並發 `shell` 呼叫足夠正常使用。**它限制的是「至多 8 個未完成的 `shell` lifecycle worker」**——而一個 worker 可能卡在**任何**階段：`PATH` 解析（`stat` 卡在故障掛載）、`Start`（`fork`／`execve` 卡住，**此時連直接子進程都還不存在**）、`Wait`（子進程在 D state）。因此那 8 條 goroutine **不都是「等待子進程的」**，把例外只描述成「未 reap 直接子進程的等待 goroutine」是不準確的：第零道永久卡住時同樣留下 goroutine ＋ 佔著 slot，而那時根本還沒有子進程。**準確的說法是：至多 8 個未完成的 lifecycle worker，其中每一個可能持有 0 或 1 個未回收的直接子進程。**

  **limiter 不限制脫離的後代，這一點不能含糊。** 一個 daemonize 的程式（子進程 `setsid` 後自己正常退場）會讓 `Wait` 正常返回、slot 隨即歸還，而那個脫離的後代還活著。反覆呼叫因此仍可累積**無界的 detached descendants**——slot 對此毫無作用，因為它數的是「我們還在等的直接子進程」，不是「這台機器上因我們而存在的進程」。要限制後者需要 **container／cgroup 等進程樹層級的隔離**，那屬擴展階段（第三條、技術方案 §6.7）。這與第四條第一道的邊界是同一件事的兩面：**脫離 process group 的東西，我們既殺不到、也數不到。**
- **容量滿時，在啟動進程之前拒絕**，回一個可操作的錯誤（說明目前有幾個命令未回收、需人介入），而不是排隊等待——排隊會把「拒絕」變成「掛住」，違背 bounded return 的初衷。

驗收必須涵蓋**並發**與**共用**，不只是循序：

1. **`N+1` 個同時卡死的呼叫**，斷言第 `N+1` 個在**啟動進程之前**就被拒絕、且實際存在的未回收進程數**不超過 8**；再讓其中一個 reap 完成，斷言 slot 被歸還、呼叫恢復可用。
2. **`N+1` 個 slot 由多個 registry 建構流程共同競爭**——**經由真實的 `buildToolRegistry` 呼叫兩次以上**（模擬 `chat` 與 `tools` 兩條路徑）拿到的 `ShellTool`，斷言總量仍然守在 8。**手動把同一個 limiter 傳給兩個 Tool 實例是不夠的**——那只驗了「共用時會共用」，驗不到「建立點在對的那一層」；而後者才是這條定案的內容。沒有這一格，日後有人把 limiter 移進 `buildToolRegistry` 內部也不會被測出來。
3. **重複的 `Start` 失敗不影響後續呼叫**——連續讓 `Start` 失敗超過 8 次（例如指向一個存在於白名單、但檔案權限被拿掉的程式），斷言之後正常的 `shell` 呼叫**仍然可用**。這一格直接對應上面的歸還條 (b)。

**第零道：解析與 `Start` 本身也會阻塞，必須一起關進期限之內。** 三道防線都建立在「`cmd.Start()` 已經返回」之上——但**它自己不保證返回**：`LookPath` 要對 `PATH` 的每一段做 `stat`／`access`，而 `Start` 要 `fork`＋`execve`；若 `PATH` 上某一段（或那個執行檔）位在故障的 NFS／FUSE 掛載上，這些系統呼叫會卡在 uninterruptible sleep，**且它們同步、不吃 `context`**。此時 reap goroutine 還沒被建立，後面三道完全介入不了。

因此**把「解析 ＋ 建構 ＋ `Start`」整段也放進一條 goroutine**，主路徑同樣以帶期限的 `select` 等它：

- **所有權移交必須是原子的，不能只靠 `select`。** 期限與 `Start` 成功可以**同時** ready：若 worker 已經把成功結果送進 buffered channel 就認定「移交完成」，而主路徑的 `select` 同時選中 deadline 分支直接返回，結果是**沒有人接管那個進程**——它留在背景跑到底。**buffered channel 加 `select` 不構成移交**。

  **三態不夠，要四態。** 若讓 worker 自己把 `pending` 改成 `handed` 然後才投遞，就會出現一個沒人負責的窗口：worker 已轉成 `handed`、**還沒實際交付**，此時期限到——主路徑看到 `handed`（不是 `pending`）卻仍走期限分支返回錯誤，而 worker 認為自己已經移交，**兩邊都不 reap**。`handed` 因此**只能由主路徑在真正收到進程時提交**，worker 不得自行寫入。

  定四態（以 mutex 保護）：`pending` → `ready` → `handed`／`abandoned`。**規則是「期限處理者永遠是決定者，而決定的當下誰持有進程，誰就負責 kill＋reap」**：

  | 事件 | 動作 |
  | --- | --- |
  | worker 的 `Start` 成功 | 鎖；若已是 `abandoned` → 自己 `SIGKILL` process group ＋ `Wait` 回收 ＋ **歸還 slot**，結束。否則設 `ready`、解鎖、把進程投遞到容量 1 的 channel。**worker 永遠不寫 `handed`。** |
  | 主路徑收到投遞 | 鎖；若狀態是 `ready` → 設 `handed`、解鎖、接管（正常路徑）。若已是 `abandoned` → 期限處理者已接手，主路徑只回逾時錯誤。 |
  | 主路徑期限到 | 鎖；記下 `prev`、設 `abandoned`、解鎖。**`prev == pending`** → worker 之後會看到 `abandoned` 並自行 kill＋reap；**`prev == ready`** → 進程已在 channel 裡（或投遞在即），**主路徑必須把它收下來**，然後交給一條 detached reaper goroutine 去 kill＋reap＋歸還 slot，自己立刻回逾時錯誤。 |

  關鍵是最後一列：`ready` 之下**主路徑不能只是返回**，否則那個已存在的進程就無人接管。它收下之後不自己等（那會破壞 bounded return），而是轉交 reaper——slot 因此持續被佔用到回收完成，與第三道的語義一致。

  這樣任一交錯都**恰好落在**「主路徑接管」「worker kill＋reap」「reaper kill＋reap」三者之一，不會皆非（留下背景進程）也不會皆是（重複回收）。**測試要能確定性地製造那個關鍵交錯**——「worker 先贏狀態鎖（已設 `ready`）、期限隨後才贏 `select`」：以測試替身在 worker 設完 `ready` 之後、投遞之前插入同步點，再觸發期限，斷言該進程**仍被 kill＋reap**、slot 最終歸還。再加一組同情境重複數十次的競態測試，斷言每次都沒有殘留。
- 「逾時之後才啟動成功」的進程**不得留在背景繼續跑**——這是上面 `abandoned` 分支存在的唯一理由。
- **slot 必須在這條 goroutine 開始之前取得**（不只是 `Start` 之前）——因為現在連解析都可能卡住，門要開在最外面。
- **測試必須能注入這兩種情形**：blocked `Start`（例如把一個假的 `PATH` 段指向會阻塞的 FUSE mount，或以測試替身讓解析函式阻塞）與 late success（讓 `Start` 在期限之後才返回成功），斷言 `Execute` 期限內返回、且 late success 的進程**被殺掉並回收**、slot 最終歸還。

**因此本 ADR 對 `shell` 逾時的保證是三句**：

- **`Execute` 一定在期限內返回**——由第零道（解析／`Start` 也在期限內）＋第三道（`Wait` 有期限）共同保證，**不是**由 `WaitDelay` 保證。
- **仍在同一 process group 內、且可被 `SIGKILL` 回收的後代一定被收掉。**
- **不保證**：脫離 process group 的後代死亡；脫離後代的**數量**有上限；卡在 uninterruptible sleep 的直接子進程被回收；卡在解析或 `Start` 的 worker 完成。**這些情形回填給 LLM 與落審計的訊息都要如實說明可能有殘留**，不得宣稱「已清乾淨」。**卡住的 lifecycle worker 會留下 goroutine**（憲法 5.3 的一個明列例外，理由同既有實作）——但**該例外被 slot 上限關在「至多 8 個未完成 worker」的有界範圍內**，不是無限度的豁免。

**不要拿 `exec.ErrWaitDelay` 當「兩種逾時」的區分契約。** 官方文件對它的觸發條件寫得很窄：「If pipes are closed due to WaitDelay, **no Cancel call has occurred**, and the command has otherwise exited with a **successful status**, Wait and similar methods will return ErrWaitDelay instead of nil.」主要的逾時路徑兩個條件都不滿足——context 到期時 `Cancel` 已經呼叫過、且進程是被 `SIGKILL` 打死的（非 successful status），`Wait` 回的是 `ExitError`（signal: killed），`ErrWaitDelay` 被遮蔽。所以 `errors.Is(err, exec.ErrWaitDelay)` 在那條路上永遠不成立，拿它區分是一個**不可實現的契約**。若 ticket 階段確實需要區分「命令自己逾時」與「pipe 沒排空」，必須**自行追蹤** cancellation 與 pipe-drain 的兩個 deadline，並分別測「正常退出但留 pipe」與「逾時殺主進程且留 pipe」兩條路徑。核心階段的最小要求只有 bounded return，不強制區分。

## Consequences

**失去的**：管線（`ls | wc -l`）與輸出重導向（`> out.txt`）不再能在一次 `shell` 呼叫內完成。需要組合時由 LLM 分多次呼叫；需要寫檔時用 `write_file`——那條路徑反而受 `file.allowed_paths` 約束，比 shell 重導向更該用。glob（`*.txt`）與變數展開（`$HOME`）同樣不再發生，因為那些是 shell 的行為。

**得到的**：`checkShellCommand` 從一份掃描器規格退化為一次字串比對；spec #4 審查的兩條 blocking（雙引號內的命令替換、切分器規格不足）連同它們的測試矩陣一起消失；`argv[0]` 這一層的攻擊面從「整個 shell 語法」收斂到「被列入的那幾個程式各自的能力」。

**沒有得到的**（三條曾被寫成所得或保證，都是高估，於此更正）：

- **逾時的後代進程清理沒有變簡單**——只少了「bash 自己」那一層。`CommandContext` 既不殺後代也不 bound `Wait`；而 `WaitDelay` 也**給不出 bounded return**（它不能讓進行中的 `Process.Wait` 提前返回），必須自己補第三道「獨立 reap goroutine ＋ 期限 ＋ 放棄等待」。實作完的保證仍**止於 process group 邊界 ＋ 可被 `SIGKILL` 回收的進程**，且走到第三道時**留下一條 goroutine**（第四條）。
- **白名單的保證不延伸到後續進程**——`find -exec`、`git -c`、`xargs` 這類非直譯器同樣能啟動清單外的程式（第二條）。
- **父進程 `PATH` 不能被排除出威脅模型**——`PATH` 目錄與 `file.allowed_paths` 重疊時，`write_file` 就是一條通往「執行白名單內程式」的內部提權路徑（第一條）。

**要付出的引導成本**：LLM 的訓練分佈裡「shell tool」預設吃 shell 語法，它很可能仍然生出 `ls | wc -l`。Tool 的 InputSchema 描述與拒絕時的錯誤訊息必須明確教它「一個程式加參數陣列」，且這件事要有 fixture 覆蓋（LLM 給出含管線的 `command` → 明確錯誤回填 → 第二輪改用單一命令）。

**工具名保留 `shell`**。`CONTEXT.md` 與需求文檔都用 ShellTools 這個術語，改名波及術語表。代價是名字與行為有落差，靠 InputSchema 的描述文字補。若 fixture 顯示 LLM 因為這個名字持續生錯，改名 `run_command` 是一次獨立的決策。
