package storage

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestToolNamesForSession 表格驅動涵蓋「這一場 Session 呼叫過哪些 Tool」的各種形狀。
//
// 記錄一律經**真正的寫入路徑**（RecordToolInvocation ＋ Flush）落庫，不直接 INSERT：
// 評測讀到的東西必須就是 Agent 執行時真的寫進去的東西，自己 INSERT 一份等於在測一個
// 與產品無關的假設。
func TestToolNamesForSession(t *testing.T) {
	// at 讓每筆記錄有明確的先後，用來驗證回傳順序。
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Second) }

	tests := []struct {
		name      string
		records   []core.ToolInvocation
		sessionID string
		want      []string
	}{
		{
			name:      "一筆記錄都沒有",
			sessionID: "s1",
			want:      nil,
		},
		{
			name: "呼叫過一個 Tool",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(0)},
			},
			sessionID: "s1",
			want:      []string{"read_file"},
		},
		{
			name: "同一個 Tool 呼叫多次只回一次",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(0)},
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(1)},
			},
			sessionID: "s1",
			want:      []string{"read_file"},
		},
		{
			name: "多個 Tool 依首次呼叫的先後回傳",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "list_dir", Status: core.AuditStatusCompleted, StartedAt: at(0)},
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(1)},
				{SessionID: "s1", ToolName: "list_dir", Status: core.AuditStatusCompleted, StartedAt: at(2)},
			},
			sessionID: "s1",
			want:      []string{"list_dir", "read_file"},
		},
		{
			name: "失敗的呼叫也算呼叫過",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "shell", Status: core.AuditStatusFailed, Error: "被白名單拒絕", StartedAt: at(0)},
			},
			sessionID: "s1",
			want:      []string{"shell"},
		},
		{
			name: "別場 Session 的記錄不算",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(0)},
				{SessionID: "s2", ToolName: "shell", Status: core.AuditStatusCompleted, StartedAt: at(1)},
			},
			sessionID: "s1",
			want:      []string{"read_file"},
		},
		{
			name: "查一個沒有任何記錄的 Session",
			records: []core.ToolInvocation{
				{SessionID: "s1", ToolName: "read_file", Status: core.AuditStatusCompleted, StartedAt: at(0)},
			},
			sessionID: "查無此人",
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			audit, db, _ := newTestAudit(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
			for _, rec := range tt.records {
				audit.RecordToolInvocation(ctx, rec)
			}
			// 審計寫入是背景旁路：不先排空就查，讀到的可能是還沒落庫的空表。
			if err := audit.Flush(ctx); err != nil {
				t.Fatalf("排空審計佇列: %v", err)
			}

			got, err := db.ToolNamesForSession(ctx, tt.sessionID)
			if err != nil {
				t.Fatalf("ToolNamesForSession: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ToolNamesForSession(%q) = %v，期望 %v", tt.sessionID, got, tt.want)
			}
		})
	}
}
