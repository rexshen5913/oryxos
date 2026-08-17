// mcp.go 收 `oryxos chat` 與 `oryxos tools` 共用的 MCP 組裝段。
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// connectProfileMcpServers 走完「宣告檔 → Profile 引用 → 憑證展開 → 連線 → tools/list
// → 註冊進 registry」整條鏈路，回傳連線服務與**這個 Profile 實際用到的** specs。
//
// **兩個命令共用同一條路徑是這個函式存在的唯一理由**：`oryxos tools` 列出來的東西必須
// 就是 `oryxos chat` 會註冊的那一組。各寫一份的話，兩邊哪天漂了，這個命令就從「查得到」
// 變成「查到的是假的」——那比沒有這個命令更糟，因為使用者會相信它。
//
// 回傳的 specs 給呼叫端按 server 分組用。**Close 的責任在呼叫端**：連線中途失敗時也要
// 收掉已經起來的子進程，所以 svc 一律回傳（對 nil 接收者 Close 是 no-op），呼叫端無條件
// 排進 defer。
func connectProfileMcpServers(ctx context.Context, out io.Writer, ws string, prof *core.Profile,
	registry *tool.Registry, logger *slog.Logger) (*tool.McpClientService, []core.McpServerSpec, error) {
	declared, err := config.LoadMcpServers(filepath.Join(ws, core.McpServersFile))
	if err != nil {
		return nil, nil, fmt.Errorf("載入 MCP server 宣告: %w", err)
	}
	refs, err := prof.McpServerRefs()
	if err != nil {
		return nil, nil, fmt.Errorf("Profile %s 的 mcp_servers 校驗失敗: %w", prof.Name, err)
	}
	// 只解析出**這個 Profile 引用到的**那幾份，未被引用的宣告連子進程都不會 spawn。
	specs, err := core.ResolveMcpServers(refs, declared)
	if err != nil {
		return nil, nil, fmt.Errorf("Profile %s 的 mcp_servers 校驗失敗: %w", prof.Name, err)
	}
	// 憑證的 ${ENV_VAR} 展開在過濾**之後**：一個沒被這個 Agent 引用的 server 缺環境變數
	// 不該擋下啟動（只接 Slack 的 Agent 不該因為機器上沒有 GitHub token 而起不來）。
	specs, err = config.ExpandMcpServerEnv(specs)
	if err != nil {
		return nil, nil, fmt.Errorf("載入 MCP server 憑證: %w", err)
	}
	// 警示在**每個 server 失敗的當下**就印（issue #26），不等整個連線階段跑完：連線是
	// 並行的，總等待仍是最慢的那一個，而那可以是連線期限的完整 30 秒。
	svc, err := tool.ConnectMcpServers(ctx, registry, specs,
		func(failure tool.McpConnectFailure) { warnMcpServerUnavailable(out, failure) },
		logger)
	if err != nil {
		return svc, specs, fmt.Errorf("連線 MCP server: %w", err)
	}
	return svc, specs, nil
}
