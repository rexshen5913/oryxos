// `oryxos server` 的 Agent 能力查詢：GET /tools 與 GET /memory，ticket #77。
//
// 與 server_test.go 同一個 seam：runServer 接收呼叫端開好的 listener，測試對它發真實的 HTTP
// 請求。seam 之下全部用真的，MCP 起真實的本地 stdio server；這兩個端點都不呼叫 Provider。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// toolListing 是 GET /api/v1/tools 回應裡的一個 Tool。Server 是指標：內建 Tool 回 null，要與
// 空字串分得開。
type toolListing struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Server      *string `json:"server"`
}

// toolGroup 是 GET /api/v1/tools 回應裡的一組：一份 Profile 與它實際可用的 Tool。
type toolGroup struct {
	Profile string        `json:"profile"`
	Tools   []toolListing `json:"tools"`
}

// getToolGroups 取 GET /api/v1/tools（query 原樣接在路徑後面），回傳各組；狀態碼不是 200 時測試失敗。
func getToolGroups(t *testing.T, s *runningServer, query string) []toolGroup {
	t.Helper()
	resp, body := s.send(t, http.MethodGet, "/api/v1/tools"+query, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools%s 狀態碼 = %d, 期望 200\nbody: %s", query, resp.StatusCode, body)
	}
	var listing struct {
		Groups []toolGroup `json:"groups"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("解析 tools 回應: %v\nbody: %s", err, body)
	}
	return listing.Groups
}

// toolSummary 把一組 Tool 攤成「名字@來源」，內建 Tool 的來源寫成 builtin，方便整組比對順序與來源。
func toolSummary(tools []toolListing) []string {
	summary := make([]string, 0, len(tools))
	for _, tl := range tools {
		source := "builtin"
		if tl.Server != nil {
			source = *tl.Server
		}
		summary = append(summary, tl.Name+"@"+source)
	}
	return summary
}

// assertErrorShape 斷言一個錯誤回應的狀態碼與 error_code，以及錯誤形狀：JSON、三個欄位都是非空
// 字串、timestamp 是 RFC 3339。標準與 TestServerErrorShape 相同，兩邊不該各有一套。
func assertErrorShape(t *testing.T, resp *http.Response, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Errorf("狀態碼 = %d, 期望 %d\nbody: %s", resp.StatusCode, wantStatus, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, 期望 application/json\nbody: %s", ct, body)
	}
	fields := decodeJSONObject(t, body)
	values := make(map[string]string, 3)
	for _, key := range []string{"error_code", "message", "timestamp"} {
		var value string
		if err := json.Unmarshal(fields[key], &value); err != nil || value == "" {
			t.Errorf("錯誤回應的 %q 缺少或不是非空字串（%v）\nbody: %s", key, err, body)
		}
		values[key] = value
	}
	if values["error_code"] != wantCode {
		t.Errorf("error_code = %q, 期望 %q", values["error_code"], wantCode)
	}
	if _, err := time.Parse(time.RFC3339, values["timestamp"]); err != nil {
		t.Errorf("timestamp = %q 不是 RFC 3339: %v", values["timestamp"], err)
	}
	// API 的時間戳一律是 UTC（見 TestServerTimestampsAreUTC）。
	if !strings.HasSuffix(values["timestamp"], "Z") {
		t.Errorf("timestamp = %q 不是 UTC", values["timestamp"])
	}
}

// TestServerToolsListsActualSubset 釘住 GET /tools 列的是這次啟動**實際可用**的子集（spec #73 第八節，
// ticket #77 AC）：Profile 的 tools 欄位過濾過、已套用 MCP 降級、含自動加入的 load_skill。
//
// 每一格都比對整組的「名字@來源」與順序，不只檢查某個名字在不在：多列了一個沒開的內建 Tool、
// 或把 MCP 工具的來源標成 builtin，都要轉紅。順序是 Profile 的 tools 宣告順序，自動加入的排最後，
// 與送給 LLM 的工具清單同一個順序。
//
// MCP 那一格的描述另外核對：測試用 MCP server 把自己的名字寫進工具描述，所以 server 欄位與描述
// 是兩個獨立的來源，兩者都要指向 demo。
func TestServerToolsListsActualSubset(t *testing.T) {
	tests := []struct {
		name string
		// profile 是 default Profile 在 name 那行之後的內容。
		profile string
		// setup 在啟動之前調整 Workspace；nil 代表不調整。
		setup func(t *testing.T, dir string)
		want  []string
	}{
		{
			name:    "tools 只列部分內建 Tool：沒列的不出現",
			profile: "provider:\n  name: openrouter\n  model: m\ntools:\n  - list_dir\n  - read_file\n",
			want:    []string{"list_dir@builtin", "read_file@builtin"},
		},
		{
			name: "宣告了 skills：自動加入 load_skill",
			profile: "provider:\n  name: openrouter\n  model: m\ntools:\n  - http_get\n" +
				"skills:\n  - demo-skill\n",
			setup: func(t *testing.T, dir string) {
				writeSkillFile(t, dir, "demo-skill", "---\nname: demo-skill\ndescription: 示範用的 Skill\n---\n\n正文\n")
			},
			want: []string{"http_get@builtin", "load_skill@builtin"},
		},
		{
			name: "兩台 MCP server、一台連不上：只列健康那台的工具，來源正確",
			profile: "provider:\n  name: openrouter\n  model: m\n" +
				"mcp_servers:\n  - demo\n  - broken_mcp\ntools:\n  - demo__echo\n  - broken_mcp__echo\n  - http_get\n",
			setup: func(t *testing.T, dir string) {
				writeMcpServers(t, dir, "mcp_servers:\n"+
					testMcpServerEntry(t, "demo", "echo")+
					"  broken_mcp:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-77]\n")
			},
			want: []string{"demo__echo@demo", "http_get@builtin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			writeProfile(t, dir, tt.profile)

			s := startServer(t, dir)
			groups := getToolGroups(t, s, "?profile=default")
			if len(groups) != 1 || groups[0].Profile != "default" {
				t.Fatalf("tools?profile=default 回了 %d 組 %+v, 期望只有 default 一組", len(groups), groups)
			}
			if got := toolSummary(groups[0].Tools); !slices.Equal(got, tt.want) {
				t.Errorf("default 的 Tool = %v, 期望 %v", got, tt.want)
			}
			for _, tl := range groups[0].Tools {
				if tl.Description == "" {
					t.Errorf("%s 的 description 是空字串", tl.Name)
				}
				if tl.Server != nil && !strings.Contains(tl.Description, *tl.Server+" 的測試工具") {
					t.Errorf("%s 的 server = %q，描述卻是 %q（兩個來源對不上）", tl.Name, *tl.Server, tl.Description)
				}
			}
		})
	}
}

// TestServerToolsProfileQuery 釘住 GET /tools 的 profile 參數（spec #73 第八節，ticket #77 AC）：
// 沒帶時列出全部**可用**的 Profile；帶了可用的只回那一組；不存在回 404、不可用回 503，兩者的
// 錯誤形狀都齊全。query 本身解析不了（無效的編碼、未編碼的分號）回 400。
//
// **空值（?profile=）回 404，不當成沒帶**：它多半是呼叫端把一個還沒填的變數接進了網址。當成「不
// 篩選」的話，呼叫端以為拿到了某一份的 Tool，其實拿到全部，錯誤被蓋過去。spec 對輸入的原則是讓
// 這類錯誤當場暴露（JSON 欄位拼錯回 400，同一個理由）。
//
// 三份 Profile：default 與 second 可用，broken 的 YAML 壞掉。
func TestServerToolsProfileQuery(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\ntools:\n  - http_get\n")
	writeNamedProfile(t, dir, "second", "provider:\n  name: openrouter\n  model: m\ntools:\n  - list_dir\n")
	writeBrokenProfile(t, dir, "name: broken\nprovider: [unclosed\n")
	s := startServer(t, dir)

	t.Run("沒帶 profile：列出全部可用的 Profile，不含不可用的", func(t *testing.T) {
		groups := getToolGroups(t, s, "")
		var names []string
		for _, g := range groups {
			names = append(names, g.Profile)
		}
		if want := []string{"default", "second"}; !slices.Equal(names, want) {
			t.Errorf("各組的 profile = %v, 期望 %v（依檔名排序，不含 broken）", names, want)
		}
	})

	t.Run("帶了可用的 Profile：只回那一組", func(t *testing.T) {
		groups := getToolGroups(t, s, "?profile=second")
		if len(groups) != 1 || groups[0].Profile != "second" {
			t.Fatalf("tools?profile=second 回了 %+v, 期望只有 second 一組", groups)
		}
		if got := toolSummary(groups[0].Tools); !slices.Equal(got, []string{"list_dir@builtin"}) {
			t.Errorf("second 的 Tool = %v, 期望 [list_dir@builtin]（不能串到 default 的）", got)
		}
	})

	errorCases := []struct {
		name       string
		query      string
		wantStatus int
		wantCode   string
		// wantInMessage 是 message 裡必須出現的片段：指名是哪一份。
		wantInMessage string
	}{
		{name: "不存在的 Profile：404", query: "?profile=ghost",
			wantStatus: http.StatusNotFound, wantCode: "profile_not_found", wantInMessage: "ghost"},
		{name: "profile 參數帶了空值：404，不當成沒帶", query: "?profile=",
			wantStatus: http.StatusNotFound, wantCode: "profile_not_found", wantInMessage: `\"\"`},
		{name: "不可用的 Profile：503，並附上原因", query: "?profile=broken",
			wantStatus: http.StatusServiceUnavailable, wantCode: "profile_unavailable", wantInMessage: "解析 Profile"},
		// r.URL.Query() 會靜默丟掉解析不了的參數，請求於是落進「沒帶參數」、回 200 與全部 Profile。
		{name: "profile 的編碼無效：400，不當成沒帶", query: "?profile=%ZZ",
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "%ZZ"},
		{name: "query 裡有未編碼的分號：400，不當成沒帶", query: "?profile=default;x",
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantInMessage: "semicolon"},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := s.send(t, http.MethodGet, "/api/v1/tools"+tt.query, "", nil)
			assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
			if !strings.Contains(string(body), tt.wantInMessage) {
				t.Errorf("錯誤回應沒有含 %q\nbody: %s", tt.wantInMessage, body)
			}
		})
	}
}

// TestServerMemory 釘住 GET /memory 回傳 MEMORY.md 的原文（spec #73 第八節，ticket #77 AC）。
//
//   - **超過注入截斷上限時仍是完整原文**：注入 system prompt 的那一份會截到 4000 rune 並去掉頭尾
//     空白，這裡寫 5000 多 rune、頭尾各帶換行，逐位元組比對。
//   - **檔案不存在時是空字串**，回 200，不是錯誤。
//   - **符號連結指到 Workspace 之外時回 500**，錯誤形狀齊全，而且回應裡找不到那個外部檔案的
//     內容（canary）。讀取仍經 Workspace root，沿用既有的符號連結防線。原因落錯誤日誌。
func TestServerMemory(t *testing.T) {
	const outsideCanary = "outside-canary-77-do-not-leak"
	fullText := "\n# 長期記憶\n\n" + strings.Repeat("使用者偏好繁體中文與精簡的回答。", 400) + "\n\n"

	tests := []struct {
		name string
		// setup 把 memory/MEMORY.md 弄成這一格的樣子；nil 代表不建立。
		setup       func(t *testing.T, memoryPath string)
		wantStatus  int
		wantContent string
		// wantCode 非空時，期望的是錯誤回應。
		wantCode string
	}{
		{
			name: "超過注入截斷上限：仍是完整原文",
			setup: func(t *testing.T, memoryPath string) {
				if err := os.WriteFile(memoryPath, []byte(fullText), 0o644); err != nil {
					t.Fatalf("寫入 MEMORY.md: %v", err)
				}
			},
			wantStatus:  http.StatusOK,
			wantContent: fullText,
		},
		{
			name:        "MEMORY.md 不存在：空字串",
			wantStatus:  http.StatusOK,
			wantContent: "",
		},
		{
			name: "MEMORY.md 是指到 Workspace 之外的符號連結：500",
			setup: func(t *testing.T, memoryPath string) {
				outside := filepath.Join(t.TempDir(), "secret.md")
				if err := os.WriteFile(outside, []byte(outsideCanary), 0o644); err != nil {
					t.Fatalf("寫入 Workspace 之外的檔案: %v", err)
				}
				if err := os.Symlink(outside, memoryPath); err != nil {
					t.Fatalf("建立符號連結: %v", err)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupChatWorkspace(t, newReplayServer(t).URL)
			memoryDir := filepath.Join(dir, workspaceDir, "memory")
			if err := os.MkdirAll(memoryDir, 0o755); err != nil {
				t.Fatalf("建立 memory/: %v", err)
			}
			if tt.setup != nil {
				tt.setup(t, filepath.Join(memoryDir, memoryFile))
			}

			s := startServer(t, dir)
			resp, body := s.send(t, http.MethodGet, "/api/v1/memory", "", nil)
			if strings.Contains(string(body), outsideCanary) {
				t.Fatalf("回應含有 Workspace 之外那個檔案的內容\nbody: %s", body)
			}
			if tt.wantCode != "" {
				assertErrorShape(t, resp, body, tt.wantStatus, tt.wantCode)
				// 呼叫端看得到訊息，但運維人員查的是日誌：原因要在那裡也查得到。
				if err := s.stop(); err != nil {
					t.Fatalf("收掉 server: %v", err)
				}
				if !logHasEntry(t, dir, `"msg":"memory_read_failed"`) {
					t.Errorf("錯誤日誌沒有 memory_read_failed:\n%s", readWorkspaceLog(t, dir))
				}
				return
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("memory 狀態碼 = %d, 期望 %d\nbody: %s", resp.StatusCode, tt.wantStatus, body)
			}
			var memory struct {
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(body, &memory); err != nil || memory.Content == nil {
				t.Fatalf("memory 回應沒有字串形態的 content: %v\nbody: %.200s", err, body)
			}
			if *memory.Content != tt.wantContent {
				t.Errorf("content 有 %d 個 rune, 期望 %d 個（逐位元組相同的原文）",
					len([]rune(*memory.Content)), len([]rune(tt.wantContent)))
			}
		})
	}
}

// TestServerToolsEmptyListIsArray 釘住「一個 Tool 都沒有的 Profile，tools 是空陣列而不是 null」。
//
// 呼叫端對 null 做迭代會直接出錯（JavaScript 的 forEach），空陣列則什麼都不做。解進結構體之後兩者
// 都是空切片，所以這裡看原始 JSON。
func TestServerToolsEmptyListIsArray(t *testing.T) {
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n")
	s := startServer(t, dir)

	resp, body := s.send(t, http.MethodGet, "/api/v1/tools?profile=default", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}
	var raw struct {
		Groups []map[string]json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Groups) != 1 {
		t.Fatalf("解析 tools 回應: %v\nbody: %s", err, body)
	}
	if got := string(raw.Groups[0]["tools"]); got != "[]" {
		t.Errorf("沒有 Tool 的 Profile，tools = %s, 期望 []", got)
	}
}

// TestServerReleasesSlowResponseReader 釘住寫入期限：client 送出請求之後不讀回應，server 不會為它
// 無限期卡在寫入上（Codex gate #77 第 1 輪）。
//
// GET /memory 回傳整份 MEMORY.md，大小沒有上限。回應大到 TCP 的傳送與接收緩衝都裝不下時，server 的
// 寫入會停在那裡等 client 讀；client 一直不讀，那條連線、那個 goroutine 與編碼好的回應就一直被佔著。
// 讀取期限（#75）管不到這一段：請求早就讀完了。
//
// 做法同 TestServerReleasesSlowConnections：寫入期限設 200ms，client 送出請求後先睡 1 秒不讀，再讀到
// 連線結束。期限有作用，server 在 200ms 放掉連線，client 讀到的是不完整的回應加上連線結束；期限沒
// 作用，client 一開始讀，server 就把整份送完，連線接著停在 keep-alive，client 等到自己的 3 秒逾時。
func TestServerReleasesSlowResponseReader(t *testing.T) {
	const (
		writeTimeout = 200 * time.Millisecond
		stallFor     = time.Second
		clientWait   = 3 * time.Second
		// memoryBytes 要大過 loopback 上的傳送加接收緩衝（macOS 自動調整的上限各是幾 MB）。
		memoryBytes = 32 << 20
	)
	dir := setupChatWorkspace(t, newReplayServer(t).URL)
	memoryDir := filepath.Join(dir, workspaceDir, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatalf("建立 memory/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memoryDir, memoryFile), []byte(strings.Repeat("m", memoryBytes)), 0o644); err != nil {
		t.Fatalf("寫入 MEMORY.md: %v", err)
	}
	s := startServerWithOptions(t, dir, serverOptions{writeTimeout: writeTimeout})
	// 先走完一個正常請求：確認組裝已經結束，下面的等待才只算慢速讀取本身。
	if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("health 狀態碼 = %d, 期望 200\nbody: %s", resp.StatusCode, body)
	}

	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatalf("開啟慢速連線: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /api/v1/memory HTTP/1.1\r\nHost: oryxos.test\r\n\r\n"); err != nil {
		t.Fatalf("寫出請求: %v", err)
	}
	time.Sleep(stallFor)
	if err := conn.SetReadDeadline(time.Now().Add(clientWait)); err != nil {
		t.Fatalf("設定 client 端期限: %v", err)
	}
	received, err := io.ReadAll(conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server 沒有在寫入期限到了之後放掉連線：client 讀了 %d bytes 之後等到自己的逾時", len(received))
	}
	if len(received) >= memoryBytes {
		t.Errorf("client 收到 %d bytes，比 MEMORY.md 還多：回應被完整送出，寫入期限沒有切斷它", len(received))
	}

	// 期限只放掉慢速的那一條，正常的請求不受影響。
	if resp, body := s.send(t, http.MethodGet, "/api/v1/health", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("慢速讀取被放掉之後，health 狀態碼 = %d, 期望 200\nbody: %.200s", resp.StatusCode, body)
	}
}

// TestNewHTTPServerWriteTimeout 釘住寫入期限的零值換成預設值，不是不設期限：理由同讀取期限（見
// TestNewHTTPServerTimeouts），http.Server 的零值就是不設。這一條從連線上看不到（預設要等一分多鐘），
// 所以直接看 newHTTPServer 的設定。
//
// 預設值自 #79 起是「讀取期限＋turn 上限＋寫出回應的寬限」：寫入期限必須大於 handler 的最長時間
// （spec #73 第二節），另見 TestServerDeadlinesFollowTurnTimeout。
func TestNewHTTPServerWriteTimeout(t *testing.T) {
	tests := []struct {
		name string
		opts serverOptions
		want time.Duration
	}{
		{name: "沒填時換成預設值", opts: serverOptions{}, want: defaultReadTimeout + defaultTurnTimeout + responseWriteGrace},
		{name: "有填時照用", opts: serverOptions{writeTimeout: 4 * time.Second}, want: 4 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newHTTPServer(http.NotFoundHandler(), tt.opts).WriteTimeout
			if got <= 0 {
				t.Fatalf("WriteTimeout = %v，等於不設期限", got)
			}
			if got != tt.want {
				t.Errorf("WriteTimeout = %v, 期望 %v", got, tt.want)
			}
		})
	}
}
