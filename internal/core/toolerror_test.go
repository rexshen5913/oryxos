// Tool 錯誤類型 → 給 LLM 的指引，這條映射的單元測試（ticket #51）。
//
// 它是**純函式**——不碰檔案、不碰網路、輸出只由類型決定——所以直接測，不必繞 seam；
// 「指引真的到得了 LLM、且沒進審計」由 agent_tool_error_test.go 經 seam 覆蓋。
//
// 純函式這個形狀正是本票與「散落在各處的錯誤字串」最大的差別：字串散在二十個 Tool
// 裡沒有辦法整批檢查，收成一張表就可以（issue #38 第二項）。
package core_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestToolErrorKindGuidance 是類型 → 指引的主表：七種類型各一格，加上未分類零值。
//
// **斷言的是「有沒有指引」而不是措辭**：措辭要能隨真實觀測調整（issue #36 那句
// 「不要逐一嘗試」就是這樣長出來的），把整段文字寫進斷言會讓每次改進都得改測試。
// 有意義且不隨措辭漂移的性質有兩條，兩條都在下面釘住：
//
//  1. 哪些類型有指引、哪些沒有（本表的 wantGuidance 欄）
//  2. 有指引的類型，指引**彼此不同**（見 TestToolErrorKindGuidanceDistinct）——
//     全部複製同一段話也能通過第 1 條，那等於沒有分類
func TestToolErrorKindGuidance(t *testing.T) {
	tests := []struct {
		name string
		kind core.ToolErrorKind
		// wantGuidance 為 false 代表這一類**沒有通用指引**，回空字串。
		wantGuidance bool
	}{
		{
			// 零值：本票之前的每一個失敗都落在這裡，回填內容必須與本票之前逐位元組
			// 相同，所以指引必須是空字串。這一格是遷移安全性的最小單位。
			name:         "未分類（零值）",
			kind:         core.ToolErrorUnclassified,
			wantGuidance: false,
		},
		{
			// sandbox 回空字串是**決策**，不是漏寫；理由見 ToolErrorSandbox 的說明。
			name:         "sandbox：白名單拒絕",
			kind:         core.ToolErrorSandbox,
			wantGuidance: false,
		},
		{name: "not_found：目標不存在", kind: core.ToolErrorNotFound, wantGuidance: true},
		{name: "invalid_args：參數錯誤", kind: core.ToolErrorInvalidArgs, wantGuidance: true},
		{name: "permission：作業系統層無權限", kind: core.ToolErrorPermission, wantGuidance: true},
		{name: "timeout：逾時", kind: core.ToolErrorTimeout, wantGuidance: true},
		{name: "upstream：對端故障", kind: core.ToolErrorUpstream, wantGuidance: true},
		{name: "limit_reached：已達上限", kind: core.ToolErrorLimitReached, wantGuidance: true},
		{
			// 型別範圍外的值：日後有人加了常數卻忘了寫指引，或呼叫端傳了髒資料。
			// 要求是**安靜地回空字串**——不 panic、也不附一段看起來像指引的預設話。
			name:         "型別範圍外的值（防禦）",
			kind:         core.ToolErrorKind(99),
			wantGuidance: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.kind.Guidance()
			if hasGuidance := got != ""; hasGuidance != tt.wantGuidance {
				t.Errorf("Guidance() 非空 = %v, 期望 %v；實際內容: %q", hasGuidance, tt.wantGuidance, got)
			}
			// 指引是要接在原始錯誤後面送給 LLM 的一段話，前後不該帶空白——組裝點
			// 自己會加分隔，這裡再帶就會出現多餘的空行。
			if got != strings.TrimSpace(got) {
				t.Errorf("Guidance() 前後帶空白: %q", got)
			}
		})
	}
}

// TestToolErrorKindGuidanceDistinct 釘住「有指引的類型，指引彼此不同」。
//
// 沒有這一格，把同一段萬用話複製給每一個類型也能讓上一支測試全綠——而那正是本票要
// 取代的東西（一段對所有失敗都適用的話，等於對每一種失敗都沒說到重點）。
func TestToolErrorKindGuidanceDistinct(t *testing.T) {
	kinds := []core.ToolErrorKind{
		core.ToolErrorNotFound, core.ToolErrorInvalidArgs, core.ToolErrorPermission,
		core.ToolErrorTimeout, core.ToolErrorUpstream, core.ToolErrorLimitReached,
	}
	seen := make(map[string]core.ToolErrorKind, len(kinds))
	for _, k := range kinds {
		g := k.Guidance()
		if prev, dup := seen[g]; dup {
			t.Errorf("類型 %d 與 %d 的指引完全相同——分類沒有帶來任何差別: %q", k, prev, g)
		}
		seen[g] = k
	}
}
