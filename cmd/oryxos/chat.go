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

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/channel/cli"
	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
)

func newChatCmd() *cobra.Command {
	var (
		profileName string
		message     string
	)
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "與 Agent 進入多輪對話（CLI Channel）",
		Long: "在已初始化的 Workspace 中與 Agent 對話。多輪對話共享同一個 Session，\n" +
			"輸入 /quit 結束；--message 送出單條訊息、輸出回應後退出。",
		Args: cobra.NoArgs,
		// 執行期錯誤（未初始化、缺 API key、Provider 故障）與用法無關，
		// 不倒 Usage 沖淡錯誤訊息。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("取得當前目錄: %w", err)
			}
			return runChat(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cwd, profileName, message)
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "default", "使用的 Profile 名（.oryxos/profiles/<name>.yaml）")
	cmd.Flags().StringVar(&message, "message", "", "送出單條訊息、輸出回應後退出")
	return cmd
}

// runChat 載入 Workspace 設定檔與 Profile、校驗 Provider 可解析，組出
// AgentService 後交給 CLI Channel；message 非空時走單訊息模式。
func runChat(ctx context.Context, in io.Reader, out io.Writer, baseDir, profileName, message string) (err error) {
	ws := filepath.Join(baseDir, workspaceDir)
	if _, err := os.Stat(ws); err != nil {
		return fmt.Errorf("找不到 Workspace %s（請先執行 oryxos init）: %w", workspaceDir, err)
	}

	cfg, err := config.Load(filepath.Join(ws, "config.yaml"))
	if err != nil {
		return fmt.Errorf("載入 Workspace 設定檔: %w", err)
	}
	prof, err := core.LoadProfile(filepath.Join(ws, "profiles", profileName+".yaml"))
	if err != nil {
		return fmt.Errorf("載入 Profile %s: %w", profileName, err)
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
	agent := core.NewAgentService(prof, provider.NewService(providerConfigs, logger))
	ch := cli.New(agent, prof.Name, prof.Identity.AgentName, in, out)

	if message != "" {
		return ch.RunOnce(ctx, message)
	}
	return ch.RunInteractive(ctx)
}
