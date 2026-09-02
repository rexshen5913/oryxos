package eval_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// TestCheckAuditComplete 釘住「評測不得以已知不完整的審計資料背書」。
//
// **為什麼這條特別重要**：本票新增的兩種斷言都是上限（「不得超過 N」）。審計漏記一筆
// 的方向是讓實際值**變小**，而變小的實際值更容易通過上限——漏記會產生綠燈。
//
// 這與既有的 `tool_called` 方向正好相反：那條斷言在資料漏列時判**失敗**，人會去查。
// 一條因為資料不全而變綠的斷言則不會有人去查，而評測宣稱了一個它根本沒驗證的性質。
func TestCheckAuditComplete(t *testing.T) {
	tests := []struct {
		name      string
		lost      uint64
		wantErr   bool
		wantParts []string // 錯誤訊息裡該出現的片段
	}{
		{
			name: "一筆都沒漏就放行",
			lost: 0,
		},
		{
			name:      "漏一筆就中止",
			lost:      1,
			wantErr:   true,
			wantParts: []string{"1"},
		},
		{
			// 筆數要出現在訊息裡：漏 1 筆與漏 200 筆是完全不同的處置，前者可能是
			// 一次偶發的寫入失敗，後者代表資料庫或佇列有系統性問題。
			name:      "漏很多筆時要說出是幾筆",
			lost:      200,
			wantErr:   true,
			wantParts: []string{"200"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := eval.CheckAuditComplete(tt.lost)
			if tt.wantErr == (err == nil) {
				t.Fatalf("CheckAuditComplete(%d) = %v，期望錯誤 = %v", tt.lost, err, tt.wantErr)
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("錯誤訊息 = %q，期望含 %q", err, want)
				}
			}
		})
	}
}
