package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 測試直接查 sessions 表，用同一個純 Go 驅動
)

// newReplayServer 起一個回放錄製回應的 httptest.Server（ADR-0002）：按請求順序
// 逐一回放 fixtures，超出錄製數量的請求視為測試錯誤。
func newReplayServer(t *testing.T, fixtures ...string) *httptest.Server {
	t.Helper()
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served >= len(fixtures) {
			t.Errorf("LLM 請求數超出錄製回應數 %d", len(fixtures))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[served]))
		served++
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("讀取 fixture %s: %v", name, err)
	}
	return string(data)
}

// setupChatWorkspace 以真實 oryxos init 建 Workspace，再把 Provider 的 base_url
// 指向回放伺服器（LLM 替換點是既有配置欄位，不是新 seam），並設好 API key。
func setupChatWorkspace(t *testing.T, baseURL string) string {
	t.Helper()
	dir := t.TempDir()
	if err := initWorkspace(io.Discard, dir); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}
	cfg := "providers:\n  openai:\n    api_key: ${OPENAI_API_KEY}\n    base_url: " + baseURL + "\nhttp:\n  allowed_domains: []\n"
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("覆寫 config.yaml: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	return dir
}

// TestChatCommandMessageMode 走完整 cobra 命令路徑，驗證 --message 單訊息模式
// 與 --profile 預設值 default。
func TestChatCommandMessageMode(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	t.Chdir(dir)

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"chat", "--message", "你好"})

	if err := root.Execute(); err != nil {
		t.Fatalf("chat --message: %v", err)
	}
	if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
		t.Errorf("輸出未含回應內容: %q", out.String())
	}
}

func TestChatInteractive(t *testing.T) {
	tests := []struct {
		name     string
		stdin    string
		fixtures []string
		wantOut  []string
	}{
		{
			name:     "多輪對話後 /quit 乾淨退出（空白行不觸發 LLM 呼叫）",
			stdin:    "第一句\n\n第二句\n/quit\n",
			fixtures: []string{"chat_reply_1.json", "chat_reply_2.json"},
			wantOut:  []string{"回應一：你好，我是 Oryx。", "回應二：我記得你剛才說的話。"},
		},
		{
			name:     "EOF 視同結束對話",
			stdin:    "第一句\n",
			fixtures: []string{"chat_reply_1.json"},
			wantOut:  []string{"回應一：你好，我是 Oryx。"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replies := make([]string, len(tt.fixtures))
			for i, f := range tt.fixtures {
				replies[i] = readFixture(t, f)
			}
			srv := newReplayServer(t, replies...)
			dir := setupChatWorkspace(t, srv.URL)

			var out bytes.Buffer
			err := runChat(context.Background(), strings.NewReader(tt.stdin), &out, dir, chatOptions{profileName: "default"})
			if err != nil {
				t.Fatalf("runChat: %v", err)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("輸出未含 %q: %q", want, out.String())
				}
			}
		})
	}
}

// TestChatInteractiveCancel 驗證互動模式在 idle prompt（阻塞等輸入）時，
// context 取消（如 Ctrl+C 經 signal.NotifyContext）能讓對話乾淨返回（憲法 5.3）。
func TestChatInteractiveCancel(t *testing.T) {
	srv := newReplayServer(t) // 不期望任何 LLM 呼叫
	dir := setupChatWorkspace(t, srv.URL)

	pr, pw := io.Pipe() // 永不送資料的 stdin，模擬使用者在 prompt 前發呆
	t.Cleanup(func() { _ = pw.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runChat(ctx, pr, &out, dir, chatOptions{profileName: "default"}) }()

	time.Sleep(50 * time.Millisecond) // 讓對話進入等待輸入狀態
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("取消後應乾淨返回，實際錯誤: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context 取消後 2 秒內未返回，idle prompt 未被取消")
	}
	if !strings.Contains(out.String(), "對話已中斷") {
		t.Errorf("輸出未含中斷提示: %q", out.String())
	}
}

// TestChatInteractiveTransientError 驗證互動模式遇暫時性 Provider 故障時：
// 印出清晰錯誤後對話續跑（失敗 turn 已 rollback，重試安全），不終結整場對話。
func TestChatInteractiveTransientError(t *testing.T) {
	var calls int
	fixture := readFixture(t, "chat_reply_1.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	t.Cleanup(srv.Close)
	dir := setupChatWorkspace(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("你好\n你好\n/quit\n")
	if err := runChat(context.Background(), in, &out, dir, chatOptions{profileName: "default"}); err != nil {
		t.Fatalf("暫時性故障不應終結對話，實際錯誤: %v", err)
	}
	if !strings.Contains(out.String(), "錯誤：") {
		t.Errorf("輸出未含錯誤提示: %q", out.String())
	}
	if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
		t.Errorf("重試後輸出未含回應內容: %q", out.String())
	}
}

// TestChatEmptyWhitelistWarning 驗證啟動即清晰告知（spec 需求 5.12 基礎校驗）：
// Profile 列了 HTTP Tool 但 http.allowed_domains 為空時，out-of-box 的每次
// Tool 呼叫都會被攔截——啟動時警示而非留到執行期才發現；已配置白名單則不警示。
func TestChatEmptyWhitelistWarning(t *testing.T) {
	tests := []struct {
		name           string
		allowedDomains string // config.yaml 的 allowed_domains YAML 片段
		wantWarn       bool
	}{
		{name: "空白名單且 Profile 列 HTTP Tool 時警示", allowedDomains: "[]", wantWarn: true},
		{name: "已配置白名單不警示", allowedDomains: "[api.example.com]", wantWarn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			cfg := "providers:\n  openai:\n    api_key: ${OPENAI_API_KEY}\n    base_url: " + srv.URL +
				"\nhttp:\n  allowed_domains: " + tt.allowedDomains + "\n"
			if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}
			warned := strings.Contains(out.String(), "allowed_domains")
			if warned != tt.wantWarn {
				t.Errorf("警示出現 = %v, 期望 %v; 輸出: %q", warned, tt.wantWarn, out.String())
			}
		})
	}
}

// sessionRow 是 sessions 表一列在 CLI 端的斷言形狀。
type sessionRow struct {
	sessionID    string
	profileName  string
	messagesJSON string
	status       string
	archivedAt   sql.NullString
}

// messageCount 回傳該列落庫的對話歷史條數。
func (r sessionRow) messageCount(t *testing.T) int {
	t.Helper()
	var messages []json.RawMessage
	if err := json.Unmarshal([]byte(r.messagesJSON), &messages); err != nil {
		t.Fatalf("解析 messages_json %q: %v", r.messagesJSON, err)
	}
	return len(messages)
}

// sessionRows 直接查 Workspace 的 sessions 表（外部可觀察產物）。
func sessionRows(t *testing.T, dbPath string) []sessionRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("開啟 %s: %v", dbPath, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉 db 檔: %v", err)
		}
	}()

	rows, err := db.QueryContext(context.Background(),
		`SELECT session_id, profile_name, messages_json, status, archived_at FROM sessions ORDER BY created_at`)
	if err != nil {
		t.Fatalf("查詢 sessions 表: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.sessionID, &r.profileName, &r.messagesJSON, &r.status, &r.archivedAt); err != nil {
			t.Fatalf("掃描 sessions 資料列: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀取 sessions 資料列: %v", err)
	}
	return got
}

// rowWithStatus 取出唯一一列指定 status 的 Session。
func rowWithStatus(t *testing.T, rows []sessionRow, status string) sessionRow {
	t.Helper()
	var found []sessionRow
	for _, r := range rows {
		if r.status == status {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("status=%s 的資料列數 = %d, 期望 1: %+v", status, len(found), rows)
	}
	return found[0]
}

// onlyActiveSession 斷言 sessions 表恰有一行且為 active，回傳其主鍵與落庫的
// 對話歷史條數。
func onlyActiveSession(t *testing.T, dbPath string) (sessionID string, messageCount int) {
	t.Helper()
	rows := sessionRows(t, dbPath)
	if len(rows) != 1 {
		t.Fatalf("sessions 資料列數 = %d, 期望 1: %+v", len(rows), rows)
	}
	if rows[0].status != "active" {
		t.Errorf("status = %q, 期望 active", rows[0].status)
	}
	return rows[0].sessionID, rows[0].messageCount(t)
}

// TestChatPersistsAndRestoresSession 是 ticket #8 在 CLI 端的端到端驗證：一輪
// 成功對話後 sessions 表存在一行 active Session；結束進程後重新執行 oryxos chat
// （同一 Profile），同一聯合標識的 Session 自動恢復——第二輪追加在同一行上，
// 而不是另開一場對話。
func TestChatPersistsAndRestoresSession(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, srv.URL)
	dbPath := filepath.Join(dir, workspaceDir, sessionDBFile)

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "第一句"}); err != nil {
		t.Fatalf("第一次 oryxos chat: %v", err)
	}
	firstID, firstCount := onlyActiveSession(t, dbPath)
	if firstCount != 2 {
		t.Fatalf("第一輪後落庫訊息數 = %d, 期望 2（user 加 assistant）", firstCount)
	}

	// runChat 已返回（儲存關閉、進程視同結束），重跑一次即模擬重啟。
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "第二句"}); err != nil {
		t.Fatalf("重啟後 oryxos chat: %v", err)
	}
	secondID, secondCount := onlyActiveSession(t, dbPath)
	if secondID != firstID {
		t.Errorf("重啟後另開了新 Session: %q → %q", firstID, secondID)
	}
	if secondCount != 4 {
		t.Errorf("重啟後落庫訊息數 = %d, 期望 4（兩輪對話累積在同一 Session）", secondCount)
	}
}

// TestChatNewArchivesActiveSession 是 ticket #9 在 CLI 端的端到端驗證：先談一場
// 對話，再帶 --new 執行——舊 Session 標記 archived 並寫 archived_at、對話歷史
// 原樣保留供日後查閱，新的 active Session 另起一列，兩行並存。
func TestChatNewArchivesActiveSession(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"))
	dir := setupChatWorkspace(t, srv.URL)
	dbPath := filepath.Join(dir, workspaceDir, sessionDBFile)

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "第一句"}); err != nil {
		t.Fatalf("第一次 oryxos chat: %v", err)
	}
	firstID, _ := onlyActiveSession(t, dbPath)

	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "第二句", newConversation: true}); err != nil {
		t.Fatalf("oryxos chat --new: %v", err)
	}

	rows := sessionRows(t, dbPath)
	if len(rows) != 2 {
		t.Fatalf("sessions 資料列數 = %d, 期望 2（archived 與 active 並存）: %+v", len(rows), rows)
	}
	archived := rowWithStatus(t, rows, "archived")
	if archived.sessionID != firstID {
		t.Errorf("被歸檔的是 %q, 期望先前那場 %q", archived.sessionID, firstID)
	}
	if !archived.archivedAt.Valid {
		t.Fatalf("歸檔的 Session archived_at 為 NULL")
	}
	if _, err := time.Parse(time.RFC3339Nano, archived.archivedAt.String); err != nil {
		t.Errorf("archived_at %q 非合法時間戳: %v", archived.archivedAt.String, err)
	}
	if got := archived.messageCount(t); got != 2 {
		t.Errorf("歸檔後舊 Session 訊息數 = %d, 期望維持 2（歸檔只改狀態，不動對話歷史）", got)
	}

	active := rowWithStatus(t, rows, "active")
	if active.sessionID == firstID {
		t.Errorf("--new 未另開 Session：active 仍是 %q", active.sessionID)
	}
	if active.archivedAt.Valid {
		t.Errorf("新 Session archived_at = %q, 期望 NULL", active.archivedAt.String)
	}
	if got := active.messageCount(t); got != 2 {
		t.Errorf("新 Session 訊息數 = %d, 期望 2（本輪 user 加 assistant，不帶舊 Session 訊息）", got)
	}
}

// TestChatNewWithoutActiveSession 驗證全新 Workspace 上 --new 等同正常開新對話：
// 沒有 active Session 可歸檔不是錯誤，對話照常談得起來。
func TestChatNewWithoutActiveSession(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	dbPath := filepath.Join(dir, workspaceDir, sessionDBFile)

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "第一句", newConversation: true}); err != nil {
		t.Fatalf("全新 Workspace 上 oryxos chat --new: %v", err)
	}
	if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
		t.Errorf("輸出未含回應內容: %q", out.String())
	}
	if _, count := onlyActiveSession(t, dbPath); count != 2 {
		t.Errorf("落庫訊息數 = %d, 期望 2（user 加 assistant）", count)
	}
}

// TestChatNewScopedToProfile 驗證歸檔的範圍就是（Channel、使用者、Profile）這組
// 聯合標識：在 work Profile 上 --new，不得波及 default Profile 的 active Session。
// 範圍是 ArchiveActive 的 WHERE 條件唯一要守的性質，也是重構時最容易悄悄放寬的。
func TestChatNewScopedToProfile(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "chat_reply_1.json"), readFixture(t, "chat_reply_2.json"),
		readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	dbPath := filepath.Join(dir, workspaceDir, sessionDBFile)
	work := "name: work\nidentity:\n  agent_name: Worker\n  prompt: 你是工作助理。\nprovider:\n  name: openai\n  model: gpt-4o-mini\n"
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "work.yaml"), []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	for _, profileName := range []string{"default", "work"} {
		if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: profileName, message: "第一句"}); err != nil {
			t.Fatalf("Profile %s 首場對話: %v", profileName, err)
		}
	}
	before := sessionRows(t, dbPath)
	if len(before) != 2 {
		t.Fatalf("兩個 Profile 各談一場後資料列數 = %d, 期望 2: %+v", len(before), before)
	}
	defaultID := profileRow(t, before, "default").sessionID
	workID := profileRow(t, before, "work").sessionID

	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "work", message: "第二句", newConversation: true}); err != nil {
		t.Fatalf("oryxos chat --profile work --new: %v", err)
	}

	rows := sessionRows(t, dbPath)
	archived := rowWithStatus(t, rows, "archived")
	if archived.sessionID != workID {
		t.Errorf("被歸檔的是 %q（Profile %s）, 期望 work 那場 %q", archived.sessionID, archived.profileName, workID)
	}
	for _, r := range rows {
		if r.sessionID == defaultID && r.status != "active" {
			t.Errorf("default 的 Session 被波及：status = %q, 期望維持 active", r.status)
		}
	}
}

// profileRow 取出唯一一列屬於指定 Profile 的 Session。
func profileRow(t *testing.T, rows []sessionRow, profileName string) sessionRow {
	t.Helper()
	var found []sessionRow
	for _, r := range rows {
		if r.profileName == profileName {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("Profile %s 的資料列數 = %d, 期望 1: %+v", profileName, len(found), rows)
	}
	return found[0]
}

// TestChatHelpDescribesNewFlag 驗證 oryxos chat --help 說清楚 --new 會做什麼
// （歸檔當前 Session、開新對話），使用者不必翻文檔才敢用。
func TestChatHelpDescribesNewFlag(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"chat", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("chat --help: %v", err)
	}
	for _, want := range []string{"--new", "歸檔", "新對話"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help 輸出未含 %q: %q", want, out.String())
		}
	}
}

// TestChatProfileFlag 驗證 --profile 指定非預設 Profile 時載入對應 YAML。
func TestChatProfileFlag(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	work := "name: work\nidentity:\n  agent_name: Worker\n  prompt: 你是工作助理。\nprovider:\n  name: openai\n  model: gpt-4o-mini\n"
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "work.yaml"), []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "work", message: "哈囉"}); err != nil {
		t.Fatalf("runChat --profile work: %v", err)
	}
	if !strings.Contains(out.String(), "回應一：你好，我是 Oryx。") {
		t.Errorf("輸出未含回應內容: %q", out.String())
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name string
		// setup 回傳 baseDir 與 profile 名。
		setup   func(t *testing.T) (dir, profileName string)
		wantSub string
	}{
		{
			name: "Workspace 未初始化",
			setup: func(t *testing.T) (string, string) {
				return t.TempDir(), "default"
			},
			wantSub: "oryxos init",
		},
		{
			name: "Profile 不存在",
			setup: func(t *testing.T) (string, string) {
				return setupChatWorkspace(t, "http://127.0.0.1:1"), "ghost"
			},
			wantSub: "ghost",
		},
		{
			name: "API key 環境變數未設定",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				os.Unsetenv("OPENAI_API_KEY") // t.Setenv 已註冊測試結束時還原
				return dir, "default"
			},
			wantSub: "OPENAI_API_KEY",
		},
		{
			name: "Profile 引用未註冊的 Tool",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				p := "name: default\nprovider:\n  name: openai\n  model: m\ntools:\n  - no_such_tool\n"
				if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "default.yaml"), []byte(p), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir, "default"
			},
			wantSub: "no_such_tool",
		},
		{
			name: "Profile 引用的 Provider 未在設定檔配置",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				p := "name: default\nprovider:\n  name: mystery\n  model: m\n"
				if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "default.yaml"), []byte(p), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir, "default"
			},
			wantSub: "mystery",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, profileName := tt.setup(t)
			var out bytes.Buffer
			err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: profileName, message: "你好"})
			if err == nil {
				t.Fatal("期望錯誤，實際成功")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
			}
		})
	}
}
