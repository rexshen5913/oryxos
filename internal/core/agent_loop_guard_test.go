// 死循環守衛的整合測試（ticket #54）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Workspace、
// ToolRegistry、SQLite 在 seam 之下全部用真的（憲法 4.3）。
//
// 斷言落在三個外部產物上：
//
//   - **送往 Provider 的 tool 訊息**：門檻前不附提示、達門檻才附，且三段順序固定
//   - **引擎落的結構化日誌**：觸發時記得到，帶 Tool 名與規範化後的參數
//   - **Session 與其落庫內容**：訊息數與角色序列不因守衛而變，恢復後能原樣重放
//
// 用 write_file 而不是 read_file 當主場景是刻意的：它有 path 與 content 兩個欄位，
// 「鍵序不同」這種等價寫法才表現得出來（read_file 只有一個欄位，鍵序無從不同）。
// 失敗選「父目錄不存在」——那是 not_found（有類型指引，驗得到三段順序），而且
// 它在開檔之前就被擋下，磁碟上不留任何痕跡，重複呼叫的結果每次都一樣。
package core_test

import (
	"encoding/json"
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

// loopGuardNoticeMark 是死循環提示裡要斷言的那句話。
//
// **斷言到措辭是刻意的**，與 #36 那則 shell 專屬訊息同一個理由：這段話本身就是本票
// 的交付物。守衛的價值不在於「有附加東西」，而在於附加的那句話真的讓 LLM 換路或
// 停下來問人——測試守著的必須是那句話，不是一個旗標。
const loopGuardNoticeMark = "再送一次相同或等價的參數不會有不同結果"

// loopGuardLogKey 是守衛觸發時落的結構化日誌事件鍵。
const loopGuardLogKey = "tool_loop_guard_tripped"

// loopGuardThreshold 是本檔各測試依賴的門檻——testProfile() 沒有設
// max_repeated_tool_failures，走的就是零值回退的預設值。
//
// 寫成常數而不是從 core 取：那個預設值是未匯出的，為了測試把它匯出會為了一個數字
// 擴大 API 表面。**代價是它與生產預設值之間沒有編譯期關聯**——所以下面每一支測試
// 的錄製回應筆數都跟著這個數字排，改預設值時這裡會以「該觸發卻沒觸發」的形式紅掉，
// 而不是安靜地測到別的東西。
const loopGuardThreshold = 3

// 主場景用的路徑：notes 在白名單內、也真的存在，但 notes/sub 不存在。
// write_file 因此在開檔之前就以 not_found 被擋下，不在磁碟上留痕跡。
const loopGuardTarget = "notes/sub/a.md"

// jsonString 把 s 編碼成一個 JSON 字串字面（含外層引號與必要跳脫）。
//
// fixture 的 arguments 欄位本身是一個「內容為 JSON 的字串」，手寫跳脫很容易錯一個
// 反斜線，而錯掉的結果是 Provider 解析失敗、測試以一個看不出原因的方式紅掉。交給
// json.Marshal 產生。
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("編碼 JSON 字串 %q: %v", s, err)
	}
	return string(b)
}

// loopGuardToolCall 以指定的 tool_call ID 與參數產生一份 write_file 的錄製回應。
//
// 用一份帶佔位的 fixture 而不是每種參數寫法各存一個檔案：本票的測試要的正是「同樣
// 的呼叫換不同寫法」，把差異留在測試碼裡，讀的人一眼看得到差在哪。
func loopGuardToolCall(t *testing.T, id, args string) string {
	t.Helper()
	return strings.NewReplacer(
		"{{ID}}", jsonString(t, id),
		"{{ARGS}}", jsonString(t, args),
	).Replace(readFixture(t, "reply_loop_guard_tool_call.json"))
}

// newLoopGuardAgent 組出只註冊 write_file、**引擎層**日誌送進 logger 的 AgentService。
//
// 既有的 newToolAgentIn 一族把引擎 logger 硬編成 discardLogger()（它們的 logger
// 參數給的是 Tool 執行日誌，走 Registry.Subset），而 tool_loop_guard_tripped 是
// ReAct 循環落的，屬引擎那一層。這裡自己組一次，不去改既有 helper 的簽章——那會
// 讓十幾支不關心引擎日誌的測試各多帶一個參數。
func newLoopGuardAgent(t *testing.T, baseURL string, profile *core.Profile,
	root *os.Root, st *testStore, logger *slog.Logger) *core.AgentService {
	t.Helper()
	r := tool.NewRegistry()
	sandbox := tool.SandboxConfig{AllowedPaths: []string{"notes"}}
	if err := tool.RegisterBuiltins(r, tool.NewSandboxChecker(sandbox), root,
		testShellRuntime(t, root.Name()), tool.NewShellLimiter()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	exec, err := r.Subset([]string{tool.WriteFileToolName}, nil, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(profile, svc, exec, newMemory(t, st.sessions()),
		st.audit, noBootstrap(t), core.NopEventSink{}, logger)
}

// loopGuardWorkspace 建一個 notes/ 存在、notes/sub/ 不存在的真實 Workspace，
// 回傳 os.Root 與它在磁碟上的路徑。
func loopGuardWorkspace(t *testing.T) (*os.Root, string) {
	t.Helper()
	root, dir := newTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatalf("建立 notes/: %v", err)
	}
	return root, dir
}

// logRecords 解析 JSON logger 寫出的每一行，供斷言事件鍵與欄位。
//
// 既有的 captureSink 只收 message，本票要斷言的是**附帶欄位**（Tool 名與規範化後
// 的參數）——那正是「key 不做雜湊」在外部看得見的樣子，只比對事件鍵驗不到它。
func logRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("解析日誌行 %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// findLogRecords 取出 msg 等於 want 的所有日誌記錄。
func findLogRecords(t *testing.T, raw, want string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range logRecords(t, raw) {
		if rec["msg"] == want {
			found = append(found, rec)
		}
	}
	return found
}

// assertRoles 斷言 Session 對話歷史的角色序列。
//
// 本票的核心約束之一是「提示附加在 tool 結果內容裡，不新增訊息」：Session 要能原樣
// 重放給 Provider，混進一條系統插話會讓恢復的對話失真。角色序列是那條約束在外部
// 看得見的形狀。
func assertRoles(t *testing.T, session *core.Session, want []core.Role) {
	t.Helper()
	if len(session.Messages) != len(want) {
		t.Fatalf("訊息數 = %d, 期望 %d——守衛不得新增訊息，提示要附在 tool 結果內容裡: %+v",
			len(session.Messages), len(want), session.Messages)
	}
	for i, msg := range session.Messages {
		if msg.Role != want[i] {
			t.Errorf("messages[%d].Role = %s, 期望 %s", i, msg.Role, want[i])
		}
	}
}

// TestLoopGuardTripsOnEquivalentArgs 是本票主場景：同一個 write_file 呼叫換三種
// **等價但寫法不同**的參數連續失敗，第三次（門檻）才附上死循環提示。
//
// 三種寫法涵蓋 ticket 點名的兩類等價：第二次改鍵序、第三次改空白。少了規範化的話
// 三次會被算成三個不同的 key，守衛永遠數不到門檻——這正是「參數規範化是這項的核心」
// 那句話的驗收方式。
func TestLoopGuardTripsOnEquivalentArgs(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		loopGuardToolCall(t, "call_loop_1", `{"path":"notes/sub/a.md","content":"待辦"}`),
		// 鍵序不同：content 排到 path 前面。
		loopGuardToolCall(t, "call_loop_2", `{"content":"待辦","path":"notes/sub/a.md"}`),
		// 空白不同：冒號與逗號旁多了空格。
		loopGuardToolCall(t, "call_loop_3", "{ \"path\" : \"notes/sub/a.md\" , \"content\" : \"待辦\" }"),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	root, _ := loopGuardWorkspace(t)
	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, testProfile(), root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 一、門檻前不得附加。差一次就觸發的話，門檻這個設定等於沒有意義。
	//
	// reqs[1] 帶的是第一次失敗的結果、reqs[2] 帶第二次——門檻是 3，兩者都不該有提示。
	for i := 1; i < loopGuardThreshold; i++ {
		if got := sentToolMessage(t, reqs[i]); strings.Contains(got, loopGuardNoticeMark) {
			t.Errorf("第 %d 次失敗（未達門檻 %d）就附上了死循環提示: %q",
				i, loopGuardThreshold, got)
		}
	}

	// 二、達門檻的那一次要附上，而且三段順序固定為「原始錯誤 → 類型指引 → 死循環提示」。
	//
	// 順序不是排版偏好：LLM 要先讀到「是哪個路徑、發生了什麼」才有辦法判斷下一步，
	// 把通則擺前面會讓最關鍵的事實被擠到最後。
	backfilled := sentToolMessage(t, reqs[3])
	guidance := core.ToolErrorNotFound.Guidance()
	if guidance == "" {
		t.Fatal("not_found 的指引是空字串——這支測試的前提不成立")
	}
	errAt := strings.Index(backfilled, "父目錄 notes/sub 不存在")
	guidanceAt := strings.Index(backfilled, guidance)
	noticeAt := strings.Index(backfilled, loopGuardNoticeMark)
	if errAt < 0 {
		t.Fatalf("回填的 tool 訊息遺失原始錯誤: %q", backfilled)
	}
	if guidanceAt < 0 {
		t.Fatalf("回填的 tool 訊息未附 not_found 指引: %q", backfilled)
	}
	if noticeAt < 0 {
		t.Fatalf("達門檻卻沒有附上死循環提示: %q", backfilled)
	}
	if !(errAt < guidanceAt && guidanceAt < noticeAt) {
		t.Errorf("三段順序應為「原始錯誤(%d) → 類型指引(%d) → 死循環提示(%d)」: %q",
			errAt, guidanceAt, noticeAt, backfilled)
	}
	// 提示帶的是**實際次數**。一句含糊的「失敗了很多次」LLM 判斷不出自己已經走多遠，
	// 而具體的數字也讓這段話在對話記錄裡可查證。
	if want := fmt.Sprintf("失敗 %d 次", loopGuardThreshold); !strings.Contains(backfilled, want) {
		t.Errorf("死循環提示沒有帶上實際次數（期望含 %q）: %q", want, backfilled)
	}

	// 三、觸發時落結構化警告日誌，帶 Tool 名與**規範化後的參數本身**。
	//
	// 斷言參數是可讀的、看得出是哪個路徑在循環，就是 ticket 那句「key 不做雜湊」的
	// 驗收方式：一個雜湊過的 key 也能通過「有這個欄位」的檢查，卻在除錯時毫無用處。
	tripped := findLogRecords(t, logs.String(), loopGuardLogKey)
	if len(tripped) != 1 {
		t.Fatalf("%s 日誌 = %d 筆, 期望 1 筆\n日誌全文:\n%s", loopGuardLogKey, len(tripped), logs.String())
	}
	rec := tripped[0]
	if rec["tool"] != tool.WriteFileToolName {
		t.Errorf("日誌的 tool = %v, 期望 %s", rec["tool"], tool.WriteFileToolName)
	}
	args, _ := rec["args"].(string)
	if !strings.Contains(args, loopGuardTarget) {
		t.Errorf("日誌的 args = %q, 期望看得出是 %s 在循環（key 不做雜湊）", args, loopGuardTarget)
	}
	if rec["level"] != "WARN" {
		t.Errorf("日誌 level = %v, 期望 WARN", rec["level"])
	}

	// 四、Session 訊息數與角色序列不因守衛而變：三輪各一對 assistant/tool，
	// 最後一條是收尾回覆。提示附在 tool 內容裡，沒有多出任何一條訊息。
	assertRoles(t, session, []core.Role{
		core.RoleUser,
		core.RoleAssistant, core.RoleTool,
		core.RoleAssistant, core.RoleTool,
		core.RoleAssistant, core.RoleTool,
		core.RoleAssistant,
	})

	// 五、落庫的 Session 恢復後能原樣重放：角色序列一致，每條 tool 訊息都帶得回
	// 對應的 tool_call_id（OpenAI 兼容協議要求成對），提示就在 tool 內容裡。
	persisted := decodePersisted(t, onlySession(t, dbPath).messagesJSON)
	if len(persisted) != len(session.Messages) {
		t.Fatalf("落庫訊息數 = %d, 期望 %d", len(persisted), len(session.Messages))
	}
	var toolMsgs int
	for i, pm := range persisted {
		if pm.Role != string(session.Messages[i].Role) {
			t.Errorf("落庫 messages[%d].role = %s, 期望 %s", i, pm.Role, session.Messages[i].Role)
		}
		if pm.Role != string(core.RoleTool) {
			continue
		}
		toolMsgs++
		if pm.ToolCallID == "" {
			t.Errorf("落庫 messages[%d] 是 tool 訊息卻沒有 tool_call_id，恢復後無法重放", i)
		}
	}
	if toolMsgs != 3 {
		t.Errorf("落庫的 tool 訊息 = %d 條, 期望 3", toolMsgs)
	}
	if !strings.Contains(persisted[len(persisted)-2].Content, loopGuardNoticeMark) {
		t.Errorf("死循環提示沒有隨 tool 訊息落庫，恢復的對話會與當初送給 LLM 的不同: %q",
			persisted[len(persisted)-2].Content)
	}
}

// TestLoopGuardResetsAfterSuccess 釘住「任一次成功即清空整張表」。
//
// 序列是：同一個 key 失敗兩次 → **一次成功** → 同一個 key 再失敗兩次。沒有歸零的話
// 最後那次會是第四次、早就過了門檻；歸零之後它只是第二次，不該觸發。
//
// 這條不是錦上添花：LLM 試錯本來就會失敗幾次再走通，一次成功代表它已經跳出了那條路，
// 讓舊計數留著會在後面一次無關的失敗上誤觸發，等於教它別再用那個 Tool。
func TestLoopGuardResetsAfterSuccess(t *testing.T) {
	const failing = `{"path":"notes/sub/a.md","content":"待辦"}`
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		loopGuardToolCall(t, "call_reset_1", failing),
		loopGuardToolCall(t, "call_reset_2", `{"content":"待辦","path":"notes/sub/a.md"}`),
		// 這一次寫進真的存在的 notes/，會成功——整張表在這裡清空。
		loopGuardToolCall(t, "call_reset_3", `{"path":"notes/ok.md","content":"待辦"}`),
		loopGuardToolCall(t, "call_reset_4", failing),
		loopGuardToolCall(t, "call_reset_5", "{ \"path\" : \"notes/sub/a.md\" , \"content\" : \"待辦\" }"),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	root, _ := loopGuardWorkspace(t)
	db := newStore(t)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, testProfile(), root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 第三次呼叫真的成功了，這支測試的前提才成立。
	if got := sentToolMessage(t, reqs[3]); strings.Contains(got, "Tool 執行失敗") {
		t.Fatalf("第三次呼叫應該成功（寫進存在的 notes/），實際失敗了: %q", got)
	}
	// 歸零後的第二次失敗（總計第四次）仍未達門檻，不得附提示。
	if got := sentToolMessage(t, reqs[5]); strings.Contains(got, loopGuardNoticeMark) {
		t.Errorf("成功之後計數沒有歸零——第二次失敗就附上了死循環提示: %q", got)
	}
	if n := len(findLogRecords(t, logs.String(), loopGuardLogKey)); n != 0 {
		t.Errorf("%s 日誌 = %d 筆, 期望 0 筆（全程未達門檻）", loopGuardLogKey, n)
	}
}

// TestLoopGuardDoesNotPersistAcrossTurns 釘住「生命週期是單次 Run」。
//
// 第一個 turn 讓同一個 key 失敗到門檻前一次，第二個 turn 用同樣的參數再失敗一次。
// 計數若跨 turn 保留，這一次就會湊滿門檻並觸發——那是把上一段對話的失敗算在這一段
// 頭上。使用者在兩個 turn 之間可能已經建好了目錄、換了問法，上一輪的計數說明不了
// 這一輪。
func TestLoopGuardDoesNotPersistAcrossTurns(t *testing.T) {
	const failing = `{"path":"notes/sub/a.md","content":"待辦"}`
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		// 第一個 turn：兩次失敗（門檻 3，差一次）。
		loopGuardToolCall(t, "call_turn1_1", failing),
		loopGuardToolCall(t, "call_turn1_2", `{"content":"待辦","path":"notes/sub/a.md"}`),
		readFixture(t, "reply_loop_guard_final.json"),
		// 第二個 turn：同樣的參數再失敗一次。
		loopGuardToolCall(t, "call_turn2_1", failing),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	root, _ := loopGuardWorkspace(t)
	db := newStore(t)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, testProfile(), root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("第一個 turn: %v", err)
	}
	if _, err := agent.Process(t.Context(), session, "再試一次"); err != nil {
		t.Fatalf("第二個 turn: %v", err)
	}

	// reqs[4] 是第二個 turn 的收尾請求，帶著該 turn 唯一那次失敗的結果。
	if got := sentToolMessage(t, reqs[4]); strings.Contains(got, loopGuardNoticeMark) {
		t.Errorf("計數跨 turn 保留了——新 turn 的第一次失敗就附上死循環提示: %q", got)
	}
	if n := len(findLogRecords(t, logs.String(), loopGuardLogKey)); n != 0 {
		t.Errorf("%s 日誌 = %d 筆, 期望 0 筆（每個 turn 都未達門檻）", loopGuardLogKey, n)
	}
}

// TestLoopGuardLogRedactsSensitiveArgs 釘住守衛日誌**與其他落盤路徑共用同一套去敏
// 規則**（見 redact.go）。
//
// 「key 不做雜湊、日誌裡看得出是哪個參數在循環」與「參數可能內嵌密鑰」是同時成立的
// 兩件事，這支測試同時斷言兩邊：路徑仍然看得見（可讀），敏感欄位的值被換掉（不外洩）。
// 少了它，一個直接把原始參數寫進日誌的實作會全綠——**而那正是最容易寫出來的版本**。
//
// 用 write_file 不認得的額外欄位（api_key）帶密鑰是刻意的：它被 Tool 的參數解析忽略、
// 不影響失敗結果，但 call.Arguments 原文裡有它——真實世界的 MCP Tool 參數就是這樣，
// 我們這一端不能假設 LLM 只會送 schema 裡宣告的欄位。
func TestLoopGuardLogRedactsSensitiveArgs(t *testing.T) {
	const (
		secret = "sk-live-DO-NOT-LOG"
		// 2^53+1：float64 表示不了。日誌若在去敏時把數字解成 float64，這裡會變成
		// 結尾 ...992——日誌記下的參數與守衛實際用的 key 就不是同一個東西了。
		bigID    = "9007199254740993"
		truncID  = "9007199254740992"
		extraArg = `"api_key":"` + secret + `","trace_id":` + bigID
	)
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		loopGuardToolCall(t, "call_secret_1", `{"path":"notes/sub/a.md","content":"待辦",`+extraArg+`}`),
		loopGuardToolCall(t, "call_secret_2", `{`+extraArg+`,"content":"待辦","path":"notes/sub/a.md"}`),
		loopGuardToolCall(t, "call_secret_3", "{ \"path\" : \"notes/sub/a.md\" , \"content\" : \"待辦\" , "+extraArg+" }"),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	root, _ := loopGuardWorkspace(t)
	db := newStore(t)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, testProfile(), root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	tripped := findLogRecords(t, logs.String(), loopGuardLogKey)
	if len(tripped) != 1 {
		t.Fatalf("%s 日誌 = %d 筆, 期望 1 筆\n日誌全文:\n%s", loopGuardLogKey, len(tripped), logs.String())
	}
	args, _ := tripped[0]["args"].(string)
	if strings.Contains(args, secret) {
		t.Errorf("守衛日誌把密鑰原樣寫出去了: %q", args)
	}
	if !strings.Contains(args, "[REDACTED]") {
		t.Errorf("守衛日誌的 args 沒有經過 RedactArgs——所有落盤路徑要共用同一套去敏規則: %q", args)
	}
	// 去敏之後仍要看得出是哪個參數在循環，否則這條日誌就失去了存在的理由。
	if !strings.Contains(args, loopGuardTarget) {
		t.Errorf("去敏把有用的資訊也一起抹掉了，args = %q, 期望仍看得到 %s", args, loopGuardTarget)
	}
	// **日誌帶的必須是規範化後的那個 key 本身，不是一個被改過的版本。**
	// 去敏路徑若把數字解成 float64，大整數會被截短——除錯時拿日誌裡的參數回頭比對，
	// 會對不上守衛實際在數的那一組。
	if !strings.Contains(args, bigID) {
		t.Errorf("守衛日誌改動了參數裡的數字，args = %q, 期望含 %s", args, bigID)
	}
	if strings.Contains(args, truncID) {
		t.Errorf("守衛日誌把大整數截短成 %s 了: %q", truncID, args)
	}
	// 整份日誌都不得出現密鑰——不只是 args 那個欄位。
	if strings.Contains(logs.String(), secret) {
		t.Errorf("密鑰出現在守衛以外的日誌欄位裡:\n%s", logs.String())
	}
}

// TestLoopGuardHonorsConfiguredThreshold 釘住 Profile 設定的門檻**真的被循環用到**。
//
// 這與 profile_test.go 那幾格分工不同：那裡證明的是 LoadProfile 把值讀進來、零值會
// 回退預設；這裡證明 Run 拿的是那個值，不是一個寫死的常數。少了這一支，一個把門檻
// 硬編在 react.go 裡的實作會通過上面所有測試——因為它們用的都是預設值 3。
//
// 門檻設 2：同一組等價參數第二次失敗就該觸發，而預設值 3 在這個序列裡不會觸發。
// 兩者的期望剛好相反，是這支測試能分辨兩種實作的原因。
func TestLoopGuardHonorsConfiguredThreshold(t *testing.T) {
	const threshold = 2
	if threshold >= loopGuardThreshold {
		t.Fatalf("門檻 %d 未低於預設值 %d，這支測試分辨不出設定有沒有生效", threshold, loopGuardThreshold)
	}

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		loopGuardToolCall(t, "call_threshold_1", `{"path":"notes/sub/a.md","content":"待辦"}`),
		loopGuardToolCall(t, "call_threshold_2", `{"content":"待辦","path":"notes/sub/a.md"}`),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	profile := testProfile()
	profile.Settings.MaxRepeatedToolFailures = threshold
	root, _ := loopGuardWorkspace(t)
	db := newStore(t)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, profile, root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 第一次仍未達門檻。
	if got := sentToolMessage(t, reqs[1]); strings.Contains(got, loopGuardNoticeMark) {
		t.Errorf("第一次失敗（門檻 %d）就附上了死循環提示: %q", threshold, got)
	}
	// 第二次達門檻——這只有在循環真的讀了 Profile 的設定時才成立。
	backfilled := sentToolMessage(t, reqs[2])
	if !strings.Contains(backfilled, loopGuardNoticeMark) {
		t.Errorf("設定門檻 %d，第二次失敗卻沒有觸發——循環沒有讀 Profile 的 max_repeated_tool_failures: %q",
			threshold, backfilled)
	}
	if want := fmt.Sprintf("失敗 %d 次", threshold); !strings.Contains(backfilled, want) {
		t.Errorf("提示的次數不是設定的門檻（期望含 %q）: %q", want, backfilled)
	}
	// 日誌的 threshold 欄位也要反映設定值，維運端才看得出這次是以什麼標準判定的。
	tripped := findLogRecords(t, logs.String(), loopGuardLogKey)
	if len(tripped) != 1 {
		t.Fatalf("%s 日誌 = %d 筆, 期望 1 筆\n日誌全文:\n%s", loopGuardLogKey, len(tripped), logs.String())
	}
	if got := tripped[0]["threshold"]; got != float64(threshold) {
		t.Errorf("日誌的 threshold = %v, 期望 %d", got, threshold)
	}
}

// TestLoopGuardKeepsPathWhitespaceDistinct 釘住「守衛的等價定義與 Tool 的實際行為對齊」。
//
// **這一支是外部審查抓出來的**。ticket #54 的原文說「路徑類欄位額外做去空白與路徑
// 標準化」，第一版照著做了 TrimSpace，於是 `a.md` 與 `a.md ` 被算成同一條路。
// 但 internal/tool 的 CheckFilePath 對原始路徑只做 filepath.Clean——尾端空白原樣
// 保留，兩者是磁碟上兩個不同的檔案。合併它們會在各失敗一次時就湊到門檻，叫 LLM
// 停止一件它其實才剛開始做的事。
//
// 測試分兩段：先用**真實檔案系統**證明前提（那兩個路徑真的是兩個檔案，憲法 4.3），
// 再證明守衛沒有把它們合併。少了第一段，第二段只是在重述實作。
func TestLoopGuardKeepsPathWhitespaceDistinct(t *testing.T) {
	const (
		plain   = "notes/sub/a.md"
		spaced  = "notes/sub/a.md "
		content = "待辦"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		loopGuardToolCall(t, "call_ws_1", `{"path":"`+plain+`","content":"`+content+`"}`),
		// 等價寫法（鍵序不同）：與第一次同一個 key，計數到 2。
		loopGuardToolCall(t, "call_ws_2", `{"content":"`+content+`","path":"`+plain+`"}`),
		// **路徑尾端多一個空白：另一個檔案、另一個 key**，計數重新從 1 算起。
		loopGuardToolCall(t, "call_ws_3", `{"path":"`+spaced+`","content":"`+content+`"}`),
		readFixture(t, "reply_loop_guard_final.json"),
	)
	root, dir := loopGuardWorkspace(t)

	// 前提：在真的檔案系統上，這兩個路徑是兩個獨立的檔案。
	// 用 notes/ 底下另一組名字驗（notes/sub 刻意不存在，是上面那三次呼叫失敗的原因）。
	for name, body := range map[string]string{"notes/probe.md": "A", "notes/probe.md ": "B"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("建立 %q: %v", name, err)
		}
	}
	for name, want := range map[string]string{"notes/probe.md": "A", "notes/probe.md ": "B"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("讀取 %q: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("前提不成立：%q 的內容是 %q, 期望 %q——這個檔案系統把尾端空白吃掉了，"+
				"本測試的推論不適用", name, got, want)
		}
	}

	db := newStore(t)
	var logs strings.Builder
	agent := newLoopGuardAgent(t, srv.URL, testProfile(), root, db,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(t.Context(), session, "把待辦寫進 notes/sub/a.md"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 第三次是另一個 key 的第一次失敗，離門檻還很遠。
	// 守衛若對 path 做了 TrimSpace，這裡會是同一個 key 的第三次——正好達門檻而觸發。
	if got := sentToolMessage(t, reqs[3]); strings.Contains(got, loopGuardNoticeMark) {
		t.Errorf("兩個尾端空白不同的路徑被算成同一條路——它們是磁碟上兩個不同的檔案: %q", got)
	}
	if n := len(findLogRecords(t, logs.String(), loopGuardLogKey)); n != 0 {
		t.Errorf("%s 日誌 = %d 筆, 期望 0 筆（沒有任何一個 key 達到門檻）", loopGuardLogKey, n)
	}
}
