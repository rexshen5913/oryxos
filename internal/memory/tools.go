package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// RecallMemoryTool 是內建 Tool recall_memory：以關鍵詞檢索長期記憶、把匹配的
// 記憶行回填給 LLM，讓 Agent 需要回想特定主題時能精準取用，不只靠整檔注入。
// 與 SaveMemoryTool 同樣由組裝點顯式註冊、受 Profile 的 tools 欄位過濾。
type RecallMemoryTool struct {
	longTerm *LongTermMemory
}

// NewRecallMemoryTool 以長期記憶讀寫建立 recall_memory。
func NewRecallMemoryTool(longTerm *LongTermMemory) *RecallMemoryTool {
	return &RecallMemoryTool{longTerm: longTerm}
}

func (t *RecallMemoryTool) Name() string { return "recall_memory" }

func (t *RecallMemoryTool) Description() string {
	return "以關鍵詞檢索長期記憶，回傳匹配的記憶行。需要回想使用者先前告訴過你的偏好或事實時使用；" +
		"注入在系統提示詞裡的記憶可能已因長度上限被截斷，這個 Tool 讀的是完整檔案。"
}

func (t *RecallMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "要檢索的關鍵詞，例如某個技術名稱、專案名或主題"}
		},
		"required": ["query"]
	}`)
}

// recallMemoryInput 是 recall_memory 的輸入參數。
type recallMemoryInput struct {
	Query string `json:"query"`
}

// Execute 以關鍵詞檢索長期記憶。沒有匹配**不是失敗**：明確回報「查過了但沒有」
// 讓 LLM 決定下一步（例如直接問使用者），而不是把空結果包成錯誤讓迴圈以為出事。
// 關鍵詞校驗失敗與 save_memory 同樣標不可重試——參數問題重試幾次都一樣。
func (t *RecallMemoryTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in recallMemoryInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 recall_memory 輸入參數: %v", err)}
	}
	matches, err := t.longTerm.RecallByKeyword(ctx, in.Query)
	if err != nil {
		if errors.Is(err, ErrInvalidEntry) {
			return core.ToolResult{Error: err.Error()}
		}
		return core.ToolResult{Error: fmt.Sprintf("recall_memory 檢索長期記憶失敗: %v", err)}
	}
	if matches == "" {
		// 引述關鍵詞讓 LLM 知道查的是什麼，但引述本身要有上限——超長的 query
		// 原樣回顯會讓這則回填無視 4000 rune 的約束。
		return core.ToolResult{OK: true, Content: fmt.Sprintf("長期記憶中沒有符合「%s」的內容。",
			abbreviate(strings.TrimSpace(in.Query), maxEchoRunes))}
	}
	return core.ToolResult{OK: true, Content: matches}
}
