package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/core"
)

// 原生 Go Tool 示例——Plugin Tool 的**方式三**（方式一是 SKILL.md、方式二是 MCP）。
//
// 給需要進程內深度整合的業務方照抄：複用既有的 Go 程式碼、或對效能敏感、不想付
// 序列化與跨進程往返的成本時走這條。寫法與內建 Tool（http.go、skill.go）**完全一樣**
// ——實作 OryxTool 的四個方法、在組裝點顯式註冊（憲法 2.3，見 cmd/oryxos/chat.go 的
// buildToolRegistry）、編進同一個二進制。不走 MCP 協議、不起獨立進程。
//
// 註冊之後它與內建 Tool 一視同仁：一樣受 Profile 的 tools 欄位過濾（那是欄位過濾，
// 不是 Tool Policy——後者屬擴展階段），一樣落 tool_invocations 審計表，ReAct 循環也
// 不感知它是誰寫的。
//
// **這裡的功能本身不重要**，統計字數只是為了讓樣板小到能一眼看完。要加自己的 Tool
// 就把 textStatsTool 整份複製走，改掉名稱、描述、schema 與 Execute 的內容。

// textStatsToolName 是這個示例對 LLM 宣告的名稱，也是 Profile 的 tools 欄位要引用的
// 那個字串。
const textStatsToolName = "text_stats"

// textStatsTool 統計一段文字的字元數與詞數。純進程內計算，不碰網路、時間或隨機值
// ——所以它的行為是確定的，測試不需要起任何外部依賴。
//
// 型別不匯出、只匯出建構函式：業務方要拿到它只有 NewTextStatsTool 一條路，日後要
// 加欄位（例如自己的 client、設定）不會變成破壞性改動。
type textStatsTool struct{}

// NewTextStatsTool 建立原生 Go Tool 示例。真實的 Tool 通常會在這裡收下它需要的依賴
// （資料庫連線、既有的 service、設定值）並存進結構體——顯式注入，不用全域變數
// （憲法 5.2）。這個示例沒有依賴，所以參數是空的。
func NewTextStatsTool() OryxTool {
	return &textStatsTool{}
}

func (t *textStatsTool) Name() string { return textStatsToolName }

// Description 是 LLM 判斷「什麼時候該用這個 Tool」的唯一依據，寫給模型看不是寫給
// 人看：講清楚它做什麼、回什麼。
func (t *textStatsTool) Description() string {
	return "統計一段文字的字元數（以 Unicode 字元計，中日韓字一個算一個）與詞數（以空白分詞）。" +
		"純本機計算，不連任何外部服務。"
}

// InputSchema 是 LLM 產生呼叫參數時遵循的 JSON Schema，由 Provider 轉成 OpenAI
// 兼容協議的 function 宣告。
func (t *textStatsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"text": {"type": "string", "description": "要統計的文字"}
		},
		"required": ["text"]
	}`)
}

// textStatsInput 是 text_stats 的輸入參數。
type textStatsInput struct {
	Text string `json:"text"`
}

// textStatsOutput 是回填給 LLM 的結果內容。
type textStatsOutput struct {
	Runes int `json:"runes"`
	Words int `json:"words"`
}

// Execute 執行一次統計。
//
// 兩件事照抄時要留意：
//
//   - **失敗一律以 ToolResult.Error 回填，不上拋、不 panic**。錯誤訊息是寫給 LLM 看
//     的修改指示，讓它換一條路回覆使用者，比讓整個 turn 崩掉有用（沿 spec #1 既有的
//     Tool 失敗語義）。這裡全部不標 Retryable——參數錯重試幾次都一樣。
//   - **ctx 這裡沒用到**，因為純計算不是阻塞路徑。你的 Tool 只要碰到 I/O（查資料庫、
//     打 HTTP、跑子進程），就必須把 ctx 傳下去（憲法 5.3），否則使用者按下 Ctrl-C
//     或 turn 逾時的時候它停不下來。
func (t *textStatsTool) Execute(_ context.Context, input string) core.ToolResult {
	var in textStatsInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", textStatsToolName, err)}
	}
	if in.Text == "" {
		return core.ToolResult{Error: fmt.Sprintf("%s 缺必填參數 text", textStatsToolName)}
	}

	out := textStatsOutput{
		Runes: utf8.RuneCountInString(in.Text),
		Words: len(strings.Fields(in.Text)),
	}
	content, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("編碼 %s 結果: %v", textStatsToolName, err)}
	}
	return core.ToolResult{OK: true, Content: string(content)}
}
