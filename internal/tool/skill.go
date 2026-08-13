package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rexshen5913/oryxos/internal/core"
)

// LoadSkillToolName 是漸進揭露第二層那個內建 Tool 的名字。
//
// 值定義在 core（那裡的 Skill 段引言與每 turn 的一致性檢查都要用到它，而 core 不能
// 反向 import 本 package）；這裡只是別名，讓組裝點與本 package 的讀者不必跳過去。
// **單一來源**：三處都指向同一個常數，改一邊漏一邊不會發生。
const LoadSkillToolName = core.LoadSkillToolName

// LoadSkillTool 是內建 Tool load_skill：取回一份 Skill 的**正文**，以 tool 訊息回填
// 進對話。這是漸進揭露的第二層——第一層只把 name 與 description 常駐在 system
// prompt，Agent 判斷某份相關時才用這個 Tool 把正文拿進來。
//
// **它取回的是指令文字，不是「執行 Skill」。** Skill 沒有 execute、沒有步驟引擎，
// OryxOS 不解析任務步驟——正文交給 LLM 閱讀並自行決定怎麼做（技術方案 §6.3）。
// `CONTEXT.md` 那條「Skill 不是可執行的 Tool」因此仍然成立：可執行的是這個取回
// 動作，不是 Skill 本身。
//
// 歸屬要分開讀：Skill 的**載入與解析**歸上下文加載那一層（實作落 internal/config）；
// 而 load_skill 作為一個 OryxTool 實作，本身就該落 internal/tool，與其他內建 Tool
// 同形——不為它拆 package（技術方案 §6、AI 編程指南 §3 第 3 點）。
type LoadSkillTool struct {
	loader core.ContextLoader
	// allowed 是這個 Profile 引用的 Skill 名單。LLM 送來的參數只能落在其中——
	// 檔案系統上有的 Skill 不代表這個 Agent 可以讀，否則 Profile 的 skills 欄位
	// 就只是個建議，多 Agent 之間的隔離也就沒有了。
	allowed []string
}

// NewLoadSkillTool 以上下文載入器與這個 Profile 引用的 Skill 名單建立 load_skill。
// allowed 為空時 Tool 仍可註冊（使用者可能在 tools 顯式列了它），呼叫時回明確的
// 錯誤回填——那比啟動失敗好：一個沒有 Skill 的 Agent 沒理由起不來。
func NewLoadSkillTool(loader core.ContextLoader, allowed []string) *LoadSkillTool {
	return &LoadSkillTool{loader: loader, allowed: allowed}
}

func (t *LoadSkillTool) Name() string { return LoadSkillToolName }

// Description 要講清楚兩件事：什麼時候該用，以及**回來的是說明不是結果**。少了
// 後者，模型容易把「載入 Skill」誤當成「這件事已經被做掉了」。
func (t *LoadSkillTool) Description() string {
	return "取回一份 Skill 的完整說明。系統提示詞裡只列出每份 Skill 的名稱與用途；" +
		"判斷某份與當前任務相關時，用這個 Tool 把它的正文拿回來再照著做。" +
		"回傳的是**任務說明文字**，不是執行結果——拿到之後仍要由你自己決定每一步怎麼做。"
}

func (t *LoadSkillTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "要取回的 Skill 名稱，就是系統提示詞裡列出的那個名字"}
		},
		"required": ["name"]
	}`)
}

// loadSkillInput 是 load_skill 的輸入參數。
type loadSkillInput struct {
	Name string `json:"name"`
}

// Execute 取回指定 Skill 的正文。
//
// 一切失敗都以 ToolResult.Error 回填、**不中斷 turn**（沿 spec #1 既有的 Tool 失敗
// 語義）：參數錯、名字不在引用範圍、檔案啟動後被刪或改壞，都讓 LLM 看到原因後換
// 一條路回覆使用者，比整個 turn 崩掉有用。
//
// 全部標 Retryable: false：這些都不是瞬時故障——參數與設定問題重試幾次都一樣，
// 本機檔案的權限與路徑問題也不會自己好。錯誤訊息本身就是給模型的修改指示。
func (t *LoadSkillTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in loadSkillInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 load_skill 輸入參數: %v", err)}
	}
	if in.Name == "" {
		return core.ToolResult{Error: "load_skill 需要 name 參數：請指定要取回哪一份 Skill。"}
	}
	if len(t.allowed) == 0 {
		return core.ToolResult{Error: fmt.Sprintf(
			"這個 Profile 沒有宣告任何 Skill，載不到 %q。請直接依你既有的知識回答使用者。", in.Name)}
	}
	if !slices.Contains(t.allowed, in.Name) {
		return core.ToolResult{Error: fmt.Sprintf(
			"這個 Profile 沒有引用名為 %q 的 Skill；可用的是：%s。",
			in.Name, strings.Join(t.allowed, "、"))}
	}

	body, err := t.loader.SkillBody(ctx, in.Name)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("load_skill 取回 %q 的正文失敗: %v", in.Name, err)}
	}
	return core.ToolResult{OK: true, Content: body}
}
