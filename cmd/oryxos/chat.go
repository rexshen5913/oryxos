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
func buildToolRegistry(allowedDomains []string, longTerm *memory.LongTermMemory) (*tool.Registry, error) {
	registry := tool.NewRegistry()
	if err := tool.RegisterBuiltins(registry, tool.NewSandboxChecker(allowedDomains)); err != nil {
		return nil, fmt.Errorf("組裝 Tool registry: %w", err)
	}
	for _, memTool := range []tool.OryxTool{memory.NewSaveMemoryTool(longTerm), memory.NewRecallMemoryTool(longTerm)} {
		if err := registry.Register(memTool); err != nil {
			return nil, fmt.Errorf("註冊 Memory Tool: %w", err)
		}
	}
	return registry, nil
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

	// 長期記憶的檔案操作經此 root：越界（含經符號連結指到 Workspace 之外）由
	// os.Root 擋下。MEMORY.md 隨 Workspace 進 git，一個惡意 repo 若把它做成指向
	// 使用者敏感檔案的符號連結，讀取端會把該檔內容注入 prompt 送往 Provider、
	// 寫入端則會覆寫它。
	//
	// 範圍僅止於長期記憶：上面的 logs/oryxos.log 與下面的 SQLite 仍各自開檔
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
	longTerm := memory.NewLongTermMemory(wsRoot, filepath.Join("memory", memoryFile))

	// Profile 的 tools 欄位過濾可用子集，引用未註冊的 Tool 在啟動即報清晰錯誤。
	registry, err := buildToolRegistry(cfg.HTTP.AllowedDomains, longTerm)
	if err != nil {
		return err
	}
	executor, err := registry.Subset(prof.Tools, logger)
	if err != nil {
		return fmt.Errorf("Profile %s 的 tools 校驗失敗: %w", prof.Name, err)
	}
	// 啟動即清晰告知（需求 5.12 基礎校驗）：空白名單是安全的預設（全拒），
	// 但 Profile 列了 HTTP Tool 時，每次呼叫都會在執行期被攔截——先提醒，
	// 不硬報錯（純對話不受影響）。
	if len(cfg.HTTP.AllowedDomains) == 0 && (slices.Contains(prof.Tools, "http_get") || slices.Contains(prof.Tools, "http_post")) {
		fmt.Fprintf(out, "提醒：%s/config.yaml 的 http.allowed_domains 為空，HTTP Tool 呼叫將全部被攔截；請把允許的域名加入白名單。\n", workspaceDir)
	}

	// 對話落 Workspace 內單一 SQLite 檔：備份或搬遷 Workspace 就是搬檔案。
	sessions, err := storage.OpenSessionManager(ctx, filepath.Join(ws, sessionDBFile))
	if err != nil {
		return fmt.Errorf("開啟 Session 儲存: %w", err)
	}
	defer func() {
		if cerr := sessions.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
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
	agent := core.NewAgentService(prof, provider.NewService(providerConfigs, logger), executor, memories)
	ch := cli.New(agent, session, prof.Identity.AgentName, in, out)

	if opts.message != "" {
		return ch.RunOnce(ctx, opts.message)
	}
	return ch.RunInteractive(ctx)
}
