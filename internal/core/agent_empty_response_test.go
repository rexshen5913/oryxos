// Provider 回傳空的最終回應時的整合測試（issue #60）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Session 儲存與審計
// 表在 seam 之下用真的（憲法 4.3）。
//
// fixture reply_empty.json 的用量數字（791／4／795）逐字取自 issue #60 記錄的那次
// 真實事故。**completion_tokens 是 4 而不是 0**，這一點是本票的前提：Provider 確實
// 產出了幾個 token，只是搬進 content 之後是空的。錄成 0 會讓 fixture 變成另一種
// 情況（模型什麼都沒產出），與被回報的形態不同。
//
// 斷言落在三個外部產物上：
//
//   - **Process 的回傳值**：空字串絕不能是一個「最終回應」
//   - **Session 的對話歷史**：空的 assistant 訊息不得進入歷史（它會被原樣重送）
//   - **llm_calls 的筆數**：守衛要真的停下來，而不是一路燒到迭代上限
package core_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// emptyResponseThreshold 是連續空回應的容忍上限。
//
// 寫成常數而不是從 core 取，理由同 loopGuardThreshold：那個值是未匯出的，為了測試
// 把它匯出會為了一個數字擴大 API 表面。代價是兩者之間沒有編譯期關聯——所以下面每支
// 測試的錄製回應筆數都跟著這個數字排，改了生產值時這裡會以「該停卻沒停」或「回應
// 數超出錄製數」的形式紅掉，而不是安靜地測到別的東西。
const emptyResponseThreshold = 3

// emptyResponseLogKey 是空回應時落的結構化日誌事件鍵。
const emptyResponseLogKey = "provider_empty_response"

// emptyFixtures 產生 n 份空回應，後面接上 tail 指定的 fixture。
func emptyFixtures(t *testing.T, n int, tail ...string) []string {
	t.Helper()
	out := make([]string, 0, n+len(tail))
	for range n {
		out = append(out, readFixture(t, "reply_empty.json"))
	}
	for _, name := range tail {
		out = append(out, readFixture(t, name))
	}
	return out
}

// countLLMCalls 數 llm_calls 的資料列數，也就是這一輪對 Provider 呼叫了幾次。
//
// 用審計表而不是回放伺服器的內部計數：那是 evals 的 max_iterations 斷言真正讀的
// 東西（internal/eval 的 Iterations 就是 COUNT(*) FROM llm_calls），拿同一個來源
// 斷言，這裡綠了評測那邊才數得出一樣的數字。
func countLLMCalls(t *testing.T, dbPath string) int {
	t.Helper()
	var n int
	eachRow(t, dbPath, `SELECT COUNT(*) FROM llm_calls`, func(rows *sql.Rows) {
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("掃描 llm_calls 筆數: %v", err)
		}
	})
	return n
}

// TestProcessRetriesEmptyResponseUntilItRecovers 驗空回應不是最終回應：循環再問一次，
// 模型補上內容之後那個內容才是答案。
//
// 兩格的差別只有空回應的次數，用來釘住**門檻是 3 而不是 2**——只錄一格的話，把門檻
// 改成 2 也照樣全綠。
//
// 每格都一併驗**空訊息沒有進歷史**：issue #60 列的第二項代價就是它會被原樣重送給
// Provider，而一則空的 assistant 訊息對後續推理沒有任何貢獻，只是佔位。
func TestProcessRetriesEmptyResponseUntilItRecovers(t *testing.T) {
	tests := []struct {
		name    string
		empties int
	}{
		{name: "一次空回應後恢復", empties: 1},
		{name: "兩次空回應後恢復（門檻是 3，不是 2）", empties: emptyResponseThreshold - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, emptyFixtures(t, tt.empties, "reply_direct.json")...)

			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			db := openStore(t, dbPath)
			// 借用 newCostAgentOn：它是唯一讓呼叫端指定**引擎 logger** 的組裝輔助
			// （prices 傳 nil 即沒有定價表），本檔要從那個 logger 斷言降級日誌。
			agent := newCostAgentOn(t, srv.URL, nil, discardLogger(), db)
			session := activeSession(t, db.sessions())

			resp, err := agent.Process(context.Background(), session, "你好")
			if err != nil {
				t.Fatalf("空回應是暫時性故障，重問之後拿到內容就不該讓 turn 失敗: %v", err)
			}
			if !strings.Contains(resp, "我是 Oryx") {
				t.Errorf("回應 = %q, 期望是恢復之後那一輪的內容", resp)
			}

			// 歷史只有 user 與恢復後的 assistant 兩則：空回應那幾輪一則都不留。
			msgs := session.Messages
			if len(msgs) != 2 {
				t.Fatalf("歷史長度 = %d, 期望 2（user ＋ 恢復後的 assistant）: %+v", len(msgs), msgs)
			}
			for i, m := range msgs {
				if m.Role == core.RoleAssistant && m.Content == "" && len(m.ToolCalls) == 0 {
					t.Errorf("messages[%d] 是一則空的 assistant 訊息——它會在下一個 turn 被原樣重送", i)
				}
			}

			// 每一次空回應都真的花了一次 Provider 呼叫，審計要記得到——那些 token
			// 已經被計費了。
			db.flush(t)
			if got, want := countLLMCalls(t, dbPath), tt.empties+1; got != want {
				t.Errorf("llm_calls 筆數 = %d, 期望 %d（%d 次空回應＋1 次恢復）", got, want, tt.empties)
			}
		})
	}
}

// TestProcessGivesUpAfterConsecutiveEmptyResponses 驗連續空回應達門檻時**明確告知**，
// 而不是回一個空字串或悄悄燒完迭代上限。
//
// testProfile() 的 max_iterations 是 10，所以「停在第 3 次」本身就是斷言的一部分：
// 停在 10 代表守衛沒有生效、只是被迭代上限接住，而那會白花 7 次 Provider 呼叫，
// 並且回一句「仍未完成任務」——把原因指向錯的地方。
func TestProcessGivesUpAfterConsecutiveEmptyResponses(t *testing.T) {
	srv := newReplayServer(t, emptyFixtures(t, emptyResponseThreshold)...)

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	var logBuf bytes.Buffer
	agent := newCostAgentOn(t, srv.URL, nil, slog.New(slog.NewJSONHandler(&logBuf, nil)), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "你好")
	// 與「已達最大迭代次數」同一類：**不是錯誤**，是一則告知使用者的回覆。已經做過
	// 的事留在歷史裡，turn 照常持久化。
	if err != nil {
		t.Fatalf("連續空回應是告知、不是硬錯誤（比照迭代上限的既有處置）: %v", err)
	}
	if resp == "" {
		t.Fatal("回應是空字串——這正是 issue #60 回報的現象：使用者看到一片空白")
	}
	// 斷言到措辭是刻意的，理由同 loopGuardNoticeMark：這句話本身就是交付物。使用者
	// 要能從它看出「不是模型答不出來，是 Provider 回了空的」，否則他只會覺得 Agent
	// 壞了。
	if want := fmt.Sprintf("連續 %d 次回傳空回應", emptyResponseThreshold); !strings.Contains(resp, want) {
		t.Errorf("回應 = %q, 期望說明原因並含 %q", resp, want)
	}

	// 停在門檻，不是停在迭代上限。
	db.flush(t)
	if got := countLLMCalls(t, dbPath); got != emptyResponseThreshold {
		t.Errorf("llm_calls 筆數 = %d, 期望 %d——停在門檻，不該一路燒到 max_iterations（10）",
			got, emptyResponseThreshold)
	}

	// 歷史是 user ＋ 那則告知，空回應一則都沒留。
	msgs := session.Messages
	if len(msgs) != 2 {
		t.Fatalf("歷史長度 = %d, 期望 2（user ＋ 告知）: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || msgs[1].Content != resp {
		t.Errorf("messages[1] = %+v, 期望就是回傳的那則告知", msgs[1])
	}

	// 維運要查得到。事件流不加第八種（那組取值是 spec #5 的對外定案），降級訊號走
	// 結構化日誌，與 context_compacted、tool_loop_guard_tripped 同一條路。
	logs := logBuf.String()
	if !strings.Contains(logs, emptyResponseLogKey) {
		t.Errorf("未落空回應的降級日誌: %s", logs)
	}
}

// TestToolCallRoundDoesNotCountAsEmptyResponse 是本票**最容易寫錯的那一格**。
//
// 要呼叫 Tool 的那一輪，content 本來就是空字串（見 reply_list_dir_tool_call.json）
// ——判斷條件若只看 `resp.Content == ""`，每一次 Tool 呼叫都會被當成空回應，整個
// ReAct 循環直接壞掉。條件必須是「空內容**且**無 tool call」。
//
// 回應序列刻意排成 空 → tool call → 空 → 空 → 最終回應，一次驗兩件事：
//
//   - **tool call 那輪不計數**：若計數，到第二個空就是第 3 次，會提早放棄
//   - **計數在非空回應後歸零**：若不歸零，1＋2＋3 同樣會在最後一個空上放棄
//
// 兩種寫錯都會讓這一格紅，而正確的實作會走完並拿到最終回應。歸零的語義與
// loopguard.go 的「任一次成功清空整張表」一致：偵測的是「Provider 現在卡住了」，
// 那本來就是個連續性質。
func TestToolCallRoundDoesNotCountAsEmptyResponse(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_empty.json"),
		readFixture(t, "reply_list_dir_tool_call.json"),
		readFixture(t, "reply_empty.json"),
		readFixture(t, "reply_empty.json"),
		readFixture(t, "reply_direct.json"),
	)
	root, dir := newTestWorkspace(t)
	seedListDirWorkspace(t, dir)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentIn(t, srv.URL, testProfile(), []string{tool.ListDirToolName},
		tool.SandboxConfig{AllowedPaths: []string{"notes"}}, root, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "notes 裡有什麼？")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(resp, "我是 Oryx") {
		t.Errorf("回應 = %q, 期望走完整個序列拿到最終回應——"+
			"提早放棄代表 tool call 那輪被誤計成空回應，或計數沒有在它之後歸零", resp)
	}

	// 歷史：user → assistant(list_dir) → tool → assistant(最終)。四則，空回應那三輪
	// 一則都不留，而 tool 呼叫與其結果照樣成對出現。
	msgs := session.Messages
	if len(msgs) != 4 {
		t.Fatalf("歷史長度 = %d, 期望 4（user、帶 tool_calls 的 assistant、tool、最終 assistant）: %+v",
			len(msgs), msgs)
	}
	if msgs[1].Role != core.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("messages[1] 應為帶 tool_calls 的 assistant 訊息（它的 content 也是空的，"+
			"但它不是空回應）: %+v", msgs[1])
	}
	if msgs[2].Role != core.RoleTool || msgs[2].ToolCallID != msgs[1].ToolCalls[0].ID {
		t.Fatalf("messages[2] 應為回應該次 tool call 的 tool 訊息: %+v", msgs[2])
	}
}
