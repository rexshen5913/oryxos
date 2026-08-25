// 長期記憶最小鏈路的整合測試（ticket #10）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——MEMORY.md 在 seam
// 之下用真實檔案（t.TempDir()，憲法 4.3）。斷言落在外部可觀察的產物上：MEMORY.md
// 的檔案內容與送往 LLM 邊界請求的 system prompt，不斷言內部呼叫序列，也不綁死
// 注入段的措辭與格式。
package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// memoryRelPath 是長期記憶檔在 Workspace 內的相對路徑。
var memoryRelPath = filepath.Join("memory", "MEMORY.md")

// workspaceRoot 在 t.TempDir() 開一個 Workspace 根——長期記憶的檔案操作一律限制
// 在它底下，越界由 os.Root 擋。回傳 root 與 MEMORY.md 的絕對路徑（後者供測試
// 直接讀寫檔案做斷言）；目錄與檔案都刻意不預先建立，lazy 建立是既定行為。
func workspaceRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return root, filepath.Join(dir, memoryRelPath)
}

// seedMemory 以真實檔案預置 MEMORY.md 的內容（模擬先前記下的長期記憶，或使用者
// 手動編輯的結果）。
func seedMemory(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建立 memory 目錄: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("預置 MEMORY.md: %v", err)
	}
}

// readMemory 讀回 MEMORY.md；檔案不存在時回空字串（等同空記憶）。
func readMemory(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}
	return string(data)
}

// saveMemoryFixture 把錄製回應中的 {{CONTENT}} 換成本次要寫入的記憶內容
// （沿用既有 fixture 模板化的做法，如 {{TARGET_URL}}）。
func saveMemoryFixture(t *testing.T, name, content string) string {
	t.Helper()
	return strings.ReplaceAll(readFixture(t, name), "{{CONTENT}}", content)
}

// newMemoryAgent 組出帶 save_memory 的 AgentService：真實 ToolRegistry 顯式註冊
// save_memory，長期記憶落 root 底下的真實 MEMORY.md，會話記憶落 t.TempDir() 下的
// 真實 SQLite；兩者都由 MemoryService 門面統一對外。subset 是 Profile 的 tools 欄位。
func newMemoryAgent(t *testing.T, baseURL string, root *os.Root, subset []string, logger *slog.Logger) *core.AgentService {
	t.Helper()
	return newMemoryAgentOn(t, baseURL, root, subset, logger, newStore(t))
}

// newMemoryAgentOn 同 newMemoryAgent，但用指定的 Session 儲存——`--new` 的歸檔
// 路徑要對同一個 db 檔先歸檔再開新 Session。
func newMemoryAgentOn(t *testing.T, baseURL string, root *os.Root, subset []string, logger *slog.Logger, st *testStore) *core.AgentService {
	t.Helper()
	longTerm := memory.NewLongTermMemory(root, memoryRelPath)
	r := tool.NewRegistry()
	for _, memTool := range []tool.OryxTool{memory.NewSaveMemoryTool(longTerm), memory.NewRecallMemoryTool(longTerm)} {
		if err := r.Register(memTool); err != nil {
			t.Fatalf("註冊 %s: %v", memTool.Name(), err)
		}
	}
	exec, err := r.Subset(subset, nil, logger)
	if err != nil {
		t.Fatalf("Subset(%v): %v", subset, err)
	}
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(testProfile(), svc, exec, memory.NewService(st.sessions(), longTerm), st.audit, config.NewContextLoader(root), core.NopEventSink{}, discardLogger())
}

// recallMemoryFixture 把錄製回應中的 {{QUERY}} 換成本次檢索的關鍵詞。
func recallMemoryFixture(t *testing.T, query string) string {
	t.Helper()
	return strings.ReplaceAll(readFixture(t, "reply_recall_memory.json"), "{{QUERY}}", query)
}

// seededEntries 組出一份 n 條、每條 runesPerEntry 字的長期記憶（同一天）。
func seededEntries(n, runesPerEntry int) string {
	var b strings.Builder
	b.WriteString("## 2026-08-01\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- 第%02d條-%s\n", i, strings.Repeat("記", runesPerEntry))
	}
	return b.String()
}

// systemPrompt 取出 LLM 邊界請求的 system 訊息內容（本切片只有一條）。
func systemPrompt(t *testing.T, body []byte) string {
	t.Helper()
	for _, m := range parseLLMRequest(t, body).Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// TestProcessSaveMemoryWritesFileAndInjectsNextTurn 是本票主場景：LLM 自主呼叫
// save_memory 把偏好寫進 MEMORY.md（真實檔案、帶日期 header），下一個 turn 的
// LLM 邊界請求 system prompt 就帶著它——turn 之間重讀、不緩存。
func TestProcessSaveMemoryWritesFileAndInjectsNextTurn(t *testing.T) {
	const fact = "使用者的專案用 Go 開發，部署在 K8s"
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs,
		saveMemoryFixture(t, "reply_save_memory.json", fact),
		readFixture(t, "reply_memory_saved.json"),
		readFixture(t, "reply_memory_recall_answer.json"))

	root, memPath := workspaceRoot(t)
	agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"}, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "我的專案用 Go 開發，部署在 K8s"); err != nil {
		t.Fatalf("第一個 turn: %v", err)
	}

	// 檔案產物：內容與日期 header 都在。
	file := readMemory(t, memPath)
	if !strings.Contains(file, fact) {
		t.Errorf("MEMORY.md 未含 save_memory 寫入的內容: %q", file)
	}
	if today := time.Now().Format("2006-01-02"); !strings.Contains(file, today) {
		t.Errorf("MEMORY.md 未含日期 header %s: %q", today, file)
	}

	// 下一個 turn：system prompt 帶上剛寫入的長期記憶。
	if _, err := agent.Process(context.Background(), session, "幫我寫個 HTTP handler"); err != nil {
		t.Fatalf("第二個 turn: %v", err)
	}
	got := systemPrompt(t, llmReqs[len(llmReqs)-1])
	if !strings.Contains(got, fact) {
		t.Errorf("下一個 turn 的 system prompt 未注入長期記憶: %q", got)
	}
	if !strings.Contains(got, testProfile().Identity.Prompt) {
		t.Errorf("system prompt 遺失 identity.prompt: %q", got)
	}
}

// TestLongTermMemorySnapshotPerTurn 釘死本票的關鍵行為：長期記憶是 **turn 級**
// 的快照，對話歷史是 **iteration 級**的即時累積。
//
// 正向錨點：同一次 Process 內 LLM 第一輪呼叫 save_memory 之後，第二次 LLM 邊界
// 請求的 system prompt 記憶段維持不變（不含剛寫入的內容）——證明載入點在迭代
// 迴圈之外。反向錨點：同一次請求仍含第一輪的 assistant 與 tool 訊息——對話歷史
// 絕不快照，否則 ReAct 循環看不到 tool 結果回填就直接壞掉。
func TestLongTermMemorySnapshotPerTurn(t *testing.T) {
	const (
		seeded = "使用者偏好繁體中文回覆"
		fresh  = "使用者的專案用 Go 開發"
	)
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs,
		saveMemoryFixture(t, "reply_save_memory.json", fresh),
		readFixture(t, "reply_memory_saved.json"),
		readFixture(t, "reply_memory_recall_answer.json"))

	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- "+seeded+"\n")
	agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"}, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "我的專案用 Go 開發"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(llmReqs) != 2 {
		t.Fatalf("LLM 邊界請求數 = %d, 期望 2（第一輪呼叫 save_memory、第二輪最終回應）", len(llmReqs))
	}

	// 正向：turn 內第二次請求的記憶段沒有變動。
	second := systemPrompt(t, llmReqs[1])
	if !strings.Contains(second, seeded) {
		t.Errorf("turn 內第二次請求遺失 turn 開始時的長期記憶快照: %q", second)
	}
	if strings.Contains(second, fresh) {
		t.Errorf("turn 內第二次請求含本 turn 剛寫入的記憶（載入點跑進迭代迴圈了）: %q", second)
	}

	// 反向：對話歷史照常累積，第一輪的 assistant 與 tool 訊息都在。
	req := parseLLMRequest(t, llmReqs[1])
	var sawToolCall, sawToolResult bool
	for _, m := range req.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].Function.Name == "save_memory" {
			sawToolCall = true
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("turn 內第二次請求缺第一輪的 assistant／tool 訊息（對話歷史被快照了）: assistant=%v tool=%v", sawToolCall, sawToolResult)
	}

	// turn 之間確實重讀：下一個 turn 才看得到本 turn 寫入的內容。
	if _, err := agent.Process(context.Background(), session, "幫我寫個 HTTP handler"); err != nil {
		t.Fatalf("下一個 turn: %v", err)
	}
	third := systemPrompt(t, llmReqs[2])
	if !strings.Contains(third, fresh) || !strings.Contains(third, seeded) {
		t.Errorf("下一個 turn 未重讀 MEMORY.md（應含新舊兩條）: %q", third)
	}
}

// TestSaveMemoryContentValidation 是 save_memory 的長度校驗矩陣（憲法 4.2）：
// 單條上限 1000 **rune**（非 byte——中文一字一 rune），空字串與純空白拒絕。
// 拒絕時不寫入檔案、錯誤訊息含目前 rune 數與上限，且不進指數退避重試路徑
// （參數校驗失敗不是瞬時故障）。
func TestSaveMemoryContentValidation(t *testing.T) {
	tests := []struct {
		name string
		// content 是 LLM 傳給 save_memory 的原始內容。
		content string
		// wantWritten 為 true 時該內容應出現在 MEMORY.md。
		wantWritten bool
	}{
		{name: "空字串拒絕", content: "", wantWritten: false},
		{name: "純空白拒絕", content: "   ", wantWritten: false},
		{name: "恰好 1000 rune 中文寫入（以 rune 計、非 byte）", content: strings.Repeat("記", 1000), wantWritten: true},
		{name: "1001 rune 拒絕", content: strings.Repeat("記", 1001), wantWritten: false},
		{name: "多位元組中文短句寫入", content: "使用者偏好繁體中文回覆", wantWritten: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			final := "reply_memory_saved.json"
			if !tt.wantWritten {
				final = "reply_memory_giveup.json"
			}
			srv := newReplayServer(t,
				saveMemoryFixture(t, "reply_save_memory.json", tt.content),
				readFixture(t, final))

			var logBuf bytes.Buffer
			root, memPath := workspaceRoot(t)
			agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"},
				slog.New(slog.NewJSONHandler(&logBuf, nil)))
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "記住這件事"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			file := readMemory(t, memPath)
			trimmed := strings.TrimSpace(tt.content)
			// 空內容不可能「寫進去」——用 Contains 判斷會恆真，須先排除。
			written := trimmed != "" && strings.Contains(file, trimmed)
			if written != tt.wantWritten {
				t.Errorf("內容寫入 MEMORY.md = %v, 期望 %v（檔案 %d bytes）", written, tt.wantWritten, len(file))
			}

			toolMsg := lastToolMessage(t, session)
			if tt.wantWritten {
				return
			}
			if strings.TrimSpace(file) != "" {
				t.Errorf("拒絕的內容不該讓 MEMORY.md 有東西: %q", file)
			}
			// 拒絕的回填要讓模型看得懂怎麼改：目前 rune 數、上限、下一步。
			if runes := len([]rune(trimmed)); runes > 0 {
				if !strings.Contains(toolMsg, fmt.Sprint(runes)) || !strings.Contains(toolMsg, "1000") {
					t.Errorf("回填訊息未含目前 rune 數 %d 與上限 1000: %q", runes, toolMsg)
				}
			}
			if !strings.Contains(toolMsg, "save_memory") {
				t.Errorf("回填訊息未指示重新呼叫 save_memory: %q", toolMsg)
			}
			// 參數校驗失敗不可重試：只執行一次，不進指數退避。
			if got := strings.Count(logBuf.String(), `"msg":"tool_invocation"`); got != 1 {
				t.Errorf("tool_invocation 日誌筆數 = %d, 期望 1（校驗失敗不重試）", got)
			}
		})
	}
}

// lastToolMessage 回傳 Session 對話歷史中最後一條 tool 訊息的內容。
func lastToolMessage(t *testing.T, session *core.Session) string {
	t.Helper()
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == core.RoleTool {
			return session.Messages[i].Content
		}
	}
	t.Fatal("對話歷史中沒有 tool 訊息")
	return ""
}

// TestSaveMemoryRejectedBranches 覆蓋被拒之後迴圈如何收尾的兩條分支——只測到
// 錯誤回填、測不到收尾，等於沒驗證這條路徑走得完。
func TestSaveMemoryRejectedBranches(t *testing.T) {
	tooLong := strings.Repeat("長", 1001)
	const shortened = "使用者的專案用 Go 開發"

	tests := []struct {
		name        string
		fixtures    []string
		wantWritten string // 期望出現在 MEMORY.md 的內容；空字串表示檔案應維持空
		wantReply   string
	}{
		{
			name:        "被拒後精簡內容重新呼叫並成功寫入",
			fixtures:    []string{"reply_save_memory_2.json", "reply_memory_saved.json"},
			wantWritten: shortened,
			wantReply:   "好的，我已經記住了，之後的對話會用上。",
		},
		{
			name:        "被拒後放棄寫入、直接回覆使用者",
			fixtures:    []string{"reply_memory_giveup.json"},
			wantWritten: "",
			wantReply:   "這段內容太長了，我沒有存進長期記憶。你可以挑重點再告訴我一次，我再記下來。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixtures := []string{saveMemoryFixture(t, "reply_save_memory.json", tooLong)}
			for _, name := range tt.fixtures {
				fixtures = append(fixtures, saveMemoryFixture(t, name, shortened))
			}
			srv := newReplayServer(t, fixtures...)

			root, memPath := workspaceRoot(t)
			agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"}, discardLogger())
			session := core.NewSession("cli", "local", "default")

			resp, err := agent.Process(context.Background(), session, "記住這一大段")
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if resp != tt.wantReply {
				t.Errorf("最終回應 = %q, 期望 %q", resp, tt.wantReply)
			}

			file := readMemory(t, memPath)
			if strings.Contains(file, tooLong) {
				t.Errorf("超長內容被寫入 MEMORY.md: %d bytes", len(file))
			}
			if tt.wantWritten == "" {
				if strings.TrimSpace(file) != "" {
					t.Errorf("MEMORY.md 應維持空，實際: %q", file)
				}
				return
			}
			if !strings.Contains(file, tt.wantWritten) {
				t.Errorf("MEMORY.md 未含精簡後的內容: %q", file)
			}
		})
	}
}

// TestLongTermMemoryLoadMatrix 是 MEMORY.md 載入矩陣（憲法 4.2）：缺檔與空檔
// 視為空記憶、對話照常，不報錯也不注入空段落。
func TestLongTermMemoryLoadMatrix(t *testing.T) {
	tests := []struct {
		name string
		// seed 為 nil 表示不建立檔案。
		seed        *string
		wantInject  string
		wantNoIntro bool // true 表示 system prompt 不該出現長期記憶段
	}{
		{name: "檔案不存在：視為空記憶", seed: nil, wantNoIntro: true},
		{name: "空檔：視為空記憶", seed: ptr(""), wantNoIntro: true},
		{name: "只有空白：視為空記憶", seed: ptr("\n  \n"), wantNoIntro: true},
		{name: "有內容：注入 system prompt", seed: ptr("## 2026-08-01\n\n- 使用者偏好繁體中文回覆\n"), wantInject: "使用者偏好繁體中文回覆"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
			root, memPath := workspaceRoot(t)
			if tt.seed != nil {
				seedMemory(t, memPath, *tt.seed)
			}
			agent := newMemoryAgent(t, srv.URL, root, nil, discardLogger())
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("Process: %v", err)
			}

			got := systemPrompt(t, llmReqs[0])
			if !strings.Contains(got, testProfile().Identity.Prompt) {
				t.Errorf("system prompt 遺失 identity.prompt: %q", got)
			}
			if tt.wantInject != "" && !strings.Contains(got, tt.wantInject) {
				t.Errorf("system prompt 未注入長期記憶 %q: %q", tt.wantInject, got)
			}
			if tt.wantNoIntro && got != testProfile().Identity.Prompt {
				t.Errorf("空記憶不該讓 system prompt 多出東西: %q", got)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// TestLongTermMemoryUnreadableFailsTurn 驗證真實故障不被靜默吞成「沒有記憶」：
// MEMORY.md 存在但讀不到（權限）時該 turn 明確失敗，錯誤鏈保留底層原因。
func TestLongTermMemoryUnreadableFailsTurn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時檔案權限不生效，跳過")
	}
	srv := newReplayServer(t) // 不期望任何 LLM 呼叫：載入失敗應在呼叫之前
	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- 使用者偏好繁體中文回覆\n")
	if err := os.Chmod(memPath, 0o000); err != nil {
		t.Fatalf("移除 MEMORY.md 讀取權限: %v", err)
	}

	agent := newMemoryAgent(t, srv.URL, root, nil, discardLogger())
	session := core.NewSession("cli", "local", "default")

	_, err := agent.Process(context.Background(), session, "你好")
	if err == nil {
		t.Fatal("期望該 turn 失敗，實際成功——讀取故障被靜默當成空記憶")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("錯誤未以 %%w 保留底層權限錯誤: %v", err)
	}
	if len(session.Messages) != 0 {
		t.Errorf("失敗 turn 未 rollback：對話歷史 = %d 條", len(session.Messages))
	}
}

// TestLongTermMemoryRereadEachTurn 驗證使用者手動編輯 MEMORY.md 後，下一個 turn
// 注入的是新內容（每個 turn 重讀、無 in-memory cache 的外部行為）。
func TestLongTermMemoryRereadEachTurn(t *testing.T) {
	const (
		before = "使用者偏好繁體中文回覆"
		after  = "使用者改用英文溝通"
	)
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs,
		readFixture(t, "reply_turn1.json"), readFixture(t, "reply_turn2.json"))

	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- "+before+"\n")
	agent := newMemoryAgent(t, srv.URL, root, nil, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "第一句"); err != nil {
		t.Fatalf("第一個 turn: %v", err)
	}
	// 使用者直接編輯檔案（MEMORY.md 是 git 追蹤的純文字檔，可手改）。
	seedMemory(t, memPath, "## 2026-08-02\n\n- "+after+"\n")
	if _, err := agent.Process(context.Background(), session, "第二句"); err != nil {
		t.Fatalf("第二個 turn: %v", err)
	}

	got := systemPrompt(t, llmReqs[1])
	if !strings.Contains(got, after) {
		t.Errorf("下一個 turn 未注入手改後的內容: %q", got)
	}
	if strings.Contains(got, before) {
		t.Errorf("下一個 turn 仍注入舊內容（有緩存）: %q", got)
	}
}

// TestNewSessionInjectsLongTermMemory 驗證長期記憶跨 Session：同一 Workspace 的
// 新對話，第一個 turn 的 LLM 邊界請求就帶著先前記下的內容——這正是 Demo 二
// 「第二次對話 Agent 仍記得偏好」的機制。
func TestNewSessionInjectsLongTermMemory(t *testing.T) {
	const fact = "使用者的專案用 Go 開發"
	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- "+fact+"\n")

	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_memory_recall_answer.json"))
	// 另一場對話用自己的 Session 儲存（同一聯合標識同時至多一個 active）；
	// 本測要驗的是新 Session 第一個 turn 就帶長期記憶，歸檔路徑歸 ticket #9。
	agent := newMemoryAgent(t, srv.URL, root, nil, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "幫我寫個 HTTP handler"); err != nil {
		t.Fatalf("新 Session 第一個 turn: %v", err)
	}
	if got := systemPrompt(t, llmReqs[0]); !strings.Contains(got, fact) {
		t.Errorf("新 Session 第一個 turn 未注入長期記憶: %q", got)
	}
}

// TestMemoryToolsFilteredByProfile 驗證兩個 Memory Tool 與其他內建 Tool 一視同仁
// 地受 Profile 的 tools 欄位過濾：未列出時不出現在送往 LLM 的 tool 清單。
func TestMemoryToolsFilteredByProfile(t *testing.T) {
	tests := []struct {
		name   string
		subset []string
		want   []string // 應出現在 LLM tool 清單的 Memory Tool
	}{
		{name: "兩個都列出", subset: []string{"save_memory", "recall_memory"}, want: []string{"save_memory", "recall_memory"}},
		{name: "只列出 save_memory：recall_memory 不出現", subset: []string{"save_memory"}, want: []string{"save_memory"}},
		{name: "只列出 recall_memory：save_memory 不出現", subset: []string{"recall_memory"}, want: []string{"recall_memory"}},
		{name: "都未列出：兩個都不出現", subset: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var llmReqs [][]byte
			srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))
			root, _ := workspaceRoot(t)
			agent := newMemoryAgent(t, srv.URL, root, tt.subset, discardLogger())
			session := core.NewSession("cli", "local", "default")

			if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
				t.Fatalf("Process: %v", err)
			}
			var got []string
			for _, declared := range parseLLMRequest(t, llmReqs[0]).Tools {
				if name := declared.Function.Name; name == "save_memory" || name == "recall_memory" {
					got = append(got, name)
				}
			}
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("LLM tool 清單中的 Memory Tool = %v, 期望 %v", got, want)
			}
		})
	}
}

// TestProcessRecallMemoryScenario 是 recall_memory 的主場景：LLM 第一輪呼叫
// recall_memory、匹配行回填進對話歷史、第二輪據此回答。無匹配時回填明確的
// 「沒有找到」結果而不是錯誤，迴圈照常收尾。
func TestProcessRecallMemoryScenario(t *testing.T) {
	const stored = "使用者的專案用 Go 開發"

	tests := []struct {
		name string
		// query 是 LLM 傳給 recall_memory 的關鍵詞。
		query string
		// finalFixture 是第二輪的最終回應。
		finalFixture string
		// wantInTool 是 tool 訊息必須含的子串。
		wantInTool string
		wantReply  string
	}{
		{
			name:         "有匹配：匹配行回填後據此回答",
			query:        "Go",
			finalFixture: "reply_memory_recall_answer.json",
			wantInTool:   stored,
			wantReply:    "依你先前告訴我的，你的專案用 Go 開發，所以我直接用 Go 的慣例來說明。",
		},
		{
			name:         "無匹配：回填明確的沒有找到，不報錯",
			query:        "Rust",
			finalFixture: "reply_recall_nomatch.json",
			wantInTool:   "沒有",
			wantReply:    "我在長期記憶裡沒有找到相關的內容，你要不要直接告訴我？",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, recallMemoryFixture(t, tt.query), readFixture(t, tt.finalFixture))
			root, memPath := workspaceRoot(t)
			seedMemory(t, memPath, "## 2026-08-01\n\n- "+stored+"\n- 部署在 K8s\n")
			agent := newMemoryAgent(t, srv.URL, root, []string{"recall_memory"}, discardLogger())
			session := core.NewSession("cli", "local", "default")

			resp, err := agent.Process(context.Background(), session, "我的專案是用什麼寫的？")
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if resp != tt.wantReply {
				t.Errorf("最終回應 = %q, 期望 %q", resp, tt.wantReply)
			}
			if got := lastToolMessage(t, session); !strings.Contains(got, tt.wantInTool) {
				t.Errorf("tool 訊息 = %q, 期望含 %q", got, tt.wantInTool)
			}
		})
	}
}

// TestRecallMemoryEchoBounded 驗證回填內容引述關鍵詞時自己有上限：LLM 送來一個
// 超長 query，「沒有符合的內容」這則回填不能就這樣把它原樣倒回去——那會讓這條
// 路徑無視 4000 rune 的約束，而它同樣會進下一次 LLM 請求。
func TestRecallMemoryEchoBounded(t *testing.T) {
	longQuery := strings.Repeat("查", 5000)
	srv := newReplayServer(t, recallMemoryFixture(t, longQuery), readFixture(t, "reply_recall_nomatch.json"))
	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- 使用者的專案用 Go 開發\n")
	agent := newMemoryAgent(t, srv.URL, root, []string{"recall_memory"}, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "查一下"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	got := lastToolMessage(t, session)
	if n := utf8.RuneCountInString(got); n > 4000 {
		t.Errorf("回填內容 %d 字，超過長期記憶的注入上限", n)
	}
	if !strings.Contains(got, "沒有") {
		t.Errorf("回填未說明沒有匹配: %q", got)
	}
}

// TestLongTermMemoryInjectionTruncated 驗證注入 system prompt 的內容超閾值時同樣
// 截斷（LLM 邊界請求斷言）：最舊的條目被丟掉、最近的留著，整份記憶不會原封不動
// 撐爆每一次請求。確切的閾值與邊界行為由 internal/memory 的截斷矩陣釘死。
func TestLongTermMemoryInjectionTruncated(t *testing.T) {
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs, readFixture(t, "reply_direct.json"))

	root, memPath := workspaceRoot(t)
	seeded := seededEntries(20, 500) // 遠超注入上限
	seedMemory(t, memPath, seeded)
	agent := newMemoryAgent(t, srv.URL, root, nil, discardLogger())
	session := core.NewSession("cli", "local", "default")

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	got := systemPrompt(t, llmReqs[0])
	if strings.Contains(got, "第00條") {
		t.Error("最舊的記憶未被截掉")
	}
	if !strings.Contains(got, "第19條") {
		t.Error("最近的記憶被截掉了（截斷應保留最近內容）")
	}
	if utf8.RuneCountInString(got) >= utf8.RuneCountInString(seeded) {
		t.Errorf("system prompt %d 字，整份記憶 %d 字——看不出截斷",
			utf8.RuneCountInString(got), utf8.RuneCountInString(seeded))
	}
}

// TestNewConversationStillInjectsLongTermMemory 是與 ticket #9 的複合斷言：走
// `oryxos chat --new` 的歸檔路徑開出的新 Session，第一個 turn 就帶著長期記憶
// ——這正是 Demo 二「第二次對話 Agent 仍記得偏好」要證明的東西，而且證明的是
// 跨 Session 的記憶注入，不是同場對話歷史。
func TestNewConversationStillInjectsLongTermMemory(t *testing.T) {
	const fact = "使用者的專案用 Go 開發"
	var llmReqs [][]byte
	srv := newRecordingReplayServer(t, &llmReqs,
		readFixture(t, "reply_turn1.json"),
		readFixture(t, "reply_memory_recall_answer.json"))

	root, memPath := workspaceRoot(t)
	seedMemory(t, memPath, "## 2026-08-01\n\n- "+fact+"\n")
	db := openStore(t, filepath.Join(t.TempDir(), "oryxos.db"))
	store := db.sessions()
	agent := newMemoryAgentOn(t, srv.URL, root, nil, discardLogger(), db)

	session := activeSession(t, store)
	if _, err := agent.Process(context.Background(), session, "第一句"); err != nil {
		t.Fatalf("第一場對話: %v", err)
	}

	// `oryxos chat --new`：歸檔當前 active Session，再開一場乾淨的新對話。
	if err := store.ArchiveActive(context.Background(), "cli", "local", "default"); err != nil {
		t.Fatalf("歸檔 active Session: %v", err)
	}
	fresh := activeSession(t, store)
	if fresh.ID == session.ID {
		t.Fatal("歸檔後未開出新的 Session")
	}
	if _, err := agent.Process(context.Background(), fresh, "第二句"); err != nil {
		t.Fatalf("新對話第一個 turn: %v", err)
	}

	if got := systemPrompt(t, llmReqs[1]); !strings.Contains(got, fact) {
		t.Errorf("--new 開的新 Session 第一個 turn 未注入長期記憶: %q", got)
	}
	// 同時確認確實是新對話：舊 Session 的訊息不進請求。
	for _, m := range parseLLMRequest(t, llmReqs[1]).Messages {
		if strings.Contains(m.Content, "第一句") {
			t.Errorf("新 Session 帶進了舊對話的訊息: %q", m.Content)
		}
	}
}

// TestSaveMemoryWriteFailureBackfilled 驗證寫入失敗（memory 目錄不可寫）不 crash
// 循環：錯誤作為 tool 結果回填給 LLM，由 LLM 決定下一步，turn 正常收尾。
func TestSaveMemoryWriteFailureBackfilled(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 執行時目錄權限不生效，跳過")
	}
	const fact = "使用者的專案用 Go 開發"
	srv := newReplayServer(t,
		saveMemoryFixture(t, "reply_save_memory.json", fact),
		readFixture(t, "reply_memory_giveup.json"))

	root, memPath := workspaceRoot(t)
	if err := os.MkdirAll(filepath.Dir(memPath), 0o555); err != nil {
		t.Fatalf("建立唯讀 memory 目錄: %v", err)
	}
	agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"}, discardLogger())
	session := core.NewSession("cli", "local", "default")

	resp, err := agent.Process(context.Background(), session, "記住這件事")
	if err != nil {
		t.Fatalf("Tool 寫入失敗不該讓 turn 失敗: %v", err)
	}
	if resp == "" {
		t.Error("turn 未正常收尾，沒有最終回應")
	}
	if got := lastToolMessage(t, session); !strings.Contains(got, "save_memory") {
		t.Errorf("回填訊息未說明是 save_memory 失敗: %q", got)
	}
	if readMemory(t, memPath) != "" {
		t.Error("寫入失敗卻留下了 MEMORY.md 內容")
	}
}
