// 審計落庫的整合測試（ticket #12）：沿用既有兩個 seam——一律從 AgentService.Process
// 驅動，LLM 以 httptest 回放（ADR-0002）——SQLite 在 seam 之下用真的。斷言直接查
// llm_calls 與 tool_invocations 兩張表，那是本票的外部產物。
package core_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// llmCallRow 是 llm_calls 一列的斷言形狀。
type llmCallRow struct {
	callID           string
	sessionID        string
	provider         string
	model            string
	promptTokens     int
	completionTokens int
	totalTokens      int
	latencyMS        int64
	status           string
	startedAt        string
	completedAt      string
}

// toolInvocationRow 是 tool_invocations 一列的斷言形狀。
type toolInvocationRow struct {
	invocationID string
	sessionID    string
	profileName  string
	toolName     string
	parameters   string
	status       string
	result       sql.NullString
	errText      sql.NullString
	startedAt    string
	completedAt  string
	tokenCost    sql.NullInt64
}

// queryLLMCalls 直接查 llm_calls（外部可觀察產物），按起始時間排序。
func queryLLMCalls(t *testing.T, dbPath string) []llmCallRow {
	t.Helper()
	var got []llmCallRow
	eachRow(t, dbPath, `SELECT call_id, session_id, provider, model, prompt_tokens, completion_tokens,
	                           total_tokens, latency_ms, status, started_at, completed_at
	                    FROM llm_calls ORDER BY started_at, call_id`,
		func(rows *sql.Rows) {
			var r llmCallRow
			if err := rows.Scan(&r.callID, &r.sessionID, &r.provider, &r.model, &r.promptTokens,
				&r.completionTokens, &r.totalTokens, &r.latencyMS, &r.status, &r.startedAt, &r.completedAt); err != nil {
				t.Fatalf("掃描 llm_calls: %v", err)
			}
			got = append(got, r)
		})
	return got
}

// queryToolInvocations 直接查 tool_invocations，按起始時間排序。
func queryToolInvocations(t *testing.T, dbPath string) []toolInvocationRow {
	t.Helper()
	var got []toolInvocationRow
	eachRow(t, dbPath, `SELECT invocation_id, session_id, profile_name, tool_name, parameters,
	                           status, result, error, started_at, completed_at, token_cost
	                    FROM tool_invocations ORDER BY started_at, invocation_id`,
		func(rows *sql.Rows) {
			var r toolInvocationRow
			if err := rows.Scan(&r.invocationID, &r.sessionID, &r.profileName, &r.toolName, &r.parameters,
				&r.status, &r.result, &r.errText, &r.startedAt, &r.completedAt, &r.tokenCost); err != nil {
				t.Fatalf("掃描 tool_invocations: %v", err)
			}
			got = append(got, r)
		})
	return got
}

// eachRow 對 query 的每一列呼叫 scan。
func eachRow(t *testing.T, dbPath, query string, scan func(*sql.Rows)) {
	t.Helper()
	db := openTestDB(t, dbPath)
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("查詢審計表: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		scan(rows)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀取審計資料列: %v", err)
	}
}

// TestAuditRecordsLLMAndToolCalls 是本票主場景：一次含 Tool 呼叫的對話跑完後，
// 每次 LLM 呼叫在 llm_calls 一行、每次 Tool 呼叫在 tool_invocations 一行，
// 兩者都以 session_id 關聯得回同一場對話。
func TestAuditRecordsLLMAndToolCalls(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
	}))
	t.Cleanup(weather.Close)

	toolCall := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", weather.URL)
	srv := newReplayServer(t, toolCall, readFixture(t, "reply_weather_final.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "查一下北京天氣並告訴我穿什麼"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 兩次 LLM 呼叫（第一次要 Tool、第二次給最終回應）各一行。
	db.flush(t)
	calls := queryLLMCalls(t, dbPath)
	if len(calls) != 2 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 2: %+v", len(calls), calls)
	}
	seenCallIDs := map[string]bool{}
	for i, c := range calls {
		if c.sessionID != session.ID {
			t.Errorf("llm_calls[%d] session_id = %q, 期望 %q", i, c.sessionID, session.ID)
		}
		if c.provider != "openai" || c.model != "gpt-4o-mini" {
			t.Errorf("llm_calls[%d] provider／model = %s／%s, 期望 openai／gpt-4o-mini", i, c.provider, c.model)
		}
		if c.status != "completed" {
			t.Errorf("llm_calls[%d] status = %q, 期望 completed", i, c.status)
		}
		if c.totalTokens == 0 || c.promptTokens == 0 || c.completionTokens == 0 {
			t.Errorf("llm_calls[%d] token 用量未落庫: %+v", i, c)
		}
		if c.promptTokens+c.completionTokens != c.totalTokens {
			t.Errorf("llm_calls[%d] token 用量不一致: %+v", i, c)
		}
		if c.latencyMS < 0 {
			t.Errorf("llm_calls[%d] latency_ms = %d", i, c.latencyMS)
		}
		assertTimestamps(t, fmt.Sprintf("llm_calls[%d]", i), c.startedAt, c.completedAt)
		if seenCallIDs[c.callID] {
			t.Errorf("llm_calls call_id 重複: %q", c.callID)
		}
		seenCallIDs[c.callID] = true
	}

	// 一次 Tool 呼叫一行。
	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1: %+v", len(invocations), invocations)
	}
	inv := invocations[0]
	if inv.sessionID != session.ID {
		t.Errorf("tool_invocations session_id = %q, 期望 %q", inv.sessionID, session.ID)
	}
	if inv.profileName != "default" || inv.toolName != "http_get" {
		t.Errorf("profile_name／tool_name = %s／%s, 期望 default／http_get", inv.profileName, inv.toolName)
	}
	if !strings.Contains(inv.parameters, "/weather") {
		t.Errorf("parameters 未落庫呼叫參數: %q", inv.parameters)
	}
	if inv.status != "completed" {
		t.Errorf("status = %q, 期望 completed", inv.status)
	}
	if !inv.result.Valid || !strings.Contains(inv.result.String, "temp_c") {
		t.Errorf("result 未落庫 Tool 結果: %+v", inv.result)
	}
	if inv.errText.Valid && inv.errText.String != "" {
		t.Errorf("成功的呼叫不該有 error: %q", inv.errText.String)
	}
	assertTimestamps(t, "tool_invocations", inv.startedAt, inv.completedAt)

	// token_cost 欄位存在但核心階段一律 NULL（定案 2026-08-07）：一輪 LLM 回應
	// 可帶多個 tool_calls，任何歸因口徑都是編造精度，錯誤精度比缺值更害。
	if inv.tokenCost.Valid {
		t.Errorf("token_cost = %d, 期望 NULL", inv.tokenCost.Int64)
	}
}

// assertTimestamps 斷言起訖時間都填了且順序合理。
func assertTimestamps(t *testing.T, what, startedAt, completedAt string) {
	t.Helper()
	start := parseTimestamp(t, what+" started_at", startedAt)
	done := parseTimestamp(t, what+" completed_at", completedAt)
	if done.Before(start) {
		t.Errorf("%s completed_at %v 早於 started_at %v", what, done, start)
	}
}

// TestAuditStatusBranches 是審計狀態分支矩陣（憲法 4.2）：Tool 呼叫成功、失敗、
// 逾時各自落正確的 status 與 error。
func TestAuditStatusBranches(t *testing.T) {
	tests := []struct {
		name string
		// targetDelay 是目標端點回應前的延遲；ctxTimeout 非零時 turn 帶 deadline。
		targetDelay time.Duration
		ctxTimeout  time.Duration
		// allowed 是 Sandbox 白名單；留空會讓呼叫被攔截（不可重試的失敗）。
		allowed    []string
		wantStatus string
		wantErrSub string
	}{
		{
			name:       "成功：status completed、error 為空",
			allowed:    []string{"127.0.0.1"},
			wantStatus: "completed",
		},
		{
			name:       "失敗：status failed、error 落庫",
			allowed:    nil, // 白名單外：SandboxViolation
			wantStatus: "failed",
			wantErrSub: "SandboxViolation",
		},
		{
			name:        "逾時：status timeout",
			targetDelay: 300 * time.Millisecond,
			ctxTimeout:  50 * time.Millisecond,
			allowed:     []string{"127.0.0.1"},
			wantStatus:  "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.targetDelay > 0 {
					time.Sleep(tt.targetDelay)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
			}))
			t.Cleanup(target.Close)

			toolCall := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", target.URL)
			srv := newReplayServer(t, toolCall, readFixture(t, "reply_weather_final.json"))

			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			db := openStore(t, dbPath)
			agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, tt.allowed, discardLogger(), db)
			session := activeSession(t, db.sessions())

			ctx := context.Background()
			if tt.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.ctxTimeout)
				defer cancel()
			}
			// 成敗都不影響本測——要驗的是審計有沒有如實記下發生的事。
			_, _ = agent.Process(ctx, session, "查一下北京天氣")

			db.flush(t)
			invocations := queryToolInvocations(t, dbPath)
			if len(invocations) == 0 {
				t.Fatal("tool_invocations 沒有任何資料列")
			}
			inv := invocations[0]
			if inv.status != tt.wantStatus {
				t.Errorf("status = %q, 期望 %q（error=%q）", inv.status, tt.wantStatus, inv.errText.String)
			}
			if tt.wantErrSub != "" && !strings.Contains(inv.errText.String, tt.wantErrSub) {
				t.Errorf("error = %q, 期望含 %q", inv.errText.String, tt.wantErrSub)
			}
			if tt.wantStatus == "completed" && inv.errText.Valid && inv.errText.String != "" {
				t.Errorf("成功的呼叫不該有 error: %q", inv.errText.String)
			}
		})
	}
}

// TestAuditSurvivesFailedTurn 驗證審計記的是「已發生的事實」：失敗 turn 的 Session
// 會 rollback、不落庫，但該 turn 已經發生過的 LLM 與 Tool 呼叫記錄要保留——與
// 「外部副作用不隨 rollback 撤銷」同一語義。這條與 Session 的 rollback 刻意相反，
// 別誤把審計一起回退掉。
func TestAuditSurvivesFailedTurn(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
	}))
	t.Cleanup(weather.Close)

	// 第一次 LLM 呼叫要 Tool，Tool 跑完後第二次 LLM 呼叫故障：turn 失敗。
	toolCall := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", weather.URL)
	srv := replayThenFail(t, toolCall)

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err == nil {
		t.Fatal("期望 turn 失敗，實際成功")
	}
	if len(session.Messages) != 0 {
		t.Fatalf("失敗 turn 未 rollback：Session 殘留 %d 條訊息", len(session.Messages))
	}
	if rows := querySessions(t, dbPath); len(rows) != 0 {
		t.Errorf("失敗 turn 不該落 Session：%+v", rows)
	}

	// 已發生的呼叫都留著：兩次 LLM 呼叫（第二次失敗）加一次 Tool 呼叫。
	db.flush(t)
	calls := queryLLMCalls(t, dbPath)
	if len(calls) != 2 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 2（含失敗那次）: %+v", len(calls), calls)
	}
	if calls[0].status != "completed" || calls[1].status != "failed" {
		t.Errorf("llm_calls status = %q／%q, 期望 completed／failed", calls[0].status, calls[1].status)
	}
	if len(queryToolInvocations(t, dbPath)) != 1 {
		t.Errorf("失敗 turn 已發生的 Tool 呼叫記錄未保留")
	}
	for _, c := range calls {
		if c.sessionID != session.ID {
			t.Errorf("審計未關聯到 Session：%q 期望 %q", c.sessionID, session.ID)
		}
	}
}

// TestAuditRecordsEveryRetryAttempt 驗證重試的每次執行各記一筆——與既有
// tool_invocation 結構化日誌「每次執行一筆」的語義一致，它們記的是同一件事。
// 沒有這條斷言，日後有人重構繞過 execute 會靜默漏記，而審計漏記是查不出來的。
func TestAuditRecordsEveryRetryAttempt(t *testing.T) {
	tests := []struct {
		name string
		// failFirst 是目標端點先以斷線失敗的次數；-1 表示永遠失敗。
		failFirst   int
		llmFixtures []string
		wantStatus  []string // 期望的每筆 tool_invocations status（按時間序）
	}{
		{
			name:        "重試後成功：失敗那次也記一筆",
			failFirst:   1,
			llmFixtures: []string{"reply_weather_tool_call.json", "reply_weather_final.json"},
			wantStatus:  []string{"failed", "completed"},
		},
		{
			name:        "重試耗盡：首次加三次重試各一筆",
			failFirst:   -1,
			llmFixtures: []string{"reply_weather_tool_call.json", "reply_retry_final.json"},
			wantStatus:  []string{"failed", "failed", "failed", "failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				if tt.failFirst < 0 || hits <= tt.failFirst {
					// 斷線不回應：client 端得到可重試的網路錯誤。
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Error("httptest server 不支援 Hijacker")
						return
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Errorf("Hijack: %v", err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
			}))
			t.Cleanup(target.Close)

			fixtures := make([]string, 0, len(tt.llmFixtures))
			for _, name := range tt.llmFixtures {
				fixtures = append(fixtures, strings.ReplaceAll(readFixture(t, name), "{{TARGET_URL}}", target.URL))
			}
			srv := newReplayServer(t, fixtures...)

			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			db := openStore(t, dbPath)
			agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), db)
			session := activeSession(t, db.sessions())

			if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			db.flush(t)
			invocations := queryToolInvocations(t, dbPath)
			var got []string
			for _, inv := range invocations {
				got = append(got, inv.status)
			}
			if !slices.Equal(got, tt.wantStatus) {
				t.Errorf("tool_invocations status 序列 = %v, 期望 %v", got, tt.wantStatus)
			}
		})
	}
}

// TestAuditKeepsTokensOnFailedCall 驗證失敗的 LLM 呼叫仍保留已知的 token 用量：
// Provider 回了 usage 卻沒有任何 choice 時呼叫算失敗，但那些 token 已經被計費，
// 記成零就是把成本歸零——錯誤精度比缺值更害，這正是 token_cost 一律寫 NULL 的
// 同一條理由。
func TestAuditKeepsTokensOnFailedCall(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_no_choice.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newAgentOn(t, srv.URL, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err == nil {
		t.Fatal("回應不含 choice 應視為失敗，實際成功")
	}

	db.flush(t)
	calls := queryLLMCalls(t, dbPath)
	if len(calls) != 1 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 1: %+v", len(calls), calls)
	}
	if calls[0].status != "failed" {
		t.Errorf("status = %q, 期望 failed", calls[0].status)
	}
	if calls[0].promptTokens != 88 || calls[0].totalTokens != 88 {
		t.Errorf("失敗的呼叫遺失了已計費的 token 用量: %+v", calls[0])
	}
}

// TestAuditRedactsSecretsInParameters 驗證落庫前去敏：db 檔是使用者會直接打開、
// 隨 Workspace 備份搬遷的東西，比日誌更持久，不該成為第二條密鑰洩漏路徑。
// 這與 sessions.messages_json 存原始 arguments 不衝突——那份原文是重放對話的
// 必要條件，審計記錄不重放，沒有留明文的必要。
func TestAuditRedactsSecretsInParameters(t *testing.T) {
	const secret = "SUPER-SECRET-KEY"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(target.Close)

	// tool_calls 的參數同時帶著兩個藏密鑰的位置：URL 的 userinfo 與 query。
	withCreds := strings.Replace(target.URL, "http://", "http://alice:"+secret+"@", 1)
	toolCall := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"),
		"{{TARGET_URL}}/weather?city=beijing", withCreds+"/weather?api_key="+secret)
	srv := newReplayServer(t, toolCall, readFixture(t, "reply_weather_final.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "查一下"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	db.flush(t)
	invocations := queryToolInvocations(t, dbPath)
	if len(invocations) != 1 {
		t.Fatalf("tool_invocations 資料列數 = %d, 期望 1", len(invocations))
	}
	inv := invocations[0]
	if strings.Contains(inv.parameters, secret) {
		t.Errorf("密鑰原文落進 tool_invocations.parameters: %q", inv.parameters)
	}
	if !strings.Contains(inv.parameters, "REDACTED") {
		t.Errorf("parameters 未經去敏: %q", inv.parameters)
	}
	if strings.Contains(inv.errText.String, secret) {
		t.Errorf("密鑰原文落進 tool_invocations.error: %q", inv.errText.String)
	}
}

// TestAuditWriteFailureDoesNotBreakConversation 驗證審計是旁路：寫入失敗時對話
// 照常返回回應，錯誤落結構化日誌。完整審計保證屬企業治理層（憲法 3.3），核心
// 階段不讓使用者的對話因為審計故障而失敗。
func TestAuditWriteFailureDoesNotBreakConversation(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_direct.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	var logBuf bytes.Buffer
	db := openStoreLogged(t, dbPath, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	session := activeSession(t, db.sessions())

	agent := newAgentOn(t, srv.URL, discardLogger(), db)
	// 讓後續所有寫入失敗：直接把審計表砍掉（比 chmod 更確定，且不影響讀取路徑）。
	dropAuditTables(t, dbPath)

	resp, err := agent.Process(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("審計寫入失敗不該中斷對話: %v", err)
	}
	if resp == "" {
		t.Error("對話未返回回應")
	}
	db.flush(t)
	if !strings.Contains(logBuf.String(), "audit_write_failed") {
		t.Errorf("審計寫入失敗未落結構化日誌: %s", logBuf.String())
	}
}

// dropAuditTables 砍掉兩張審計表，用來製造「審計寫入必然失敗」的環境。
func dropAuditTables(t *testing.T, dbPath string) {
	t.Helper()
	db := openTestDB(t, dbPath)
	for _, table := range []string{"llm_calls", "tool_invocations"} {
		if _, err := db.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
			t.Fatalf("移除 %s: %v", table, err)
		}
	}
}
