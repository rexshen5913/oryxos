// 上下文壓縮的整合測試（ticket #48）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、
// ToolRegistry、SQLite 在 seam 之下全部用真的（憲法 4.3）。
//
// 斷言落在兩個外部產物上：
//
//   - **送往 Provider 的請求**：久遠的 Tool 結果整條換掉、最近的保留頭尾，
//     而訊息的數量、順序與 Tool 呼叫欄位一個都不許動
//   - **引擎落的結構化日誌**：壓縮發生時記得到，帶 Profile 名、被壓條數與預算
//
// 用 read_file 讀真實的大檔而不是回傳假內容，是因為票面的場景就是它：讀了幾個
// 各自完全合法的大檔之後，後續每個 iteration 把它們重送一次。真檔案讓「合法」
// 這件事不是靠測試碼宣稱的。
package core_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// contextCompactLogKey 是壓縮發生時落的結構化日誌事件鍵。
const contextCompactLogKey = "context_compacted"

// 整合測試用的預算與檔案大小。
//
// 預算刻意寫進 Profile 而不是靠生產預設值：那個預設是十萬 rune，要在整合測試裡
// 造出溢出得寫十萬字的檔案。**寫進 Profile 同時也是驗收**——它證明循環真的讀了
// 這個設定，而不是用寫死的常數（與死循環守衛那票的門檻測試同一個手法）。
const (
	compactBudgetRunes = 600
	compactFileRunes   = 2000
	compactFileCount   = 6
)

// compactFileHead／compactFileTail 產生第 n 份檔案的可辨識頭尾。
//
// 每份檔案的頭尾都不同，「保留的是最近那一份的頭尾」才驗得出來——全部長一樣的話，
// 一個把最久遠那條的內容留下來的實作也會過。
func compactFileHead(n int) string { return fmt.Sprintf("開頭%d", n) }
func compactFileTail(n int) string { return fmt.Sprintf("結尾%d", n) }

// compactFileName 是第 n 份大檔在 Workspace 裡的相對路徑。
func compactFileName(n int) string { return fmt.Sprintf("notes/big%d.md", n) }

// compactWorkspace 建一個 notes/ 底下放著 compactFileCount 份大檔的真實 Workspace。
//
// 每份 compactFileRunes 個 rune——**單獨看每一份都遠低於 read_file 的 1 MiB 回填
// 上限，完全合法**。票面點名的正是這個形狀：沒有任何一次呼叫犯規，膨脹來自它們
// 被重送的次數。
func compactWorkspace(t *testing.T) *os.Root {
	t.Helper()
	root, dir := newTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	for n := 1; n <= compactFileCount; n++ {
		head, tail := compactFileHead(n), compactFileTail(n)
		pad := compactFileRunes - len([]rune(head)) - len([]rune(tail))
		body := head + strings.Repeat("內", pad) + tail
		if err := os.WriteFile(filepath.Join(dir, compactFileName(n)), []byte(body), 0o644); err != nil {
			t.Fatalf("寫入 %s: %v", compactFileName(n), err)
		}
	}
	return root
}

// compactToolCall 產生讀第 n 份大檔的錄製回應。
func compactToolCall(t *testing.T, n int) string {
	t.Helper()
	return strings.NewReplacer(
		"{{ID}}", jsonString(t, fmt.Sprintf("call_compact_%d", n)),
		"{{ARGS}}", jsonString(t, fmt.Sprintf(`{"path":%q}`, compactFileName(n))),
	).Replace(readFixture(t, "reply_context_compact_tool_call.json"))
}

// compactProfile 回傳把上下文預算設成 compactBudgetRunes 的 Profile。
func compactProfile() *core.Profile {
	p := testProfile()
	p.Settings.MaxContextRunes = compactBudgetRunes
	return p
}

// newCompactAgent 組出只註冊 read_file、**引擎層**日誌送進 logger 的 AgentService。
//
// 與 newLoopGuardAgent 同一個理由自己組一次：既有的 newToolAgentIn 一族把引擎
// logger 硬編成 discardLogger()，而 context_compacted 是 ReAct 循環落的，屬引擎
// 那一層。不去改既有 helper 的簽章——那會讓十幾支不關心引擎日誌的測試各多一個參數。
func newCompactAgent(t *testing.T, baseURL string, profile *core.Profile,
	root *os.Root, st *testStore, logger *slog.Logger) *core.AgentService {
	t.Helper()
	r := tool.NewRegistry()
	sandbox := tool.SandboxConfig{AllowedPaths: []string{"notes"}}
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(sandbox), root,
		testShellRuntime(t, root.Name()), tool.NewShellLimiter()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	exec, err := r.Subset([]string{tool.ReadFileToolName}, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(profile, svc, exec, newMemory(t, st.sessions()),
		st.audit, noBootstrap(t), core.NopEventSink{}, nil, logger)
}

// sentToolMessages 取出一次 LLM 邊界請求裡**全部**的 tool 訊息內容，依原順序。
//
// 既有的 sentToolMessage 只回最後一條；本票要比較的是「久遠的那條」與「最近的那條」
// 之間的差別，非得看到全部不可。
func sentToolMessages(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, m := range parseLLMRequest(t, body).Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// assertToolPairing 斷言一次請求的訊息序列仍是合法的 tool 序列：每條 tool 訊息的
// tool_call_id 都找得到前面某條 assistant 訊息宣告過的同一個 ID。
//
// 這是票面點名的那一格——兩層機制（turn 級截斷與本票的壓縮）能安全疊加的關鍵。
// turn 級截斷靠「截斷點落 user 邊界」守住成對，壓縮靠「不動數量與順序」守住；
// 兩條各自成立不代表疊起來成立，所以在真的疊過一次之後驗一次。
func assertToolPairing(t *testing.T, body []byte) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range parseLLMRequest(t, body).Messages {
		for _, c := range m.ToolCalls {
			declared[c.ID] = true
		}
		if m.Role != "tool" {
			continue
		}
		if m.ToolCallID == "" {
			t.Errorf("tool 訊息沒有 tool_call_id，OpenAI 兼容協議要求它必填: %q", m.Content)
			continue
		}
		if !declared[m.ToolCallID] {
			t.Errorf("tool 訊息的 tool_call_id %q 在它之前沒有任何 assistant 宣告過——序列已經不成對",
				m.ToolCallID)
		}
	}
}

// TestContextCompactedWhenOverBudget 是本票主場景：連讀六份各 2000 rune 的大檔，
// 最後一次請求裡久遠的結果已被整條換掉、最近的保留頭尾。
//
// 三條不變式與合法 tool 序列都掛在**同一次執行**下的子測試：它們描述的是同一份
// 產物的不同面向，分成各自跑一次的測試會讓「數量對、順序卻錯」這種組合從縫隙漏掉。
func TestContextCompactedWhenOverBudget(t *testing.T) {
	fixtures := make([]string, 0, compactFileCount+1)
	for n := 1; n <= compactFileCount; n++ {
		fixtures = append(fixtures, compactToolCall(t, n))
	}
	fixtures = append(fixtures, readFixture(t, "reply_context_compact_final.json"))

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, fixtures...)
	var logs strings.Builder
	db := newStore(t)
	agent := newCompactAgent(t, srv.URL, compactProfile(), compactWorkspace(t), db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把 notes 底下六份筆記都讀一遍"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 最後一次請求帶著全部六條 Tool 結果，是壓縮壓力最大的那一次。
	last := reqs[len(reqs)-1]
	sent := sentToolMessages(t, last)
	if len(sent) != compactFileCount {
		t.Fatalf("最後一次請求帶了 %d 條 tool 訊息, 期望 %d——壓縮不得改變訊息的數量",
			len(sent), compactFileCount)
	}

	t.Run("久遠的 Tool 結果被整條換掉", func(t *testing.T) {
		oldest := sent[0]
		if strings.Contains(oldest, compactFileHead(1)) || strings.Contains(oldest, compactFileTail(1)) {
			t.Errorf("最久遠的 Tool 結果仍原封不動地被重送: %q", oldest)
		}
		if oldest == "" {
			t.Error("最久遠的 Tool 結果被換成空字串——佔位說明要能讓 LLM 知道這裡曾經有東西")
		}
	})

	t.Run("最近的 Tool 結果保留開頭與結尾", func(t *testing.T) {
		newest := sent[len(sent)-1]
		if !strings.Contains(newest, compactFileHead(compactFileCount)) {
			t.Errorf("最近的 Tool 結果掉了開頭: %q", newest)
		}
		if !strings.Contains(newest, compactFileTail(compactFileCount)) {
			t.Errorf("最近的 Tool 結果掉了結尾: %q", newest)
		}
		if n := len([]rune(newest)); n >= compactFileRunes {
			t.Errorf("最近的 Tool 結果 %d rune，沒有被截短——它超過單條上限了", n)
		}
	})

	t.Run("不變式一：系統提示詞不被修改", func(t *testing.T) {
		msgs := parseLLMRequest(t, last).Messages
		if len(msgs) == 0 || msgs[0].Role != "system" {
			t.Fatalf("第一條訊息不是 system: %+v", msgs)
		}
		first := parseLLMRequest(t, reqs[0]).Messages
		if msgs[0].Content != first[0].Content {
			t.Errorf("系統提示詞被壓縮動過了:\n壓縮後 %q\n第一次 %q", msgs[0].Content, first[0].Content)
		}
	})

	t.Run("不變式二：Tool 呼叫欄位不被修改", func(t *testing.T) {
		var ids []string
		for _, m := range parseLLMRequest(t, last).Messages {
			for _, c := range m.ToolCalls {
				if c.Function.Name != tool.ReadFileToolName {
					t.Errorf("tool_calls 的 Tool 名被動過了: %q", c.Function.Name)
				}
				ids = append(ids, c.ID)
			}
		}
		if len(ids) != compactFileCount {
			t.Fatalf("tool_calls 共 %d 筆, 期望 %d——那是模型行動的證據，不是壓縮對象",
				len(ids), compactFileCount)
		}
		for n, id := range ids {
			if want := fmt.Sprintf("call_compact_%d", n+1); id != want {
				t.Errorf("tool_calls[%d].id = %q, 期望 %q", n, id, want)
			}
		}
	})

	t.Run("不變式三：訊息的數量與順序不變", func(t *testing.T) {
		msgs := parseLLMRequest(t, last).Messages
		// system ＋ user ＋（assistant, tool）×N
		want := 2 + compactFileCount*2
		if len(msgs) != want {
			t.Fatalf("訊息數 = %d, 期望 %d", len(msgs), want)
		}
		wantRoles := append([]string{"system", "user"}, func() []string {
			var rs []string
			for range compactFileCount {
				rs = append(rs, "assistant", "tool")
			}
			return rs
		}()...)
		for i, want := range wantRoles {
			if msgs[i].Role != want {
				t.Errorf("messages[%d].Role = %q, 期望 %q", i, msgs[i].Role, want)
			}
		}
	})

	t.Run("壓縮後仍是合法的 tool 序列", func(t *testing.T) {
		assertToolPairing(t, last)
	})

	t.Run("壓縮發生時落結構化警告日誌", func(t *testing.T) {
		found := findLogRecords(t, logs.String(), contextCompactLogKey)
		if len(found) == 0 {
			t.Fatalf("沒有落 %s 日誌，壓縮就成了看不見的降級:\n%s", contextCompactLogKey, logs.String())
		}
		rec := found[len(found)-1]
		if rec["level"] != "WARN" {
			t.Errorf("日誌等級 = %v, 期望 WARN——比照 Skill 段截斷的既有作法", rec["level"])
		}
		if rec["profile"] != "default" {
			t.Errorf("profile 欄位 = %v, 期望 default", rec["profile"])
		}
		if got, ok := rec["compacted"].(float64); !ok || got <= 0 {
			t.Errorf("compacted 欄位 = %v, 期望大於 0 的數字", rec["compacted"])
		}
		if got, ok := rec["budget_runes"].(float64); !ok || int(got) != compactBudgetRunes {
			t.Errorf("budget_runes 欄位 = %v, 期望 %d——日誌要說得出用的是哪個預算",
				rec["budget_runes"], compactBudgetRunes)
		}
	})
}

// TestContextNotCompactedUnderBudget 釘住「未超出預算時完全不改動訊息，逐條位元組
// 相等」，也就是既有 Profile 免遷移的那一半。
//
// 斷言拿**兩個獨立來源**對：送給 Provider 的那條，與 Session 對話歷史裡的那條。
// Session 不經壓縮（壓縮只發生在讀取側），兩者逐位元組相等才算「完全沒改動」——
// 只驗「內容裡沒有省略標記」的話，一個把內容重新編碼一次的實作也會過。
func TestContextNotCompactedUnderBudget(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		compactToolCall(t, 1),
		readFixture(t, "reply_context_compact_final.json"),
	)
	db := newStore(t)
	// 用 testProfile()——它沒有設 max_context_runes，走的就是零值回退的預設值。
	// 一份 2000 rune 的檔案離十萬 rune 的預設預算還很遠，不該觸發任何壓縮。
	agent := newCompactAgent(t, srv.URL, testProfile(), compactWorkspace(t), db, discardLogger())
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "讀 notes/big1.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	sent := sentToolMessages(t, reqs[len(reqs)-1])
	if len(sent) != 1 {
		t.Fatalf("最後一次請求帶了 %d 條 tool 訊息, 期望 1", len(sent))
	}
	var stored string
	for _, m := range session.Messages {
		if m.Role == core.RoleTool {
			stored = m.Content
		}
	}
	if stored == "" {
		t.Fatal("Session 對話歷史裡沒有 tool 訊息，對照組不成立")
	}
	if sent[0] != stored {
		t.Errorf("未超預算卻改動了訊息:\n送出 %d bytes\n歷史 %d bytes", len(sent[0]), len(stored))
	}
	if !strings.Contains(sent[0], compactFileHead(1)) || !strings.Contains(sent[0], compactFileTail(1)) {
		t.Errorf("未超預算時內容該是完整的，卻少了頭或尾: %q", sent[0])
	}
}

// TestContextNotCompactedUnderBudgetLogsNothing 釘住警告日誌只在**真的壓縮了**
// 的時候落。
//
// 少了這一格的話，一個「每個 iteration 都落一次」的實作也會全綠——而那會讓日誌
// 從降級訊號退化成噪音，維運再也分不出哪一次是真的。
func TestContextNotCompactedUnderBudgetLogsNothing(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		compactToolCall(t, 1),
		readFixture(t, "reply_context_compact_final.json"),
	)
	db := newStore(t)
	var logs strings.Builder
	agent := newCompactAgent(t, srv.URL, testProfile(), compactWorkspace(t), db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "讀 notes/big1.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if found := findLogRecords(t, logs.String(), contextCompactLogKey); len(found) > 0 {
		t.Errorf("未超預算卻落了 %d 筆 %s 日誌:\n%s", len(found), contextCompactLogKey, logs.String())
	}
}
