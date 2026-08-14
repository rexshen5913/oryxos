package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestTextStatsExecute 是原生 Go Tool 示例的行為矩陣。示例的整條鏈路（LLM 真的呼叫
// 到、結果回填、落審計表）由 internal/core/agent_plugin_tool_test.go 從既有 seam 驗；
// 這裡只釘住它自己算得對、以及各種壞輸入回的是**錯誤回填**而不是 panic。
func TestTextStatsExecute(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantContent string // 非空表示期望成功
		wantErrPart string // 非空表示期望失敗，且錯誤訊息含此片段
	}{
		{
			name:        "純英文",
			input:       `{"text":"hello world"}`,
			wantContent: `{"runes":11,"words":2}`,
		},
		{
			// 中日韓字元一個算一個 rune，不是 len() 的 byte 數——這是這個示例
			// 唯一有內容的一行邏輯，值得釘死。
			name:        "多位元組字元",
			input:       `{"text":"OryxOS 是 Agent OS"}`,
			wantContent: `{"runes":17,"words":4}`,
		},
		{
			name:        "連續空白與換行只算一個分隔",
			input:       "{\"text\":\"a  b\\n\\tc\"}",
			wantContent: `{"runes":7,"words":3}`,
		},
		{
			name:        "非 JSON 輸入",
			input:       `not json`,
			wantErrPart: "解析 text_stats 輸入參數",
		},
		{
			// 缺欄位與空值走同一條錯誤，與 http.go 的 url 參數同形——樣板要跟內建
			// Tool 一致，照抄的人才不會學到第二套語義。
			name:        "缺 text 欄位",
			input:       `{}`,
			wantErrPart: "缺必填參數 text",
		},
		{
			name:        "text 為空字串",
			input:       `{"text":""}`,
			wantErrPart: "缺必填參數 text",
		},
	}

	tl := tool.NewTextStatsTool()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tl.Execute(context.Background(), tt.input)
			if tt.wantErrPart != "" {
				if got.OK {
					t.Fatalf("期望失敗, 實際成功: %+v", got)
				}
				if !strings.Contains(got.Error, tt.wantErrPart) {
					t.Errorf("錯誤 = %q, 期望含 %q", got.Error, tt.wantErrPart)
				}
				return
			}
			if !got.OK {
				t.Fatalf("期望成功, 實際失敗: %+v", got)
			}
			if got.Content != tt.wantContent {
				t.Errorf("結果 = %q, 期望 %q", got.Content, tt.wantContent)
			}
		})
	}
}

// TestTextStatsIsRepeatable 釘住同一個輸入重複呼叫結果一致——會抓到「示例被改成帶
// 狀態」這一類退化。
//
// 但它**不是**「不依賴時間或隨機值」的完整證據（那種相依可能是秒級的，連跑幾次看不
// 出來）。那件事真正的外部證據是 internal/core 的整合測試：除了 LLM 回放 fixture 之外
// 不需要任何東西就跑得起來，不像 HTTP Tool 的測試還得再起一個目標端點。
func TestTextStatsIsRepeatable(t *testing.T) {
	tl := tool.NewTextStatsTool()
	const input = `{"text":"OryxOS 是 Agent OS"}`
	first := tl.Execute(context.Background(), input)
	for i := 2; i <= 4; i++ {
		if again := tl.Execute(context.Background(), input); again != first {
			t.Fatalf("第 %d 次結果 = %+v, 與第一次 %+v 不同", i, again, first)
		}
	}
}

// TestTextStatsToolDeclaration 釘住送往 LLM 的宣告三件事都填了——名稱、描述、
// 輸入 schema 任一為空或不合法，LLM 就不會（或無法正確）呼叫它。
func TestTextStatsToolDeclaration(t *testing.T) {
	tl := tool.NewTextStatsTool()
	if tl.Name() != "text_stats" {
		t.Errorf("Name() = %q, 期望 text_stats", tl.Name())
	}
	if tl.Description() == "" {
		t.Error("Description() 為空")
	}
	if !json.Valid(tl.InputSchema()) {
		t.Errorf("InputSchema() 不是合法 JSON: %s", tl.InputSchema())
	}
}
