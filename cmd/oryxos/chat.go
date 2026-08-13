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

// buildToolRegistry 顯式註冊這個 Workspace 的全部內建 Tool（憲法 2.3）：
// internal/tool 自帶的 HTTP Tool，加上住在 internal/memory、需要 Workspace 路徑的
// Memory Tool。每個組裝點都該經此函式取得 Registry——`oryxos init` 的預設 Profile
// 已列出 save_memory，漏註冊會讓 stock Workspace 在 Subset 時直接啟動失敗。
func buildToolRegistry(allowedDomains []string, longTerm *memory.LongTermMemory,
	skills core.ContextLoader, skillRefs []string) (*tool.Registry, error) {
	registry := tool.NewRegistry()
	if err := tool.RegisterBuiltins(registry, tool.NewSandboxChecker(allowedDomains)); err != nil {
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

	// config（YAML 檔案形狀）與 provider（執行期配置）刻意不共用型別，
	// 避免 internal/provider 依賴設定檔格式；同形搬運是這條解耦的成本。
	providerConfigs := make(map[string]provider.Config, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		providerConfigs[name] = provider.Config{APIKey: pc.APIKey, BaseURL: pc.BaseURL}
	}

	// 長期記憶與 Bootstrap 的檔案操作都經此 root：越界（含經符號連結指到
	// Workspace 之外）由 os.Root 擋下。這幾份 .md 隨 Workspace 進 git，一個惡意
	// repo 若把它們做成指向使用者敏感檔案的符號連結，讀取端會把該檔內容注入
	// system prompt 送往 Provider（MEMORY.md 的寫入端則會覆寫它）。
	//
	// 範圍僅止於這些 .md：上面的 logs/oryxos.log 與下面的 SQLite 仍各自開檔
	// （SQLite 由驅動自己開，接不進 os.Root）。Workspace 級的路徑防護屬 Sandbox
	// 職責（CONTEXT.md：核心階段做應用層的路徑／命令／域名白名單校驗），隨
	// File Tool 那張統一處理。
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
	registry, err := buildToolRegistry(cfg.HTTP.AllowedDomains, longTerm, contextLoader, skillRefs)
	if err != nil {
		return err
	}
	executor, err := registry.Subset(prof.Tools, autoIncludedTools(skillRefs), logger)
	if err != nil {
		return fmt.Errorf("Profile %s 的 tools 校驗失敗: %w", prof.Name, err)
	}
	// 啟動即清晰告知（需求 5.12 基礎校驗）：空白名單是安全的預設（全拒），
	// 但 Profile 列了 HTTP Tool 時，每次呼叫都會在執行期被攔截——先提醒，
	// 不硬報錯（純對話不受影響）。
	if len(cfg.HTTP.AllowedDomains) == 0 && (slices.Contains(prof.Tools, "http_get") || slices.Contains(prof.Tools, "http_post")) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 http.allowed_domains 為空，HTTP Tool 呼叫將全部被攔截；請把允許的域名加入白名單。\n", workspaceDir)
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
	agent := core.NewAgentService(prof, provider.NewService(providerConfigs, logger), executor, memories, audit, contextLoader, logger)
	ch := cli.New(agent, session, prof.Identity.AgentName, in, out)

	if opts.message != "" {
		return ch.RunOnce(ctx, opts.message)
	}
	return ch.RunInteractive(ctx)
}
