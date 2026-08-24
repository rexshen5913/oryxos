// tools.go 實作 `oryxos tools`：列出這個 Workspace 在某個 Profile 之下可用的全部 Tool，
// 回答「Profile 的 tools 欄位可以寫什麼名字」（issue #27）。
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// toolsOptions 是 tools 命令的旗標集合。
type toolsOptions struct {
	profileName string
}

func newToolsCmd() *cobra.Command {
	var opts toolsOptions
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "列出可以寫進 Profile tools 欄位的全部 Tool",
		Long: "連上 Profile 引用的 MCP server、取回它們的工具清單，連同內建 Tool 一起列出。\n" +
			"輸出的名字可以直接複製進 Profile 的 tools 欄位。\n" +
			"--profile 指定要看哪一份 Profile（它決定連哪些 MCP server）。",
		Args: cobra.NoArgs,
		// 執行期錯誤（未初始化、Profile 壞掉、server 連不上）與用法無關，
		// 不倒 Usage 沖淡錯誤訊息。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("取得當前目錄: %w", err)
			}
			return runTools(cmd.Context(), cmd.OutOrStdout(), cwd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.profileName, "profile", "default", "使用的 Profile 名（.oryxos/profiles/<name>.yaml）")
	return cmd
}

// runTools 組出與 `oryxos chat` **完全相同**的 Registry，再把內容印出來。
//
// 「相同」是這個命令唯一的價值來源：它列的若不是 chat 真的會註冊的那一組，使用者照抄
// 之後仍然啟動失敗，那比沒有這個命令更糟——他會相信它。所以 Tool 的來源一律走既有的
// buildToolRegistry 與 connectProfileMcpServers，這裡不自己拼一份。
//
// 不碰 Provider：列工具與 LLM 無關，一個還沒設好 API key 的 Workspace 也該查得出來。
func runTools(ctx context.Context, out io.Writer, baseDir string, opts toolsOptions) (err error) {
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

	// MCP 連線的結構化日誌與 chat 落同一個檔：查「這台 server 為什麼沒工具」時，
	// 兩個命令的記錄在一起才看得出前後。
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

	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		return fmt.Errorf("開啟 Workspace %s: %w", workspaceDir, err)
	}
	defer func() {
		if cerr := wsRoot.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("關閉 Workspace: %w", cerr)
		}
	}()

	// Profile 的 skills 壞掉一律報錯，與 chat 一致：這個命令要照出 chat 會看到的東西，
	// 對設定錯誤睜一隻眼會讓它報喜不報憂。
	skillRefs, err := prof.SkillRefs()
	if err != nil {
		return fmt.Errorf("Profile %s 的 skills 校驗失敗: %w", prof.Name, err)
	}
	longTerm := memory.NewLongTermMemory(wsRoot, filepath.Join("memory", memoryFile))
	sandbox := sandboxConfig(cfg)
	registry, err := buildToolRegistry(sandbox, shellRuntime(sandbox, ws), wsRoot, longTerm,
		config.NewContextLoader(wsRoot), skillRefs)
	if err != nil {
		return err
	}

	mcpClients, mcpSpecs, err := connectProfileMcpServers(ctx, out, ws, prof, registry, logger)
	// Close 無條件排進 defer：這個命令會 spawn 子進程，退出前要收乾淨，否則留下孤兒。
	defer func() {
		if cerr := mcpClients.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err != nil {
		return err
	}

	// **刻意不呼叫 Registry.Subset**：那是「哪些進得了這個 Agent 的可用子集」的白名單
	// 校驗，而列清單不需要那道關。加上去的話，使用者最需要這個命令的時刻——Profile 的
	// tools 剛好寫錯、chat 起不來——它也會跟著報同一個錯，兩個命令互相指向對方。
	printTools(out, registry.All(), prof, mcpSpecs)
	return nil
}

// printTools 按來源分組印出全部 Tool，並標記 Profile 已經列了哪些。
//
// **分組**是因為使用者跑這個命令的當下多半剛接上一台新 server，想看的是「這台給了我
// 什麼」；五十個名字混在一份字母序清單裡答不出那個問題。組的順序跟著 Profile 的
// mcp_servers 宣告順序走。
//
// **標記**是因為接第二台 server 時真正要找的是「還沒加進去的是哪些」。沒有標記的話
// 使用者得自己拿 Profile 逐行比對，而那正是這個命令該替他做的事。
func printTools(out io.Writer, all []tool.ToolInfo, prof *core.Profile, specs []core.McpServerSpec) {
	listed := make(map[string]bool, len(prof.Tools))
	for _, name := range prof.Tools {
		listed[name] = true
	}

	// 來源直接讀 ToolInfo.Server（註冊時記下的），不從註冊名反推：server 名可以含雙底線，
	// `foo` 與 `foo__bar` 同時存在時，`foo__bar__echo` 用前綴去猜會掛到錯的那一台。
	byServer := make(map[string][]tool.ToolInfo, len(specs))
	var builtin []tool.ToolInfo
	for _, info := range all {
		if info.Server != "" {
			byServer[info.Server] = append(byServer[info.Server], info)
			continue
		}
		builtin = append(builtin, info)
	}

	printToolGroup(out, "內建 Tool", builtin, listed)
	for _, spec := range specs {
		printToolGroup(out, "MCP server: "+spec.Name, byServer[spec.Name], listed)
	}
	fmt.Fprintf(out, "\n✓ = 已列在 Profile %s 的 tools 裡；其餘要用的話把名字照抄進去。\n", prof.Name)
}

// printToolGroup 印一組 Tool；空組整組略過（沒有工具的標題只是雜訊）。
//
// 名字與描述分兩行：描述長短差很多（`recall_memory` 有兩百多字），排成一欄會讓短的那些
// 擠在左邊、長的那些自己折行，反而更難掃。名字獨佔一行也方便直接複製。
func printToolGroup(out io.Writer, title string, tools []tool.ToolInfo, listed map[string]bool) {
	if len(tools) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s\n", title)
	for _, info := range tools {
		mark := "  "
		if listed[info.Name] {
			mark = "✓ "
		}
		fmt.Fprintf(out, "  %s%s\n      %s\n", mark, info.Name, info.Description)
	}
}
