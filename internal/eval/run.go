package eval

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/storage"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// 本檔是**驅動真實 Agent 的那一層，刻意沒有對應的自動化測試**（ticket #50 驗收條件）。
//
// 理由不是懶：它每跑一次就會呼叫真實 Provider，而憲法 4.4 明訂自動化測試中一律回放
// 錄製回應、絕不打真實 API。要嘛它有測試而違憲並產生帳單，要嘛它沒有測試——沒有第三
// 種選擇。它的正確性由執行時的實際使用承擔，與需求文檔第 13 章「真實 Provider 呼叫的
// 驗證由驗收 demo 承擔」是同一條原則。
//
// 這正是本票要把判卷切成純函式的原因：**判斷邏輯全部搬到測得到的那一側**，這裡只剩
// 沒有分支的組裝與傳遞。這一層越薄，沒有測試這件事的代價就越小。

const (
	// sessionDBFile 與 memoryFile 是 Workspace 內的固定位置，與 `oryxos chat` 同名。
	sessionDBFile = "oryxos.db"
	memoryFile    = "MEMORY.md"

	// evalChannel 與 evalUserID 是評測執行時 Session 的聯合標識。
	//
	// 刻意不借用 CLI 的那一組：Session 由 Channel、使用者、Profile 聯合標識，借用會讓
	// 評測產生的 Session 與使用者自己的對話混在同一個標識底下。實務上每個用例都用一個
	// 全新的資料庫，撞不到；但標識本來就該說實話——這一場不是使用者在 CLI 上的對話。
	evalChannel = "eval"
	evalUserID  = "eval"
)

// RunCase 在一個乾淨的 Workspace 裡把用例的任務送給真實 Agent 跑一輪，回傳結果摘要。
//
// **會呼叫真實 Provider 並產生費用。**
//
// caseRoot 是這個用例專屬的根目錄，由呼叫端提供且必須是空的——乾淨的 Workspace 建在
// 它底下。執行結束後刻意不刪：評測失敗時要看的東西（Agent 讀寫過的檔案、logs/、
// 審計資料庫）全都在那裡，刪掉等於把唯一的線索丟了。
func RunCase(ctx context.Context, sourceWS, caseRoot string, c Case) (result RunResult, err error) {
	ws, err := PrepareWorkspace(sourceWS, caseRoot, c.Setup.Files)
	if err != nil {
		return RunResult{}, err
	}

	cfg, err := config.Load(filepath.Join(ws, "config.yaml"))
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 載入 Workspace 設定檔: %w", c.Name, err)
	}
	// 路徑經 ProfilePath 組出來而不是就地 filepath.Join：那個方法會再校驗一次 profile
	// 名，確保載入的是**複製進這份乾淨 Workspace** 的 Profile（見它的說明）。
	profilePath, err := c.ProfilePath(ws)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: %w", c.Name, err)
	}
	prof, err := core.LoadProfile(profilePath)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 載入 Profile %s: %w", c.Name, c.Profile, err)
	}
	if _, ok := cfg.Providers[prof.Provider.Name]; !ok {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 引用的 Provider %q 未在 config.yaml 的 providers 段配置",
			c.Name, prof.Name, prof.Provider.Name)
	}
	// MCP 不在本票範圍。**明白擋下而不是讓它自己壞**：不接的話，Profile 引用的 MCP
	// 工具會在 Registry.Subset 那一層被判成「Tool 未註冊」，使用者看到的是一句與 MCP
	// 毫無關聯的錯誤訊息（憲法 5.1 要求錯誤被顯式處理，一個誤導性的錯誤不算被處理）。
	refs, err := prof.McpServerRefs()
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 mcp_servers 校驗失敗: %w", c.Name, prof.Name, err)
	}
	if len(refs) > 0 {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 引用了 MCP server %v，評測 harness 目前不支援外部 MCP"+
			"（ticket #50 範圍）；請改用只含內建 Tool 的 Profile", c.Name, prof.Name, refs)
	}

	logFile, err := os.OpenFile(filepath.Join(ws, "logs", "oryxos.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 開啟日誌檔: %w", c.Name, err)
	}
	defer func() {
		if cerr := logFile.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("用例 %s: 關閉日誌檔: %w", c.Name, cerr)
		}
	}()
	logger := slog.New(slog.NewJSONHandler(logFile, nil))

	providers, err := config.ExpandProviderEnv(cfg.Providers)
	if err != nil {
		// 缺憑證最常見的形態就是這一條：config.yaml 寫 ${OPENROUTER_API_KEY}、環境變數
		// 沒設。錯誤訊息已經指名是哪一個變數，這裡只補上「是評測在要它」。
		return RunResult{}, fmt.Errorf("用例 %s: 載入 Provider 憑證（評測需要真實 API 憑證）: %w", c.Name, err)
	}
	// 憑證展開成空字串也要擋。**在送出請求之前擋**是重點：不擋的話這一次會照樣送出、
	// 拿一個 401 回來，時間花了、錯誤訊息還是 Provider 那邊的英文。
	//
	// 不需要憑證的本機端點（如 ollama）請在 api_key 填任意非空字串——那些端點不驗它。
	if providers[prof.Provider.Name].APIKey == "" {
		return RunResult{}, fmt.Errorf("用例 %s: providers.%s.api_key 是空的；評測會呼叫真實 Provider，請先設定憑證",
			c.Name, prof.Provider.Name)
	}
	providerConfigs := make(map[string]provider.Config, len(providers))
	for name, pc := range providers {
		providerConfigs[name] = provider.Config{APIKey: pc.APIKey, BaseURL: pc.BaseURL}
	}

	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 開啟 Workspace: %w", c.Name, err)
	}
	defer func() {
		if cerr := wsRoot.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("用例 %s: 關閉 Workspace: %w", c.Name, cerr)
		}
	}()

	bootSel, err := prof.BootstrapSelection()
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 bootstrap 校驗失敗: %w", c.Name, prof.Name, err)
	}
	if err := config.ValidateBootstrapFiles(wsRoot, bootSel); err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 bootstrap 校驗失敗: %w", c.Name, prof.Name, err)
	}
	contextLoader := config.NewContextLoader(wsRoot)
	skillRefs, err := prof.SkillRefs()
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 skills 校驗失敗: %w", c.Name, prof.Name, err)
	}
	if _, err := contextLoader.Skills(ctx, skillRefs); err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 skills 校驗失敗: %w", c.Name, prof.Name, err)
	}

	longTerm := memory.NewLongTermMemory(wsRoot, filepath.Join("memory", memoryFile))
	registry, err := buildToolRegistry(cfg, ws, wsRoot, longTerm, contextLoader, skillRefs)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: %w", c.Name, err)
	}
	executor, err := registry.Subset(prof.Tools, autoIncludedTools(skillRefs), logger)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Profile %s 的 tools 校驗失敗: %w", c.Name, prof.Name, err)
	}

	store, err := storage.Open(ctx, filepath.Join(ws, sessionDBFile))
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 開啟 Workspace 資料庫: %w", c.Name, err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("用例 %s: 關閉資料庫: %w", c.Name, cerr)
		}
	}()
	sessions := storage.NewSessionManager(store)
	session, err := sessions.ActiveSession(ctx, evalChannel, evalUserID, prof.Name)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 建立 Session: %w", c.Name, err)
	}
	audit := storage.NewAuditLog(store, logger)
	// Close 要排在 store.Close 之前跑，否則佇列裡還沒寫出去的記錄會隨進程消失——
	// defer 是後進先出，所以這行寫在開 store 之後。
	defer func() {
		if cerr := audit.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("用例 %s: 關閉審計儲存: %w", c.Name, cerr)
		}
	}()

	// 事件流傳不做事的實作：評測要的是一輪跑完之後的結果，不需要過程播報，而讓進度
	// 訊息與判卷結果交錯輸出只會讓報表難讀。日後要用事件算指標時，換一個記錄型實作
	// 掛在這裡即可——那正是 EventSink 這個出向介面存在的意義。
	agent := core.NewAgentService(prof, provider.NewService(providerConfigs, logger), executor,
		memory.NewService(sessions, longTerm), audit, contextLoader,
		core.NopEventSink{}, config.PriceListOf(providers), logger)

	reply, err := agent.Process(ctx, session, c.Task)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: Agent 執行失敗: %w", c.Name, err)
	}

	// **查審計表之前一定要先排空。** 審計寫入走背景 worker，Process 回來的當下記錄可能
	// 都還在佇列裡——不排空就查，讀到的會是一張還沒落庫的空表，而評測會據此宣告
	// 「一個 Tool 都沒呼叫過」。這是本票最容易踩、而且失敗時完全看不出原因的一步。
	if err := audit.Flush(ctx); err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: 排空審計佇列: %w", c.Name, err)
	}
	tools, err := store.ToolNamesForSession(ctx, session.ID)
	if err != nil {
		return RunResult{}, fmt.Errorf("用例 %s: %w", c.Name, err)
	}
	return RunResult{Reply: reply, ToolsCalled: tools}, nil
}

// buildToolRegistry 顯式註冊這個 Workspace 的全部內建 Tool（憲法 2.3）。
//
// **這是 `cmd/oryxos` 那一份的第二份，不是共用的。** 記在這裡是因為它有真實代價：兩邊
// 若哪天漂了，評測量到的就不是使用者實際跑的那個 Agent——而報表照樣印出綠色。真正的
// 解法是把組裝抽到一個雙方共用的地方，那是一次跨 `cmd/oryxos` 的重構，不在 ticket #50
// 範圍內（已記進工作記錄）。在那之前，這裡加減 Tool 都必須與 chat 那一份同步。
func buildToolRegistry(cfg *config.Config, ws string, wsRoot *os.Root,
	longTerm *memory.LongTermMemory, skills core.ContextLoader, skillRefs []string) (*tool.Registry, error) {
	sandbox := tool.SandboxConfig{
		AllowedDomains:  cfg.HTTP.AllowedDomains,
		AllowedPaths:    cfg.File.AllowedPaths,
		AllowedCommands: cfg.Shell.AllowedCommands,
		ShellTimeout:    cfg.Shell.EffectiveTimeout(),
	}
	shell := tool.ShellRuntime{Dir: ws, PathDirs: tool.ParentPathDirs(), Timeout: sandbox.ShellTimeout}

	registry := tool.NewRegistry()
	// shell 的 admission limiter 整個進程共用一份。這裡只有一個呼叫點，所以就地建立；
	// `cmd/oryxos` 那邊有兩個呼叫點，才必須由更上層建好再傳進去（ticket #35）。
	if err := tool.RegisterBuiltins(registry, tool.NewSandboxChecker(sandbox), wsRoot, shell, tool.NewShellLimiter()); err != nil {
		return nil, fmt.Errorf("組裝 Tool registry: %w", err)
	}
	for _, memTool := range []tool.OryxTool{memory.NewSaveMemoryTool(longTerm), memory.NewRecallMemoryTool(longTerm)} {
		if err := registry.Register(memTool); err != nil {
			return nil, fmt.Errorf("註冊 Memory Tool: %w", err)
		}
	}
	if err := registry.Register(tool.NewLoadSkillTool(skills, skillRefs)); err != nil {
		return nil, fmt.Errorf("註冊 load_skill: %w", err)
	}
	if err := registry.Register(tool.NewTextStatsTool()); err != nil {
		return nil, fmt.Errorf("註冊原生 Go Tool 示例 text_stats: %w", err)
	}
	return registry, nil
}

// autoIncludedTools 依配置推導出要自動加進可用子集的 Tool，與 `cmd/oryxos` 同一條規則：
// Profile 的 skills 非空 → load_skill。少了它，一份宣告了 skills 的 Profile 會安靜退化
// 成「LLM 看得到 Skill 描述、永遠載不到正文」。
func autoIncludedTools(skillRefs []string) []string {
	if len(skillRefs) == 0 {
		return nil
	}
	return []string{tool.LoadSkillToolName}
}
