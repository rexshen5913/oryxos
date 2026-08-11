// Session SQLite 持久化的整合測試（ticket #8）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——SQLite 在 seam
// 之下用真的（modernc 純 Go 驅動 ＋ t.TempDir()，憲法 4.3）。斷言落在外部可
// 觀察的產物上：sessions 表的資料列與 LLM 邊界請求，不斷言內部呼叫序列。
package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 測試直接查 sessions 表，用同一個純 Go 驅動

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/storage"
)

// sessionRow 是 sessions 表一列的斷言形狀。
type sessionRow struct {
	sessionID    string
	profileName  string
	channel      string
	userID       string
	messagesJSON string
	status       string
	createdAt    string
	lastActiveAt string
	archivedAt   sql.NullString
}

// persistedMessage 是 messages_json 的斷言形狀——落庫格式是本 ticket 的對外
// 產物（使用者可直接開 db 檔查看），故以欄位名逐條核對。
type persistedMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	ToolCalls []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

// querySessions 直接查 sessions 表（外部可觀察產物），按 created_at 排序。
func querySessions(t *testing.T, dbPath string) []sessionRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("開啟 db 檔 %s: %v", dbPath, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉 db 檔: %v", err)
		}
	}()

	rows, err := db.QueryContext(context.Background(),
		`SELECT session_id, profile_name, channel, user_id, messages_json, status,
		        created_at, last_active_at, archived_at
		 FROM sessions ORDER BY created_at`)
	if err != nil {
		t.Fatalf("查詢 sessions 表: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.sessionID, &r.profileName, &r.channel, &r.userID,
			&r.messagesJSON, &r.status, &r.createdAt, &r.lastActiveAt, &r.archivedAt); err != nil {
			t.Fatalf("掃描 sessions 資料列: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀取 sessions 資料列: %v", err)
	}
	return got
}

// onlySession 斷言 sessions 表恰有一列並回傳它。
func onlySession(t *testing.T, dbPath string) sessionRow {
	t.Helper()
	rows := querySessions(t, dbPath)
	if len(rows) != 1 {
		t.Fatalf("sessions 資料列數 = %d, 期望 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// decodePersisted 解析 messages_json。
func decodePersisted(t *testing.T, messagesJSON string) []persistedMessage {
	t.Helper()
	var msgs []persistedMessage
	if err := json.Unmarshal([]byte(messagesJSON), &msgs); err != nil {
		t.Fatalf("解析 messages_json %q: %v", messagesJSON, err)
	}
	return msgs
}

// newSessionStore 開一個落在 t.TempDir() 的真實 SQLite Session 儲存，給不關心
// 持久化細節、但仍不 mock 可確定化依賴的測試用（憲法 4.3）。
func newSessionStore(t *testing.T) *storage.SessionManager {
	t.Helper()
	return openSessionStore(t, filepath.Join(t.TempDir(), "oryxos.db"))
}

// openSessionStore 在 dbPath 開啟真實的 SQLite Session 儲存；測試結束時關閉
// （重複關閉是安全的，跨重啟測試會先自行關掉模擬進程結束）。
func openSessionStore(t *testing.T, dbPath string) *storage.SessionManager {
	t.Helper()
	store, err := storage.OpenSessionManager(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSessionManager(%s): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("關閉 Session 儲存: %v", err)
		}
	})
	return store
}

// activeSession 依 CLI 的聯合標識取回 active Session（無則為新建的空 Session）。
func activeSession(t *testing.T, store *storage.SessionManager) *core.Session {
	t.Helper()
	session, err := store.ActiveSession(context.Background(), "cli", "local", "default")
	if err != nil {
		t.Fatalf("ActiveSession: %v", err)
	}
	return session
}

// replayThenFail 依序回放 fixtures，用盡後一律回 500——用來製造「LLM 呼叫故障」
// 的失敗 turn（含本輪已跑過 Tool 才故障的分支）。
func replayThenFail(t *testing.T, fixtures ...string) *httptest.Server {
	t.Helper()
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served >= len(fixtures) {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[served]))
		served++
	}))
	t.Cleanup(srv.Close)
	return srv
}

// conversation 把 LLM 邊界請求的非 system 訊息壓成 "role:content" 序列，
// 用來斷言「哪些對話歷史進了請求」，不綁死 prompt 內文措辭。
func conversation(req llmRequest) []string {
	var got []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		got = append(got, m.Role+":"+m.Content)
	}
	return got
}

// TestProcessPersistsSuccessfulTurn 驗證一輪成功對話後 sessions 表存在一列
// active Session，且 messages_json 與 Session 對話歷史一致（含 role、content、
// timestamp、tool_calls；tool 訊息的 tool_call_id 一併落庫，否則恢復的歷史在
// OpenAI 兼容協議下不合法）。
func TestProcessPersistsSuccessfulTurn(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
	}))
	t.Cleanup(weather.Close)

	toolCallFixture := strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", weather.URL)
	srv := newReplayServer(t, toolCallFixture, readFixture(t, "reply_weather_final.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	store := openSessionStore(t, dbPath)
	agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), store)
	session := activeSession(t, store)

	if _, err := agent.Process(context.Background(), session, "查一下北京天氣並告訴我穿什麼"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	row := onlySession(t, dbPath)
	if row.sessionID != session.ID {
		t.Errorf("session_id = %q, 期望 %q", row.sessionID, session.ID)
	}
	if row.profileName != "default" || row.channel != "cli" || row.userID != "local" {
		t.Errorf("聯合標識落庫 = (%s, %s, %s), 期望 (default, cli, local)", row.profileName, row.channel, row.userID)
	}
	if row.status != "active" {
		t.Errorf("status = %q, 期望 active", row.status)
	}
	if row.archivedAt.Valid {
		t.Errorf("archived_at = %q, 期望 NULL（未歸檔）", row.archivedAt.String)
	}
	if row.createdAt == "" || row.lastActiveAt == "" {
		t.Errorf("created_at／last_active_at 未填: %q／%q", row.createdAt, row.lastActiveAt)
	}

	// messages_json 與 Session 對話歷史逐條核對。
	got := decodePersisted(t, row.messagesJSON)
	if len(got) != len(session.Messages) {
		t.Fatalf("messages_json 訊息數 = %d, 期望 %d: %s", len(got), len(session.Messages), row.messagesJSON)
	}
	for i, want := range session.Messages {
		g := got[i]
		if g.Role != string(want.Role) || g.Content != want.Content {
			t.Errorf("messages_json[%d] = {%s %q}, 期望 {%s %q}", i, g.Role, g.Content, want.Role, want.Content)
		}
		if !g.Timestamp.Equal(want.Timestamp) {
			t.Errorf("messages_json[%d] timestamp = %v, 期望 %v", i, g.Timestamp, want.Timestamp)
		}
		if g.ToolCallID != want.ToolCallID {
			t.Errorf("messages_json[%d] tool_call_id = %q, 期望 %q", i, g.ToolCallID, want.ToolCallID)
		}
		if len(g.ToolCalls) != len(want.ToolCalls) {
			t.Fatalf("messages_json[%d] tool_calls 數 = %d, 期望 %d", i, len(g.ToolCalls), len(want.ToolCalls))
		}
		for j, wantCall := range want.ToolCalls {
			if g.ToolCalls[j].ID != wantCall.ID || g.ToolCalls[j].Name != wantCall.Name || g.ToolCalls[j].Arguments != wantCall.Arguments {
				t.Errorf("messages_json[%d].tool_calls[%d] = %+v, 期望 %+v", i, j, g.ToolCalls[j], wantCall)
			}
		}
	}
	// 這輪確實含 tool 序列——否則上面的逐條核對測不到 tool_calls 與 tool_call_id。
	if !strings.Contains(row.messagesJSON, `"tool_calls"`) || !strings.Contains(row.messagesJSON, `"tool_call_id"`) {
		t.Errorf("messages_json 未含 tool_calls／tool_call_id: %s", row.messagesJSON)
	}
}

// TestSessionRestoreMatrix 是 Session 恢復矩陣：同一個 db 檔先後組兩次儲存與
// 引擎（第二次模擬重啟），斷言恢復的對話歷史確實參與行為——進到 LLM 邊界的
// 請求裡，而不是只比對記憶體結構。
func TestSessionRestoreMatrix(t *testing.T) {
	const (
		turn1Msg   = "我的專案用 Go 開發"
		turn1Reply = "好的，已記下：你的專案用 Go 開發。"
		turn2Msg   = "我剛才說專案用什麼開發？"
	)
	tests := []struct {
		name      string
		seedTurn1 bool
		// wantConversation 是重啟後那次 LLM 邊界請求應帶的非 system 訊息序列。
		wantConversation []string
	}{
		{
			name:             "無既有 active Session：建新，對話歷史自本輪起算",
			seedTurn1:        false,
			wantConversation: []string{"user:" + turn2Msg},
		},
		{
			name:             "有 active Session：恢復，先前的對話歷史進 LLM 請求",
			seedTurn1:        true,
			wantConversation: []string{"user:" + turn1Msg, "assistant:" + turn1Reply, "user:" + turn2Msg},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixtures := []string{readFixture(t, "reply_turn2.json")}
			if tt.seedTurn1 {
				fixtures = append([]string{readFixture(t, "reply_turn1.json")}, fixtures...)
			}
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, fixtures...)
			dbPath := filepath.Join(t.TempDir(), "oryxos.db")

			if tt.seedTurn1 {
				// 第一個進程：對話一輪後關閉儲存，模擬進程結束。
				store := openSessionStore(t, dbPath)
				session := activeSession(t, store)
				if _, err := newAgentOn(t, srv.URL, discardLogger(), store).Process(context.Background(), session, turn1Msg); err != nil {
					t.Fatalf("重啟前對話: %v", err)
				}
				if err := store.Close(); err != nil {
					t.Fatalf("關閉 Session 儲存: %v", err)
				}
			}

			// 第二個進程：同一個 db 檔重新組儲存與引擎（不 mock「重啟」）。
			store := openSessionStore(t, dbPath)
			session := activeSession(t, store)
			if _, err := newAgentOn(t, srv.URL, discardLogger(), store).Process(context.Background(), session, turn2Msg); err != nil {
				t.Fatalf("重啟後續談: %v", err)
			}

			got := conversation(parseLLMRequest(t, llmReqs[len(llmReqs)-1]))
			if !slices.Equal(got, tt.wantConversation) {
				t.Errorf("重啟後 LLM 請求的對話歷史 = %q, 期望 %q", got, tt.wantConversation)
			}
			// 恢復的是同一列，不是又開一場：整場對話仍只有一個 active Session。
			row := onlySession(t, dbPath)
			if len(decodePersisted(t, row.messagesJSON)) != len(session.Messages) {
				t.Errorf("落庫訊息數 = %d, 期望與 Session 一致 %d", len(decodePersisted(t, row.messagesJSON)), len(session.Messages))
			}
		})
	}
}

// TestFailedTurnNotPersisted 驗證失敗 turn 沿 spec #1 的 rollback 語義不落庫：
// DB 維持前一個協議合法的狀態，不留半截 tool 序列。
func TestFailedTurnNotPersisted(t *testing.T) {
	tests := []struct {
		name string
		// extraFixtures 是失敗那一輪在故障前先回放的錄製回應。
		extraFixtures func(t *testing.T, targetURL string) []string
	}{
		{
			name:          "Provider 錯誤：第一次 LLM 呼叫就故障",
			extraFixtures: func(*testing.T, string) []string { return nil },
		},
		{
			name: "Tool 執行後 Provider 錯誤：不留半截 tool 序列",
			extraFixtures: func(t *testing.T, targetURL string) []string {
				t.Helper()
				return []string{strings.ReplaceAll(readFixture(t, "reply_weather_tool_call.json"), "{{TARGET_URL}}", targetURL)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"city":"beijing","temp_c":5,"condition":"晴"}`)
			}))
			t.Cleanup(weather.Close)

			// 先成功一輪（落庫），再讓下一輪故障：斷言 DB 停在前一狀態。
			fixtures := append([]string{readFixture(t, "reply_direct.json")}, tt.extraFixtures(t, weather.URL)...)
			srv := replayThenFail(t, fixtures...)

			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			store := openSessionStore(t, dbPath)
			agent := newToolAgentOn(t, srv.URL, testProfile(), []string{"http_get"}, []string{"127.0.0.1"}, discardLogger(), store)
			session := activeSession(t, store)

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("第一輪對話: %v", err)
			}
			afterTurn1 := onlySession(t, dbPath).messagesJSON

			if _, err := agent.Process(context.Background(), session, "查一下北京天氣"); err == nil {
				t.Fatal("期望第二輪失敗，實際成功")
			}

			row := onlySession(t, dbPath)
			if row.messagesJSON != afterTurn1 {
				t.Errorf("失敗 turn 落庫了：messages_json = %s, 期望維持 %s", row.messagesJSON, afterTurn1)
			}
			for i, msg := range decodePersisted(t, row.messagesJSON) {
				if msg.Role == "tool" || len(msg.ToolCalls) > 0 {
					t.Errorf("DB 留下半截 tool 序列：messages_json[%d] = %+v", i, msg)
				}
			}
		})
	}
}

// TestLastActiveAtUpdatedEachTurn 驗證每輪成功後 last_active_at 更新，
// created_at 維持首次落庫的時刻。
func TestLastActiveAtUpdatedEachTurn(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_turn1.json"), readFixture(t, "reply_turn2.json"))
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	store := openSessionStore(t, dbPath)
	agent := newAgentOn(t, srv.URL, discardLogger(), store)
	session := activeSession(t, store)

	if _, err := agent.Process(context.Background(), session, "我的專案用 Go 開發"); err != nil {
		t.Fatalf("第一輪對話: %v", err)
	}
	first := onlySession(t, dbPath)

	if _, err := agent.Process(context.Background(), session, "我剛才說專案用什麼開發？"); err != nil {
		t.Fatalf("第二輪對話: %v", err)
	}
	second := onlySession(t, dbPath)

	firstActive := parseTimestamp(t, "last_active_at", first.lastActiveAt)
	secondActive := parseTimestamp(t, "last_active_at", second.lastActiveAt)
	if !secondActive.After(firstActive) {
		t.Errorf("last_active_at 未隨第二輪更新：%v → %v", firstActive, secondActive)
	}
	if second.createdAt != first.createdAt {
		t.Errorf("created_at 被改寫：%q → %q", first.createdAt, second.createdAt)
	}
}

// parseTimestamp 解析落庫的時間戳欄位（RFC3339Nano）。
func parseTimestamp(t *testing.T, field, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("解析 %s %q: %v", field, value, err)
	}
	return ts
}
