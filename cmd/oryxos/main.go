// oryxos 是 OryxOS 的命令行入口，cobra 命令樹在此註冊。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oryxos",
		Short: "OryxOS — 用 Go 實作的企業級 Agent OS",
		Long: "OryxOS 是面向企業場景的 Agent OS：以 Profile 配置 Agent，\n" +
			"透過 CLI 與其對話，Agent 經 ReAct 循環呼叫 LLM 與 Tool 完成任務。",
		// 裸跑與 --help 都顯示幫助（無 Run 時 cobra 會省略 Usage 區塊）。
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newChatCmd())
	cmd.AddCommand(newToolsCmd())
	return cmd
}

func main() {
	// Ctrl+C／SIGTERM 取消 context，讓阻塞路徑（如進行中的 LLM 呼叫）可中斷（憲法 5.3）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// cobra 已將錯誤輸出到 stderr，這裡只負責非零退出碼。
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
