// 原生 Go Tool 示例（Plugin Tool 方式三）的整合測試：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——ToolRegistry 與
// SQLite 在 seam 之下用真的。
//
// 這裡刻意連 helper 都與內建 Tool 共用（newToolAgentOn、queryToolInvocations、
// assertTimestamps 全是既有的那一組）：本票要證明的正是「方式三的寫法與內建 Tool 完全
// 一樣」，若示例需要另一套組裝或另一套斷言才驗得起來，那句話就不成立。
//
// 示例本身不連任何外部依賴，所以這裡不像 HTTP Tool 的測試那樣要再起一個目標端點
// ——除了 LLM 回放 fixture 之外不需要任何東西，這正是 ticket 要的「行為可確定化」。
package core_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestProcessNativeGoToolScenario 是本票的主場景：LLM 決定呼叫原生 Go Tool 示例，
// 它**真的被執行到**、結果回填進對話，並落 tool_invocations 審計表。
func TestProcessNativeGoToolScenario(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_text_stats_tool_call.json"),
		readFixture(t, "reply_text_stats_final.json"),
	)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"text_stats"}, nil,
		discardLogger(), db, tool.NewTextStatsTool())
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "「OryxOS 是 Agent OS」這句有幾個字？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	const wantReply = "「OryxOS 是 Agent OS」共 17 個字元、4 個詞。"
	if resp != wantReply {
		t.Errorf("回應 = %q, 期望 %q", resp, wantReply)
	}

	// 結果回填：user → assistant(tool_calls) → tool → assistant。tool 訊息的內容是
	// 示例真的算出來的那一份，斷言到確切數值——「有一列 tool 訊息」不足以證明它被
	// 執行過，回填一句固定字串也能通過。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "text_stats" {
		t.Errorf("messages[1] 應為帶 text_stats tool_calls 的 assistant 訊息: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != "call_text_stats_1" {
		t.Errorf("messages[2] 應為回應 call_text_stats_1 的 tool 訊息: %+v", msgs[2])
	}
	const wantContent = `{"runes":17,"words":4}`
	if msgs[2].Content != wantContent {
		t.Errorf("tool 結果 = %q, 期望 %q", msgs[2].Content, wantContent)
	}

	// 落 tool_invocations——欄位與斷言逐條對齊 agent_audit_test.go 對 http_get 的那組
	// （含核心階段 token_cost 一律 NULL 那條），證明「寫法完全一樣」不是口號。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	inv := invocations[0]
	if inv.sessionID != session.ID {
		t.Errorf("tool_invocations session_id = %q, 期望 %q", inv.sessionID, session.ID)
	}
	if inv.profileName != "default" || inv.toolName != "text_stats" {
		t.Errorf("profile_name／tool_name = %s／%s, 期望 default／text_stats", inv.profileName, inv.toolName)
	}
	if !strings.Contains(inv.parameters, "OryxOS 是 Agent OS") {
		t.Errorf("parameters 未落庫呼叫參數: %q", inv.parameters)
	}
	if inv.status != "completed" {
		t.Errorf("status = %q, 期望 completed", inv.status)
	}
	if !inv.result.Valid || inv.result.String != wantContent {
		t.Errorf("result 未落庫 Tool 結果: %+v, 期望 %q", inv.result, wantContent)
	}
	if inv.errText.Valid && inv.errText.String != "" {
		t.Errorf("成功的呼叫不該有 error: %q", inv.errText.String)
	}
	assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)
	if inv.tokenCost.Valid {
		t.Errorf("token_cost = %d, 期望 NULL（核心階段一律不歸因）", inv.tokenCost.Int64)
	}
}

// TestProcessNativeGoToolFilteredByProfile 驗證原生 Go Tool 受 Profile 的 tools 欄位
// 過濾，與內建 Tool 一視同仁。
//
// 這與 agent_tool_test.go 的 TestProcessToolsFilteredByProfile 不重複：那邊的 Registry
// 裡**根本沒有** text_stats，證不了「註冊了但沒列到就用不到」。這裡三個工具全都註冊在
// 同一個 Registry，所以第二格（只列內建 Tool）才是示例的反向：它在場、但沒被列到，
// 就不該出現在 LLM 的工具清單。只驗正向的話，完全不過濾也會通過。
func TestProcessNativeGoToolFilteredByProfile(t *testing.T) {
	tests := []struct {
		name  string
		tools []string
	}{
		{name: "只列原生 Go Tool 示例", tools: []string{"text_stats"}},
		{name: "只列內建 Tool", tools: []string{"http_get"}},
		{name: "兩者都列", tools: []string{"text_stats", "http_get"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
			agent := newToolAgentOn(t, srv.URL, testProfile(), tt.tools, nil,
				discardLogger(), newStore(t), tool.NewTextStatsTool())
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			if got := declaredToolNames(t, llmReqs, 0); !slices.Equal(got, tt.tools) {
				t.Errorf("送往 LLM 的工具清單 = %v, 期望 %v", got, tt.tools)
			}
		})
	}
}
