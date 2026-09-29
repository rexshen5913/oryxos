// `oryxos server` 的多 Profile 並存與失敗語義，ticket #76。
//
// 與 server_test.go 同一個 seam：runServer 接收呼叫端開好的 listener，測試對它發真實的 HTTP
// 請求。seam 之下全部用真的，Provider 以回放伺服器代替（ADR-0002）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// listProfiles 取 GET /api/v1/profiles，回傳 Profile 名 → 那一筆的欄位（原始 JSON）。
//
// 欄位留成原始 JSON 而不是解進結構體：「欄位是 null」與「沒有這個欄位」要分得開，解進結構體
// 之後兩者都是零值。
func listProfiles(t *testing.T, s *runningServer) map[string]map[string]json.RawMessage {
	t.Helper()
	resp, body := s.send(t, http.MethodGet, "/api/v1/profiles", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("profiles 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	var listing struct {
		Profiles []map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("解析 profiles 回應: %v\nbody: %s", err, body)
	}
	byName := make(map[string]map[string]json.RawMessage, len(listing.Profiles))
	for _, entry := range listing.Profiles {
		var name string
		if err := json.Unmarshal(entry["name"], &name); err != nil {
			t.Fatalf("profiles 的某一筆沒有字串形態的 name: %v\nbody: %s", err, body)
		}
		byName[name] = entry
	}
	return byName
}

// stringField 取出一筆的某個字串欄位；欄位不存在或不是字串時測試失敗。
func stringField(t *testing.T, entry map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := entry[key]
	if !ok {
		t.Fatalf("缺少欄位 %q: %v", key, entry)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("欄位 %q 不是字串: %s", key, raw)
	}
	return value
}

// infoProfileCounts 取 GET /api/v1/info 裡可用與不可用的 Profile 數。
func infoProfileCounts(t *testing.T, s *runningServer) (available, unavailable int) {
	t.Helper()
	resp, body := s.send(t, http.MethodGet, "/api/v1/info", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	var info struct {
		Profiles struct {
			Available   int `json:"available"`
			Unavailable int `json:"unavailable"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("解析 info 回應: %v\nbody: %s", err, body)
	}
	return info.Profiles.Available, info.Profiles.Unavailable
}

// lineAfter 回傳 out 裡從 prefix 開始、到換行為止的那一段；沒有這一行時回傳空字串。
func lineAfter(out, prefix string) string {
	_, rest, found := strings.Cut(out, prefix)
	if !found {
		return ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	return prefix + line
}

// writeBrokenProfile 寫入 profiles/broken.yaml，內容原樣寫入（不自動補 name 那行）。
func writeBrokenProfile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "profiles", "broken.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 broken.yaml: %v", err)
	}
}

// neverSetProviderKey 是一個刻意從不設定的環境變數，讓 Provider 的憑證展開失敗。
const neverSetProviderKey = "ORYXOS_TEST_NEVER_SET_PROVIDER_KEY_76"

// configWithUnusableProvider 是 config.yaml 在 setupChatWorkspace 的 openrouter 之外，另外配置
// 一個憑證展不開的 Provider（other）時，接在後面的那一段。
const configWithUnusableProvider = "  other:\n    api_key: ${" + neverSetProviderKey + "}\n" +
	"    base_url: http://127.0.0.1:1\nhttp:\n  allowed_domains: []\n"

// TestServerProfileFailureMatrix 是失敗語義的矩陣（spec #73 第三節，ticket #76 AC）：每一格讓
// broken 以一種方式壞掉，default 保持正常。
//
// 每一格都斷言同一組性質，少一件這張票就沒有意義：
//
//   - **壞掉的那份不可用，而且原因是這一種壞法**（wantReason）。只斷言「不可用」的話，一個把
//     所有 Profile 都判成不可用的實作也會過。
//   - **另一份照常可用**：一份的設定錯誤不擋其他 Agent（使用者故事 4）。
//   - **info 的數量與 profiles 端點一致**：兩個端點各算一次的話，遲早會對不上。
//   - **原因印在啟動輸出，也落錯誤日誌**：運維人員不必翻 API 就知道該修什麼（使用者故事 5）。
//
// tools 那一格的 MCP 在 Subset 擋下之前**已經連上**。那份 Profile 從此用不到它，子進程要在
// 啟動完成時就收掉，不是閒置到 server 關閉——所以那一格在關閉之前就驗 exit marker。
func TestServerProfileFailureMatrix(t *testing.T) {
	tests := []struct {
		name string
		// profile 是 profiles/broken.yaml 的完整內容。
		profile string
		// setup 在啟動之前調整 Workspace；nil 代表不調整。marker 是 MCP exit marker 的路徑。
		setup func(t *testing.T, dir, providerURL, marker string)
		// wantReason 是原因裡必須出現的片段。
		wantReason string
		// wantMarker 為真時，另外驗 broken 已經連上的 MCP 子進程在啟動完成時就被收掉了。
		wantMarker bool
	}{
		{
			name:       "YAML 解析失敗",
			profile:    "name: broken\nprovider: [unclosed\n",
			wantReason: "解析 Profile",
		},
		{
			// name 欄位與檔名不一致：URL 上的名字（檔名）和 Session 記的名字（name 欄位）會是兩個說法。
			name:       "檔名與 name 欄位不一致",
			profile:    "name: renamed\nprovider:\n  name: openrouter\n  model: m\n",
			wantReason: `"renamed"`,
		},
		{
			name:       "引用未配置的 Provider",
			profile:    "name: broken\nprovider:\n  name: ghost\n  model: m\n",
			wantReason: `Provider "ghost"`,
		},
		{
			name:    "引用的 Provider 憑證環境變數缺失",
			profile: "name: broken\nprovider:\n  name: other\n  model: m\n",
			setup: func(t *testing.T, dir, providerURL, _ string) {
				writeWorkspaceConfig(t, dir, providerURL, configWithUnusableProvider)
			},
			wantReason: neverSetProviderKey,
		},
		{
			name:    "bootstrap 明確引用的檔案不存在",
			profile: "name: broken\nprovider:\n  name: openrouter\n  model: m\nbootstrap:\n  - SOUL.md\n",
			setup: func(t *testing.T, dir, _, _ string) {
				// default 沒寫 bootstrap（載入預設三檔，缺檔視為該層為空），所以只有 broken 受影響。
				if err := os.Remove(filepath.Join(dir, workspaceDir, "SOUL.md")); err != nil {
					t.Fatalf("刪除 SOUL.md: %v", err)
				}
			},
			wantReason: "bootstrap 校驗失敗",
		},
		{
			name:       "skills 引用的 Skill 不存在",
			profile:    "name: broken\nprovider:\n  name: openrouter\n  model: m\nskills:\n  - missing-skill\n",
			wantReason: "skills 校驗失敗",
		},
		{
			name:       "mcp_servers 引用未宣告的 server",
			profile:    "name: broken\nprovider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - undeclared\n",
			wantReason: "mcp_servers 校驗失敗",
		},
		{
			name: "tools 引用未註冊的 Tool（MCP 已連上之後才被擋下）",
			profile: "name: broken\nprovider:\n  name: openrouter\n  model: m\n" +
				"mcp_servers:\n  - alive\ntools:\n  - alive__echo\n  - no_such_tool\n",
			setup: func(t *testing.T, dir, _, marker string) {
				writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntryWithExitMarker(t, "alive", marker, "echo"))
			},
			wantReason: "no_such_tool",
			wantMarker: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newReplayServer(t)
			dir := setupChatWorkspace(t, provider.URL)
			marker := filepath.Join(t.TempDir(), "alive.exited")
			if tt.setup != nil {
				tt.setup(t, dir, provider.URL, marker)
			}
			writeBrokenProfile(t, dir, tt.profile)

			s := startServer(t, dir)
			profiles := listProfiles(t, s)

			broken, ok := profiles["broken"]
			if !ok {
				t.Fatalf("profiles 沒有列出 broken（對外的名字是檔名）: %v", profiles)
			}
			if status := stringField(t, broken, "status"); status != "unavailable" {
				t.Errorf("broken 的 status = %q, 期望 unavailable", status)
			}
			if reason := stringField(t, broken, "error"); !strings.Contains(reason, tt.wantReason) {
				t.Errorf("broken 的 error = %q, 期望含 %q", reason, tt.wantReason)
			}
			good, ok := profiles["default"]
			if !ok {
				t.Fatalf("profiles 沒有列出 default: %v", profiles)
			}
			if status := stringField(t, good, "status"); status != "available" {
				t.Errorf("default 的 status = %q, 期望 available（一份壞掉不該擋其他 Agent）", status)
			}
			if available, unavailable := infoProfileCounts(t, s); available != 1 || unavailable != 1 {
				t.Errorf("info 的 profiles = {available: %d, unavailable: %d}, 期望 {1, 1}（與 profiles 端點一致）",
					available, unavailable)
			}
			if tt.wantMarker {
				if _, err := os.Stat(marker); err != nil {
					t.Errorf("broken 不可用，它已經連上的 MCP 子進程卻還沒被收掉（marker %s 不存在）", marker)
				}
			}

			if err := s.stop(); err != nil {
				t.Fatalf("收掉 server: %v", err)
			}
			if line := lineAfter(s.out.String(), "Profile broken 不可用："); !strings.Contains(line, tt.wantReason) {
				t.Errorf("啟動輸出沒有「Profile broken 不可用：<含 %q 的原因>」這一行:\n%s", tt.wantReason, s.out.String())
			}
			if !logHasEntry(t, dir, `"msg":"profile_unavailable"`, `"profile":"broken"`) {
				t.Errorf("錯誤日誌沒有 broken 的 profile_unavailable 記錄:\n%s", readWorkspaceLog(t, dir))
			}
			if !strings.Contains(s.out.String(), "就緒：1 份 Profile 可用、1 份不可用") {
				t.Errorf("啟動輸出的就緒行沒有反映 1 份可用、1 份不可用:\n%s", s.out.String())
			}
		})
	}
}

// logHasEntry 回報 Workspace 日誌裡是否有一行同時含有 parts 的每一段。
func logHasEntry(t *testing.T, dir string, parts ...string) bool {
	t.Helper()
	for line := range strings.SplitSeq(readWorkspaceLog(t, dir), "\n") {
		matched := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// runServerExpectingFailure 以 runServer 啟動一台預期起不來的 server，回傳它的輸出、監聽位址與錯誤。
//
// 設期限而不是 Background：實作若誤把失敗吞掉、照常開始服務，這個呼叫會一直阻塞；期限到了它會
// 以「成功返回」收場，呼叫端的斷言照樣抓得到。
func runServerExpectingFailure(t *testing.T, dir string) (out, addr string, err error) {
	t.Helper()
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("開 listener: %v", listenErr)
	}
	addr = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var buf bytes.Buffer
	err = runServer(ctx, &buf, dir, serverOptions{}, listener)
	return buf.String(), addr, err
}

// TestServerStartupFailsWithoutAvailableProfile 釘住「啟動即失敗」的三種情形（spec #73 第三節）：
// 可用的 Profile 數為 0（全部壞掉，或 profiles/ 底下根本沒有 YAML），以及 Workspace 層級的錯誤。
//
// 取代 #75 的 TestServerStartupFailsWhenAnyProfileFails，它守的三件事在這裡都保留：
//
//   - **錯誤指名每一份 Profile**：default 這一格的原因（MCP server 缺憑證）本身不含 Profile 名，
//     錯誤裡出現 default，只可能來自命令層的列舉。
//   - **listener 已關**：埠號不會被一個起不來的進程佔著。
//   - **MCP 子進程已結束**：broken 的 MCP 在 Subset 擋下之前已經連上。
func TestServerStartupFailsWithoutAvailableProfile(t *testing.T) {
	tests := []struct {
		name string
		// setup 把 Workspace 弄成這一格的樣子。marker 是 MCP exit marker 的路徑。
		setup func(t *testing.T, dir, marker string)
		// wantSubs 是錯誤訊息裡必須出現的片段。
		wantSubs []string
		// wantMarker 為真時，另外驗 MCP 子進程已經被收掉。
		wantMarker bool
	}{
		{
			name: "每一份 Profile 都壞掉：錯誤列出每份的原因",
			setup: func(t *testing.T, dir, marker string) {
				writeMcpServers(t, dir, "mcp_servers:\n"+
					testMcpServerEntryWithExitMarker(t, "alive", marker, "echo")+
					testMcpServerEntry(t, "needs_token", "echo")+
					"      TOKEN: ${ORYXOS_TEST_NEVER_SET_TOKEN_76}\n")
				writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - needs_token\n")
				writeBrokenProfile(t, dir, "name: broken\nprovider:\n  name: openrouter\n  model: m\n"+
					"mcp_servers:\n  - alive\ntools:\n  - alive__echo\n  - no_such_tool\n")
			},
			wantSubs:   []string{"default", "ORYXOS_TEST_NEVER_SET_TOKEN_76", "broken", "no_such_tool"},
			wantMarker: true,
		},
		{
			name: "profiles/ 底下沒有任何 YAML",
			setup: func(t *testing.T, dir, _ string) {
				if err := os.Remove(filepath.Join(dir, workspaceDir, "profiles", "default.yaml")); err != nil {
					t.Fatalf("刪除 default.yaml: %v", err)
				}
			},
			wantSubs: []string{"沒有任何 Profile"},
		},
		{
			name: "config.yaml 無法解析（Workspace 層級）",
			setup: func(t *testing.T, dir, _ string) {
				if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte("providers: [unclosed\n"), 0o644); err != nil {
					t.Fatalf("覆寫 config.yaml: %v", err)
				}
			},
			wantSubs: []string{"Workspace 設定檔"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			marker := filepath.Join(t.TempDir(), "alive.exited")
			tt.setup(t, dir, marker)

			out, addr, err := runServerExpectingFailure(t, dir)
			if err == nil {
				t.Fatalf("這一格應該啟動失敗，runServer 卻成功返回\n輸出:\n%s", out)
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("錯誤沒有含 %q:\n%v", want, err)
				}
			}
			if conn, dialErr := net.DialTimeout("tcp", addr, time.Second); dialErr == nil {
				_ = conn.Close()
				t.Errorf("啟動失敗之後 %s 仍接受連線——listener 沒有關", addr)
			}
			if tt.wantMarker {
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Errorf("runServer 返回時 MCP 子進程還沒被收掉（marker %s 不存在）", marker)
				}
			}
		})
	}
}

// TestUnreferencedProviderMissingCredential 把同一份設定交給 chat 與 server，釘住兩者刻意不對稱
// 的憑證語義（spec #73 第三節）：config.yaml 多配置了一個憑證展不開的 Provider，但沒有任何 Profile
// 引用它。
//
//   - **server 逐個 Provider 展開**：缺憑證只影響引用它的 Profile，這裡一份都沒有，所以照常啟動。
//   - **chat 維持原本的行為**：任何一個 Provider 缺憑證就啟動失敗，以守住遷移安全性。
//
// chat 那一格不是多餘的：既有的 chat 測試只驗「引用的那個 Provider 缺憑證」，而那種情形在逐個
// 展開之下也會失敗、錯誤裡也有同一個環境變數名。chat 若被誤接上逐個展開，只有這一格看得出來。
func TestUnreferencedProviderMissingCredential(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		provider := newReplayServer(t)
		dir := setupChatWorkspace(t, provider.URL)
		writeWorkspaceConfig(t, dir, provider.URL, configWithUnusableProvider)
		return dir
	}

	t.Run("server：其餘 Profile 照常可用", func(t *testing.T) {
		s := startServer(t, setup(t))
		profiles := listProfiles(t, s)
		good, ok := profiles["default"]
		if !ok {
			t.Fatalf("profiles 沒有列出 default: %v", profiles)
		}
		if status := stringField(t, good, "status"); status != "available" {
			t.Errorf("default 的 status = %q, 期望 available（它引用的 Provider 憑證完整）", status)
		}
	})

	t.Run("chat：任何一個 Provider 缺憑證仍然啟動失敗", func(t *testing.T) {
		var out bytes.Buffer
		err := runChat(context.Background(), strings.NewReader(""), &out, setup(t),
			chatOptions{profileName: "default"})
		if err == nil || !strings.Contains(err.Error(), neverSetProviderKey) {
			t.Errorf("runChat 的錯誤 = %v, 期望指出 %s 未設定（chat 維持全部展開）", err, neverSetProviderKey)
		}
	})
}

// TestServerMcpUnavailableKeepsProfileAvailable 釘住「MCP server 連不上不算 Profile 不可用」（spec
// #73 第三節）：沿用 chat 的降級，只拿掉那台 server 的工具，啟動輸出的提醒與 chat 一字不差，只多了
// 行首的 Profile 名。
//
// demo 是真的起得來的本地 stdio server；broken_mcp 指向一個不存在的執行檔。
func TestServerMcpUnavailableKeepsProfileAvailable(t *testing.T) {
	provider := newReplayServer(t)
	workspace := setupChatWorkspace(t, provider.URL)
	writeMcpServers(t, workspace, "mcp_servers:\n"+
		testMcpServerEntry(t, "demo", "echo")+
		"  broken_mcp:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-76]\n")
	writeProfile(t, workspace, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - demo\n  - broken_mcp\ntools:\n  - demo__echo\n  - broken_mcp__echo\n")

	// chat 一個 turn 都不跑（stdin 立刻 EOF），只取它的降級提醒當期望值。
	var chatOut bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &chatOut, workspace,
		chatOptions{profileName: "default"}); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	chatWarning := lineAfter(chatOut.String(), "警告：MCP server broken_mcp")
	if chatWarning == "" {
		t.Fatalf("chat 沒有印出 broken_mcp 的降級提醒，這一格的期望值取不到:\n%s", chatOut.String())
	}

	s := startServer(t, workspace)
	if status := stringField(t, listProfiles(t, s)["default"], "status"); status != "available" {
		t.Errorf("default 的 status = %q, 期望 available（MCP 連不上只降級，不算不可用）", status)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("收掉 server: %v", err)
	}
	if !slices.Contains(strings.Split(s.out.String(), "\n"), "[Profile default] "+chatWarning) {
		t.Errorf("啟動輸出沒有「[Profile default] ＋ chat 的那行降級提醒」\nchat 那行: %q\nserver 輸出:\n%s",
			chatWarning, s.out.String())
	}
}

// TestServerProfilesEndpoint 釘住 GET /api/v1/profiles 每一筆的形狀（spec #73 第八節）。
//
// 兩份可用的 Profile 引用不同的 Provider 與模型，斷言各自的 provider.name 與 provider.model 沒有
// 串到別份。另外兩份不可用，分出「不知道」與「知道但不能用」：
//
//   - broken 的 YAML 讀不出來：description、agent_name、provider 回 null，不回空字串——空字串會被
//     讀成「作者沒寫描述」，而事實是「讀不出來，不知道」。
//   - typo 的 YAML 讀得出來，只是 bootstrap 列了一個不存在的檔名，沒通過 LoadProfile 的校驗：這三個
//     值都寫得好好的，要照實列出來。運維人員靠它們認出是哪一個 Agent 壞了。
func TestServerProfilesEndpoint(t *testing.T) {
	provider := newReplayServer(t)
	workspace := setupChatWorkspace(t, provider.URL)
	writeWorkspaceConfig(t, workspace, provider.URL,
		"  deepseek:\n    api_key: literal-key\n    base_url: http://127.0.0.1:1\nhttp:\n  allowed_domains: []\n")
	writeProfile(t, workspace, "description: 預設助理\nidentity:\n  agent_name: Oryx\n"+
		"provider:\n  name: openrouter\n  model: model-a\n")
	writeNamedProfile(t, workspace, "analyst", "description: 分析工單\nidentity:\n  agent_name: Lynx\n"+
		"provider:\n  name: deepseek\n  model: model-b\n")
	writeBrokenProfile(t, workspace, "name: broken\nprovider: [unclosed\n")
	writeNamedProfile(t, workspace, "typo", "description: 拼錯一個檔名\nidentity:\n  agent_name: Typo\n"+
		"provider:\n  name: openrouter\n  model: model-c\nbootstrap:\n  - TYPO.md\n")

	s := startServer(t, workspace)
	profiles := listProfiles(t, s)
	if len(profiles) != 4 {
		t.Fatalf("profiles 列出 %d 筆, 期望 4（可用與不可用都要列）: %v", len(profiles), profiles)
	}

	tests := []struct {
		name          string
		wantDesc      string
		wantAgentName string
		wantProvider  string
		wantModel     string
	}{
		{name: "default", wantDesc: "預設助理", wantAgentName: "Oryx", wantProvider: "openrouter", wantModel: "model-a"},
		{name: "analyst", wantDesc: "分析工單", wantAgentName: "Lynx", wantProvider: "deepseek", wantModel: "model-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := profiles[tt.name]
			if !ok {
				t.Fatalf("profiles 沒有列出 %s: %v", tt.name, profiles)
			}
			if got := stringField(t, entry, "status"); got != "available" {
				t.Errorf("status = %q, 期望 available", got)
			}
			if got := stringField(t, entry, "description"); got != tt.wantDesc {
				t.Errorf("description = %q, 期望 %q", got, tt.wantDesc)
			}
			if got := stringField(t, entry, "agent_name"); got != tt.wantAgentName {
				t.Errorf("agent_name = %q, 期望 %q", got, tt.wantAgentName)
			}
			var ref struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			}
			if err := json.Unmarshal(entry["provider"], &ref); err != nil {
				t.Fatalf("provider 不是 {name, model} 物件: %s", entry["provider"])
			}
			if ref.Name != tt.wantProvider || ref.Model != tt.wantModel {
				t.Errorf("provider = {%q, %q}, 期望 {%q, %q}", ref.Name, ref.Model, tt.wantProvider, tt.wantModel)
			}
			if _, has := entry["error"]; has {
				t.Errorf("可用的 Profile 不該帶 error 欄位: %s", entry["error"])
			}
		})
	}

	t.Run("讀不出來的那份：欄位都在，值是 null", func(t *testing.T) {
		entry := profiles["broken"]
		if got := stringField(t, entry, "status"); got != "unavailable" {
			t.Errorf("status = %q, 期望 unavailable", got)
		}
		if got := stringField(t, entry, "error"); got == "" {
			t.Error("不可用的 Profile 要附上原因，error 是空字串")
		}
		for _, key := range []string{"description", "agent_name", "provider"} {
			raw, has := entry[key]
			if !has {
				t.Errorf("缺少欄位 %q（不可用也要有，值是 null）", key)
				continue
			}
			if string(raw) != "null" {
				t.Errorf("%s = %s, 期望 null（YAML 讀不出來，這個值不知道）", key, raw)
			}
		}
	})

	t.Run("讀得出來但沒通過校驗的那份：欄位照實列出", func(t *testing.T) {
		entry := profiles["typo"]
		if got := stringField(t, entry, "status"); got != "unavailable" {
			t.Errorf("status = %q, 期望 unavailable", got)
		}
		if got := stringField(t, entry, "error"); !strings.Contains(got, "TYPO.md") {
			t.Errorf("error = %q, 期望指出 TYPO.md", got)
		}
		if got := stringField(t, entry, "description"); got != "拼錯一個檔名" {
			t.Errorf("description = %q, 期望 %q（YAML 讀得出來，這個值是知道的）", got, "拼錯一個檔名")
		}
		if got := stringField(t, entry, "agent_name"); got != "Typo" {
			t.Errorf("agent_name = %q, 期望 Typo", got)
		}
		var ref struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(entry["provider"], &ref); err != nil || ref.Name != "openrouter" || ref.Model != "model-c" {
			t.Errorf("provider = %s, 期望 {openrouter, model-c}", entry["provider"])
		}
	})
}

// TestServerUnavailableReasonIsRedacted 釘住不可用的原因套用錯誤文字去敏（spec #73 第八節，ticket
// #76 AC）：與審計、事件流同一套規則（core.RedactErrorText）。
//
// 原因是使用者手寫的字串拼出來的：tools 引用未註冊的 Tool 時，錯誤會把那個名字原樣帶出來。這裡
// 把名字寫成一個帶帳密與 query 的網址，模擬「把 MCP 的網址貼錯欄位」。三條輸出路徑——API 的
// error 欄位、啟動輸出、錯誤日誌——都不能出現帳密與 query；網址的主機名要留著，證明做的是遮蔽，
// 不是把整段原因刪掉。
//
// 唯一一份 Profile 也壞掉時，runServer 回傳的錯誤是第四條路徑，另一格驗它。
func TestServerUnavailableReasonIsRedacted(t *testing.T) {
	const (
		userCanary  = "oryx-user-canary-76"
		passCanary  = "pw-canary-76"
		queryCanary = "query-canary-76"
		host        = "tools.example"
	)
	leaky := "name: broken\nprovider:\n  name: openrouter\n  model: m\ntools:\n" +
		"  - https://" + userCanary + ":" + passCanary + "@" + host + "/x?key=" + queryCanary + "\n"
	assertRedacted := func(t *testing.T, where, text string) {
		t.Helper()
		for _, secret := range []string{userCanary, passCanary, queryCanary} {
			if strings.Contains(text, secret) {
				t.Errorf("%s含有 %q，原因沒有去敏:\n%s", where, secret, text)
			}
		}
		if !strings.Contains(text, host) || !strings.Contains(text, "未註冊") {
			t.Errorf("%s沒有保留原因本身（主機名 %s 與「未註冊」都該在）:\n%s", where, host, text)
		}
	}

	t.Run("API、啟動輸出與錯誤日誌", func(t *testing.T) {
		workspace := setupChatWorkspace(t, newReplayServer(t).URL)
		writeBrokenProfile(t, workspace, leaky)
		s := startServer(t, workspace)
		assertRedacted(t, "profiles 的 error ", stringField(t, listProfiles(t, s)["broken"], "error"))
		if err := s.stop(); err != nil {
			t.Fatalf("收掉 server: %v", err)
		}
		assertRedacted(t, "啟動輸出", lineAfter(s.out.String(), "Profile broken 不可用："))
		logs := readWorkspaceLog(t, workspace)
		for _, secret := range []string{userCanary, passCanary, queryCanary} {
			if strings.Contains(logs, secret) {
				t.Errorf("錯誤日誌含有 %q，原因沒有去敏:\n%s", secret, logs)
			}
		}
	})

	t.Run("沒有可用 Profile 時 runServer 回傳的錯誤", func(t *testing.T) {
		workspace := setupChatWorkspace(t, newReplayServer(t).URL)
		if err := os.Remove(filepath.Join(workspace, workspaceDir, "profiles", "default.yaml")); err != nil {
			t.Fatalf("刪除 default.yaml: %v", err)
		}
		writeBrokenProfile(t, workspace, leaky)
		out, _, err := runServerExpectingFailure(t, workspace)
		if err == nil {
			t.Fatalf("唯一一份 Profile 壞掉，runServer 卻成功返回\n輸出:\n%s", out)
		}
		assertRedacted(t, "runServer 的錯誤", err.Error())
	})
}

// TestServerProfileRejectedBeforeMcpConnects 釘住兩個「擋在 MCP 連線之前」的判斷：引用的 Provider
// 憑證展不開、檔名與 name 欄位不一致。這兩種 Profile 注定不可用，不該為它們起任何 MCP 子進程。
//
// 驗法是 exit marker：子進程只要起來過，被收掉時就會寫 marker。所以 server 關閉之後 marker 仍然不
// 存在，才證明它從沒起來。判斷若挪到 MCP 連線之後，Profile 照樣不可用、其餘測試照樣綠，唯一的差別
// 是子進程先起來、再被收掉——marker 就出現了。
func TestServerProfileRejectedBeforeMcpConnects(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		// setup 在啟動之前調整 Workspace；nil 代表不調整。
		setup      func(t *testing.T, dir, providerURL string)
		wantReason string
	}{
		{
			name:    "引用的 Provider 憑證展不開",
			profile: "name: broken\nprovider:\n  name: other\n  model: m\nmcp_servers:\n  - alive\n",
			setup: func(t *testing.T, dir, providerURL string) {
				writeWorkspaceConfig(t, dir, providerURL, configWithUnusableProvider)
			},
			wantReason: neverSetProviderKey,
		},
		{
			name:       "檔名與 name 欄位不一致",
			profile:    "name: renamed\nprovider:\n  name: openrouter\n  model: m\nmcp_servers:\n  - alive\n",
			wantReason: `"renamed"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newReplayServer(t)
			dir := setupChatWorkspace(t, provider.URL)
			marker := filepath.Join(t.TempDir(), "alive.exited")
			writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntryWithExitMarker(t, "alive", marker, "echo"))
			if tt.setup != nil {
				tt.setup(t, dir, provider.URL)
			}
			writeBrokenProfile(t, dir, tt.profile)

			s := startServer(t, dir)
			if reason := stringField(t, listProfiles(t, s)["broken"], "error"); !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("broken 的 error = %q, 期望含 %q（這一格要驗的是這一種壞法）", reason, tt.wantReason)
			}
			if err := s.stop(); err != nil {
				t.Fatalf("收掉 server: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Errorf("broken 注定不可用，卻起過它引用的 MCP 子進程（marker %s 存在）", marker)
			}
		})
	}
}
