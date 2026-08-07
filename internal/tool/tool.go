package tool

import (
	"context"
	"encoding/json"

	"github.com/rexshen5913/oryxos/internal/core"
)

// OryxTool 是 OryxOS 內部統一的 Tool 抽象：內建 Tool、原生 Go 的 Plugin Tool、
// MCP Tool（後續 ticket）都以此介面註冊到 ToolRegistry，ReAct 循環不感知
// 具體 Tool 的來源（技術方案 §6.1）。
type OryxTool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	// Execute 以 JSON 編碼的輸入執行 Tool，一律以 ToolResult 回報結果或錯誤。
	Execute(ctx context.Context, input string) core.ToolResult
}
