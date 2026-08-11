package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rexshen5913/oryxos/internal/core"
)

// SaveMemoryTool 是內建 Tool save_memory：把 LLM 自主判斷值得長期記住的偏好或
// 事實追加進 MEMORY.md。實作 tool.OryxTool（結構化滿足，不反向依賴 internal/tool），
// 由組裝點顯式註冊進 ToolRegistry，並受 Profile 的 tools 欄位過濾（憲法 2.3）。
type SaveMemoryTool struct {
	longTerm *LongTermMemory
}

// NewSaveMemoryTool 以長期記憶讀寫建立 save_memory。
func NewSaveMemoryTool(longTerm *LongTermMemory) *SaveMemoryTool {
	return &SaveMemoryTool{longTerm: longTerm}
}

func (t *SaveMemoryTool) Name() string { return "save_memory" }

func (t *SaveMemoryTool) Description() string {
	return fmt.Sprintf("把值得長期記住的使用者偏好或關鍵事實寫進長期記憶，跨對話保留。"+
		"一次只記一條穩定、可複用的事實，內容上限 %d 字。", maxEntryRunes)
}

func (t *SaveMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"content": {"type": "string", "description": "要長期記住的一條偏好或事實，簡潔陳述，上限 %d 字"}
		},
		"required": ["content"]
	}`, maxEntryRunes))
}

// saveMemoryInput 是 save_memory 的輸入參數。
type saveMemoryInput struct {
	Content string `json:"content"`
}

// Execute 追加一條長期記憶。校驗失敗（空內容、超過單條上限）以 Retryable: false
// 回填——參數問題重試幾次都一樣，錯誤訊息本身就是給模型的修改指示；寫入失敗
// （目錄不可寫、I/O 錯誤）同樣不可重試：本機檔案的權限與路徑問題不會自己好，
// 讓 LLM 看到錯誤改走別的路，比空轉三次指數退避有用。
func (t *SaveMemoryTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in saveMemoryInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 save_memory 輸入參數: %v", err)}
	}
	if err := t.longTerm.Append(ctx, in.Content); err != nil {
		if errors.Is(err, ErrInvalidEntry) {
			return core.ToolResult{Error: err.Error()}
		}
		return core.ToolResult{Error: fmt.Sprintf("save_memory 寫入長期記憶失敗: %v", err)}
	}
	return core.ToolResult{OK: true, Content: "已寫入長期記憶。"}
}
