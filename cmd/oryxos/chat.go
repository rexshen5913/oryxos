// chat.go 實作 `oryxos chat`：載入 Workspace 配置與 Profile，組出引擎後
// 進入 CLI Channel 對話。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/channel/cli"
	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
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
// 它必須是整個 OryxOS 進程共用的**那一份**，而這個函式**有兩個呼叫點**（assembleProfile
// 與 runTools）——在這裡 `tool.NewShellLimiter()` 就是每次呼叫一份新的，跨 session 的總量
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

// countUnmatchableDomains 數一份**已收斂**的網域白名單裡，有幾條含有 host 裡幾乎不會出現
// 的字元，供啟動提醒使用。每條最多計一次，同時符合多個判準也只計一次。
//
// 判準取自 CheckHTTPURL 真的會走的那條路（url.Parse → Hostname()），每個字元都實測過：
//
//   - `/`、`?`、`#`：authority 遇到它們就結束，進不了 host。scheme 的 `://` 由 `/` 涵蓋。
//   - `@`：userinfo 的分隔符，host 在它之後。
//   - ASCII 空白與控制字元（0x00–0x20、0x7F）：url.Parse 在 host 裡直接拒絕。
//
// **這是「幾乎一定比不中」，不是保證。** 實測找到一個例外：zoned IPv6 的 zone 裡以 `%20`
// 寫進的空白會留在 Hostname() 裡（`http://[fe80::1%25a%20b]/` → `fe80::1%a b`）。所以它
// 只配當一行提醒，不配當剔除的依據——提醒印錯了，使用者忽略它就好。
//
// **刻意不算的字元**：它們出現在比得中的合法條目裡，算進來就是誤報。
//
//   - `:` 與 `%`：IPv6（`::1`）與 zoned IPv6（`fe80::1%en0`）。
//   - `*`：`*example.com` 是 url.Parse 接受的字面 host。
//   - NBSP、全形空白與 C1 控制字元：非 ASCII，url.Parse 接受它們當 host。C1 寫成 YAML
//     逸出（`\u0085`）就進得了 config.yaml，不是理論上的邊角。
func countUnmatchableDomains(domains []string) int {
	count := 0
	for _, domain := range domains {
		if strings.ContainsAny(domain, "/?#@") || strings.ContainsFunc(domain, isASCIISpaceOrControl) {
			count++
		}
	}
	return count
}

// isASCIISpaceOrControl 判斷 r 是不是 ASCII 空白或控制字元（0x00–0x20、0x7F）。
func isASCIISpaceOrControl(r rune) bool {
	return r <= ' ' || r == 0x7f
}

// runChat 組一次進程層級、再組一次 Profile 層級（見 assembly.go），把組出的 AgentService
// 交給 CLI Channel；message 非空時走單訊息模式。
func runChat(ctx context.Context, in io.Reader, out io.Writer, baseDir string, opts chatOptions) (err error) {
	proc, err := assembleProcess(ctx, out, baseDir)
	// 收尾無條件排進 defer，而且整個函式只排這一個：失敗時的半成品也要收（見
	// assembleProcess），Profile 層級的 MCP 連線也交給它按順序收（見 processAssembly.Close）。
	var assembled *profileAssembly
	defer func() {
		if cerr := proc.Close(assembled); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err != nil {
		return err
	}

	prof, err := core.LoadProfile(filepath.Join(proc.ws, "profiles", opts.profileName+".yaml"))
	if err != nil {
		return fmt.Errorf("載入 Profile %s: %w", opts.profileName, err)
	}

	// 執行過程的事件流：CLI 用它在等待期間顯示進度。輸出不是終端機時
	// （`--message` 接管線、重導向到檔案、測試接 buffer）ProgressSinkFor 會退回
	// 不做事的實作，既有輸出格式一個字都不變。
	//
	// 它要在組 Profile 之前建好，是因為 Tool 事件由掛在 Executor 上的中介層播報——
	// Subset 需要它。
	events := cli.ProgressSinkFor(out)
	assembled, err = assembleProfile(ctx, out, proc, prof, events)
	if err != nil {
		return err
	}

	// --new：先歸檔當前 active Session，下面的 ActiveSession 就取不到 active 列，
	// 自然開出一場乾淨的新對話（沒有 active Session 時歸檔是 no-op，不報錯）。
	if opts.newConversation {
		if err := proc.sessions.ArchiveActive(ctx, cli.ChannelName, cli.LocalUserID, prof.Name); err != nil {
			return fmt.Errorf("歸檔當前 active Session: %w", err)
		}
	}
	// 同一（Channel、使用者、Profile）聯合標識的 active Session 自動恢復，
	// 沒有時開新的：重新執行 oryxos chat 就接得上先前的上下文。
	session, err := proc.sessions.ActiveSession(ctx, cli.ChannelName, cli.LocalUserID, prof.Name)
	if err != nil {
		return fmt.Errorf("取回 active Session: %w", err)
	}

	ch := cli.New(assembled.agent, session, prof.Identity.AgentName, in, out)
	if opts.message != "" {
		return ch.RunOnce(ctx, opts.message)
	}
	return ch.RunInteractive(ctx)
}
