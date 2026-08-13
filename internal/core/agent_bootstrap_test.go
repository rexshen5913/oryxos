// Bootstrap 上下文載入的整合測試（ticket #16）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Bootstrap 三檔在
// seam 之下用真實檔案（t.TempDir()，憲法 4.3）。斷言落在外部可觀察的產物上：
// 送往 LLM 邊界請求的 system prompt。**只驗結構性事實**（某段有沒有進去、順序是
// 什麼、哪一層被排除），不綁死引言與標題的措辭。
package core_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/provider"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// seedBootstrap 在 Workspace 根寫下一份 Bootstrap 檔案（真實檔案）。
func seedBootstrap(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("預置 %s: %v", name, err)
	}
}

// mkdirBootstrap 把一個 Bootstrap 檔名做成目錄：它**存在**（所以通過啟動時的
// 存在性校驗）但不是普通檔，讀取端一碰就報錯。用來驗「沒被選中的檔案根本沒被讀」。
func mkdirBootstrap(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("建立目錄 %s: %v", name, err)
	}
}

// bootstrapWorkspace 開一個 Workspace 根並回傳 root 與其絕對路徑（後者供測試直接
// 寫檔）。三份 Bootstrap 檔案刻意不預先建立——缺檔視為該層為空是既定行為。
func bootstrapWorkspace(t *testing.T) (*os.Root, string) {
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
	return root, dir
}

// newBootstrapAgent 組出一個只驗 Bootstrap 注入的 AgentService：無 Tool，長期記憶
// 與會話記憶都用真的，Bootstrap 從 root 底下的真實檔案載入。identityPrompt 為空
// 字串時模擬「Profile 沒有設 identity.prompt」，讓 SOUL.md 有機會生效。
func newBootstrapAgent(t *testing.T, baseURL string, root *os.Root, identityPrompt string) *core.AgentService {
	t.Helper()
	prof := testProfile()
	prof.Identity.Prompt = identityPrompt
	return newBootstrapAgentWithProfile(t, baseURL, root, prof)
}

// newBootstrapAgentWithProfile 同 newBootstrapAgent，但 Profile 由呼叫端給——
// ticket #17 要驗的正是「不同 Profile 各自選不同的 Bootstrap 檔案」，Profile 本身
// 就是被測變數，不能藏在 helper 裡。
func newBootstrapAgentWithProfile(t *testing.T, baseURL string, root *os.Root, prof *core.Profile) *core.AgentService {
	t.Helper()
	return newBootstrapAgentWithLogger(t, baseURL, root, prof, discardLogger())
}

// newBootstrapAgentWithLogger 同上，但 logger 由呼叫端給——驗引擎層結構化日誌的
// 測試需要一個看得到記錄的 handler。
//
// Tool 子集**照組裝點的規則推導**：Profile 的 skills 非空時把 load_skill 加進來
// （見 cmd/oryxos/chat.go 的 autoIncludedTools）。這不是為了讓測試通過而加的順從——
// Skill 段的引言承諾了那個 Tool，宣告了 skills 卻沒有它的 Agent 是組裝點永遠不會
// 產生的狀態，helper 照著真實組裝走，測試才不會建出一個現實中不存在的東西。
func newBootstrapAgentWithLogger(t *testing.T, baseURL string, root *os.Root, prof *core.Profile, logger *slog.Logger) *core.AgentService {
	t.Helper()
	st := newStore(t)
	loader := config.NewContextLoader(root)
	longTerm := memory.NewLongTermMemory(root, memoryRelPath)
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(prof, svc, skillAwareTools(t, loader, prof),
		memory.NewService(st.sessions(), longTerm), st.audit, loader, logger)
}

// skillAwareTools 回傳這個 Profile 該有的 Tool 子集：沒有 Skill 時是空集合，
// 有 Skill 時帶著 load_skill（與組裝點同一條推導）。
func skillAwareTools(t *testing.T, loader core.ContextLoader, prof *core.Profile) core.ToolExecutor {
	t.Helper()
	if len(prof.Skills) == 0 {
		return noTools(t)
	}
	registry := tool.NewRegistry()
	if err := registry.Register(tool.NewLoadSkillTool(loader, prof.Skills)); err != nil {
		t.Fatalf("註冊 load_skill: %v", err)
	}
	exec, err := registry.Subset(nil, []string{tool.LoadSkillToolName}, discardLogger())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	return exec
}

// promptAfterTurn 跑一個 turn 並回傳該次 LLM 邊界請求的 system prompt。
func promptAfterTurn(t *testing.T, agent *core.AgentService, session *core.Session, reqs *[][]byte, msg string) string {
	t.Helper()
	if _, err := agent.Process(context.Background(), session, msg); err != nil {
		t.Fatalf("Process(%q): %v", msg, err)
	}
	return systemPrompt(t, (*reqs)[len(*reqs)-1])
}

// TestBootstrapInjectionOrder 釘住 ADR-0003 的拼接順序：最穩定普遍 → 最具體當下，
// 衝突時後者勝出。順序以各段在 system prompt 裡的位置斷言，不看措辭。
func TestBootstrapInjectionOrder(t *testing.T) {
	const (
		identity = "你是 Oryx，一個樂於助人的通用助理。回答力求精確、直接。"
		agents   = "本專案的慣例是測試先行"
		user     = "使用者偏好繁體中文回覆"
		longTerm = "使用者的後端統一用 Go"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", agents)
	seedBootstrap(t, dir, "USER.md", user)
	seedMemory(t, filepath.Join(dir, memoryRelPath), "## 2026-08-01\n\n- "+longTerm+"\n")

	agent := newBootstrapAgent(t, srv.URL, root, identity)
	got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

	// 四層都在，且按序出現。
	var last int
	for _, want := range []string{identity, agents, user, longTerm} {
		at := strings.Index(got, want)
		if at < 0 {
			t.Fatalf("system prompt 遺失 %q: %q", want, got)
		}
		if at < last {
			t.Errorf("順序錯誤：%q 出現在前一層之前（ADR-0003 要求最穩定普遍 → 最具體當下）\n%s", want, got)
		}
		last = at
	}
}

// TestIdentityPromptAndSoulAreExclusive 釘住 ADR-0003 那條**互斥**：Profile 的
// identity.prompt 存在時 SOUL.md 完全不進 prompt，不是疊加。疊加會產生雙重人格，
// 這是刻意設計、未來讀者不得「修正」。
func TestIdentityPromptAndSoulAreExclusive(t *testing.T) {
	const (
		identity = "你是 Oryx，回答力求精確"
		soul     = "你是 Nova，說話浮誇愛用比喻"
	)

	tests := []struct {
		name           string
		identityPrompt string
		soul           string
		wantIn         []string
		wantNotIn      []string
	}{
		{
			name:           "兩者都在：identity.prompt 勝，SOUL.md 完全不進",
			identityPrompt: identity,
			soul:           soul,
			wantIn:         []string{identity},
			wantNotIn:      []string{soul},
		},
		{
			name:           "只有 SOUL.md：它生效",
			identityPrompt: "",
			soul:           soul,
			wantIn:         []string{soul},
		},
		{
			name:           "只有 identity.prompt",
			identityPrompt: identity,
			wantIn:         []string{identity},
		},
		{
			name:      "兩者都沒有：不留空段落",
			wantNotIn: []string{identity, soul},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			if tt.soul != "" {
				seedBootstrap(t, dir, "SOUL.md", tt.soul)
			}

			agent := newBootstrapAgent(t, srv.URL, root, tt.identityPrompt)
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("system prompt 遺失 %q: %q", want, got)
				}
			}
			for _, notWant := range tt.wantNotIn {
				if strings.Contains(got, notWant) {
					t.Errorf("system prompt 不該含 %q（互斥不是疊加）: %q", notWant, got)
				}
			}
		})
	}
}

// TestBootstrapFileMatrix 是缺檔與空檔的組合矩陣：任一份不存在或為空都視為該層
// 為空、對話一切照常，且不留一個空標題給 LLM 猜。
func TestBootstrapFileMatrix(t *testing.T) {
	const (
		agents = "本專案的慣例是測試先行"
		user   = "使用者偏好繁體中文回覆"
	)
	// ptr 讓「不建立檔案」與「建立一個空檔」在表格裡分得出來。
	ptr := func(s string) *string { return &s }

	tests := []struct {
		name      string
		agents    *string
		user      *string
		wantIn    []string
		wantNotIn []string
		// wantOnlyIdentity 為真時，system prompt 必須**恰好**是 identity.prompt
		// ——所有 Bootstrap 層都為空時不得留下空標題或多餘的空行給 LLM 猜。
		wantOnlyIdentity bool
	}{
		{name: "兩份都有", agents: ptr(agents), user: ptr(user), wantIn: []string{agents, user}},
		{name: "只有 AGENTS.md", agents: ptr(agents), wantIn: []string{agents}, wantNotIn: []string{user}},
		{name: "只有 USER.md", user: ptr(user), wantIn: []string{user}, wantNotIn: []string{agents}},
		{name: "兩份都缺", wantOnlyIdentity: true},
		{name: "存在但為空", agents: ptr(""), user: ptr(""), wantOnlyIdentity: true},
		{name: "只有空白字元", agents: ptr("   \n\t\n"), user: ptr("\n\n"), wantOnlyIdentity: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			if tt.agents != nil {
				seedBootstrap(t, dir, "AGENTS.md", *tt.agents)
			}
			if tt.user != nil {
				seedBootstrap(t, dir, "USER.md", *tt.user)
			}

			agent := newBootstrapAgent(t, srv.URL, root, testProfile().Identity.Prompt)
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			// 不論哪一列，identity.prompt 永遠在，且對話都要成功。
			if !strings.Contains(got, testProfile().Identity.Prompt) {
				t.Errorf("system prompt 遺失 identity.prompt: %q", got)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("system prompt 遺失 %q: %q", want, got)
				}
			}
			for _, notWant := range tt.wantNotIn {
				if strings.Contains(got, notWant) {
					t.Errorf("system prompt 不該含 %q: %q", notWant, got)
				}
			}
			if tt.wantOnlyIdentity && strings.TrimSpace(got) != testProfile().Identity.Prompt {
				t.Errorf("Bootstrap 各層皆空時 system prompt 應恰好是 identity.prompt，實際 %q", got)
			}
		})
	}
}

// TestBootstrapRereadEveryTurn 釘住「每個 turn 重讀、不緩存」：使用者手改 USER.md，
// **下一個 turn** 立刻生效，不必重啟。這條同時證明沒有 in-memory cache。
func TestBootstrapRereadEveryTurn(t *testing.T) {
	const (
		before = "使用者偏好繁體中文回覆"
		after  = "使用者改成偏好英文回覆"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "USER.md", before)

	agent := newBootstrapAgent(t, srv.URL, root, testProfile().Identity.Prompt)
	session := core.NewSession("cli", "local", "default")

	if got := promptAfterTurn(t, agent, session, &reqs, "早安"); !strings.Contains(got, before) {
		t.Fatalf("第一個 turn 未注入 USER.md: %q", got)
	}

	seedBootstrap(t, dir, "USER.md", after) // 使用者中途手改

	got := promptAfterTurn(t, agent, session, &reqs, "再問一次")
	if !strings.Contains(got, after) {
		t.Errorf("下一個 turn 未讀到改後的 USER.md（每 turn 重讀、無快取）: %q", got)
	}
	if strings.Contains(got, before) {
		t.Errorf("下一個 turn 仍帶著舊內容: %q", got)
	}
}

// TestBootstrapSnapshotPerTurn 釘住載入點在**迭代迴圈之外**：同一個 turn 內的多次
// iteration，system prompt 保持不變——即使檔案在兩次 iteration 之間被改掉。
//
// 同時驗相反的那一半：**對話歷史絕不快照**。第二次 iteration 的 messages 必須含
// 本 turn 剛追加的 assistant 與 tool 訊息，否則 tool 結果回填看不到、ReAct 循環壞掉。
func TestBootstrapSnapshotPerTurn(t *testing.T) {
	const (
		before = "使用者偏好繁體中文回覆"
		after  = "turn 中途被改掉的內容"
	)

	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "USER.md", before)

	// 第一次回應呼叫 save_memory，讓同一個 turn 產生第二次 iteration；handler 在
	// 兩次之間把 USER.md 改掉，第二次的 system prompt 必須維持第一次的快照。
	var reqs [][]byte
	srv := newMutatingReplayServer(t, &reqs,
		func() { seedBootstrap(t, dir, "USER.md", after) },
		saveMemoryFixture(t, "reply_save_memory.json", "使用者的專案用 Go"),
		readFixture(t, "reply_memory_saved.json"))

	agent := newMemoryAgent(t, srv.URL, root, []string{"save_memory"}, discardLogger())
	if _, err := agent.Process(context.Background(), core.NewSession("cli", "local", "default"), "記一下"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(reqs) < 2 {
		t.Fatalf("預期同一個 turn 內有兩次 LLM 呼叫，實際 %d", len(reqs))
	}

	first, second := systemPrompt(t, reqs[0]), systemPrompt(t, reqs[1])
	if first != second {
		t.Errorf("同一個 turn 內 system prompt 變了（載入點應在迭代迴圈之外）:\n第一次: %q\n第二次: %q", first, second)
	}
	if strings.Contains(second, after) {
		t.Errorf("同一個 turn 內讀到了中途改掉的內容: %q", second)
	}

	// 對話歷史則是 iteration 級的即時累積，絕不快照。
	if roles := messageRoles(t, reqs[1]); !slices.Contains(roles, "tool") {
		t.Errorf("第二次 iteration 的 messages 不含本 turn 的 tool 訊息，tool 結果回填會看不到: %v", roles)
	}
}

// newMutatingReplayServer 同 newRecordingReplayServer，但在**回應第一個請求之後**
// 呼叫 mutate——用來在同一個 turn 的兩次 iteration 之間改動檔案。
func newMutatingReplayServer(t *testing.T, bodies *[][]byte, mutate func(), fixtures ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("讀取 LLM 請求 body: %v", err)
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		idx := served
		served++
		mu.Unlock()
		if idx >= len(fixtures) {
			t.Errorf("LLM 請求數超出錄製回應數 %d", len(fixtures))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtures[idx]))
		if idx == 0 {
			mutate()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// messageRoles 取出一次 LLM 邊界請求裡各訊息的 role 序列。
func messageRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析 LLM 請求: %v", err)
	}
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	return roles
}

// skipIfNoSymlink 在建立符號連結不可行的平台上回傳跳過理由。Windows 建立符號
// 連結需要開發者模式或管理員權限，一般 CI 使用者做不到。
func skipIfNoSymlink() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir, err := os.MkdirTemp("", "symlink-probe")
	if err != nil {
		return "無法建立探測目錄: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		return "本平台無法建立符號連結（Windows 需開發者模式或管理員權限）"
	}
	return ""
}

// TestBootstrapReadFailureFailsTurn 釘住失敗語義的分界：檔案**不存在**視為空、
// 對話照常；**讀取故障**（權限、非普通檔）則 fail 該 turn 並以 %w 上拋——把故障
// 吞成空值會讓 Agent 在使用者不知情下失去上下文。
func TestBootstrapReadFailureFailsTurn(t *testing.T) {
	tests := []struct {
		name string
		// skip 回傳非空字串時跳過該列並以其為理由。逐列判斷而不是整個矩陣一起
		// 跳：權限那列需要非 root 且 chmod 語義在 Windows 不成立，符號連結那兩列
		// 在 Windows 需要額外權限，但「目錄而非普通檔」到處都成立——整包跳掉會讓
		// CI 容器或 Windows 上的驗證全部消失。
		skip func() string
		// wantErrSubstr 是錯誤訊息必須帶到的關鍵字。訊息品質在這裡是功能的一部分：
		// 使用者拿到「不是普通檔（實際為 L---------）」修不了，拿到「是符號連結」
		// 才知道要做什麼。
		wantErrSubstr string
		setup         func(t *testing.T, dir string)
	}{
		{
			name: "檔案不可讀",
			skip: func() string {
				if runtime.GOOS == "windows" {
					return "Windows 的 chmod 不阻止讀取，這個情境造不出來"
				}
				if os.Geteuid() == 0 {
					return "root 不受檔案權限限制，無法以唯讀模擬失敗"
				}
				return ""
			},
			wantErrSubstr: "AGENTS.md",
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, "AGENTS.md")
				seedBootstrap(t, dir, "AGENTS.md", "內容")
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatalf("設定不可讀: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
			},
		},
		{
			name:          "路徑是目錄而非普通檔",
			wantErrSubstr: "不是普通檔",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "USER.md"), 0o755); err != nil {
					t.Fatalf("建立目錄: %v", err)
				}
			},
		},
		{
			// 指到 Workspace 之外：os.Root 就擋掉了。
			name:          "符號連結指向 Workspace 之外",
			skip:          skipIfNoSymlink,
			wantErrSubstr: "符號連結",
			setup: func(t *testing.T, dir string) {
				target := filepath.Join(t.TempDir(), "outside.md")
				if err := os.WriteFile(target, []byte("Workspace 之外的內容"), 0o644); err != nil {
					t.Fatalf("建立目標檔: %v", err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "SOUL.md")); err != nil {
					t.Fatalf("建立符號連結: %v", err)
				}
			},
		},
		{
			// 指到 Workspace 之**內**：os.Root 會放行，只有載入器自己那道檢查擋得住。
			// Bootstrap 是「使用者手寫的實體檔案」，需要解析連結才讀得到的東西
			// 不符合那個語義，而它的內容會被送往 Provider。
			name:          "符號連結指向 Workspace 之內",
			skip:          skipIfNoSymlink,
			wantErrSubstr: "符號連結",
			setup: func(t *testing.T, dir string) {
				target := filepath.Join(dir, "actual.md")
				if err := os.WriteFile(target, []byte("Workspace 之內的內容"), 0o644); err != nil {
					t.Fatalf("建立目標檔: %v", err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "SOUL.md")); err != nil {
					t.Fatalf("建立符號連結: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip != nil {
				if reason := tt.skip(); reason != "" {
					t.Skip(reason)
				}
			}
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			tt.setup(t, dir)

			// identity.prompt 留空，讓 SOUL.md 那列也走到讀取路徑。
			agent := newBootstrapAgent(t, srv.URL, root, "")
			_, err := agent.Process(context.Background(), core.NewSession("cli", "local", "default"), "早安")
			if err == nil {
				t.Fatal("讀取故障應 fail 該 turn，不得靜默當成空內容")
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("錯誤訊息未帶 %q，使用者無從修正: %v", tt.wantErrSubstr, err)
			}
			if len(reqs) != 0 {
				t.Errorf("載入失敗時不該呼叫 LLM，實際呼叫 %d 次", len(reqs))
			}
		})
	}
}

// TestBootstrapHonoursContextCancellation 釘住阻塞路徑吃 ctx（憲法 5.3）：呼叫端
// 已取消時不該還去讀檔，該 turn 直接失敗。
func TestBootstrapHonoursContextCancellation(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", "本專案的慣例是測試先行")

	agent := newBootstrapAgent(t, srv.URL, root, testProfile().Identity.Prompt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := agent.Process(ctx, core.NewSession("cli", "local", "default"), "早安"); err == nil {
		t.Fatal("呼叫端已取消時應失敗")
	}
	if len(reqs) != 0 {
		t.Errorf("已取消時不該呼叫 LLM，實際呼叫 %d 次", len(reqs))
	}
}

// TestBrokenSoulDoesNotBreakTurnWhenIdentitySet 釘住互斥的另一半代價：Profile 已有
// identity.prompt 時 SOUL.md 被排除、根本不會進 prompt，那麼一個壞掉的 SOUL.md
// 就**不該**讓每個 turn 都失敗——讓一個用不到的檔案中斷對話是不成比例的。
//
// 這條同時界定了載入端的責任：它只讀呼叫端真的需要的東西。
func TestBrokenSoulDoesNotBreakTurnWhenIdentitySet(t *testing.T) {
	tests := []struct {
		name  string
		skip  func() string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "SOUL.md 是符號連結",
			skip: skipIfNoSymlink,
			setup: func(t *testing.T, dir string) {
				target := filepath.Join(dir, "actual.md")
				if err := os.WriteFile(target, []byte("別的內容"), 0o644); err != nil {
					t.Fatalf("建立目標檔: %v", err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "SOUL.md")); err != nil {
					t.Fatalf("建立符號連結: %v", err)
				}
			},
		},
		{
			name: "SOUL.md 是目錄",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "SOUL.md"), 0o755); err != nil {
					t.Fatalf("建立目錄: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip != nil {
				if reason := tt.skip(); reason != "" {
					t.Skip(reason)
				}
			}
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			tt.setup(t, dir)
			seedBootstrap(t, dir, "AGENTS.md", "本專案的慣例是測試先行")

			// identity.prompt 有設 → SOUL.md 被互斥排除 → 它壞掉也不該有影響。
			agent := newBootstrapAgent(t, srv.URL, root, testProfile().Identity.Prompt)
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			if !strings.Contains(got, testProfile().Identity.Prompt) {
				t.Errorf("system prompt 遺失 identity.prompt: %q", got)
			}
			if !strings.Contains(got, "本專案的慣例是測試先行") {
				t.Errorf("其餘 Bootstrap 層應照常載入: %q", got)
			}
		})
	}
}
