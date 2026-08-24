package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 測試直接查 sessions 表，用同一個純 Go 驅動

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/tool"
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

// newRecordingReplayServer 同 newReplayServer，但把每次 LLM 請求的 body 收下來供
// 斷言——組裝點的行為（例如送出去的工具清單）只有從那裡看得到。
func newRecordingReplayServer(t *testing.T, bodies *[][]byte, fixtures ...string) *httptest.Server {
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
	cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + baseURL + "\nhttp:\n  allowed_domains: []\n"
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("覆寫 config.yaml: %v", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	return dir
}

// writeSkillFile 在 Workspace 的 skills/ 底下寫一份 SKILL.md（原始內容，供不合法
// frontmatter 的案例使用）。`oryxos init` 不建這個目錄——引用 Skill 的 Profile 才需要
// 它，既有 Workspace 因此免遷移。
func writeSkillFile(t *testing.T, dir, name, doc string) {
	t.Helper()
	skills := filepath.Join(dir, workspaceDir, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("建立 skills/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skills, name+".md"), []byte(doc), 0o644); err != nil {
		t.Fatalf("寫入 %s.md: %v", name, err)
	}
}

// writeProfile 覆寫 Workspace 的 default Profile；body 不含 name 那行（一律 default）。
func writeProfile(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, workspaceDir, "profiles", "default.yaml")
	if err := os.WriteFile(path, []byte("name: default\n"+body), 0o644); err != nil {
		t.Fatalf("覆寫 Profile: %v", err)
	}
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
			cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + srv.URL +
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

// TestChatEmptyFilePathWhitelistWarning 是同一條提醒的第二種（路徑白名單），形狀比照
// 上面那條 http.allowed_domains 的。
//
// 沒有這一行，使用者會遇到「Tool 有了、每次呼叫都被攔」而完全不知道原因——而兩段
// 白名單出廠就是空的，所以這是 out-of-box 一定會走到的路徑。
//
// **警告而非 fail fast**：空白名單是安全的預設，純對話（沒列 File Tool 的 Profile）
// 完全不受影響，不該因此起不來。三格分別驗：該警告時警告、配置好了就閉嘴、Profile
// 沒列 File Tool 時也閉嘴。
// 這三段用 raw string 而不是塞滿跳脫字元的單行：它們的重點就是 YAML 裡那個
// 「看起來有寫、其實等於沒寫」的條目，寫成 \"\" 反而看不出在測什麼。
const fileSectionEmptyStringEntry = `file:
  allowed_paths:
    - ""
`

const fileSectionBlankEntry = `file:
  allowed_paths:
    - "   "
`

const fileSectionBlankAndValidEntry = `file:
  allowed_paths:
    - ""
    - notes
`

const fileSectionAbsoluteEntry = `file:
  allowed_paths:
    - /Users/someone/notes
`

const fileSectionEscapingEntry = `file:
  allowed_paths:
    - ../shared
`

func TestChatEmptyFilePathWhitelistWarning(t *testing.T) {
	const profileWithReadFile = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools:
  - read_file
`
	// Profile 只列 write_file（不列 read_file）：提醒的判斷要涵蓋每一個 File Tool，
	// 只認 read_file 的實作會讓「開了 write_file、每次呼叫都被攔」這種配置毫無線索。
	const profileWithWriteFile = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools:
  - write_file
`
	// Profile 只列 list_dir：這一格與上面那格同因——判斷漏掉任何一個 File Tool，
	// 那個 Tool 的使用者就得自己查為什麼每次呼叫都被攔。
	const profileWithListDir = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools:
  - list_dir
`
	const profileWithoutFileTool = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools: []
`
	tests := []struct {
		name string
		// fileSection 是 config.yaml 的整段 file 設定；空字串代表這份 config.yaml
		// **根本沒有這一段**（既有 Workspace 的形態）。
		fileSection string
		profile     string
		wantWarn    bool
	}{
		{
			name:        "空白名單且 Profile 列了 File Tool 時警示",
			fileSection: "file:\n  allowed_paths: []\n",
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			// 免遷移的錨點：既有 Workspace 的 config.yaml 沒有 file 段，啟動照常成功、
			// 視為空白名單（於是同樣印那行提醒），不需要任何遷移動作。
			name:        "缺 file 段時視為空白名單、照常啟動",
			fileSection: "",
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			// 「有效條目」與「切片長度」是兩件事：只寫了一條空字串的白名單，校驗器
			// 眼中仍然是空的（那條條目放行不了任何路徑），所以提醒照樣要出現。
			// 只看長度的話使用者會遇到「Tool 有了、每次呼叫都被攔」而毫無線索——
			// 正是這行提醒存在的理由。
			name:        "白名單只有一條空字串時仍要警示",
			fileSection: fileSectionEmptyStringEntry,
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			name:        "白名單只有純空白條目時仍要警示",
			fileSection: fileSectionBlankEntry,
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			// 同一類的第二種形態：條目寫成絕對路徑。白名單的基準是 Workspace 根，
			// 絕對條目永遠比不中任何請求路徑——使用者「照著錯誤訊息把目錄加進去了」
			// 卻還是每次被攔，是最難自己查出來的一種。
			name:        "白名單條目是絕對路徑時仍要警示",
			fileSection: fileSectionAbsoluteEntry,
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			name:        "白名單條目穿越出 Workspace 時仍要警示",
			fileSection: fileSectionEscapingEntry,
			profile:     profileWithReadFile,
			wantWarn:    true,
		},
		{
			name:        "已配置白名單不警示",
			fileSection: "file:\n  allowed_paths: [notes]\n",
			profile:     profileWithReadFile,
			wantWarn:    false,
		},
		{
			// 對照：混了一條空白條目，但有一條真的路徑，就不該再喊。
			name:        "白名單含空白條目但也有有效條目時不警示",
			fileSection: fileSectionBlankAndValidEntry,
			profile:     profileWithReadFile,
			wantWarn:    false,
		},
		{
			name:        "Profile 只列 write_file 時同樣警示",
			fileSection: "file:\n  allowed_paths: []\n",
			profile:     profileWithWriteFile,
			wantWarn:    true,
		},
		{
			name:        "Profile 只列 list_dir 時同樣警示",
			fileSection: "file:\n  allowed_paths: []\n",
			profile:     profileWithListDir,
			wantWarn:    true,
		},
		{
			name:        "Profile 沒列 File Tool 時不警示（純對話不受影響）",
			fileSection: "file:\n  allowed_paths: []\n",
			profile:     profileWithoutFileTool,
			wantWarn:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			writeProfile(t, dir, tt.profile)
			cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + srv.URL +
				"\nhttp:\n  allowed_domains: []\n" + tt.fileSection
			if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}
			warned := strings.Contains(out.String(), "allowed_paths")
			if warned != tt.wantWarn {
				t.Errorf("警示出現 = %v, 期望 %v; 輸出: %q", warned, tt.wantWarn, out.String())
			}
		})
	}
}

// TestChatStartupValidationFailsBeforeAnyTurn 釘住「**啟動**即報錯」這件事本身：
// 不送任何訊息（stdin 直接 EOF）時仍要報錯。
//
// 為什麼需要獨立一條：`TestChatErrors` 用 `--message` 驅動，會跑一個 turn，而
// bootstrap 與 skills 的載入**每個 turn 都會重做一次**——把啟動校驗整個拿掉，第一個
// turn 照樣失敗、`runChat` 照樣回錯誤，那組測試分不出「啟動就擋」與「第一句話才爆」。
// 實測確認過：拿掉 chat.go 的兩處校驗，`TestChatErrors` 全綠。
//
// 差別在互動模式：有啟動校驗，`oryxos chat` 根本起不來；沒有的話使用者會拿到提示符、
// 打完一句話才發現設定錯了。AC 要的是前者。
func TestChatStartupValidationFailsBeforeAnyTurn(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantSub string
	}{
		{
			name: "bootstrap 列出的檔案不存在",
			setup: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, workspaceDir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - AGENTS.md\n")
			},
			wantSub: "AGENTS.md",
		},
		{
			name: "skills 引用不存在的 Skill",
			setup: func(t *testing.T, dir string) {
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n  - ghost\n")
			},
			wantSub: "ghost",
		},
		{
			// 設定錯誤（打錯 server 名），與「server 連不上」的環境問題不同質：
			// 這裡 fail fast，連一個 turn 都不跑。
			name: "mcp_servers 引用宣告檔裡沒有的 server",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n  demo:\n    transport: stdio\n    command: [true]\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - ghost-server\n")
			},
			wantSub: "ghost-server",
		},
		{
			name: "mcp_servers 重複列出同一個 server",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n  demo:\n    transport: stdio\n    command: [true]\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - demo\n  - demo\n")
			},
			wantSub: "demo",
		},
		{
			// **即使這個 Profile 根本不用 MCP 也要擋**。壞掉的宣告檔最常見的形態是某個
			// server 的縮排或引號寫錯，靜默忽略會讓那個 server 無聲消失——使用者只會
			// 看到 Agent 少了一半能力，卻沒有任何線索。這與「檔案不存在」不同質，後者
			// 是明確的「這個 Workspace 不用 MCP」。
			name: "mcp_servers.yaml 解析失敗：即使 Profile 沒引用任何 server 也擋下啟動",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n  demo:\n   transport: stdio\n     command: [true]\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n")
			},
			wantSub: "mcp_servers.yaml",
		},
		{
			// 同名重複宣告不得靜默覆蓋成最後一份。這條由 YAML 的 map key 唯一性擋下，
			// 在組裝點再驗一次是因為使用者實際踩到的是這一層。
			name: "同名 server 重複宣告：啟動報錯",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n  demo:\n    transport: stdio\n    command: [a]\n"+
					"  demo:\n    transport: stdio\n    command: [b]\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - demo\n")
			},
			wantSub: "demo",
		},
		{
			// 非 stdio 是**靜態宣告缺陷**：換一台機器也不會好，所以 fail fast 而不是
			// 降級警示。錯誤要指出核心階段只支援 stdio，不能只說「不支援」。
			name: "宣告 transport: sse：啟動報錯指出只支援 stdio",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n  remote:\n    transport: sse\n    command: [x]\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - remote\n")
			},
			wantSub: "stdio",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t) // 不期望任何 LLM 呼叫——連一個 turn 都不該跑到
			dir := setupChatWorkspace(t, srv.URL)
			tt.setup(t, dir)

			var out bytes.Buffer
			// 互動模式 ＋ 空 stdin：設定沒問題的話會直接 EOF 乾淨返回（nil），
			// 所以這裡拿到的錯誤只可能來自啟動校驗。
			err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default"})
			if err == nil {
				t.Fatal("設定錯誤應在啟動時報錯，實際乾淨返回（校驗被推遲到第一個 turn 了）")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestChatSkillSectionOverflowWarning 釘住 Skill 段截斷的**啟動警示**（ticket #19）。
//
// 被截掉的不是「內容變短」而是**整份 Skill 從 LLM 視野消失**——使用者會看到 Agent
// 莫名其妙不會做某件事，卻查不出原因。system prompt 裡的標記只有 LLM 看得到，所以
// 必須另外在啟動時對**使用者**喊一聲。
//
// prior art：TestChatEmptyWhitelistWarning（斷言 CLI 輸出含警示字串）。
func TestChatSkillSectionOverflowWarning(t *testing.T) {
	// 每份 description 取 900 rune（在標準 1024 上限內），十五份合計約 13500 rune，
	// 超過 Skill 段的 10000 上限。
	const (
		descRunes = 900
		fitting   = 3
		overflow  = 15
	)

	tests := []struct {
		name     string
		count    int
		wantWarn bool
	}{
		{name: "沒有超過上限：不警示", count: fitting},
		{name: "超過上限：啟動時警示", count: overflow, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)

			refs := make([]string, 0, tt.count)
			for i := range tt.count {
				name := "skill-" + strconv.Itoa(i)
				doc := "---\nname: " + name + "\ndescription: " + strings.Repeat("述", descRunes) + "\n---\n\n正文\n"
				writeSkillFile(t, dir, name, doc)
				refs = append(refs, "  - "+name)
			}
			writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n"+strings.Join(refs, "\n")+"\n")

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}

			warned := strings.Contains(out.String(), "Skill")
			if warned != tt.wantWarn {
				t.Errorf("警示出現 = %v, 期望 %v; 輸出: %q", warned, tt.wantWarn, out.String())
			}
			if tt.wantWarn && !strings.ContainsAny(out.String(), "0123456789") {
				// 警示要說出**有幾份消失了**，不然使用者只知道「有問題」、
				// 不知道問題多大。份數本身不寫死——那取決於描述長度。
				t.Errorf("警示未說出被丟棄的份數: %q", out.String())
			}
		})
	}
}

// TestChatSkillOverflowLoggedWithZeroTurns 釘住啟動時的結構化日誌**不能被每-turn
// 日誌取代**：互動模式開起來就 EOF（一個 turn 都沒跑）時，引擎層一次都沒被呼叫過，
// 只有啟動那一次記得下來。
//
// 兩個管道各自涵蓋不同的情形：啟動這次涵蓋「零 turn」，每-turn 那次涵蓋「對話中途
// 才溢出」（見 core 的 TestSkillSectionTruncationLoggedEveryTurn）。
func TestChatSkillOverflowLoggedWithZeroTurns(t *testing.T) {
	const (
		count     = 15
		descRunes = 900
	)
	srv := newReplayServer(t) // 不期望任何 LLM 呼叫
	dir := setupChatWorkspace(t, srv.URL)

	refs := make([]string, 0, count)
	for i := range count {
		name := "skill-" + strconv.Itoa(i)
		writeSkillFile(t, dir, name,
			"---\nname: "+name+"\ndescription: "+strings.Repeat("述", descRunes)+"\n---\n\n正文\n")
		refs = append(refs, "  - "+name)
	}
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n"+strings.Join(refs, "\n")+"\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}

	logged, err := os.ReadFile(filepath.Join(dir, workspaceDir, "logs", "oryxos.log"))
	if err != nil {
		t.Fatalf("讀取日誌檔: %v", err)
	}
	// 斷言事件鍵而非措辭（spec #3 對降級事件的既有形狀）。
	if !strings.Contains(string(logged), "skill_section_truncated") {
		t.Errorf("一個 turn 都沒跑時仍應有啟動的結構化警示，日誌內容: %q", string(logged))
	}
}

// TestChatLoadSkillAutoIncluded 釘住 ticket #20 的自動加入語義，從**組裝點**驗——
// 三個邊界格分別對應三種使用者寫法。
//
// 驗的方式是看送往 LLM 邊界的 tools 清單裡有沒有 load_skill：那是「這個 Agent 能不
// 能用它」的唯一外部可觀察形式。
func TestChatLoadSkillAutoIncluded(t *testing.T) {
	tests := []struct {
		name string
		// profileBody 是 default.yaml 除 name 外的內容。
		profileBody string
		seedSkill   bool
		wantTool    bool
	}{
		{
			// 最該保護的一格：宣告了 skills 卻沒列 load_skill，不能安靜退化成
			// 「看得到描述、永遠載不到正文」。
			name:        "skills 非空、tools 沒列：自動加入",
			profileBody: "provider:\n  name: openrouter\n  model: m\nskills:\n  - digest\n",
			seedSkill:   true,
			wantTool:    true,
		},
		{
			// 使用者把設定寫清楚不該被懲罰——冪等合併，不報錯也不重複。
			name:        "skills 非空、tools 也顯式列了：冪等合併不報錯",
			profileBody: "provider:\n  name: openrouter\n  model: m\ntools:\n  - load_skill\nskills:\n  - digest\n",
			seedSkill:   true,
			wantTool:    true,
		},
		{
			// 允許啟動；呼叫時才回「這個 Profile 沒有宣告任何 Skill」的錯誤回填。
			// 一個沒有 Skill 的 Agent 沒理由起不來。
			name:        "skills 為空但 tools 顯式列了：允許啟動",
			profileBody: "provider:\n  name: openrouter\n  model: m\ntools:\n  - load_skill\n",
			wantTool:    true,
		},
		{
			name:        "skills 為空、tools 也沒列：不出現",
			profileBody: "provider:\n  name: openrouter\n  model: m\n",
			wantTool:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			if tt.seedSkill {
				writeSkillFile(t, dir, "digest",
					"---\nname: digest\ndescription: 做摘要。需要摘要時使用。\n---\n\n## 步驟\n\n1. 做摘要\n")
			}
			writeProfile(t, dir, tt.profileBody)

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
				chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}

			if len(reqs) == 0 {
				t.Fatal("沒有收到任何 LLM 請求，取不到工具清單")
			}
			got := requestToolNames(t, reqs[0])
			if slices.Contains(got, "load_skill") != tt.wantTool {
				t.Errorf("送往 LLM 的工具清單 = %v，load_skill 出現 = %v，期望 %v",
					got, slices.Contains(got, "load_skill"), tt.wantTool)
			}
			// 冪等：出現就只能出現一次，重複會讓 OpenAI 兼容端點拒絕請求。
			if n := slices.Index(got, "load_skill"); n >= 0 {
				if c := countName(got, "load_skill"); c != 1 {
					t.Errorf("load_skill 在工具清單裡出現 %d 次，期望 1 次: %v", c, got)
				}
			}
		})
	}
}

// TestChatRegistersNativeGoToolExample 釘住原生 Go Tool 示例（Plugin Tool 方式三）
// **在組裝點被顯式註冊**（憲法 2.3）。少了 buildToolRegistry 那一行 Register，Profile
// 一列 text_stats 就會在 Subset 被判成「未註冊」而整個起不來——業務方照抄樣板時第一
// 個會撞上的坑，而 internal/core 那邊自組 Registry 的測試看不到它。
//
// 驗的方式與 TestChatLoadSkillAutoIncluded 同形：看送往 LLM 邊界的 tools 清單，那是
// 「這個 Agent 能不能用它」唯一的外部可觀察形式。
func TestChatRegistersNativeGoToolExample(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\ntools:\n  - text_stats\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "你好"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}

	if len(reqs) == 0 {
		t.Fatal("沒有收到任何 LLM 請求，取不到工具清單")
	}
	if got := requestToolNames(t, reqs[0]); !slices.Contains(got, "text_stats") {
		t.Errorf("送往 LLM 的工具清單 = %v, 期望含 text_stats", got)
	}
}

// requestToolNames 取出一次 LLM 邊界請求宣告的工具名。
func requestToolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析 LLM 請求: %v", err)
	}
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}
	return names
}

func countName(names []string, want string) int {
	n := 0
	for _, s := range names {
		if s == want {
			n++
		}
	}
	return n
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
	work := "name: work\nidentity:\n  agent_name: Worker\n  prompt: 你是工作助理。\nprovider:\n  name: openrouter\n  model: gpt-4o-mini\n"
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
	work := "name: work\nidentity:\n  agent_name: Worker\n  prompt: 你是工作助理。\nprovider:\n  name: openrouter\n  model: gpt-4o-mini\n"
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
				os.Unsetenv("OPENROUTER_API_KEY") // t.Setenv 已註冊測試結束時還原
				return dir, "default"
			},
			wantSub: "OPENROUTER_API_KEY",
		},
		{
			name: "Profile 引用未註冊的 Tool",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				p := "name: default\nprovider:\n  name: openrouter\n  model: m\ntools:\n  - no_such_tool\n"
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
		{
			// bootstrap 明確列了一份檔案，磁碟上卻沒有 → 設定錯誤，啟動即報錯。
			// 與「省略欄位時缺檔視為該層為空」是刻意不同的兩件事：省略是沒意見，
			// 列出是明確要求。
			name: "bootstrap 列出的檔案不存在",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				if err := os.Remove(filepath.Join(dir, workspaceDir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - AGENTS.md\n")
				return dir, "default"
			},
			wantSub: "AGENTS.md",
		},
		{
			name: "bootstrap 列出未知檔名",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - NOTES.md\n")
				return dir, "default"
			},
			wantSub: "NOTES.md",
		},
		{
			name: "bootstrap 重複列出同一份",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nbootstrap:\n  - USER.md\n  - USER.md\n")
				return dir, "default"
			},
			wantSub: "USER.md",
		},
		{
			name: "skills 引用不存在的 Skill",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n  - ghost\n")
				return dir, "default"
			},
			wantSub: "ghost",
		},
		{
			// 引用值不是合法的 Skill 名稱——路徑逃逸在讀檔之前就被擋掉。
			name: "skills 引用值含路徑逃逸",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n  - ../../etc/passwd\n")
				return dir, "default"
			},
			wantSub: "../../etc/passwd",
		},
		{
			name: "skills 引用的 SKILL.md frontmatter 不合法",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeSkillFile(t, dir, "digest", "---\nname: digest\n---\n\n正文\n") // 缺 description
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n  - digest\n")
				return dir, "default"
			},
			wantSub: "description",
		},
		{
			name: "skills 的 frontmatter name 與引用名不一致",
			setup: func(t *testing.T) (string, string) {
				dir := setupChatWorkspace(t, "http://127.0.0.1:1")
				writeSkillFile(t, dir, "digest", "---\nname: other\ndescription: 做摘要。\n---\n\n正文\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nskills:\n  - digest\n")
				return dir, "default"
			},
			wantSub: "other",
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

// TestStockWorkspaceInjectsNoPlaceholder 是 init → chat 的整合驗證：一個**未經編輯**
// 的出廠 Workspace，跑一輪對話後 system prompt 只有 Profile 的 identity.prompt，
// 不含任何 Bootstrap 模板文字。
//
// 這條防的是一個很容易漏掉的回歸：Bootstrap 檔案的內容會被逐字注入每個 turn，
// 所以出廠模板一旦帶說明文字，LLM 就會把「描述這個專案怎麼做事：慣例、流程、
// 禁忌」當成真的專案慣例來遵循；SOUL.md 更糟——Profile 沒設 identity.prompt 時
// 那段說明會變成 Agent 的整個人格。
func TestStockWorkspaceInjectsNoPlaceholder(t *testing.T) {
	var reqs [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("讀取 LLM 請求: %v", err)
		}
		reqs = append(reqs, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(readFixture(t, "chat_reply_1.json")))
	}))
	t.Cleanup(srv.Close)

	dir := setupChatWorkspace(t, srv.URL) // 只覆寫 config.yaml，Bootstrap 三檔維持出廠狀態
	if err := runChat(context.Background(), strings.NewReader("早安\n/quit\n"), io.Discard, dir, chatOptions{profileName: "default"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if len(reqs) == 0 {
		t.Fatal("沒有送出任何 LLM 請求")
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[0], &req); err != nil {
		t.Fatalf("解析 LLM 請求: %v", err)
	}
	var system string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = m.Content
			break
		}
	}

	// 出廠 Workspace 的 system prompt 應恰好是 Profile 的 identity.prompt。
	const wantIdentity = "你是 Oryx，一個樂於助人的通用助理。回答力求精確、直接。"
	if strings.TrimSpace(system) != wantIdentity {
		t.Errorf("出廠 Workspace 的 system prompt 應恰好是 identity.prompt，實際:\n%s", system)
	}
}

// legacyBootstrapTemplates 是 spec #1～#2 時期 `oryxos init` 寫進 Workspace 的三份
// Bootstrap 說明文字。當時它們從未被載入，所以是無害的；spec #3 讓 Bootstrap 生效
// 之後，**既有 Workspace 升級就會把這些字逐字注入每個 turn**。
//
// 這份副本刻意寫死在測試裡而不從 internal/config 引用：它模擬的是「舊版二進制在
// 使用者磁碟上留下的東西」，而不是當前程式碼的任何常數——兩者若哪天不同步，該轉紅
// 的正是這條測試。
var legacyBootstrapTemplates = map[string]string{
	"AGENTS.md": `# AGENTS.md — 專案級行為說明

由你手寫、OryxOS 只讀不寫。描述這個專案怎麼做事：慣例、流程、禁忌。
內容之後會載入 Agent 的系統提示詞；留空亦可。
`,
	"SOUL.md": `# SOUL.md — 預設 Agent 人格定義

由你手寫、OryxOS 只讀不寫。定義 Agent 的人格與語氣。
注意：若 Profile 已設定 identity.prompt，則以其為準，本檔不載入（兩者互斥）。
`,
	"USER.md": `# USER.md — 使用者偏好

由你手寫、OryxOS 只讀不寫。記錄你的偏好：語言、輸出風格、常用約定等。
`,
}

// crlfLegacyBootstrapTemplates 是同一份舊模板的 CRLF 版本，模擬 Windows 上
// `core.autocrlf=true`（Git for Windows 安裝時的預設）checkout 出來的形態。
// Bootstrap 檔案設計成隨 Workspace 進 git，所以這是真會發生的磁碟狀態。
//
// 刻意獨立寫死、不從 LF 版轉換：要驗的正是「換行形態不同的同一份未編輯模板」
// 也要被辨識出來，用程式轉換等於拿實作的假設去驗實作。
var crlfLegacyBootstrapTemplates = map[string]string{
	"AGENTS.md": "# AGENTS.md — 專案級行為說明\r\n\r\n由你手寫、OryxOS 只讀不寫。描述這個專案怎麼做事：慣例、流程、禁忌。\r\n內容之後會載入 Agent 的系統提示詞；留空亦可。\r\n",
	"SOUL.md":   "# SOUL.md — 預設 Agent 人格定義\r\n\r\n由你手寫、OryxOS 只讀不寫。定義 Agent 的人格與語氣。\r\n注意：若 Profile 已設定 identity.prompt，則以其為準，本檔不載入（兩者互斥）。\r\n",
	"USER.md":   "# USER.md — 使用者偏好\r\n\r\n由你手寫、OryxOS 只讀不寫。記錄你的偏好：語言、輸出風格、常用約定等。\r\n",
}

// TestUpgradedLegacyWorkspaceInjectsNoPlaceholder 是升級路徑的回歸測試：一個
// spec #1～#2 時期建立的 Workspace（三份 Bootstrap 帶著當時的說明文字），換上新
// 二進制後跑對話，那些說明文字**不得**進 system prompt。
//
// 「刪掉三份檔案」不能代替這條——真實的舊 Workspace 是檔案在、且帶著舊內容，而
// `oryxos init` 偵測到既有 Workspace 就完全不動它，使用者不會、也不該被要求手動清檔。
//
// 兩種 Profile 都驗：有 identity.prompt 的（舊 AGENTS.md／USER.md 會被注入），
// 以及**沒有** identity.prompt 的——後者是最糟的形態，舊 SOUL.md 的說明文字會直接
// 變成 Agent 的整個人格。
func TestUpgradedLegacyWorkspaceInjectsNoPlaceholder(t *testing.T) {
	tests := []struct {
		name           string
		identityPrompt string // 空字串代表 Profile 不設 identity.prompt
		// legacy 是磁碟上那三份舊檔的內容，涵蓋 LF 與 CRLF 兩種換行形態。
		legacy map[string]string
	}{
		{name: "LF：Profile 有 identity.prompt", identityPrompt: "你是 Oryx，回答力求精確、直接。", legacy: legacyBootstrapTemplates},
		{name: "LF：Profile 沒有 identity.prompt（舊 SOUL.md 會當人格）", legacy: legacyBootstrapTemplates},
		{name: "CRLF：Profile 有 identity.prompt", identityPrompt: "你是 Oryx，回答力求精確、直接。", legacy: crlfLegacyBootstrapTemplates},
		{name: "CRLF：Profile 沒有 identity.prompt", legacy: crlfLegacyBootstrapTemplates},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("讀取 LLM 請求: %v", err)
				}
				reqs = append(reqs, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(readFixture(t, "chat_reply_1.json")))
			}))
			t.Cleanup(srv.Close)

			dir := setupChatWorkspace(t, srv.URL)
			// 把 Workspace 倒回「舊版 init 剛跑完」的狀態。
			for name, content := range tt.legacy {
				if err := os.WriteFile(filepath.Join(dir, workspaceDir, name), []byte(content), 0o644); err != nil {
					t.Fatalf("還原舊版 %s: %v", name, err)
				}
			}
			profile := "name: default\nprovider:\n  name: openrouter\n  model: m\n"
			if tt.identityPrompt != "" {
				profile = "name: default\nidentity:\n  agent_name: Oryx\n  prompt: " + tt.identityPrompt +
					"\nprovider:\n  name: openrouter\n  model: m\n"
			}
			if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "default.yaml"), []byte(profile), 0o644); err != nil {
				t.Fatalf("覆寫 Profile: %v", err)
			}

			if err := runChat(context.Background(), strings.NewReader("早安\n/quit\n"), io.Discard, dir, chatOptions{profileName: "default"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}
			if len(reqs) == 0 {
				t.Fatal("沒有送出任何 LLM 請求")
			}

			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(reqs[0], &req); err != nil {
				t.Fatalf("解析 LLM 請求: %v", err)
			}
			var system string
			for _, m := range req.Messages {
				if m.Role == "system" {
					system = m.Content
					break
				}
			}

			// 舊模板的任何一句都不得出現。
			for _, leaked := range []string{
				"由你手寫、OryxOS 只讀不寫",
				"描述這個專案怎麼做事",
				"定義 Agent 的人格與語氣",
				"記錄你的偏好",
			} {
				if strings.Contains(system, leaked) {
					t.Errorf("升級後的舊 Workspace 把出廠說明文字注入了 system prompt（%q）:\n%s", leaked, system)
				}
			}
			if strings.TrimSpace(system) != strings.TrimSpace(tt.identityPrompt) {
				t.Errorf("system prompt 應恰好是 identity.prompt（本例 %q），實際:\n%s", tt.identityPrompt, system)
			}
		})
	}
}

// profileWithShell 是列了 shell 的 Profile，供啟動提醒那幾格使用。
const profileWithShell = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools:
  - shell
`

// TestChatEmptyShellWhitelistWarning 是啟動提醒的第三種（命令白名單），形狀比照
// 上面兩條。少了這一行，使用者會遇到「Tool 有了、每次呼叫都被攔」而毫無線索——
// 而三段白名單出廠就是空的，所以這是 out-of-box 一定會走到的路徑。
func TestChatEmptyShellWhitelistWarning(t *testing.T) {
	const profileWithoutShell = `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools: []
`
	tests := []struct {
		name string
		// shellSection 是 config.yaml 的整段 shell 設定；空字串代表這份 config.yaml
		// **根本沒有這一段**（既有 Workspace 的形態）。
		shellSection string
		profile      string
		wantWarn     bool
	}{
		{
			name:         "空白名單且 Profile 列了 shell 時警示",
			shellSection: "shell:\n  allowed_commands: []\n",
			profile:      profileWithShell,
			wantWarn:     true,
		},
		{
			// 免遷移：既有 Workspace 的 config.yaml 沒有 shell 段，啟動照常成功、
			// 視為空白名單（於是同樣印那行提醒）。
			name:         "缺 shell 段時視為空白名單、照常啟動",
			shellSection: "",
			profile:      profileWithShell,
			wantWarn:     true,
		},
		{
			// 「有效條目」與「切片長度」是兩件事：寫成路徑的條目永遠比不中任何請求
			// （合法的 command 不含路徑分隔符），只看長度會把它當成「已配置」而閉嘴。
			name:         "白名單條目寫成路徑時仍要警示",
			shellSection: "shell:\n  allowed_commands:\n    - /usr/bin/git\n",
			profile:      profileWithShell,
			wantWarn:     true,
		},
		{
			name:         "白名單只有空字串條目時仍要警示",
			shellSection: "shell:\n  allowed_commands:\n    - \"\"\n",
			profile:      profileWithShell,
			wantWarn:     true,
		},
		{
			name:         "已配置白名單不警示",
			shellSection: "shell:\n  allowed_commands: [git]\n",
			profile:      profileWithShell,
			wantWarn:     false,
		},
		{
			name:         "Profile 沒列 shell 時不警示（純對話不受影響）",
			shellSection: "shell:\n  allowed_commands: []\n",
			profile:      profileWithoutShell,
			wantWarn:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			writeProfile(t, dir, tt.profile)
			cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + srv.URL +
				"\nhttp:\n  allowed_domains: []\n" + tt.shellSection
			if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}
			warned := strings.Contains(out.String(), "allowed_commands")
			if warned != tt.wantWarn {
				t.Errorf("警示出現 = %v, 期望 %v; 輸出: %q", warned, tt.wantWarn, out.String())
			}
		})
	}
}

// TestChatPathOverlapWarning 是**內部提權路徑**的啟動提醒（ADR-0005 第一條，US 59／62）。
//
// 要防的是這件事：父進程的 PATH 若含有一個落在 file.allowed_paths 之內的目錄，Agent
// 光靠**已被授權的 write_file** 就能在那裡放一個與白名單命令同名的檔案、或覆寫該目錄
// 下既有的可執行檔——**把「寫檔權限」升級成「執行白名單內程式的權限」**。兩個能力都是
// 使用者自己開的，所以「攻擊者本來就能寫檔了」那套論證在此不成立。
//
// 三格分別打掉三種會做錯的比對：純字串比對（漏掉符號連結那格）、只看字面前綴
// （foobar 那格會誤報）、以及完全不做（第一格）。**警告而非 fail fast**：重疊可能是
// 使用者刻意的。
func TestChatPathOverlapWarning(t *testing.T) {
	// setupPath 在 Workspace 底下建目錄、回傳要放進 PATH 的那個路徑。
	tests := []struct {
		name string
		// setupPath 回傳要塞進 PATH 的目錄；ws 是 Workspace 根（.oryxos 那一層）。
		setupPath   func(t *testing.T, ws string) string
		allowedPath string
		wantWarn    bool
	}{
		{
			name: "PATH 含落在白名單內的絕對目錄要警告",
			setupPath: func(t *testing.T, ws string) string {
				dir := filepath.Join(ws, "scripts", "bin")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("建立 scripts/bin: %v", err)
				}
				return dir
			},
			allowedPath: "scripts",
			wantWarn:    true,
		},
		{
			name: "PATH 是指向白名單內目錄的符號連結也要警告",
			setupPath: func(t *testing.T, ws string) string {
				target := filepath.Join(ws, "scripts", "bin")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatalf("建立 scripts/bin: %v", err)
				}
				// 連結放在 Workspace **之外**：字面上完全看不出重疊。
				link := filepath.Join(t.TempDir(), "tools")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("此環境不支援建立符號連結: %v", err)
				}
				return link
			},
			allowedPath: "scripts",
			wantWarn:    true,
		},
		{
			name: "子樹前綴假匹配不得警告",
			setupPath: func(t *testing.T, ws string) string {
				dir := filepath.Join(ws, "foobar")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("建立 foobar: %v", err)
				}
				if err := os.MkdirAll(filepath.Join(ws, "foo"), 0o755); err != nil {
					t.Fatalf("建立 foo: %v", err)
				}
				return dir
			},
			allowedPath: "foo",
			wantWarn:    false,
		},
		{
			name: "Workspace 之外的 PATH 目錄不警告",
			setupPath: func(t *testing.T, ws string) string {
				if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
					t.Fatalf("建立 scripts: %v", err)
				}
				return t.TempDir()
			},
			allowedPath: "scripts",
			wantWarn:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, "chat_reply_1.json"))
			dir := setupChatWorkspace(t, srv.URL)
			ws := filepath.Join(dir, workspaceDir)
			pathDir := tt.setupPath(t, ws)
			// PATH 只放那一個目錄：不摻真實 PATH，讓這一格只驗重疊判斷本身。
			t.Setenv("PATH", pathDir)

			cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + srv.URL +
				"\nhttp:\n  allowed_domains: []\nfile:\n  allowed_paths: [" + tt.allowedPath + "]\n"
			if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := runChat(context.Background(), strings.NewReader(""), &out, dir, chatOptions{profileName: "default", message: "你好"}); err != nil {
				t.Fatalf("runChat: %v", err)
			}
			warned := strings.Contains(out.String(), "升級成執行權限")
			if warned != tt.wantWarn {
				t.Errorf("重疊警示出現 = %v, 期望 %v; 輸出: %q", warned, tt.wantWarn, out.String())
			}
		})
	}
}

// TestChatWithoutFileAndShellSectionsRejectsBothToolGroups 是**免遷移的完整錨點**。
//
// 一份**兩段都沒有**的 config.yaml（既有 Workspace 的形態）必須：啟動成功，**而且
// 兩組 Tool 一律拒絕**。這一條同時證明「免遷移」與「空白名單即全拒」兩件事——
// #30 只驗了 file 那一半，這裡收尾。
//
// 斷言落在**落庫的對話歷史**上：兩則 tool 訊息都要是 SandboxViolation。只斷言「啟動
// 成功」不算通過——一個把缺欄位讀成「全部放行」的實作也會啟動成功。
func TestChatWithoutFileAndShellSectionsRejectsBothToolGroups(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "chat_reply_read_file_denied.json"),
		readFixture(t, "chat_reply_shell_denied.json"),
		readFixture(t, "chat_reply_both_denied_final.json"),
	)
	// setupChatWorkspace 寫出的 config.yaml 本來就只有 providers 與 http 兩段——
	// 那正是既有 Workspace 的形態，不需要另外做什麼。
	dir := setupChatWorkspace(t, srv.URL)
	writeProfile(t, dir, `identity:
  agent_name: Oryx
  prompt: 你是 Oryx。
provider:
  name: openrouter
  model: deepseek/deepseek-v4-flash
tools:
  - read_file
  - shell
`)

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out,
		dir, chatOptions{profileName: "default", message: "看一下設定檔，再列個目錄"}); err != nil {
		t.Fatalf("缺 file 與 shell 兩段時應照常啟動: %v", err)
	}

	rows := sessionRows(t, filepath.Join(dir, workspaceDir, sessionDBFile))
	if len(rows) != 1 {
		t.Fatalf("sessions 資料列數 = %d, 期望 1", len(rows))
	}
	var messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rows[0].messagesJSON), &messages); err != nil {
		t.Fatalf("解析 messages_json: %v", err)
	}

	var toolMessages []string
	for _, m := range messages {
		if m.Role == "tool" {
			toolMessages = append(toolMessages, m.Content)
		}
	}
	if len(toolMessages) != 2 {
		t.Fatalf("tool 訊息數 = %d, 期望 2（read_file 一次、shell 一次）: %v", len(toolMessages), messages)
	}
	for i, content := range toolMessages {
		if !strings.Contains(content, "SandboxViolation") {
			t.Errorf("tool 訊息[%d] = %q, 期望被空白名單擋下（缺欄位 = 空白名單 = 全拒）", i, content)
		}
	}
	// 訊息要指得出去改哪一段，兩組各自的那一段。
	if !strings.Contains(toolMessages[0], "file.allowed_paths") {
		t.Errorf("read_file 的拒絕訊息沒指向 file.allowed_paths: %q", toolMessages[0])
	}
	if !strings.Contains(toolMessages[1], "shell.allowed_commands") {
		t.Errorf("shell 的拒絕訊息沒指向 shell.allowed_commands: %q", toolMessages[1])
	}
}

// TestShellRuntimeTimeoutComesFromSandboxConfig 釘住「同一份 config.yaml 只搬一次」。
//
// `SandboxConfig.ShellTimeout` 存在的理由就是這句話（見它的欄位說明）。`shellRuntime`
// 若自己再去 `cfg.Shell.EffectiveTimeout()` 取一次，那個欄位就變成沒有人讀的死欄位，
// 而同一個值有了兩條來源——**兩條來源遲早會分岔，而且分岔時沒有任何東西會報錯**。
//
// 真正擋住回歸的是簽章（`shellRuntime` 收的是 `tool.SandboxConfig`，拿不到 `cfg`，
// 編譯器不讓它有第二條來源）；這一格釘的是另一半：那個值真的流到子進程的上限上，
// 而且就是使用者在 `timeout_seconds` 寫的那個數字。
func TestShellRuntimeTimeoutComesFromSandboxConfig(t *testing.T) {
	cfg := &config.Config{Shell: config.ShellConfig{TimeoutSeconds: 7}}
	sandbox := sandboxConfig(cfg)

	runtime := shellRuntime(sandbox, t.TempDir())
	if runtime.Timeout != sandbox.ShellTimeout {
		t.Errorf("ShellRuntime.Timeout = %v, 期望與 SandboxConfig.ShellTimeout %v 同源",
			runtime.Timeout, sandbox.ShellTimeout)
	}
	if runtime.Timeout != 7*time.Second {
		t.Errorf("ShellRuntime.Timeout = %v, 期望是 config.yaml 寫的 7 秒", runtime.Timeout)
	}

	// 三態回退也要一路流下來：省略／零值回退預設，不是回退成零（那會讓每次呼叫
	// 在啟動的瞬間就假逾時）。
	if got := shellRuntime(sandboxConfig(&config.Config{}), t.TempDir()).Timeout; got != 30*time.Second {
		t.Errorf("缺 shell 段時的 Timeout = %v, 期望回退預設的 30 秒", got)
	}
}

// TestShellLimiterIsSharedAcrossBuildToolRegistryCalls 是矩陣 (4b)：admission slot 的
// 上限**跨 buildToolRegistry 的兩個呼叫點仍然守得住**。
//
// **為什麼不能只驗「共用時會共用」。** spec #29 明寫「手動把同一個 limiter 傳給兩個
// Tool 實例**不算通過**——那只驗了共用的效果，驗不到**建立點在對的那一層**，而後者才是
// 定案的內容（下修表第十七列：第六輪的修法把 limiter 建在 buildToolRegistry 裡，而它有
// **兩個呼叫點**，於是「整個進程一份」當場失效）。
//
// 所以這一格照 composition root 的實際形狀走：limiter 在**呼叫 buildToolRegistry 之前**
// 建立一次，然後餵給**兩次真實的 buildToolRegistry 呼叫**（模擬 chat 與 tools 兩條路徑），
// 讓兩邊拿到的 shell 共同競爭同一組名額。
//
// **這一格會抓到的回歸**：有人把 tool.NewShellLimiter() 移進 buildToolRegistry。那時
// 兩個 registry 各拿一份 8 格的池子，9 個並發呼叫全都進得去，下面的「至少一個被拒」
// 就會失敗。
func TestShellLimiterIsSharedAcrossBuildToolRegistryCalls(t *testing.T) {
	const maxWorkers = 8 // 與 internal/tool 的 maxShellLifecycleWorkers 對齊（對外契約）

	srv := newReplayServer(t)
	baseDir := setupChatWorkspace(t, srv.URL)
	ws := filepath.Join(baseDir, workspaceDir)
	cfgYAML := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + srv.URL +
		"\nhttp:\n  allowed_domains: []\nshell:\n  allowed_commands: [sleep]\n  timeout_seconds: 2\n"
	if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(ws, "config.yaml"))
	if err != nil {
		t.Fatalf("載入設定: %v", err)
	}
	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatalf("開啟 Workspace: %v", err)
	}
	defer func() { _ = wsRoot.Close() }()

	sandbox := sandboxConfig(cfg)
	longTerm := memory.NewLongTermMemory(wsRoot, filepath.Join("memory", "MEMORY.md"))
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// ── composition root：建立一次 ──
	shellLimiter := tool.NewShellLimiter()

	// ── 兩次真實的 buildToolRegistry 呼叫（chat 與 tools 兩條路徑） ──
	var shells []*tool.Executor
	for i := range 2 {
		registry, err := buildToolRegistry(sandbox, shellRuntime(sandbox, ws), shellLimiter,
			wsRoot, longTerm, config.NewContextLoader(wsRoot), nil)
		if err != nil {
			t.Fatalf("第 %d 次 buildToolRegistry: %v", i, err)
		}
		exec, err := registry.Subset([]string{"shell"}, nil, logger)
		if err != nil {
			t.Fatalf("第 %d 次 Subset: %v", i, err)
		}
		shells = append(shells, exec)
	}

	// N+1 個**並發**呼叫，橫跨兩個 registry。循序驗不到——slot 會在每次之間歸還。
	const calls = maxWorkers + 1
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]core.ToolResult, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 一起出發，確保 N+1 個真的同時在場
			results[i] = shells[i%len(shells)].Execute(context.Background(),
				core.ToolCall{ID: strconv.Itoa(i), Name: "shell", Arguments: `{"command":"sleep","args":["30"]}`})
		}()
	}
	close(start)
	wg.Wait()

	rejected := 0
	for _, result := range results {
		if !result.OK && strings.Contains(result.Error, "已達上限") {
			rejected++
		}
	}
	if rejected == 0 {
		t.Errorf("%d 個並發呼叫橫跨兩個 registry，沒有任何一個被 slot 擋下——"+
			"limiter 不是共用的那一份（是不是被移進 buildToolRegistry 內部建立了？）", calls)
	}
	if rejected > calls-maxWorkers {
		t.Errorf("被拒 %d 個，期望至多 %d 個——上限比 %d 更嚴，兩個 registry 沒有共用同一組名額",
			rejected, calls-maxWorkers, maxWorkers)
	}
}
