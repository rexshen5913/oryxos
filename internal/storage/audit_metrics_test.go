package storage

import (
	"context"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestMetricsForSession 表格驅動涵蓋「這一場 Session 的指標長什麼樣」。
//
// 記錄一律經**真正的寫入路徑**（RecordLLMCall／RecordToolInvocation ＋ Flush）落庫，
// 不直接 INSERT：評測讀到的數字必須就是 Agent 執行時真的寫進去的數字，自己 INSERT
// 一份等於在測一個與產品無關的假設（與 TestToolNamesForSession 同一條理由）。
func TestMetricsForSession(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Second) }

	// cost 讓表格寫得出「這一筆有算成本」。
	cost := func(v int64) *int64 { return &v }

	// call 是一筆 llm_calls 記錄的簡寫。
	call := func(session string, total int, c *int64, offset int) core.LLMCall {
		return core.LLMCall{
			SessionID:    session,
			Provider:     "openai",
			Model:        "gpt-4o-mini",
			Usage:        core.TokenUsage{PromptTokens: total, TotalTokens: total},
			Status:       core.AuditStatusCompleted,
			StartedAt:    at(offset),
			CompletedAt:  at(offset),
			CostMicroUSD: c,
		}
	}
	// inv 是一筆 tool_invocations 記錄的簡寫。
	inv := func(session, status string, offset int) core.ToolInvocation {
		return core.ToolInvocation{
			SessionID:   session,
			ProfileName: "default",
			ToolName:    "shell",
			Parameters:  `{}`,
			Status:      status,
			StartedAt:   at(offset),
			CompletedAt: at(offset),
		}
	}

	tests := []struct {
		name      string
		calls     []core.LLMCall
		invs      []core.ToolInvocation
		sessionID string
		want      SessionMetrics
	}{
		{
			name:      "一筆記錄都沒有",
			sessionID: "s1",
			// 成本是 nil 而不是 0：一次都沒呼叫過與「呼叫了但不用錢」是兩件事，
			// 這條與 ticket #49「沒算不寫零」是同一條語義。
			want: SessionMetrics{},
		},
		{
			name: "iteration 數就是 Provider 呼叫次數",
			calls: []core.LLMCall{
				call("s1", 10, nil, 0),
				call("s1", 20, nil, 1),
				call("s1", 30, nil, 2),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 3, TotalTokens: 60},
		},
		{
			name: "失敗的 Provider 呼叫一樣算一次 iteration",
			calls: []core.LLMCall{
				call("s1", 10, nil, 0),
				func() core.LLMCall {
					c := call("s1", 0, nil, 1)
					c.Status = core.AuditStatusFailed
					return c
				}(),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 2, TotalTokens: 10},
		},
		{
			name: "Tool 失敗數只數失敗與逾時，成功不算",
			invs: []core.ToolInvocation{
				inv("s1", core.AuditStatusCompleted, 0),
				inv("s1", core.AuditStatusFailed, 1),
				inv("s1", core.AuditStatusTimeout, 2),
				inv("s1", core.AuditStatusFailed, 3),
			},
			sessionID: "s1",
			want:      SessionMetrics{ToolFailures: 3},
		},
		{
			name: "全部呼叫都有定價時成本加總",
			calls: []core.LLMCall{
				call("s1", 10, cost(1200), 0),
				call("s1", 20, cost(3400), 1),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 2, TotalTokens: 30, CostMicroUSD: cost(4600)},
		},
		{
			// **只有部分算得出來時，總額是 nil 而不是那個部分和。** 一個少算了一半的
			// 數字比沒有數字更糟——它看起來像事實，讀的人不會去追它漏了什麼。
			name: "只有部分呼叫算得出成本時總額為空",
			calls: []core.LLMCall{
				call("s1", 10, cost(1200), 0),
				call("s1", 20, nil, 1),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 2, TotalTokens: 30},
		},
		{
			name: "完全沒配置定價時成本為空",
			calls: []core.LLMCall{
				call("s1", 10, nil, 0),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 1, TotalTokens: 10},
		},
		{
			name: "別場 Session 的記錄一律不算",
			calls: []core.LLMCall{
				call("s1", 10, cost(1000), 0),
				call("s2", 999, cost(999000), 1),
			},
			invs: []core.ToolInvocation{
				inv("s1", core.AuditStatusFailed, 0),
				inv("s2", core.AuditStatusFailed, 1),
			},
			sessionID: "s1",
			want:      SessionMetrics{Iterations: 1, ToolFailures: 1, TotalTokens: 10, CostMicroUSD: cost(1000)},
		},
		{
			name: "查一個沒有任何記錄的 Session",
			calls: []core.LLMCall{
				call("s1", 10, cost(1000), 0),
			},
			sessionID: "查無此人",
			want:      SessionMetrics{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			audit, db, _ := newTestAudit(t, discardLogger())
			for _, c := range tt.calls {
				audit.RecordLLMCall(ctx, c)
			}
			for _, i := range tt.invs {
				audit.RecordToolInvocation(ctx, i)
			}
			// 審計寫入是背景旁路：不先排空就查，讀到的可能是還沒落庫的空表。
			if err := audit.Flush(ctx); err != nil {
				t.Fatalf("排空審計佇列: %v", err)
			}

			got, err := db.MetricsForSession(ctx, tt.sessionID)
			if err != nil {
				t.Fatalf("MetricsForSession: %v", err)
			}
			if got.Iterations != tt.want.Iterations {
				t.Errorf("Iterations = %d，期望 %d", got.Iterations, tt.want.Iterations)
			}
			if got.ToolFailures != tt.want.ToolFailures {
				t.Errorf("ToolFailures = %d，期望 %d", got.ToolFailures, tt.want.ToolFailures)
			}
			if got.TotalTokens != tt.want.TotalTokens {
				t.Errorf("TotalTokens = %d，期望 %d", got.TotalTokens, tt.want.TotalTokens)
			}
			switch {
			case tt.want.CostMicroUSD == nil && got.CostMicroUSD != nil:
				t.Errorf("CostMicroUSD = %d，期望空值（沒算 ≠ 不用錢）", *got.CostMicroUSD)
			case tt.want.CostMicroUSD != nil && got.CostMicroUSD == nil:
				t.Errorf("CostMicroUSD 是空值，期望 %d", *tt.want.CostMicroUSD)
			case tt.want.CostMicroUSD != nil && *got.CostMicroUSD != *tt.want.CostMicroUSD:
				t.Errorf("CostMicroUSD = %d，期望 %d", *got.CostMicroUSD, *tt.want.CostMicroUSD)
			}
		})
	}
}
