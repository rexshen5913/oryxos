// assembly.go 收 Agent 的共用組裝，分成兩層（spec #7 Implementation Decisions 三、ticket #74）：
// 進程層級每個進程組一次，Profile 層級每份 Profile 組一次。
//
// 分層的判準是「這個東西在同一個進程裡能不能有第二份」，不是它原本寫在 runChat 的哪一行。
// SQLite、審計與 shell admission limiter 多一份就會出事；Tool Registry、MCP 連線與 Executor
// 則本來就該因 Profile 而異。chat 各組一次；之後的 server 會對每份 Profile 各組一次
// Profile 層級。
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/storage"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// processAssembly 是整個進程只該有一份的依賴。
type processAssembly struct {
	// ws 是 Workspace 目錄的路徑（baseDir 底下的 .oryxos）。
	ws            string
	cfg           *config.Config
	logFile       *os.File
	logger        *slog.Logger
	wsRoot        *os.Root
	contextLoader *config.ContextLoader
	longTerm      *memory.LongTermMemory
	shellLimiter  *tool.ShellLimiter
	providers     *provider.Service
	prices        core.PriceList
	store         *storage.DB
	sessions      *storage.SessionManager
	memories      *memory.Service
	audit         *storage.AuditLog
}

// profileAssembly 是一份 Profile 組出來的可運作 Agent。
type profileAssembly struct {
	mcpClients *tool.McpClientService
	// executor 與 agent 分開留著：Profile 過濾後的 Executor 是這一層的產物之一，不只是
	// AgentService 的內部零件——spec #7 的 Tool 清單與無狀態呼叫都要共用同一份。
	executor *tool.Executor
	agent    *core.AgentService
}

// assembleProcess 組出整個進程只該有一份的依賴，並印出只跟 Workspace 有關的啟動提醒。
//
// **失敗時也一律回傳非 nil 的半成品，呼叫端無條件排進 defer 呼叫 Close**，形狀與
// connectProfileMcpServers 相同。半途失敗時已經開起來的日誌檔、Workspace root 與 SQLite
// 要有人收；交給呼叫端收，「造成失敗的錯誤優先、收尾錯誤不蓋過它」這條語義就只寫在
// 呼叫端的那一個 defer 裡，不必在這裡另寫一份清理。
func assembleProcess(ctx context.Context, out io.Writer, baseDir string) (*processAssembly, error) {
	proc := &processAssembly{ws: filepath.Join(baseDir, workspaceDir)}
	if _, err := os.Stat(proc.ws); err != nil {
		return proc, fmt.Errorf("找不到 Workspace %s（請先執行 oryxos init）: %w", workspaceDir, err)
	}

	cfg, err := config.Load(filepath.Join(proc.ws, "config.yaml"))
	if err != nil {
		return proc, fmt.Errorf("載入 Workspace 設定檔: %w", err)
	}
	proc.cfg = cfg

	// 每次 LLM 呼叫的結構化日誌落 Workspace 的 logs/ 目錄。
	logFile, err := os.OpenFile(filepath.Join(proc.ws, "logs", "oryxos.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return proc, fmt.Errorf("開啟日誌檔: %w", err)
	}
	proc.logFile = logFile
	proc.logger = slog.New(slog.NewJSONHandler(logFile, nil))

	// 憑證的 ${ENV_VAR} 展開是**顯式的一步**（issue #27）：Load 不再代勞，因為與
	// Provider 無關的命令（`oryxos tools`）不該被缺一個環境變數擋下。chat 要真的呼叫
	// LLM，所以在這裡展開，缺 key 仍然啟動即報錯。
	providers, err := config.ExpandProviderEnv(cfg.Providers)
	if err != nil {
		return proc, fmt.Errorf("載入 Provider 憑證: %w", err)
	}
	// config（YAML 檔案形狀）與 provider（執行期配置）刻意不共用型別，
	// 避免 internal/provider 依賴設定檔格式；同形搬運是這條解耦的成本。
	providerConfigs := make(map[string]provider.Config, len(providers))
	for name, pc := range providers {
		providerConfigs[name] = provider.Config{APIKey: pc.APIKey, BaseURL: pc.BaseURL}
	}
	proc.providers = provider.NewService(providerConfigs, proc.logger)
	// 定價表攤平給 ReAct 循環算成本（ticket #49）。取自**展開後**的那份是為了與
	// 憑證同源，不是因為定價需要展開——定價是數字，resolveEnv 那條路徑只服務
	// api_key 與 base_url。沒有配置定價段時這裡是空表，成本欄位於是落 NULL。
	proc.prices = config.PriceListOf(providers)

	// 長期記憶、Bootstrap 與 File Tool 的檔案操作都經**同一個** root：越界（含經
	// 符號連結指到 Workspace 之外）由 os.Root 擋下。這幾份 .md 隨 Workspace 進 git，
	// 一個惡意 repo 若把它們做成指向使用者敏感檔案的符號連結，讀取端會把該檔內容
	// 注入 system prompt 送往 Provider（MEMORY.md 的寫入端則會覆寫它）。
	//
	// File Tool 在 os.Root 之上還有一道**應用層白名單**（file.allowed_paths，見
	// internal/tool 的 SandboxChecker.CheckFilePath）：os.Root 界定的是「不出
	// Workspace」，白名單界定的是「Workspace 之內能碰哪幾棵子樹」，兩者不互相取代。
	//
	// 範圍僅止於上述路徑：上面的 logs/oryxos.log 與下面的 SQLite 仍各自開檔
	// （SQLite 由驅動自己開，接不進 os.Root）。
	wsRoot, err := os.OpenRoot(proc.ws)
	if err != nil {
		return proc, fmt.Errorf("開啟 Workspace %s: %w", workspaceDir, err)
	}
	proc.wsRoot = wsRoot
	proc.contextLoader = config.NewContextLoader(wsRoot)
	proc.longTerm = memory.NewLongTermMemory(wsRoot, filepath.Join("memory", memoryFile))
	// **composition root：shell 的 admission limiter 在這裡建立，整個進程就這一份。**
	// 不能下放到 buildToolRegistry——它有兩個呼叫點，在裡面建立就是一份一份（ticket #35）。
	// 也不能下放到 assembleProfile：同一個進程會對每份 Profile 各呼叫它一次，在那裡建立
	// 就是每份 Profile 各一個池子，跨 Session 的總量同樣變回沒有上限（ticket #74）。
	proc.shellLimiter = tool.NewShellLimiter()

	// **PATH 目錄與 file.allowed_paths 重疊是一條內部提權路徑**。
	//
	// 父進程的 PATH 若含有一個落在 file.allowed_paths 之內的目錄，Agent 光靠**已被
	// 授權的 write_file** 就能在那裡放一個與白名單命令同名的檔案、或覆寫該目錄下既有
	// 的可執行檔——把「寫檔權限」升級成「執行白名單內程式的權限」。這不需要任何外部
	// 攻擊者：兩個能力都是使用者自己開的。
	//
	// **警告而非 fail fast**：重疊可能是使用者刻意的（`node_modules/.bin` 這類寫法
	// 很常見），而且與 Profile 有沒有列 Tool 無關——設定本身就是那個形狀。
	//
	// **它在進程層級印，因為判斷裡沒有任何一樣東西屬於 Profile**：PATH 是父進程的、
	// allowed_paths 是 config.yaml 的。放在 Profile 層級的話，載入 N 份 Profile 就把同一句
	// 話印 N 次，讀起來像 N 個不同的問題。
	if overlapping := tool.PathDirsOverlappingAllowedPaths(
		tool.ParentPathDirs(), cfg.File.AllowedPaths, proc.ws); len(overlapping) > 0 {
		fmt.Fprintf(out, "提醒：PATH 上的 %s 落在 %s/config.yaml 的 file.allowed_paths 之內；"+
			"這代表 write_file 能新增或改掉 shell 跑得到的程式（等於把寫檔權限升級成執行權限）。"+
			"若非刻意，請讓兩者不要重疊。\n", strings.Join(overlapping, "、"), workspaceDir)
	}

	// 對話與審計落 Workspace 內單一 SQLite 檔：備份或搬遷 Workspace 就是搬檔案。
	store, err := storage.Open(ctx, filepath.Join(proc.ws, sessionDBFile))
	if err != nil {
		return proc, fmt.Errorf("開啟 Workspace 資料庫: %w", err)
	}
	proc.store = store
	proc.sessions = storage.NewSessionManager(store)
	// Memory 統一門面：Session 的持久化委託 SQLite、長期記憶委託 MEMORY.md，
	// 引擎只認這一個介面。
	proc.memories = memory.NewService(proc.sessions, proc.longTerm)
	// 審計與 Session 同庫；寫入在背景進行、失敗只落錯誤日誌，不中斷對話
	// （憲法 6.2、3.3）。關閉順序見 Close。
	proc.audit = storage.NewAuditLog(store, proc.logger)
	return proc, nil
}

// Close 依序關閉審計、SQLite、各份 Profile 的 MCP 連線、Workspace root 與日誌檔。每一項
// 都會試著關，不因前一項失敗而跳過——漏掉的 MCP 連線會變成孤兒進程。
//
// **順序沿用 ticket #74 抽取之前 runChat 那一串 defer 實際的執行順序**。spec #73 第二節把
// chat 的收尾順序寫成「審計、MCP、SQLite」，與當時的程式碼不符，這裡以程式碼為準。其中
// 硬性的一條是審計先於 SQLite：佇列裡還沒寫出去的記錄要寫進那個資料庫，先關庫的話它們隨
// 進程消失，而對話本身一切正常，沒有人會發現。
//
// **Profile 層級的 MCP 連線交進這裡收，不讓呼叫端各排一個 defer**：Profile 層級一定組在
// 進程層級之後，各自 defer 的話後進先出會讓 MCP 最先關、排到審計前面。呼叫端把組好的
// Profile 交進來（失敗時的半成品也要交），順序由這一處決定。
//
// 回傳遇到的**第一個**錯誤，不合併全部：與呼叫端 defer 的 `cerr != nil && err == nil` 同一個
// 語義，後面的收尾錯誤不蓋過前面的。
//
// 對半成品安全：沒開起來的欄位是 nil，跳過；profiles 裡的 nil 也跳過。
func (p *processAssembly) Close(profiles ...*profileAssembly) error {
	var first error
	keepFirst := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if p.audit != nil {
		keepFirst(p.audit.Close())
	}
	if p.store != nil {
		keepFirst(p.store.Close())
	}
	for _, assembled := range profiles {
		if assembled != nil {
			keepFirst(assembled.mcpClients.Close()) // 對 nil 的連線服務是 no-op
		}
	}
	if p.wsRoot != nil {
		if err := p.wsRoot.Close(); err != nil {
			keepFirst(fmt.Errorf("關閉 Workspace: %w", err))
		}
	}
	if p.logFile != nil {
		if err := p.logFile.Close(); err != nil {
			keepFirst(fmt.Errorf("關閉日誌檔: %w", err))
		}
	}
	return first
}

// assembleProfile 把一份 Profile 組成可運作的 Agent：Bootstrap 與 Skill 的啟動校驗、Tool
// Registry（內建 Tool 加上這份 Profile 引用的 MCP server）、MCP 降級、Profile 過濾後的
// Executor 與 AgentService，並印出跟這份 Profile 有關的啟動提醒。
//
// **收的是已經載入的 Profile，不是檔名**：「載入」與「要不要組它」之間，呼叫端可能還有
// 自己的判斷（spec #7：server 要先比對檔名與 name 欄位，不一致就不組）。在這裡面才載入
// 的話，那個判斷只能等 MCP 子進程都起來之後才做得到。
//
// events 由呼叫端決定：CLI 要在等待期間顯示進度，不關心執行過程的呼叫端傳 NopEventSink。
//
// **失敗時也回傳非 nil 的半成品，呼叫端要把它交給 processAssembly.Close**：MCP 可能已經
// 連上、之後才在 Subset 擋下，那些子進程要有人收。
func assembleProfile(ctx context.Context, out io.Writer, proc *processAssembly, prof *core.Profile,
	events core.EventSink) (*profileAssembly, error) {
	assembled := &profileAssembly{}
	if _, ok := proc.cfg.Providers[prof.Provider.Name]; !ok {
		return assembled, fmt.Errorf("Profile %s 引用的 Provider %q 未在 %s/config.yaml 的 providers 段配置",
			prof.Name, prof.Provider.Name, workspaceDir)
	}

	// Profile 明確列出的 Bootstrap 檔案必須存在，否則啟動即報錯（設定錯誤，fail
	// fast）。校驗的是**載入端實際會碰的那些**（同一組 selection），不是欄位的字面
	// 清單——否則一份被 ADR-0003 互斥排除的 SOUL.md 會變成「壞掉可以跑、缺檔卻起不
	// 來」。每個 turn 的把關在載入端，這裡只是提前一步回報。
	bootSel, err := prof.BootstrapSelection()
	if err != nil {
		return assembled, fmt.Errorf("Profile %s 的 bootstrap 校驗失敗: %w", prof.Name, err)
	}
	if err := config.ValidateBootstrapFiles(proc.wsRoot, bootSel); err != nil {
		return assembled, fmt.Errorf("Profile %s 的 bootstrap 校驗失敗: %w", prof.Name, err)
	}

	// Skill 同樣在啟動時載入一次做校驗（引用不存在、frontmatter 不合法、name 與引用
	// 名不一致都是設定錯誤，fail fast）。每個 turn 的實際載入仍在 ReAct 循環裡重讀
	// ——這裡只是提前一步回報，順便拿到份數算 Skill 段會不會溢出。
	skillRefs, err := prof.SkillRefs()
	if err != nil {
		return assembled, fmt.Errorf("Profile %s 的 skills 校驗失敗: %w", prof.Name, err)
	}
	skills, err := proc.contextLoader.Skills(ctx, skillRefs)
	if err != nil {
		return assembled, fmt.Errorf("Profile %s 的 skills 校驗失敗: %w", prof.Name, err)
	}
	// Skill 段溢出時**整份 Skill 從 LLM 視野消失**，不是「內容變短」——使用者會看到
	// Agent 莫名其妙不會做某件事，卻查不出原因。prompt 裡的截斷標記只有 LLM 看得到，
	// 所以這裡對使用者喊一聲。
	//
	// **這只是啟動時的快照。** description 每個 turn 重讀，使用者在對話中途把某份寫長
	// 就可能跨過上限，那時這行早就印完了——所以 ReActLoop 每個 turn 另記一筆結構化
	// 日誌（見 core.ReActLoop.Run）。兩者不互相取代：啟動這次涵蓋「一個 turn 都沒跑」
	// （互動模式開起來就 EOF）的情形，那時引擎層一次都沒被呼叫過。
	//
	// CLI 提醒只在啟動發一次：對話進行中插播會打斷使用者，而這是持續存在的設定問題、
	// 不是某一個 turn 的事件。
	if _, dropped := core.ComposeSkillSection(skills); dropped > 0 {
		// 措辭不說「尾端」：`ComposeSkillSection` 在一份都塞不下時會整段略過
		// （dropped == 全部），那時說「尾端 N 份」會讓人以為前面幾份還在。用一句對
		// 兩種情況都成立的話，勝過為一個組裝點走不到的分支加一段測不到的判斷。
		fmt.Fprintf(out, "提醒：Profile %s 引用的 Skill 描述合計超過 %d 字上限，有 %d 份未進入 Agent 的視野；"+
			"請減少 skills 或精簡各份 description。\n", prof.Name, core.MaxSkillSectionRunes, dropped)
		proc.logger.Warn("skill_section_truncated",
			"profile", prof.Name, "declared", len(skills), "dropped", dropped,
			"limit_runes", core.MaxSkillSectionRunes, "phase", "startup")
	}

	// Profile 的 tools 欄位過濾可用子集，引用未註冊的 Tool 在啟動即報清晰錯誤。
	sandbox := sandboxConfig(proc.cfg)
	registry, err := buildToolRegistry(sandbox, shellRuntime(sandbox, proc.ws), proc.shellLimiter,
		proc.wsRoot, proc.longTerm, proc.contextLoader, skillRefs)
	if err != nil {
		return assembled, err
	}

	// 外部 MCP server：宣告檔 → Profile 引用 → 連線 → tools/list → 包成 OryxTool 註冊
	// 進**同一個** Registry。之後的 Subset 對它們與內建 Tool 一視同仁，ReAct 循環也
	// 不感知工具來自哪裡。
	//
	// 順序上必須夾在 buildToolRegistry 與 Subset 之間：早於註冊的話 Registry 還不存在，
	// 晚於 Subset 的話 Profile 引用 MCP 工具會被判成「未註冊」。
	// 這條鏈路與 `oryxos tools` 共用（見 connectProfileMcpServers）：那個命令列出來的
	// 東西必須就是這裡會註冊的那一組，否則它查到的是假的。
	mcpClients, mcpSpecs, err := connectProfileMcpServers(ctx, out, proc.ws, prof, registry, proc.logger)
	// 連線服務在檢查錯誤**之前**就放進半成品：連線中途失敗時也要收掉已經起來的那些
	// 子進程。ConnectMcpServers 在回錯誤前已自行收過一次，processAssembly.Close 是第二道
	// 保險（Close 對空清單是 no-op），漏掉的話會留下孤兒進程。
	assembled.mcpClients = mcpClients
	if err != nil {
		return assembled, err
	}
	// 連不上的 server 已經降級（它的工具沒有註冊、錯誤已落日誌、「哪一台連不上」也在
	// 失敗當下喊過了），這裡是最後一步：連線與註冊都結束了，現在才算得準**哪些 Profile
	// 工具真的缺**，把它們從送進 Subset 的清單裡拿掉，否則 Subset 會以「Tool 未註冊」
	// 擋下整個啟動。
	profileTools := degradeUnavailableMcpTools(out, prof, mcpSpecs, mcpClients.Failures(), registry)

	// 中介層掛在 **Profile 過濾後的** Executor 上，不掛在全域 Registry 上：
	// 「這個 Agent 的 Tool 要怎麼被攔」本來就該因 Agent 而異。目前只掛事件播報一層；
	// Tool Policy（issue #39）是可預見的第二層，屬擴展階段。
	executor, err := registry.Subset(profileTools, autoIncludedTools(skillRefs), proc.logger,
		tool.NewEventMiddleware(events, proc.logger))
	if err != nil {
		// 指向 `oryxos tools`（issue #27）：Subset 那一層已經把同一台 server 的可用名字
		// 列出來了，但工具多時會截斷，而且使用者常是連前綴都記錯。查詢途徑的名字放在
		// 這一層而不是 internal/tool——那個 package 不該知道 CLI 有哪些命令。
		return assembled, fmt.Errorf("Profile %s 的 tools 校驗失敗（執行 oryxos tools 可看到完整清單）: %w",
			prof.Name, err)
	}
	assembled.executor = executor

	// 啟動即清晰告知（需求 5.12 基礎校驗）：空白名單是安全的預設（全拒），
	// 但 Profile 列了 HTTP Tool 時，每次呼叫都會在執行期被攔截——先提醒，
	// 不硬報錯（純對話不受影響）。
	//
	// **判斷「空」用校驗器自己的那一份**（EffectiveAllowedDomains），理由與下面路徑那行
	// 相同。原本數的是 slice 長度，`allowed_domains: [""]` 因此被當成「已配置」而閉嘴，
	// 實際上每次呼叫都被攔（ADR-0007）。這份清單與 HTTP Tool 的判斷，最後那種提醒還要用。
	effectiveDomains := tool.EffectiveAllowedDomains(proc.cfg.HTTP.AllowedDomains)
	hasHTTPTool := slices.Contains(prof.Tools, "http_get") || slices.Contains(prof.Tools, "http_post")
	if len(effectiveDomains) == 0 && hasHTTPTool {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 http.allowed_domains 為空，HTTP Tool 呼叫將全部被攔截；請把允許的域名加入白名單。\n", workspaceDir)
	}
	// 路徑白名單同一條，理由也同一條：兩段白名單的預設值都是 []，少了這行使用者會
	// 遇到「Tool 有了、每次呼叫都被攔」而不知道原因。
	//
	// **判斷「空」用的是校驗器自己的那一份，不是 slice 長度**：`allowed_paths: [""]`
	// 或寫成絕對路徑的條目在校驗器眼中都不存在（見 tool.EffectiveAllowedPaths），
	// 只數長度會把它們當成「已配置」而閉嘴——而那正是最需要這行提醒的情形：使用者
	// 覺得自己照著錯誤訊息把目錄加進去了，卻還是每次被攔。
	//
	// 判斷要涵蓋**每一個** File Tool：只認 read_file 的實作會讓「只開了 list_dir、
	// 每次呼叫都被攔」這種配置毫無線索。
	if len(tool.EffectiveAllowedPaths(proc.cfg.File.AllowedPaths)) == 0 &&
		(slices.Contains(prof.Tools, tool.ReadFileToolName) ||
			slices.Contains(prof.Tools, tool.WriteFileToolName) ||
			slices.Contains(prof.Tools, tool.ListDirToolName)) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 file.allowed_paths 為空，File Tool 呼叫將全部被攔截；請把允許的路徑加入白名單。\n", workspaceDir)
	}
	// 命令白名單同一條。判斷「空」同樣用校驗器自己的那一份（EffectiveAllowedCommands）：
	// `allowed_commands: [/usr/bin/git]` 這種寫成路徑的條目永遠比不中任何請求（合法的
	// command 不含路徑分隔符），只數長度會把它當成「已配置」而閉嘴。
	if len(tool.EffectiveAllowedCommands(proc.cfg.Shell.AllowedCommands)) == 0 &&
		slices.Contains(prof.Tools, tool.ShellToolName) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 shell.allowed_commands 為空，shell 呼叫將全部被攔截；請把允許的程式名加入白名單。\n", workspaceDir)
	}
	// **網域白名單裡有條目可能永遠比不中**（ADR-0007）。PATH 與路徑白名單重疊那種提醒
	// 只跟 Workspace 有關，在 assembleProcess 印；這一種要看 Profile 有沒有列 HTTP Tool，
	// 所以留在這一層。
	//
	// EffectiveAllowedDomains 只剔除空字串，其餘一律保留——剔錯的代價是使用者授權過的
	// 網域被靜默拒絕。所以「這條大概寫成了網址」的疑慮由這一行承擔，不由剔除承擔。
	//
	// 契約三條：
	//
	//   - **只印條數與判準，不印條目值。** 條目是使用者手寫的 YAML 字串，含換行時原樣印出
	//     會偽造一行終端輸出，條目很多時也會洗版。上面三行空白名單提醒同樣一個條目值都
	//     不印。
	//   - **條數取自去重後的有效清單**（effectiveDomains）。否則 `["Example.com/x",
	//     "example.com/x"]` 會讓這行說 2 條，而拒絕訊息的總數是 1 條。
	//   - **措辭不承諾它們會出現在拒絕訊息裡**：拒絕訊息另有兩個顯示上限，它們一樣受約束。
	//     準確的說法只到「仍會參與白名單比對」。
	if unmatchable := countUnmatchableDomains(effectiveDomains); unmatchable > 0 && hasHTTPTool {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 http.allowed_domains 有 %d 條可能永遠比不中（含 URL 分隔符、scheme、空白或控制字元）；"+
			"它們仍會參與白名單比對，請確認寫的是網域而不是網址。\n", workspaceDir, unmatchable)
	}

	// Bootstrap 上下文（AGENTS.md／USER.md／SOUL.md）：每個 turn 由 ReAct 循環
	// 載入一次注入 system prompt，順序與覆蓋語義見 ADR-0003。
	assembled.agent = core.NewAgentService(prof, proc.providers, executor, proc.memories, proc.audit,
		proc.contextLoader, events, proc.prices, proc.logger)
	return assembled, nil
}
