package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rexshen5913/oryxos/internal/core"
)

const (
	// channelName 是 CLI Channel 在 Session 聯合標識中的 Channel 取值。
	channelName = "cli"
	// localUserID 是 CLI 在 Session 聯合標識中的使用者取值。核心階段 CLI 為
	// 本機單使用者場景，固定為 local（spec #1 未決事項在本張的實作決定）。
	localUserID = "local"

	goodbyeMsg     = "再見。"
	interruptedMsg = "對話已中斷。"
)

// Channel 是 CLI Channel：讀 in 寫 out 的互動式對話，維護當前 Session，
// 每次輸入調 AgentService.Process。
type Channel struct {
	agent     *core.AgentService
	session   *core.Session
	in        io.Reader
	out       io.Writer
	agentName string
}

// New 建立 CLI Channel，內部以 Channel＋使用者＋Profile 聯合標識建立記憶體版
// Session；同一次 oryxos chat 內的多輪對話共享此 Session。
func New(agent *core.AgentService, profileName, agentName string, in io.Reader, out io.Writer) *Channel {
	return &Channel{
		agent:     agent,
		session:   core.NewSession(channelName, localUserID, profileName),
		in:        in,
		out:       out,
		agentName: agentName,
	}
}

// RunOnce 送出單條訊息、輸出回應後返回（--message 模式）。
func (c *Channel) RunOnce(ctx context.Context, message string) error {
	resp, err := c.agent.Process(ctx, c.session, message)
	if err != nil {
		return fmt.Errorf("CLI Channel 處理訊息: %w", err)
	}
	fmt.Fprintln(c.out, resp)
	return nil
}

// RunInteractive 進入多輪對話，直到 /quit、輸入結束（EOF）或 ctx 取消。
// 輸入讀取走獨立 goroutine：idle prompt 阻塞等輸入時也能被 ctx 取消
// （如 Ctrl+C 經 signal.NotifyContext，憲法 5.3）。ctx 取消視為使用者主動
// 中斷，乾淨返回不算錯誤。
func (c *Channel) RunInteractive(ctx context.Context) error {
	fmt.Fprintf(c.out, "與 %s 對話中（輸入 /quit 結束）\n", c.agentName)

	// 讀取 goroutine 在輸入來源結束（EOF／錯誤）時關閉 lines 後退出。
	// stdin 的阻塞 read 本身不可取消：ctx 取消後它可能仍卡在 Scan，直到
	// 下一行輸入或來源關閉才退出。這是接受的折衷——RunInteractive 每個
	// 進程只跑一次，返回後進程隨即結束、由進程回收；若未來被長駐流程
	//（如 Web Service）重用，需先改造這裡的輸入讀取。
	lines := make(chan string)
	var scanErr error // 僅讀取 goroutine 寫入；close(lines) 先行發生，主迴圈讀取無競態
	go func() {
		scanner := bufio.NewScanner(c.in)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		scanErr = scanner.Err()
		close(lines)
	}()

	for {
		fmt.Fprint(c.out, "> ")
		var input string
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.out, "\n"+interruptedMsg)
			return nil
		case line, ok := <-lines:
			if !ok {
				if scanErr != nil {
					return fmt.Errorf("讀取輸入: %w", scanErr)
				}
				fmt.Fprintln(c.out, goodbyeMsg)
				return nil
			}
			input = strings.TrimSpace(line)
		}
		if input == "" {
			continue
		}
		if input == "/quit" {
			fmt.Fprintln(c.out, goodbyeMsg)
			return nil
		}
		resp, err := c.agent.Process(ctx, c.session, input)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(c.out, interruptedMsg)
				return nil
			}
			// 失敗 turn 已由 AgentService rollback，暫時性故障不終結整場
			// 對話（記憶體版 Session 一斷就沒了）。本輪若執行過 Tool，
			// 副作用註記由 Process 的錯誤訊息承載，這裡不重複判斷。
			fmt.Fprintf(c.out, "錯誤：%v（本輪對話已回退，可重試或輸入 /quit 離開）\n", err)
			continue
		}
		fmt.Fprintf(c.out, "%s> %s\n", c.agentName, resp)
	}
}
