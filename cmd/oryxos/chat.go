// chat.go 實作 `oryxos chat`：載入 Workspace 配置與 Profile，組出引擎後
// 進入 CLI Channel 對話。
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

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/channel/cli"
	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/storage"
	"github.com/rexshen5913/oryxos/internal/tool"
)

const (
	// sessionDBFile 是 Workspace 內的 SQLite 資料庫檔名（技術方案 §9.2）。
	sessionDBFile = "oryxos.db"
	// memoryFile 是 Workspace 內長期記憶的檔名，落在 memory/ 下（技術方案 §5.2）。
	memoryFile = "MEMORY.md"
)

// chatOptions 是 chat 命令的旗標集合。旗標多於兩個後改用具名欄位傳遞，
// 免得呼叫端排出一串無從辨識的位置參數。
type chatOptions struct {
	profileName string
	message     string
	// newConversation 對應 --new：歸檔當前 active Session 再開一場新對話。
	newConversation bool
}

func newChatCmd() *cobra.Command {
	var opts chatOptions
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "與 Agent 進入多輪對話（CLI Channel）",
		Long: "在已初始化的 Workspace 中與 Agent 對話。多輪對話共享同一個 Session，\n" +
			"重新執行時自動恢復先前的 active Session、接著上次的話題繼續；\n" +
			"輸入 /quit 結束。--message 送出單條訊息、輸出回應後退出；\n" +
			"--new 歸檔當前 active Session 後開一場全新對話。",
		Args: cobra.NoArgs,
		// 執行期錯誤（未初始化、缺 API key、Provider 故障）與用法無關，
		// 不倒 Usage 沖淡錯誤訊息。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("取得當前目錄: %w", err)
			}
			return runChat(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cwd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.profileName, "profile", "default", "使用的 Profile 名（.oryxos/profiles/<name>.yaml）")
	cmd.Flags().StringVar(&opts.message, "message", "", "送出單條訊息、輸出回應後退出")
	cmd.Flags().BoolVar(&opts.newConversation, "new", false,
		"歸檔當前 active Session，開始一場全新對話（不帶舊 Session 的任何訊息）")
	return cmd
}

// sandboxConfig 把 config.yaml 的三段 Sandbox 設定搬成 internal/tool 的執行期形狀。
//
// 搬運集中在這一個函式，是因為 buildToolRegistry 有**兩個呼叫點**（chat 與 tools）：
// 各自展開欄位的話，日後多一段設定就會有一邊漏掉，而兩個命令看到的可用 Tool 不一致
// 正是 `oryxos tools` 最不該有的性質。
//
// 三段白名單都**不經 resolveEnv 展開**：那條路徑目前只用於 Provider 憑證與 MCP env，
// 白名單不是憑證，不擴大它。
func sandboxConfig(cfg *config.Config) tool.SandboxConfig {
	return tool.SandboxConfig{
		AllowedDomains:  cfg.HTTP.AllowedDomains,
		AllowedPaths:    cfg.File.AllowedPaths,
		AllowedCommands: cfg.Shell.AllowedCommands,
		ShellTimeout:    cfg.Shell.EffectiveTimeout(),
	}
}

// shellRuntime 把 shell 子進程的執行上下文組起來，與 sandboxConfig 並列在同一層，
// 理由也相同：buildToolRegistry 有**兩個呼叫點**，各自展開的話兩個命令看到的執行
// 範圍會不一致。
//
// PATH 只在這裡取一次（tool.ParentPathDirs），解析執行檔與子進程 Env 之後共用**同一
// 份**過濾後清單——「環境已收窄但實際執行的檔案仍由繼承的 PATH 決定」那個落差因此
// 在結構上不存在。
//
// **超時值從 sandbox 那個結構體拿，不再自己去 cfg 取一次。** SandboxConfig.ShellTimeout
// 存在的理由就是「同一份 config.yaml 只搬一次」（見它的欄位說明）；這裡若另外呼叫一次
// cfg.Shell.EffectiveTimeout()，那個欄位就變成沒有人讀的死欄位，而同一個值有了兩條
// 來源——兩條來源遲早會分岔，而且分岔時沒有任何東西會報錯。
func shellRuntime(sandbox tool.SandboxConfig, ws string) tool.ShellRuntime {
	return tool.ShellRuntime{
		Dir:      ws,
		PathDirs: tool.ParentPathDirs(),
		Timeout:  sandbox.ShellTimeout,
	}
}

// buildToolRegistry 顯式註冊這個 Workspace 的全部內建 Tool（憲法 2.3）：
// internal/tool 自帶的 HTTP Tool 與 File Tool，加上住在 internal/memory、需要
// Workspace 路徑的 Memory Tool。每個組裝點都該經此函式取得 Registry——`oryxos init`
// 的預設 Profile 已列出 save_memory，漏註冊會讓 stock Workspace 在 Subset 時直接
// 啟動失敗。
//
// wsRoot 是 Workspace 的根：File Tool 一律經它開檔，能力因此界定在 Workspace 之內。
// 它與長期記憶、Bootstrap 用的是**同一個** root，不另開一份。
//
// **shell 不受它約束**：os.Root 管的是這個 Go 進程自己的開檔（openat），不改變進程
// 的檔案系統視圖，對子進程完全無效。shell 能碰的範圍是 oryxos 進程本身的權限，
// 要真隔離得把 oryxos 跑在容器裡（容器級隔離屬擴展階段）。
//
// **shellLimiter 是唯一一個不在這裡建立的依賴，這一點是定案而不是風格**（ticket #35）：
// 它必須是整個 OryxOS 進程共用的**那一份**，而這個函式**有兩個呼叫點**（runChat 與
// runTools）——在這裡 `tool.NewShellLimiter()` 就是每次呼叫一份新的，跨 session 的總量
// 又變回無界，整段威脅模型自我作廢。所以它由呼叫方那一層（composition root）建立一次
// 再傳進來，形狀與 wsRoot 相同（那個也是呼叫方開好再交進來的）。
func buildToolRegistry(sandbox tool.SandboxConfig, shell tool.ShellRuntime,
	shellLimiter *tool.ShellLimiter, wsRoot *os.Root,
	longTerm *memory.LongTermMemory, skills core.ContextLoader, skillRefs []string) (*tool.Registry, error) {
	registry := tool.NewRegistry()
	if err := tool.RegisterBuiltins(registry, tool.NewSandboxChecker(sandbox), wsRoot, shell, shellLimiter); err != nil {
		return nil, fmt.Errorf("組裝 Tool registry: %w", err)
	}
	for _, memTool := range []tool.OryxTool{memory.NewSaveMemoryTool(longTerm), memory.NewRecallMemoryTool(longTerm)} {
		if err := registry.Register(memTool); err != nil {
			return nil, fmt.Errorf("註冊 Memory Tool: %w", err)
		}
	}
	// load_skill **一律註冊**進全域 Registry，與 skills 是否為空無關：註冊與「進不進
	// 這個 Agent 的可用子集」是兩件事，後者由 Subset 的 autoIncluded 決定。一律註冊
	// 讓「skills 為空但使用者顯式列了 load_skill」這個邊界格能正常啟動——那時它會在
	// 呼叫時回明確的錯誤回填，比啟動失敗好。
	if err := registry.Register(tool.NewLoadSkillTool(skills, skillRefs)); err != nil {
		return nil, fmt.Errorf("註冊 load_skill: %w", err)
	}
	// 原生 Go Tool 示例（Plugin Tool 方式三）。**這一行就是業務方要照抄的東西**：
	// 自己寫一個實作 OryxTool 的型別，在這裡多加一次 Register，它就與內建 Tool 一視
	// 同仁——受 Profile 的 tools 欄位過濾、落 tool_invocations、ReAct 循環不感知來源。
	//
	// 一律註冊，理由同上：沒列到它的 Profile 完全不受影響，既有 Workspace 免遷移。
	if err := registry.Register(tool.NewTextStatsTool()); err != nil {
		return nil, fmt.Errorf("註冊原生 Go Tool 示例 text_stats: %w", err)
	}
	return registry, nil
}

// autoIncludedTools 依配置推導出要自動加進可用子集的 Tool。
//
// 目前唯一一條：Profile 的 skills 非空 → load_skill。若要求使用者自行列出，宣告了
// `skills:` 卻忘記帶 load_skill 的 Profile 會**安靜退化**成「LLM 看得到 Skill 描述、
// 永遠載不到正文」——漸進揭露這條鏈路最該避免的失敗形態，而且從外部完全看不出來。
//
// 這仍是顯式的（憲法 2.3）：觸發條件是使用者自己寫的 skills 欄位，不是反射或型別
// 掃描；Tool 本身也仍要先 Register 才推導得到。
func autoIncludedTools(skillRefs []string) []string {
	if len(skillRefs) == 0 {
		return nil
	}
	return []string{tool.LoadSkillToolName}
}

// warnMcpServerUnavailable 對一個連不上的 MCP server 發 CLI 警示。
//
// **安靜地少了幾個工具比起不來更糟**：Agent 會表現成莫名其妙不會做某件事，而使用者
// 不會想到去翻日誌（spec #3 使用者故事 29）。
//
// 它由 ConnectMcpServers 在**每個 server 失敗的當下**回呼（issue #26），不是等整個連線
// 階段跑完才一次印。連線期限給到 30 秒，等最慢的那一個結束才開口，中間那段時間使用者
// 只看得到游標在閃。
//
// 措辭與輸出目的地留在這一層：internal/tool 只負責把「這個 server 這次沒連上」交出來，
// 怎麼講給使用者聽是組裝點的事。
//
// **措辭不提「啟動」與「對話」**：`oryxos tools` 也走這條警示（issue #27），而那個命令
// 既不啟動 Agent 也不對話——說「這次啟動不會有它的工具」在那裡是錯的。一句對兩個命令
// 都成立的話，勝過為此拆成兩份幾乎一樣的措辭。
//
// **它只講「哪一台連不上」，不講「哪些工具因此不可用」**。後者要等整個連線階段跑完才
// 算得準——這個回呼發生在失敗的當下，那時其他 server 還在連，誰會提供什麼還不知道。
// 由 degradeUnavailableMcpTools 在最後一次算清楚。
//
// 警示帶上原始錯誤而不是摘要成一句「連線失敗」：「命令找不到」與「交握逾時」要修的
// 東西完全不同，那句原文是使用者唯一的線索。
func warnMcpServerUnavailable(out io.Writer, failure tool.McpConnectFailure) {
	fmt.Fprintf(out, "警告：MCP server %s 連線失敗，這次拿不到它的工具"+
		"（其餘 Tool 不受影響）：%v\n", failure.Server, failure.Err)
}

// degradeUnavailableMcpTools 把**這次確實拿不到**的工具從 Profile 的 tools 清單裡拿掉，
// 印出提醒，並回傳要送進 Registry.Subset 的那一份。
//
// **為什麼要拿掉，而不是讓 Subset 照常報錯**：Subset 對未註冊的 Tool fail fast，那條
// 語義是給**設定錯誤**（工具名打錯、server 改了名）用的——換幾台機器都一樣壞，早點
// 擋下才對。但「今天這台機器連不上 Slack」是**環境問題**，把它也判成打錯字的話，降級
// 在現實中永遠走不到：使用者當然會在 tools 列出他要用的工具，於是任何一個 server 掛掉
// 都會讓整個 Agent 起不來——正是「一個外部依賴掛掉不該讓整個 Agent 起不來」要防的事
// （spec #3 使用者故事 28）。
//
// **判準是「這個名字有沒有被註冊」，不是「名字長得像誰的」。** 註冊了就代表真的有一台
// 健康的 server 提供它，不管是哪一台——這一點非讓它精確不可：server 名可以含雙底線
// （`foo` 與 `foo__bar` 能同時宣告），以前綴判斷歸屬的話，`foo` 連不上會讓健康的
// `foo__bar` 的 `foo__bar__echo` 一起被刪掉。那不只是訊息難看，是 Agent 的能力真的少了
// 一塊，而 `oryxos tools` 同時還把它列成可用——兩邊對不上，最難查的那種。
//
// 沒註冊的名字才需要判斷歸屬，判準是 specs（這個 Profile 引用到的全部 server）裡**最長**
// 的那個前綴匹配：屬於連不上的 server 就拿掉，否則留著讓 Subset 照常擋下（打錯字、或
// 引用了根本沒宣告的 server，那些是設定錯誤）。
func degradeUnavailableMcpTools(out io.Writer, prof *core.Profile, specs []core.McpServerSpec,
	failures []tool.McpConnectFailure, registry *tool.Registry) []string {
	if len(failures) == 0 {
		return prof.Tools
	}
	failed := make(map[string]bool, len(failures))
	for _, failure := range failures {
		failed[failure.Server] = true
	}
	registered := make(map[string]bool)
	for _, info := range registry.All() {
		registered[info.Name] = true
	}

	// Clone 一份再刪：prof.Tools 是 Profile 的欄位，就地刪除會讓後面任何人看到的
	// Profile 與使用者寫的那份不一樣。
	var dropped []string
	remaining := slices.DeleteFunc(slices.Clone(prof.Tools), func(name string) bool {
		if registered[name] {
			return false // 有健康的來源真的提供它
		}
		if owner := longestServerPrefix(name, specs); owner == "" || !failed[owner] {
			return false // 不屬於任何連不上的 server：設定錯誤，交給 Subset 擋
		}
		dropped = append(dropped, name)
		return true
	})
	if len(dropped) > 0 {
		fmt.Fprintf(out, "提醒：Profile %s 列出的 %s 這次不可用（提供它們的 MCP server 連線失敗）。\n",
			prof.Name, strings.Join(dropped, "、"))
	}
	return remaining
}

// longestServerPrefix 回傳 specs 裡「name 以 `<server>__` 開頭」且**最長**的那個 server 名，
// 沒有匹配時回空字串。
//
// 取最長而不是第一個：server 名沒有字元限制，`foo` 與 `foo__bar` 能同時宣告，
// `foo__bar__echo` 對兩者都是前綴匹配。specs 是一個已知集合，拿它來比就沒有歧義。
func longestServerPrefix(name string, specs []core.McpServerSpec) string {
	var longest string
	for _, spec := range specs {
		if !strings.HasPrefix(name, spec.Name+tool.McpToolSeparator) {
			continue
		}
		if len(spec.Name) > len(longest) {
			longest = spec.Name
		}
	}
	return longest
}

// runChat 載入 Workspace 設定檔與 Profile、校驗 Provider 可解析，組出
// AgentService 後交給 CLI Channel；message 非空時走單訊息模式。
func runChat(ctx context.Context, in io.Reader, out io.Writer, baseDir string, opts chatOptions) (err error) {
	ws := filepath.Join(baseDir, workspaceDir)
	if _, err := os.Stat(ws); err != nil {
		return fmt.Errorf("找不到 Workspace %s（請先執行 oryxos init）: %w", workspaceDir, err)
	}

	cfg, err := config.Load(filepath.Join(ws, "config.yaml"))
	if err != nil {
		return fmt.Errorf("載入 Workspace 設定檔: %w", err)
	}
	prof, err := core.LoadProfile(filepath.Join(ws, "profiles", opts.profileName+".yaml"))
	if err != nil {
		return fmt.Errorf("載入 Profile %s: %w", opts.profileName, err)
	}
	if _, ok := cfg.Providers[prof.Provider.Name]; !ok {
		return fmt.Errorf("Profile %s 引用的 Provider %q 未在 %s/config.yaml 的 providers 段配置",
			prof.Name, prof.Provider.Name, workspaceDir)
	}

	// 每次 LLM 呼叫的結構化日誌落 Workspace 的 logs/ 目錄。
	logFile, err := os.OpenFile(filepath.Join(ws, "logs", "oryxos.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("開啟日誌檔: %w", err)
	}
	defer func() {
		if cerr := logFile.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("關閉日誌檔: %w", cerr)
		}
	}()
	logger := slog.New(slog.NewJSONHandler(logFile, nil))

	// 憑證的 ${ENV_VAR} 展開是**顯式的一步**（issue #27）：Load 不再代勞，因為與
	// Provider 無關的命令（`oryxos tools`）不該被缺一個環境變數擋下。chat 要真的呼叫
	// LLM，所以在這裡展開，缺 key 仍然啟動即報錯。
	providers, err := config.ExpandProviderEnv(cfg.Providers)
	if err != nil {
		return fmt.Errorf("載入 Provider 憑證: %w", err)
	}
	// config（YAML 檔案形狀）與 provider（執行期配置）刻意不共用型別，
	// 避免 internal/provider 依賴設定檔格式；同形搬運是這條解耦的成本。
	providerConfigs := make(map[string]provider.Config, len(providers))
	for name, pc := range providers {
		providerConfigs[name] = provider.Config{APIKey: pc.APIKey, BaseURL: pc.BaseURL}
	}
	// 定價表攤平給 ReAct 循環算成本（ticket #49）。取自**展開後**的那份是為了與
	// 憑證同源，不是因為定價需要展開——定價是數字，resolveEnv 那條路徑只服務
	// api_key 與 base_url。沒有配置定價段時這裡是空表，成本欄位於是落 NULL。
	prices := config.PriceListOf(providers)

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
	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		return fmt.Errorf("開啟 Workspace %s: %w", workspaceDir, err)
	}
	defer func() {
		if cerr := wsRoot.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("關閉 Workspace: %w", cerr)
		}
	}()
	// Profile 明確列出的 Bootstrap 檔案必須存在，否則啟動即報錯（設定錯誤，fail
	// fast）。校驗的是**載入端實際會碰的那些**（同一組 selection），不是欄位的字面
	// 清單——否則一份被 ADR-0003 互斥排除的 SOUL.md 會變成「壞掉可以跑、缺檔卻起不
	// 來」。每個 turn 的把關在載入端，這裡只是提前一步回報。
	bootSel, err := prof.BootstrapSelection()
	if err != nil {
		return fmt.Errorf("Profile %s 的 bootstrap 校驗失敗: %w", prof.Name, err)
	}
	if err := config.ValidateBootstrapFiles(wsRoot, bootSel); err != nil {
		return fmt.Errorf("Profile %s 的 bootstrap 校驗失敗: %w", prof.Name, err)
	}

	// Skill 同樣在啟動時載入一次做校驗（引用不存在、frontmatter 不合法、name 與引用
	// 名不一致都是設定錯誤，fail fast）。每個 turn 的實際載入仍在 ReAct 循環裡重讀
	// ——這裡只是提前一步回報，順便拿到份數算 Skill 段會不會溢出。
	contextLoader := config.NewContextLoader(wsRoot)
	skillRefs, err := prof.SkillRefs()
	if err != nil {
		return fmt.Errorf("Profile %s 的 skills 校驗失敗: %w", prof.Name, err)
	}
	skills, err := contextLoader.Skills(ctx, skillRefs)
	if err != nil {
		return fmt.Errorf("Profile %s 的 skills 校驗失敗: %w", prof.Name, err)
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
		// 兩種情況都成立的話，勝過為一個 runChat 走不到的分支加一段測不到的判斷。
		fmt.Fprintf(out, "提醒：Profile %s 引用的 Skill 描述合計超過 %d 字上限，有 %d 份未進入 Agent 的視野；"+
			"請減少 skills 或精簡各份 description。\n", prof.Name, core.MaxSkillSectionRunes, dropped)
		logger.Warn("skill_section_truncated",
			"profile", prof.Name, "declared", len(skills), "dropped", dropped,
			"limit_runes", core.MaxSkillSectionRunes, "phase", "startup")
	}

	longTerm := memory.NewLongTermMemory(wsRoot, filepath.Join("memory", memoryFile))

	// Profile 的 tools 欄位過濾可用子集，引用未註冊的 Tool 在啟動即報清晰錯誤。
	sandbox := sandboxConfig(cfg)
	// **composition root：shell 的 admission limiter 在這裡建立，整個進程就這一份。**
	// 不能下放到 buildToolRegistry——它有兩個呼叫點，在裡面建立就是一份一份（ticket #35）。
	shellLimiter := tool.NewShellLimiter()
	registry, err := buildToolRegistry(sandbox, shellRuntime(sandbox, ws), shellLimiter,
		wsRoot, longTerm, contextLoader, skillRefs)
	if err != nil {
		return err
	}

	// 外部 MCP server：宣告檔 → Profile 引用 → 連線 → tools/list → 包成 OryxTool 註冊
	// 進**同一個** Registry。之後的 Subset 對它們與內建 Tool 一視同仁，ReAct 循環也
	// 不感知工具來自哪裡。
	//
	// 順序上必須夾在 buildToolRegistry 與 Subset 之間：早於註冊的話 Registry 還不存在，
	// 晚於 Subset 的話 Profile 引用 MCP 工具會被判成「未註冊」。
	// 這條鏈路與 `oryxos tools` 共用（見 connectProfileMcpServers）：那個命令列出來的
	// 東西必須就是這裡會註冊的那一組，否則它查到的是假的。
	mcpClients, mcpSpecs, err := connectProfileMcpServers(ctx, out, ws, prof, registry, logger)
	// Close 無條件排進 defer，連線中途失敗時也要收掉**已經起來的**那些子進程：
	// ConnectMcpServers 在回錯誤前已自行收過一次，這裡是第二道保險（Close 對空清單
	// 是 no-op），漏掉的話會留下孤兒進程。
	defer func() {
		if cerr := mcpClients.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err != nil {
		return err
	}
	// 連不上的 server 已經降級（它的工具沒有註冊、錯誤已落日誌、「哪一台連不上」也在
	// 失敗當下喊過了），這裡是最後一步：連線與註冊都結束了，現在才算得準**哪些 Profile
	// 工具真的缺**，把它們從送進 Subset 的清單裡拿掉，否則 Subset 會以「Tool 未註冊」
	// 擋下整個啟動。
	profileTools := degradeUnavailableMcpTools(out, prof, mcpSpecs, mcpClients.Failures(), registry)

	// 執行過程的事件流：CLI 用它在等待期間顯示進度。輸出不是終端機時
	// （`--message` 接管線、重導向到檔案、測試接 buffer）ProgressSinkFor 會退回
	// 不做事的實作，既有輸出格式一個字都不變。
	//
	// 它在這裡建立而不是等到組 AgentService 時才建，是因為 Tool 事件由掛在 Executor
	// 上的中介層播報——Subset 需要它。
	events := cli.ProgressSinkFor(out)
	// 中介層掛在 **Profile 過濾後的** Executor 上，不掛在全域 Registry 上：
	// 「這個 Agent 的 Tool 要怎麼被攔」本來就該因 Agent 而異。目前只掛事件播報一層；
	// Tool Policy（issue #39）是可預見的第二層，屬擴展階段。
	executor, err := registry.Subset(profileTools, autoIncludedTools(skillRefs), logger,
		tool.NewEventMiddleware(events, logger))
	if err != nil {
		// 指向 `oryxos tools`（issue #27）：Subset 那一層已經把同一台 server 的可用名字
		// 列出來了，但工具多時會截斷，而且使用者常是連前綴都記錯。查詢途徑的名字放在
		// 這一層而不是 internal/tool——那個 package 不該知道 CLI 有哪些命令。
		return fmt.Errorf("Profile %s 的 tools 校驗失敗（執行 oryxos tools 可看到完整清單）: %w",
			prof.Name, err)
	}
	// 啟動即清晰告知（需求 5.12 基礎校驗）：空白名單是安全的預設（全拒），
	// 但 Profile 列了 HTTP Tool 時，每次呼叫都會在執行期被攔截——先提醒，
	// 不硬報錯（純對話不受影響）。
	if len(cfg.HTTP.AllowedDomains) == 0 && (slices.Contains(prof.Tools, "http_get") || slices.Contains(prof.Tools, "http_post")) {
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
	if len(tool.EffectiveAllowedPaths(cfg.File.AllowedPaths)) == 0 &&
		(slices.Contains(prof.Tools, tool.ReadFileToolName) ||
			slices.Contains(prof.Tools, tool.WriteFileToolName) ||
			slices.Contains(prof.Tools, tool.ListDirToolName)) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 file.allowed_paths 為空，File Tool 呼叫將全部被攔截；請把允許的路徑加入白名單。\n", workspaceDir)
	}
	// 命令白名單同一條。判斷「空」同樣用校驗器自己的那一份（EffectiveAllowedCommands）：
	// `allowed_commands: [/usr/bin/git]` 這種寫成路徑的條目永遠比不中任何請求（合法的
	// command 不含路徑分隔符），只數長度會把它當成「已配置」而閉嘴。
	if len(tool.EffectiveAllowedCommands(cfg.Shell.AllowedCommands)) == 0 &&
		slices.Contains(prof.Tools, tool.ShellToolName) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 shell.allowed_commands 為空，shell 呼叫將全部被攔截；請把允許的程式名加入白名單。\n", workspaceDir)
	}
	// 第二種提醒：**PATH 目錄與 file.allowed_paths 重疊是一條內部提權路徑**。
	//
	// 父進程的 PATH 若含有一個落在 file.allowed_paths 之內的目錄，Agent 光靠**已被
	// 授權的 write_file** 就能在那裡放一個與白名單命令同名的檔案、或覆寫該目錄下既有
	// 的可執行檔——把「寫檔權限」升級成「執行白名單內程式的權限」。這不需要任何外部
	// 攻擊者：兩個能力都是使用者自己開的。
	//
	// **警告而非 fail fast**：重疊可能是使用者刻意的（`node_modules/.bin` 這類寫法
	// 很常見），而且與 Profile 有沒有列 Tool 無關——設定本身就是那個形狀。
	if overlapping := tool.PathDirsOverlappingAllowedPaths(
		tool.ParentPathDirs(), cfg.File.AllowedPaths, ws); len(overlapping) > 0 {
		fmt.Fprintf(out, "提醒：PATH 上的 %s 落在 %s/config.yaml 的 file.allowed_paths 之內；"+
			"這代表 write_file 能新增或改掉 shell 跑得到的程式（等於把寫檔權限升級成執行權限）。"+
			"若非刻意，請讓兩者不要重疊。\n", strings.Join(overlapping, "、"), workspaceDir)
	}

	// 對話與審計落 Workspace 內單一 SQLite 檔：備份或搬遷 Workspace 就是搬檔案。
	store, err := storage.Open(ctx, filepath.Join(ws, sessionDBFile))
	if err != nil {
		return fmt.Errorf("開啟 Workspace 資料庫: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	sessions := storage.NewSessionManager(store)
	// --new：先歸檔當前 active Session，下面的 ActiveSession 就取不到 active 列，
	// 自然開出一場乾淨的新對話（沒有 active Session 時歸檔是 no-op，不報錯）。
	if opts.newConversation {
		if err := sessions.ArchiveActive(ctx, cli.ChannelName, cli.LocalUserID, prof.Name); err != nil {
			return fmt.Errorf("歸檔當前 active Session: %w", err)
		}
	}
	// 同一（Channel、使用者、Profile）聯合標識的 active Session 自動恢復，
	// 沒有時開新的：重新執行 oryxos chat 就接得上先前的上下文。
	session, err := sessions.ActiveSession(ctx, cli.ChannelName, cli.LocalUserID, prof.Name)
	if err != nil {
		return fmt.Errorf("取回 active Session: %w", err)
	}

	// Memory 統一門面：會話記憶委託 SQLite 的 Session 儲存、長期記憶委託
	// MEMORY.md，引擎只認這一個介面。
	memories := memory.NewService(sessions, longTerm)
	// 審計與 Session 同庫；寫入在背景進行、失敗只落錯誤日誌，不中斷對話
	// （憲法 6.2、3.3）。Close 要排在 store.Close 之前跑，否則佇列裡還沒寫出去
	// 的記錄會隨進程消失——defer 是後進先出，所以這行寫在開 store 之後。
	audit := storage.NewAuditLog(store, logger)
	defer func() {
		if cerr := audit.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	// Bootstrap 上下文（AGENTS.md／USER.md／SOUL.md）：每個 turn 由 ReAct 循環
	// 載入一次注入 system prompt，順序與覆蓋語義見 ADR-0003。
	agent := core.NewAgentService(prof, provider.NewService(providerConfigs, logger), executor, memories, audit, contextLoader, events, prices, logger)
	ch := cli.New(agent, session, prof.Identity.AgentName, in, out)

	if opts.message != "" {
		return ch.RunOnce(ctx, opts.message)
	}
	return ch.RunInteractive(ctx)
}
