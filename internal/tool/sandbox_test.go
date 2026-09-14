package tool_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestSandboxCheckerCheckHTTPURL 是域名白名單的行為矩陣：host 解析＋通配符匹配，
// 預設拒絕（空白名單全擋）。任何拒絕都必須是 SandboxViolation（可被 errors.Is 識別）。
func TestSandboxCheckerCheckHTTPURL(t *testing.T) {
	tests := []struct {
		name          string
		allowed       []string
		url           string
		wantViolation bool
	}{
		{
			name:    "完全匹配放行",
			allowed: []string{"api.example.com"},
			url:     "https://api.example.com/weather?city=beijing",
		},
		{
			name:          "host 不在白名單被攔截",
			allowed:       []string{"api.example.com"},
			url:           "https://evil.com/steal",
			wantViolation: true,
		},
		{
			name:          "空白名單全部拒絕",
			allowed:       nil,
			url:           "https://api.example.com/",
			wantViolation: true,
		},
		{
			name:    "通配符匹配一級子域名",
			allowed: []string{"*.example.com"},
			url:     "https://api.example.com/x",
		},
		{
			name:    "通配符匹配多級子域名",
			allowed: []string{"*.example.com"},
			url:     "https://a.b.example.com/x",
		},
		{
			name:          "通配符不匹配裸域名",
			allowed:       []string{"*.example.com"},
			url:           "https://example.com/x",
			wantViolation: true,
		},
		{
			name:          "字面後綴相似不構成匹配",
			allowed:       []string{"example.com"},
			url:           "https://evil-example.com/x",
			wantViolation: true,
		},
		{
			name:          "通配符後綴相似不構成匹配",
			allowed:       []string{"*.example.com"},
			url:           "https://evil-example.com/x",
			wantViolation: true,
		},
		{
			name:    "URL 帶 port 時只比對 host",
			allowed: []string{"127.0.0.1"},
			url:     "http://127.0.0.1:8080/weather",
		},
		{
			name:    "大小寫不敏感",
			allowed: []string{"API.Example.com"},
			url:     "https://api.example.COM/x",
		},
		{
			name:          "非 http/https scheme 拒絕",
			allowed:       []string{"example.com"},
			url:           "ftp://example.com/file",
			wantViolation: true,
		},
		{
			name:          "無法解析的 URL 拒絕",
			allowed:       []string{"example.com"},
			url:           "://bad-url",
			wantViolation: true,
		},
		{
			name:          "缺 host 的 URL 拒絕",
			allowed:       []string{"example.com"},
			url:           "https:///path-only",
			wantViolation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: tt.allowed}).CheckHTTPURL(tt.url)
			if tt.wantViolation {
				if !errors.Is(err, tool.ErrSandboxViolation) {
					t.Errorf("CheckHTTPURL(%q) = %v, 期望 SandboxViolation", tt.url, err)
				}
			} else if err != nil {
				t.Errorf("CheckHTTPURL(%q) = %v, 期望放行", tt.url, err)
			}
		})
	}
}

// TestSandboxViolationErrorOmitsQuery 驗證校驗錯誤訊息不內嵌 URL query——
// 錯誤會落日誌與回填 LLM，query 常帶 api key，任何分支都不得原樣帶出。
func TestSandboxViolationErrorOmitsQuery(t *testing.T) {
	const secret = "S3CRET-VALUE"
	urls := []string{
		"https://evil.com/x?api_key=" + secret,  // host 不在白名單
		"ftp://example.com/x?api_key=" + secret, // scheme 拒絕
		"://bad?api_key=" + secret,              // 無法解析
	}
	checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: []string{"trusted.example.com"}})
	for _, u := range urls {
		_, err := checker.CheckHTTPURL(u)
		if !errors.Is(err, tool.ErrSandboxViolation) {
			t.Fatalf("CheckHTTPURL(%q) = %v, 期望 SandboxViolation", u, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("CheckHTTPURL(%q) 錯誤訊息洩漏 query: %q", u, err.Error())
		}
	}
}

// retainedDomainShape 是一個「看起來不像網域、但現在真的比得中」的白名單條目，連同一個
// 會被它放行的網址。
type retainedDomainShape struct {
	name      string
	entry     string // 使用者寫在 http.allowed_domains 的原樣
	effective string // 收斂之後的樣子（只差在轉小寫）
	rawURL    string // 一個會被這條條目放行的網址
}

// retainedDomainShapes 是 EffectiveAllowedDomains **必須保留**的條目形狀（ADR-0007「收斂
// 規則」一節的保留形狀）。
//
// **兩支測試共用這一份，不各寫一份**：TestEffectiveAllowedDomains 驗「收斂後還在」，
// TestCheckHTTPURLStillMatchesRetainedDomainShapes 驗「還在的真的比得中」。同一組形狀寫
// 兩處，就是下一個過期的來源——而那一節在 ADR 階段被連續駁回四輪，四次都是同一個錯：
// 撰寫者憑語法猜「這個比不中」，然後被一條真的比得中的條目打穿。
//
// 用函式回傳而不是套件層級變數：每支測試拿到自己的一份，改了也不會污染別支。
func retainedDomainShapes() []retainedDomainShape {
	return []retainedDomainShape{
		{name: "含底線", entry: "exa_mple.com", effective: "exa_mple.com", rawURL: "https://exa_mple.com/x"},
		{name: "帶尾點的絕對域名", entry: "example.com.", effective: "example.com.", rawURL: "https://example.com./x"},
		{name: "Unicode 主機名", entry: "bücher.example", effective: "bücher.example", rawURL: "https://bücher.example/x"},
		// `*` 不在開頭接點時不是萬用字元，而是字面：url.Parse 允許 host 含 `*`。
		{name: "星號字面", entry: "*example.com", effective: "*example.com", rawURL: "https://*example.com/x"},
		// `*.` 的後綴是空字串，於是匹配任何以點結尾的 host。
		{name: "星號點匹配帶尾點的 host", entry: "*.", effective: "*.", rawURL: "https://a./x"},
		{name: "多層星號", entry: "*.*.example.com", effective: "*.*.example.com", rawURL: "https://a.*.example.com/x"},
		// zoned IPv6 是合法的 Hostname，但 net.ParseIP 不收——用 ParseIP 判準會剔錯它。
		{name: "zoned IPv6", entry: "fe80::1%en0", effective: "fe80::1%en0", rawURL: "http://[fe80::1%25en0]/x"},
		{name: "IPv6", entry: "::1", effective: "::1", rawURL: "http://[::1]:8080/x"},
		{name: "IPv4", entry: "192.168.0.1", effective: "192.168.0.1", rawURL: "http://192.168.0.1/x"},
		{name: "大小寫混雜", entry: "Example.COM", effective: "example.com", rawURL: "https://EXAMPLE.com/x"},
		// NBSP 與全形空白會被 strings.TrimSpace 吃掉，但 url.Parse 接受它們當 host——
		// 這正是「trim 後為空就剔除」那一版被打穿的第四個反例。
		{name: "NBSP", entry: "\u00a0", effective: "\u00a0", rawURL: "https://\u00a0/x"},
		{name: "全形空白", entry: "\u3000", effective: "\u3000", rawURL: "https://\u3000/x"},
		// 非 ASCII 的控制字元（C1）以 UTF-8 編碼後每個 byte 都 ≥ 0x80，url.Parse 的控制
		// 字元檢查只擋 < 0x20 與 0x7F，所以它同樣是比得中的 host。
		{name: "C1 控制字元", entry: "a\u0085b.example", effective: "a\u0085b.example", rawURL: "https://a\u0085b.example/x"},
		// zone 裡以 %20 寫進的空白會留在 Hostname() 裡。所以「含 ASCII 空白」只配觸發啟動
		// 提醒，不配當剔除的依據——這一條就是反例。
		{name: "zone 含空白的 IPv6", entry: "fe80::1%a b", effective: "fe80::1%a b", rawURL: "http://[fe80::1%25a%20b]/x"},
	}
}

// TestCheckHTTPURLStillMatchesRetainedDomainShapes 是網域收斂的**全鏈路回歸**（ADR-0007）。
//
// 判準是這條路本身——`url.Parse` → `Hostname()` → 白名單比對，也就是 CheckHTTPURL 真的
// 會走的那一條——**不是任何語法描述**。每一格建一個只含那條條目的校驗器，丟一個網址
// 進去，斷言放行。
//
// **它在收斂落地之前就是綠的，這是刻意的。** 它不是在驗新行為，是在守一條不能被縮減的
// 授權集合：日後有人替 EffectiveAllowedDomains 加一條「看起來比不中就剔除」的規則，
// 這裡就會轉紅，並指名是哪一個形狀被剔錯了。
func TestCheckHTTPURLStillMatchesRetainedDomainShapes(t *testing.T) {
	for _, shape := range retainedDomainShapes() {
		t.Run(shape.name, func(t *testing.T) {
			checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: []string{shape.entry}})
			decision, err := checker.CheckHTTPURL(shape.rawURL)
			if decision != tool.SandboxAllow || err != nil {
				t.Errorf("白名單 [%q] 之下 CheckHTTPURL(%q) = (%v, %v)，期望放行——這條條目被收斂剔除或比對失效了",
					shape.entry, shape.rawURL, decision, err)
			}
		})
	}
}

// TestSandboxCheckerCheckFilePath 是路徑白名單的拒絕矩陣：解析基準固定為 Workspace
// 根，先標準化再比對，比對是**子樹包含**而不是字串前綴，空白名單全部拒絕。
//
// 這裡只涵蓋**應用層白名單**這一道防線——它是純字串判斷，不碰檔案系統。符號連結、
// 檔案型別那幾格屬開檔層的把關，由 file_test.go 以真實檔案覆蓋（兩道防線分工明確、
// 不互相取代）。
//
// 放行的格子一併斷言回傳的標準化路徑：那份結果會被拿去 os.Root 開檔，回一個沒清乾淨
// 的字串等於把 `../` 留到下一站才處理。
func TestSandboxCheckerCheckFilePath(t *testing.T) {
	tests := []struct {
		name          string
		allowed       []string
		path          string
		wantRel       string // 期望放行時回傳的標準化路徑
		wantViolation bool
	}{
		{
			name:    "白名單內的相對路徑放行",
			allowed: []string{"notes"},
			path:    "notes/todo.md",
			wantRel: filepath.Join("notes", "todo.md"),
		},
		{
			name:    "白名單條目本身就是目標時放行",
			allowed: []string{"notes/todo.md"},
			path:    "notes/todo.md",
			wantRel: filepath.Join("notes", "todo.md"),
		},
		{
			name:    "多層子目錄仍在子樹內",
			allowed: []string{"docs"},
			path:    "docs/a/b/c.md",
			wantRel: filepath.Join("docs", "a", "b", "c.md"),
		},
		{
			name:    "無害的 ./ 與重複斜線標準化後放行",
			allowed: []string{"notes"},
			path:    "./notes//todo.md",
			wantRel: filepath.Join("notes", "todo.md"),
		},
		{
			name:    "白名單條目為 . 時整個 Workspace 都在子樹內",
			allowed: []string{"."},
			path:    "anything/x.md",
			wantRel: filepath.Join("anything", "x.md"),
		},
		{
			name:          "白名單外的路徑拒絕",
			allowed:       []string{"notes"},
			path:          "secrets/api.txt",
			wantViolation: true,
		},
		{
			// 這是這個檢查存在的理由，不是邊角案例：`../` 必須在比對**之前**解掉，
			// 否則 notes/../secrets 會因為開頭是 notes 而被放行。
			name:          "../ 穿越出白名單後拒絕",
			allowed:       []string{"notes"},
			path:          "notes/../secrets/api.txt",
			wantViolation: true,
		},
		{
			name:          "../ 穿越出 Workspace 拒絕",
			allowed:       []string{"notes"},
			path:          "notes/../../etc/passwd",
			wantViolation: true,
		},
		{
			name:          "純 .. 拒絕",
			allowed:       []string{"."},
			path:          "..",
			wantViolation: true,
		},
		{
			name:          "絕對路徑拒絕",
			allowed:       []string{"notes"},
			path:          "/etc/passwd",
			wantViolation: true,
		},
		{
			// 比對是子樹包含不是字串前綴：work 不得放行 workspace-secrets/x。
			name:          "子樹前綴的假匹配不放行（work vs workspace-secrets）",
			allowed:       []string{"work"},
			path:          "workspace-secrets/x",
			wantViolation: true,
		},
		{
			// 同一個假匹配的多層形態。白名單的基準是 Workspace 根，所以 spec 舉的
			// /tmp/foo 對 /tmp/foobar 在這裡的對應形態是 tmp/foo 對 tmp/foobar
			// ——絕對路徑本身另有一格擋下。
			name:          "子樹前綴的假匹配不放行（tmp/foo vs tmp/foobar）",
			allowed:       []string{"tmp/foo"},
			path:          "tmp/foobar/x",
			wantViolation: true,
		},
		{
			name:          "空白名單全部拒絕",
			allowed:       nil,
			path:          "notes/todo.md",
			wantViolation: true,
		},
		{
			// 白名單裡的空字串條目標準化後會變成 `.`（＝整個 Workspace）。那是使用者
			// 寫了一條沒有意義的設定，不該被解讀成「全部放行」。
			name:          "白名單裡的空字串條目不放行任何路徑",
			allowed:       []string{""},
			path:          "secrets/api.txt",
			wantViolation: true,
		},
		{
			name:          "白名單裡的純空白條目不放行任何路徑",
			allowed:       []string{"   "},
			path:          "notes/todo.md",
			wantViolation: true,
		},
		{
			// 條目寫成絕對路徑：基準是 Workspace 根，兩邊永遠對不上。
			name:          "白名單條目是絕對路徑時不放行",
			allowed:       []string{"/Users/someone/notes"},
			path:          "notes/todo.md",
			wantViolation: true,
		},
		{
			name:          "白名單條目穿越出 Workspace 時不放行",
			allowed:       []string{"../shared"},
			path:          "shared/x.md",
			wantViolation: true,
		},
		{
			// 對照：混了無效條目，但有一條有效的，那一條照樣生效。
			name:    "無效條目不影響同一份白名單裡有效的那條",
			allowed: []string{"", "/abs/notes", "notes"},
			path:    "notes/todo.md",
			wantRel: filepath.Join("notes", "todo.md"),
		},
		{
			name:          "空路徑拒絕",
			allowed:       []string{"."},
			path:          "",
			wantViolation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: tt.allowed})
			_, rel, err := checker.CheckFilePath(tt.path)
			if tt.wantViolation {
				if !errors.Is(err, tool.ErrSandboxViolation) {
					t.Fatalf("CheckFilePath(%q) = %q, %v, 期望 SandboxViolation", tt.path, rel, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckFilePath(%q) = %v, 期望放行", tt.path, err)
			}
			if rel != tt.wantRel {
				t.Errorf("CheckFilePath(%q) 回傳 %q, 期望標準化為 %q", tt.path, rel, tt.wantRel)
			}
		})
	}
}

// TestSandboxFilePathErrorIsActionableAndListsWhitelist 驗證拒絕訊息**可行動**：它要指出
// 使用者該改哪一段設定（沒有這句，使用者只會看到「被拒絕」卻不知往哪加），並**列出白名單
// 其餘條目**。
//
// **後一半在 ticket #68 反轉了**，這支測試原名 …AndNarrow，斷言的是「不得把其餘條目倒
// 出來」。那條規則的代價是 issue #58：訊息完全不提白名單裡有什麼，模型就把沉默讀成
// 「白名單是空的」，對使用者說出一句假話。ADR-0007 決定列出來，並把保密性損失照實記在
// 威脅模型一節——這裡守的是那個決定，理由不在這裡重寫。
func TestSandboxFilePathErrorIsActionableAndListsWhitelist(t *testing.T) {
	const otherEntry = "internal/private-notes"
	checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: []string{"notes", otherEntry}})

	_, _, err := checker.CheckFilePath("secrets/api.txt")
	if !errors.Is(err, tool.ErrSandboxViolation) {
		t.Fatalf("CheckFilePath = %v, 期望 SandboxViolation", err)
	}
	if !strings.Contains(err.Error(), "file.allowed_paths") {
		t.Errorf("錯誤訊息沒告訴使用者要改哪段設定: %q", err.Error())
	}
	if !strings.Contains(err.Error(), otherEntry) {
		t.Errorf("錯誤訊息沒列出白名單的其他條目 %q——模型看不到它，就可能把沉默讀成「白名單是空的」: %q",
			otherEntry, err.Error())
	}
}

// TestEffectiveAllowedPaths 釘住「白名單是不是空的」那個**單一定義點**。
//
// 這個函式有兩個消費者：SandboxChecker 拿它決定要比對哪些子樹，組裝點拿它決定要不要
// 印空白名單的啟動提醒。兩邊必須得到同一個答案——否則會出現最難查的那種失敗：
// 系統一句話都不說，而每一次 read_file 呼叫都被攔。
func TestEffectiveAllowedPaths(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    []string
	}{
		{name: "nil 白名單", entries: nil, want: nil},
		{name: "空切片", entries: []string{}, want: nil},
		{name: "空字串條目等於沒寫", entries: []string{""}, want: nil},
		{name: "純空白條目等於沒寫", entries: []string{" ", "\t"}, want: nil},
		{name: "絕對路徑條目永遠比不中", entries: []string{"/Users/someone/notes"}, want: nil},
		{name: "穿越出 Workspace 的條目永遠比不中", entries: []string{"../shared", ".."}, want: nil},
		{name: "一般條目標準化後保留", entries: []string{"./notes/", "docs//public"},
			want: []string{"notes", filepath.Join("docs", "public")}},
		{name: "Workspace 根本身是合法條目", entries: []string{"."}, want: []string{"."}},
		{name: "無效與有效條目混在一起時只留有效的",
			entries: []string{"", "/abs", "../x", "notes"}, want: []string{"notes"}},
		{
			// 只有「trim 後為空」才算沒寫：條目前後真的帶空白時原樣保留，因為檔名
			// 前後本來就可以有空白，替使用者猜會讓那種路徑永遠碰不到。
			name:    "前後帶空白的條目原樣保留，不替使用者 trim",
			entries: []string{"  notes  "},
			want:    []string{"  notes  "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tool.EffectiveAllowedPaths(tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("EffectiveAllowedPaths(%q) = %q, 期望 %q", tt.entries, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("EffectiveAllowedPaths(%q)[%d] = %q, 期望 %q", tt.entries, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSandboxCheckerCheckShellCommand 是命令白名單的行為矩陣。
//
// 結構化 exec 之下這個檢查退化成**一次字串比對**——`argv[0]` 是不是白名單裡的那個
// 名字。沒有切分器，因此沒有切分器可以被騙（ADR-0005）。矩陣要證明的正是這件事：
// 最後兩格（`echo;rm` 被拒、`args` 裡的 metacharacter 放行）合起來說明白名單比對的
// 對象是**一個程式名**，不是一段命令文字。
func TestSandboxCheckerCheckShellCommand(t *testing.T) {
	tests := []struct {
		name          string
		allowed       []string
		command       string
		wantViolation bool
	}{
		{
			name:    "白名單內的程式名放行",
			allowed: []string{"echo", "git"},
			command: "echo",
		},
		{
			name:          "不在白名單拒絕",
			allowed:       []string{"echo"},
			command:       "rm",
			wantViolation: true,
		},
		{
			name:          "空白名單全部拒絕（deny by default）",
			allowed:       nil,
			command:       "echo",
			wantViolation: true,
		},
		{
			name:          "command 為空字串拒絕",
			allowed:       []string{"echo"},
			command:       "",
			wantViolation: true,
		},
		{
			// 含分隔符的名字 exec.Command 會當路徑用、不查 PATH，放行等於讓
			// ./x 與 /tmp/x 繞過「白名單是一份程式名清單」的語義。
			name:          "command 是相對路徑拒絕",
			allowed:       []string{"echo"},
			command:       "./echo",
			wantViolation: true,
		},
		{
			// 「echo 在白名單時 /usr/bin/echo 算不算」兩種答案都說得通，選最保守的
			// 那種最好解釋。
			name:          "command 是絕對路徑拒絕",
			allowed:       []string{"echo"},
			command:       "/usr/bin/echo",
			wantViolation: true,
		},
		{
			// 這一格是白名單「比對的是程式名」的正面證據：整串 `echo;rm` 不是任何
			// 白名單條目，所以被拒——不是因為裡面有分號，而是因為沒有一個叫這個
			// 名字的程式被列出來。**這裡沒有切分器**。
			name:          "command 含 shell metacharacter 拒絕",
			allowed:       []string{"echo", "rm"},
			command:       "echo;rm",
			wantViolation: true,
		},
		{
			// 白名單條目本身寫成路徑：它永遠比不中任何合法的 command（合法的
			// command 不含分隔符），列出來等於沒列。
			name:          "白名單條目寫成路徑時比不中裸命令名",
			allowed:       []string{"/usr/bin/echo"},
			command:       "echo",
			wantViolation: true,
		},
		{
			// 字面完全匹配：不做萬用字元、不做 basename 正規化（spec 定案）。
			name:          "不做前綴或子字串匹配",
			allowed:       []string{"echo"},
			command:       "echoes",
			wantViolation: true,
		},
	}

	checkerFor := func(allowed []string) *tool.SandboxChecker {
		return tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: allowed})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkerFor(tt.allowed).CheckShellCommand(tt.command)
			if got := errors.Is(err, tool.ErrSandboxViolation); got != tt.wantViolation {
				t.Fatalf("CheckShellCommand(%q) 違規 = %v (err=%v), 期望 %v",
					tt.command, got, err, tt.wantViolation)
			}
		})
	}
}

// TestSandboxShellCommandErrorIsActionable 驗證拒絕訊息**可行動**：說得出是哪個
// 命令名被擋，也說得出要往 `config.yaml` 的哪一段加。少了後者，使用者只知道「被擋了」
// 而不知道去哪裡開。
//
// 同時釘住訊息**列出白名單其餘條目與總數**（ticket #68 反轉；原本斷言的是「不得倒出來、
// 連基數都不提」，推翻的理由見下方反轉那一段）。
func TestSandboxShellCommandErrorIsActionable(t *testing.T) {
	checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"echo", "internal-deploy-tool"}})

	_, err := checker.CheckShellCommand("rm")
	if err == nil {
		t.Fatal("rm 不在白名單，期望被拒")
	}
	if !strings.Contains(err.Error(), "rm") {
		t.Errorf("訊息 %q 沒說是哪個命令名被擋", err)
	}
	if !strings.Contains(err.Error(), "shell.allowed_commands") {
		t.Errorf("訊息 %q 沒說要往 config.yaml 的哪一段加", err)
	}
	if !strings.Contains(err.Error(), "internal-deploy-tool") {
		t.Errorf("訊息 %q 沒列出白名單其餘條目", err)
	}

	// **訊息要對 LLM 說話，不只對使用者說話**（issue #36）。
	//
	// #34 的真實 API 驗收量到一組對比：同一個模型、同樣形狀的 SandboxViolation，
	// **路徑**被拒 2 次就停下來告知使用者，**命令**被拒卻換了 10 個名字（df、diskutil、
	// stat、du⋯⋯）用光 max_iterations，全程沒告訴使用者辦不到。
	//
	// 差別不在模型，在於**可猜的候選數**：路徑被拒時它推得出沒有別的路徑可試；命令被
	// 拒時候選名近乎無限，而訊息（正確地）不揭露白名單其餘條目，於是它只能一個一個猜。
	//
	// 修法是純措辭：補一句**對 LLM 的行為指示**——不要逐一嘗試，直接轉向使用者。
	//
	// **同一個模型、同一句 prompt 重驗過：10 次變 1 次。** 這一格擋的是回退：把引導拿掉，
	// 10 次那個形態就回來。
	//
	// **ticket #68 列出白名單之後，這一格一個字都沒改**（ADR-0007「保留 shell 那則的防猜
	// 句」）。有人會推論「清單都列出來了，候選不再近乎無限，防猜句可以拿掉」——那是推論，
	// 這一格守的 10 → 1 是量測。用推論換掉量測，換來的只是訊息短一點。
	for _, want := range []string{"逐一", "告訴使用者"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("訊息 %q 未提到 %q——少了對 LLM 的引導，它會改猜下一個命令名而不是轉向使用者", err, want)
		}
	}

	// **反轉：訊息要列出白名單的內容與總數**（ticket #68，ADR-0007）。
	//
	// 這一段原本是反向斷言——「引導不得以洩漏白名單為代價，連『有幾個』都不說」——而上面
	// 那段註解原本還寫著：「順便把白名單列出來」是看似更有幫助、實際上退回 #33 定案的改法。
	// **那句話預先擋住的正是 ADR-0007 走的這條路**，所以不能默默刪掉，要說清楚為什麼推翻：
	//
	//   - **分歧在證據的時間差。** 那句話寫於 issue #36 落地時，當時「不列出」的代價還沒有
	//     被量到。issue #58 之後才有資料：模型把沉默讀成「白名單是空的」，對使用者講出一句
	//     假話，而四次措辭介入都沒有在謊稱率上建立效益（沒量出效益，不等於證明沒有效果）。
	//   - **「#33 定案」查無論證。** #33 與上游 spec 的全文只寫了訊息**要**包含什麼，沒寫它
	//     **不得**包含什麼。ADR-0007 是這條規則的第一次論證，結論是反轉，保密性損失照實記在
	//     它的威脅模型一節。
	//   - **它警告的另一半本決定照單全收**：把引導拿掉會讓 10 次那個形態回來，所以上面那格
	//     不動。
	for _, want := range []string{"echo", "共 2 條"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("訊息 %q 沒列出白名單的內容或總數 %q", err, want)
		}
	}
}

// TestEffectiveAllowedCommands 是命令白名單的「有效條目」收斂，形狀與理由完全比照
// EffectiveAllowedPaths：**讓「白名單是不是空的」只有一個定義點**。
//
// 兩種條目回不來，因為它們永遠比不中任何請求：空白條目，以及**含路徑分隔符**的條目
// （合法的 command 不含分隔符，所以 `/usr/bin/git` 這種寫法永遠對不上）。
//
// 少了這個收斂，組裝點的啟動提醒會把 `allowed_commands: [/usr/bin/git]` 當成「已配置」
// 而閉嘴，實際上每次呼叫都被攔——使用者覺得自己照著錯誤訊息把命令加進去了，卻繼續
// 失敗，而系統一句話都沒說。
func TestEffectiveAllowedCommands(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    []string
	}{
		{name: "一般條目原樣保留", entries: []string{"echo", "git"}, want: []string{"echo", "git"}},
		{name: "空字串條目丟掉", entries: []string{"", "echo"}, want: []string{"echo"}},
		{name: "純空白條目丟掉", entries: []string{"   ", "echo"}, want: []string{"echo"}},
		{name: "絕對路徑條目丟掉", entries: []string{"/usr/bin/git"}, want: []string{}},
		{name: "相對路徑條目丟掉", entries: []string{"./bin/tool"}, want: []string{}},
		{name: "全部無效時是空的", entries: []string{"", "  ", "/usr/bin/git"}, want: []string{}},
		{name: "nil 是空的", entries: nil, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tool.EffectiveAllowedCommands(tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("EffectiveAllowedCommands(%v) = %v, 期望 %v", tt.entries, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("EffectiveAllowedCommands(%v)[%d] = %q, 期望 %q", tt.entries, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestEffectiveAllowedDomains 釘住網域段「白名單是不是空的」那個單一定義點（ADR-0007
// 「先決條件」一節）。消費者有三個：SandboxChecker 拿它比對、組裝點拿它決定要不要印
// 空白名單提醒、ticket #68 之後的拒絕訊息拿它列出內容。三者必須得到同一個答案。
//
// **它刻意比另外兩段寬鬆：只剔除真正的空字串。** 收斂有兩種錯法，嚴重程度不對等——剔得
// 太少只是少印一行提醒；剔得太多是一條使用者授權過、而且比得中的網域被靜默剔除，請求
// 被拒而系統一句話都不說。`""` 是唯一能確定比不中的：CheckHTTPURL 在進入比對迴圈之前
// 就擋下空 host。
func TestEffectiveAllowedDomains(t *testing.T) {
	type testCase struct {
		name    string
		entries []string
		want    []string
	}
	tests := []testCase{
		{name: "nil 是空的", entries: nil, want: []string{}},
		{name: "空字串條目等於沒寫", entries: []string{""}, want: []string{}},
		{name: "空字串與有效條目混在一起時只留有效的",
			entries: []string{"", "api.example.com", ""}, want: []string{"api.example.com"}},
		{
			// 最容易寫錯的一格：TrimSpace 會把這兩個字元當空白吃掉，但它們是合法且比得中
			// 的 host（見 TestCheckHTTPURLStillMatchesRetainedDomainShapes）。
			name:    "NBSP 與全形空白判為非空並保留",
			entries: []string{"\u00a0", "\u3000"},
			want:    []string{"\u00a0", "\u3000"},
		},
		{
			// url.Parse 自己就會拒絕含 ASCII 空白或 tab 的 host，所以它們比不中——但留著
			// 無害，疑慮交給啟動提醒。剔除它們得先證明「比不中」，而這一節的教訓是別猜。
			name:    "ASCII 空白與 tab 判為非空並保留",
			entries: []string{" ", "\t"},
			want:    []string{" ", "\t"},
		},
		{
			// 疑慮由啟動時的一行聚合提醒承擔，不由剔除承擔（見 chat.go）。
			name:    "看起來寫成網址的條目留在清單裡",
			entries: []string{"https://example.com", "example.com/path", "user@example.com", " example.com "},
			want:    []string{"https://example.com", "example.com/path", "user@example.com", " example.com "},
		},
	}
	// 保留形狀那一格從共用清單組出來，不在這裡另抄一份（理由見 retainedDomainShapes）。
	retained := testCase{name: "保留形狀全部保留（只轉小寫）"}
	for _, shape := range retainedDomainShapes() {
		retained.entries = append(retained.entries, shape.entry)
		retained.want = append(retained.want, shape.effective)
	}
	tests = append(tests, retained)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tool.EffectiveAllowedDomains(tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("EffectiveAllowedDomains(%q) = %q, 期望 %q", tt.entries, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("EffectiveAllowedDomains(%q)[%d] = %q, 期望 %q", tt.entries, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestEffectiveAllowedDedup 釘住三段共通的去重規則（ADR-0007）：**依有效值判重、保留首次
// 出現、在計數之前完成**。
//
// **為什麼要去重**：ticket #68 之後拒絕訊息最多列 32 條。重複條目若佔掉名額，會把真正
// 可用的後續條目擠出訊息之外——使用者看到「共 40 條，其中 8 條未列出」，而列出的那 32
// 條裡有一半是同一個網域。
//
// **為什麼是「依有效值」而不是字面**：三段各自的比對語義決定了什麼叫「同一條」。paths
// 經 filepath.Clean 標準化，`notes`／`notes/`／`./notes` 在校驗器眼中是同一棵子樹；
// domains 兩側轉小寫比對，大小寫不同的是同一個網域；commands 是字面完全相等（spec #4
// 定案不做 basename 或大小寫正規化），所以 `git` 與 `Git` 是兩條。
//
// **去重不改變任何比對結果**：重複條目放行的集合與單一條目完全相同。所以這裡只驗清單的
// 形狀，比對行為由三個 Check* 的既有矩陣守著。
func TestEffectiveAllowedDedup(t *testing.T) {
	tests := []struct {
		name      string
		effective func([]string) []string
		entries   []string
		want      []string
	}{
		{name: "paths 在標準化之後判重", effective: tool.EffectiveAllowedPaths,
			entries: []string{"notes", "notes/", "./notes"}, want: []string{"notes"}},
		{name: "paths 保留首次出現的順序", effective: tool.EffectiveAllowedPaths,
			entries: []string{"docs", "notes", "./docs/"}, want: []string{"docs", "notes"}},
		{name: "commands 字面判重", effective: tool.EffectiveAllowedCommands,
			entries: []string{"git", "git", "git"}, want: []string{"git"}},
		{name: "commands 保留首次出現的順序", effective: tool.EffectiveAllowedCommands,
			entries: []string{"ls", "git", "ls"}, want: []string{"ls", "git"}},
		{name: "commands 大小寫不同是兩條", effective: tool.EffectiveAllowedCommands,
			entries: []string{"git", "Git"}, want: []string{"git", "Git"}},
		{name: "domains 轉小寫後判重", effective: tool.EffectiveAllowedDomains,
			entries: []string{"Example.com", "example.COM"}, want: []string{"example.com"}},
		{name: "domains 保留首次出現的順序", effective: tool.EffectiveAllowedDomains,
			entries: []string{"b.example", "a.example", "B.EXAMPLE"}, want: []string{"b.example", "a.example"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.effective(tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("收斂(%q) = %q, 期望 %q", tt.entries, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("收斂(%q)[%d] = %q, 期望 %q", tt.entries, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// whitelistHandoffMark 是三則白名單拒絕訊息共同要帶的**行動指示**：轉向使用者、說出
// 你需要哪一個。
//
// **它與 ticket #68 之前那句「白名單的內容不會在這裡列出，所以看不到⋯⋯不代表白名單是
// 空的」的差別，是「指令」與「禁令」的差別，而那個差別是量出來的。**
//
// issue #36 在 shell 那則補的是指令（「不要逐一嘗試——請直接告訴使用者你需要哪一個
// 命令」），真實 API 上 10 次 iteration 變 1 次。issue #58 在路徑那則補的是禁令
// （「看不到允許的路徑不代表白名單是空的」），2026-09-04 的受控 A／B（24 輪、同批次、
// 同模型、同用例，唯一的變數就是這則措辭）量到謊稱率 **4/12 對 4/12，Fisher exact
// 雙尾 p = 1.000**——不是差異小，是兩組相同。
//
// 機制上說得通：模型走到最後一個 iteration 時必須**生出一段話**交代為什麼辦不到。
// 禁令只告訴它「有一句話不能說」，沒有給它一句可以說的話，於是它照樣自己編一個解釋
// 出來。同一批 A／B 另有一個未達顯著的反向訊號（走到第 3 次 Tool 呼叫 3/12 → 7/12，
// p = 0.214）：「白名單可能有東西」對模型而言同時是「那再試試」。
//
// 所以第 6 項不是「再加一句話」，是**把下半句從一句陳述補成一個出口**。它同時是
// core.ToolErrorSandbox 維護契約要求的東西——那一類的 Guidance() 刻意回空字串，下一步
// 由每一則訊息自己帶，而路徑與 HTTP 兩則在這之前一步都沒帶。
//
// **標記取「告訴使用者你需要哪一個」而不是「告訴使用者」**：後者鬆到一句「請告訴使用者
// 這件事辦不到」就矇混得過去，而那句話不構成任何出口——模型仍然得自己決定要說什麼，
// 正是這個 bug 的成因。
//
// **它只是出口的下半，不能單獨存在**——上半是第 7 項的換路分支，見 wantRouting 欄。
const whitelistHandoffMark = "告訴使用者你需要哪一個"

// TestWhitelistDenialMessagesShareTheSameContract 是三則白名單拒絕訊息的**共同契約**
// （issue #58）。
//
// **為什麼要一張橫跨三則的表，而不是各自補一支測試**：這個 bug 的根因不是某一則訊息
// 寫錯了，是三則各寫各的、沒有任何東西在比對它們，於是它們漂成了三種品質——shell 那則
// （issue #36 改過）三項齊備、路徑那則缺反推論句、HTTP 那則連「該往哪加」都沒有。
// 逐則補測試修得掉這一次的症狀，修不掉「下一則又漂掉」的成因。表格驅動讓七項契約在
// 三則上一次成立，日後新增第四種白名單時，加一列就得同時滿足全部七項。
//
// 七項契約（ticket #68 依 ADR-0007「契約七項的完整遷移」改過一次，每項括號裡標了動向）：
//
//  1. **指名被拒的那一個**——沒有它，使用者不知道是什麼被擋了（不動）
//  2. **說出要往 config.yaml 的哪一段加**——沒有它，使用者知道被擋了卻不知道怎麼放行
//     （不動）
//  3. **列出該段白名單實際生效的內容與總數**——模型手上有了清單，「白名單是空的」這句話
//     就與一條已知事實直接矛盾（**換新契約**，原本是「說出白名單的內容不會在這裡列出」）。
//     空白名單、兩個顯示上限、三個分支由 TestWhitelistDenialMessagesListEffectiveEntries
//     逐格守，這裡只驗一般分支
//  4. **清單之後接上「所以⋯⋯」**（每列的 wantConsequence）——路徑與 HTTP 的「不代表白名單
//     是空的」在清單出現後成了贅語，清單本身就是結論，那兩列留空；shell 的「不要逐一嘗試」
//     保留（**順序錨點從第 3 項那句改為清單**）
//  5. **白名單其餘條目必須出現**（**反轉**，原本是 issue #33 定案的「不得洩漏」；推翻的理由
//     見 TestSandboxShellCommandErrorIsActionable 反轉那一段，保密性損失見 ADR-0007）
//  6. **那個「所以⋯⋯」必須是一道指令，不能只是一句禁令**——說出轉向使用者、指名你
//     需要哪一個（whitelistHandoffMark），且排在清單之後（標記不動，**順序錨點改為清單**）
//  7. **轉向使用者之前要先回答「還能不能換一條路」**（每列的 wantRouting，措辭各自
//     不同），且排在 whitelistHandoffMark 之前（不動——路徑與 HTTP 的「已經確認可用」
//     現在有了來源）
//
// **第 3 項與第 5 項原本是同一個張力的兩端**：不揭露內容（舊 5）正是模型無從得知白名單
// 狀態的原因，所以必須明說「看不到不等於沒有」（舊 3）。四次措辭介入都沒有在謊稱率上建立
// 效益，ADR-0007 改從前提那端著手：把內容列出來，模型手上就有一份可核對的事實（是否因此
// 少說錯尚未驗證）——所以一項換掉、一項反轉，而且舊的那兩句補缺席的話**不得留下**，清單
// 出現之後它們是假話。
//
// **第 4 項鎖的是每列一個「最低語意標記」，不是整句措辭。** shell 擋的是行為（標記「不要
// 逐一嘗試」）；路徑與 HTTP 原本擋推論，那個推論現在由清單本身擋——契約要求的是標記在、
// 且排在清單之後，其餘用字自由。
//
// 這與 TestSandboxShellCommandErrorIsActionable 的分工要說清楚，因為兩邊都會查
// shell 那則：那一支是 issue #36 的成果測試，查的是那則訊息**整組**性質（含「告訴
// 使用者」，是真實 API 量出來的，10 次 iteration 變 1 次）；這裡只查「清單之後有沒有
// 接上結論」這一件三則共有的事。同一個字串被兩支測試查到，但它們問的是不同的問題。
//
// 這也是 issue #58「只把『內容不會列出』搬進路徑那則、不搬防猜句」的正確讀法：不搬的
// 是**防猜的措辭**（「不要逐一嘗試其他命令名」），因為那句治的是候選近乎無限時逐一猜
// 名字的形態；但「要有下半句」這個結構是三則共有的，而第 6 項進一步要求那個下半句得
// 是一個**出口**。
//
// **第 6 項是 A／B 之後才長出來的，它推翻的是第 4 項當時的隱含假設**：第 4 項只要求
// 「有下半句」，於是路徑與 HTTP 兩則各拿一句禁令就滿足了它，而受控 A／B 量到那種形狀
// 對謊稱率 p = 1.000。第 4 項因此仍然成立、一個斷言都沒改，只是它不夠——推導見
// whitelistHandoffMark。
func TestWhitelistDenialMessagesShareTheSameContract(t *testing.T) {
	tests := []struct {
		name string
		// deny 觸發這一種白名單的拒絕，回傳那個錯誤。
		deny func(*tool.SandboxChecker) error
		// checker 帶**兩條**白名單條目：listedEntry 與 otherEntry，兩條都必須列出。
		checker *tool.SandboxChecker
		// wantSubject 是被拒的那一個，必須出現在訊息裡。
		wantSubject string
		// wantSetting 是 config.yaml 裡該改的那一段。
		wantSetting string
		// listedEntry 是白名單裡的第一條，必須以 %q 出現（第 3 項）。
		listedEntry string
		// otherEntry 是白名單裡與被拒對象無關的另一條，必須以 %q 出現（第 5 項，ticket #68
		// 反轉——原本是「絕不可出現」）。
		otherEntry string
		// wantConsequence 是接在清單後面那句「所以⋯⋯」的關鍵字；**空字串代表清單本身就是
		// 結論**。
		//
		// **路徑與 HTTP 留空**：它們原本的「不代表白名單是空的」擋的是推論，而清單出現之後
		// 那個推論已經與眼前的事實矛盾，再說一次是贅語（ADR-0007 契約遷移第 4 項）。**shell
		// 保留「不要逐一嘗試」**：它擋的是行為，而那個行為的效果是量出來的（issue #36）。
		wantConsequence string
		// wantRouting 是「這次該做什麼」那半句的關鍵字，排在 wantHandoff 之前。
		//
		// **為什麼它必須存在，而且必須是每列各自的措辭**：core.ToolErrorKind.Guidance()
		// 的措辭規則明訂每一段要回答兩件事——「這次該做什麼」與「什麼時候該停下來問人」
		// ——而既有的每一段都是那個形狀（not_found：「用確認過的確切名字呼叫一次；沒有
		// 辦法確認⋯⋯就直接告訴使用者」；timeout、upstream 同樣是「先換做法，或告訴
		// 使用者」）。少了這半句，訊息就成了無條件的「去問人」。
		//
		// **無條件的「去問人」會與既有的恢復契約矛盾**：
		// TestProcessReadFileSandboxRejectionRecovers 與
		// TestProcessListDirSandboxRejectionRecovers 都把「被拒後改走已知可用的那條」
		// 列為恢復行為。那兩支走固定回放，訊息措辭再怎麼改它們都綠——**所以這個矛盾
		// 不會有任何測試轉紅，只會在真實模型上發生**（外部審查抓到，本輪第一版就是
		// 無條件的）。
		//
		// **三則的答案不同，這正是它要按列宣告的原因**：路徑與 HTTP 有可換的路（條件是
		// 「已經確認可用」），shell 沒有——#36 量到命令白名單被拒時候選近乎無限，
		// 逐一猜就是那個病，所以它的答案是「不要逐一嘗試」。同一個問題，兩種合法的
		// 回答，但**不回答不行**。
		wantRouting string
	}{
		{
			name: "檔案路徑",
			checker: tool.NewSandboxChecker(tool.SandboxConfig{
				AllowedPaths: []string{"notes", "internal/private-notes"}}),
			deny: func(c *tool.SandboxChecker) error {
				_, _, err := c.CheckFilePath("secrets/api.txt")
				return err
			},
			wantSubject: "secrets/api.txt",
			wantSetting: "file.allowed_paths",
			listedEntry: "notes",
			otherEntry:  "internal/private-notes",
			// wantConsequence 留空：清單本身就是結論（見欄位說明）。
			wantRouting: "已經確認可用的路徑",
		},
		{
			name: "shell 命令",
			checker: tool.NewSandboxChecker(tool.SandboxConfig{
				AllowedCommands: []string{"echo", "internal-deploy-tool"}}),
			deny: func(c *tool.SandboxChecker) error {
				_, err := c.CheckShellCommand("rm")
				return err
			},
			wantSubject: "rm",
			wantSetting: "shell.allowed_commands",
			listedEntry: "echo",
			otherEntry:  "internal-deploy-tool",
			// shell 這句由 issue #36 量出來（10 次 iteration 變 1 次），
			// TestSandboxShellCommandErrorIsActionable 另有專屬斷言；這裡收的是
			// 「no-listing 那句話必須有下半句」這個共通性質，不是 #36 的措辭本身。
			wantConsequence: "不要逐一嘗試",
			// shell 的換路答案與它的 consequence 是同一句，**這不是重複而是它的答案本身**：
			// 命令白名單被拒時沒有「已經確認可用的替代命令」這種東西可換（#36 量到候選
			// 近乎無限），所以它對「還能不能換一條路」的回答就是「不要逐一嘗試」。
			wantRouting: "不要逐一嘗試",
		},
		{
			name: "HTTP 域名",
			checker: tool.NewSandboxChecker(tool.SandboxConfig{
				AllowedDomains: []string{"trusted.example.com", "internal.example.org"}}),
			deny: func(c *tool.SandboxChecker) error {
				_, err := c.CheckHTTPURL("https://blocked.example.net/x")
				return err
			},
			wantSubject: "blocked.example.net",
			wantSetting: "http.allowed_domains",
			listedEntry: "trusted.example.com",
			otherEntry:  "internal.example.org",
			// wantConsequence 留空：清單本身就是結論（見欄位說明）。
			wantRouting: "已經確認可用的網域",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.deny(tt.checker)
			if !errors.Is(err, tool.ErrSandboxViolation) {
				t.Fatalf("拒絕的錯誤 = %v, 期望 SandboxViolation", err)
			}
			msg := err.Error()

			if !strings.Contains(msg, tt.wantSubject) {
				t.Errorf("訊息 %q 沒指名被拒的是 %q", msg, tt.wantSubject)
			}
			// **兩個都要查，缺一不可。** 只查設定段名會被矇混過去：「不在
			// http.allowed_domains 白名單」這種句子含有段名，卻沒有一個字叫人去改
			// 設定檔——那正是 issue #58 落地前 HTTP 那則的狀態。加上 config.yaml
			// 才分得開「提到了那一段」與「告訴你去改它」。
			for _, want := range []string{tt.wantSetting, "config.yaml"} {
				if !strings.Contains(msg, want) {
					t.Errorf("訊息 %q 未提到 %q——使用者知道被擋了卻不知道怎麼放行",
						msg, want)
				}
			}
			// **第 3 項：列出該段白名單實際生效的內容與總數**（ticket #68 換新契約）。以 %q
			// 查而不是裸字串：條目是使用者手寫的 YAML 字串，序列化方式本身就是契約的一部分。
			listed := fmt.Sprintf("%q", tt.listedEntry)
			for _, want := range []string{listed, "共 2 條"} {
				if !strings.Contains(msg, want) {
					t.Errorf("訊息 %q 沒列出白名單的內容 %q——模型手上沒有清單，可能把沉默讀成"+
						"「白名單是空的」，對使用者講出一句聽起來合理的錯誤事實（issue #58）", msg, want)
				}
			}
			// 清單出現之前，三則訊息各用一句話補上缺席。清單列出來之後那兩句成了假話——「不會在
			// 這裡列出」與眼前的清單直接矛盾，「你無從得知它的狀態」也不再成立。留著等於同一則
			// 訊息對模型說兩件互斥的事，所以遷移不只是加上清單，還得把它們拿掉。
			for _, stale := range []string{"不會在這裡列出", "無從得知它的狀態"} {
				if strings.Contains(msg, stale) {
					t.Errorf("訊息 %q 列出了清單，卻還留著 %q——那句話現在與清單矛盾", msg, stale)
				}
			}
			// **第 5 項（ticket #68 反轉）：白名單其餘條目必須出現。** 原本是 issue #33 定案的
			// 「不得洩漏」；推翻的理由見 TestSandboxShellCommandErrorIsActionable 反轉那一段。
			other := fmt.Sprintf("%q", tt.otherEntry)
			if !strings.Contains(msg, other) {
				t.Errorf("訊息 %q 沒列出白名單的其他條目 %q", msg, other)
			}

			// 第 4、6 項的順序錨點：**清單結束的位置**（原本錨在「不會在這裡列出」那句）。
			// 兩條條目有一條沒找到時上面已經報錯，順序就不查，免得多報一串連帶的錯。
			listEnd := -1
			if atListed, atOther := strings.Index(msg, listed), strings.Index(msg, other); atListed >= 0 && atOther >= 0 {
				listEnd = max(atListed+len(listed), atOther+len(other))
			}

			// **第 4 項：清單之後接上「所以⋯⋯」。** 下半句要單獨查：只查清單的話，把「所以
			// ⋯⋯」刪掉測試照樣綠。**而且要查它排在後面**，不只是出現在訊息裡某處——下半句
			// 是個結論子句，放到清單之前就不成話，而 strings.Contains 對順序一無所知（外部
			// 審查抓到）。路徑與 HTTP 這一欄留空，清單本身就是結論。
			if tt.wantConsequence != "" {
				atConsequence := strings.Index(msg, tt.wantConsequence)
				if atConsequence < 0 {
					t.Errorf("訊息 %q 列出了清單，卻沒接上 %q——"+
						"一句沒有出口的陳述，模型會自己補一個出口出來", msg, tt.wantConsequence)
				} else if listEnd >= 0 && atConsequence < listEnd {
					t.Errorf("訊息 %q 的 %q 出現在清單結束**之前**——"+
						"下半句是結論子句，排到前面就不成話", msg, tt.wantConsequence)
				}
			}
			// **第 7 項：轉向使用者之前要先回答「還能不能換一條路」。** 推導見
			// wantRouting 欄。它與第 6 項是一句話的兩半，所以順序也要查——換路的
			// 判斷排在轉向使用者之後就不成話，而且那個順序正好是「先做什麼、
			// 什麼時候停」的順序。
			atRouting := strings.Index(msg, tt.wantRouting)
			if atRouting < 0 {
				t.Errorf("訊息 %q 沒有回答「還能不能換一條路」（期望 %q）——"+
					"無條件叫模型去問人，會與 TestProcessReadFileSandboxRejectionRecovers "+
					"那一類「被拒後改走已知可用的那條」的恢復契約矛盾，而那些測試走固定回放、"+
					"抓不到這個矛盾", msg, tt.wantRouting)
			}

			// **第 6 項：那個出口要是一道指令，不只是一句禁令。** 推導與量測見
			// whitelistHandoffMark。順序同樣要查——它回答的是「所以你該做什麼」，
			// 排到清單之前一樣不成話。
			atHandoff := strings.Index(msg, whitelistHandoffMark)
			if atHandoff < 0 {
				t.Errorf("訊息 %q 沒有給出口 %q——只告訴模型不能怎麼推論，它到了最後一個 "+
					"iteration 仍然得自己編一段話交代給使用者（issue #58 的 A／B：純禁令 p=1.000）",
					msg, whitelistHandoffMark)
			} else if listEnd >= 0 && atHandoff < listEnd {
				t.Errorf("訊息 %q 的 %q 出現在清單結束**之前**——出口是結論子句，排到前面就不成話",
					msg, whitelistHandoffMark)
			} else if atRouting >= 0 && atHandoff < atRouting {
				t.Errorf("訊息 %q 的 %q 出現在 %q **之前**——"+
					"「什麼時候停下來問人」排到「這次該做什麼」前面，等於那個條件從沒被提出過",
					msg, whitelistHandoffMark, tt.wantRouting)
			}
		})
	}
}

// TestWhitelistDenialMessagesListEffectiveEntries 是三則拒絕訊息**列出白名單**的序列化
// 規則（ADR-0007「清單序列化的登記測試」，ticket #68）。
//
// **每一格對三則訊息各跑一次，而且走三個 Check* 方法，不直接測渲染函式。** 這條規則要防的
// 失敗形態是：渲染邏輯寫在一處、三則各自呼叫它，其中一則忘了呼叫——直接測渲染函式的話測試
// 照樣綠，而模型在那一則上照樣看不到清單。
//
// 清單的內容是**使用者可控的字串**，而它會進 Provider context、落日誌、落審計，所以下面每一條
// 都是「漏了不會有人發現」的收口：
//
//   - **兩個顯示上限**：最多列 32 條；%q 渲染後超過 256 bytes 的不列。兩者都計入總數、**不做
//     字串截斷**——列出去的每一條都完整、可直接使用，模型不會拿一條殘缺的路徑去呼叫、再被拒。
//   - **套用順序：去重 → 濾掉過長 → 取前 32。**「33 條、第 1 條過長」那格是它與「先取 32 再
//     濾」的唯一分辨點：前者列滿 32 條，後者只列 31 條。
//   - **三個分支**：空白名單／零筆可顯示／一般。「零筆可顯示」是有條目但全部過長，它和空白
//     名單一樣不得輸出清單的冒號後接空白。
//   - **%q 逸出**：條目含換行時若原樣輸出，訊息會長出一行看起來像系統輸出的文字——既污染
//     日誌，也在模型的 context 裡製造一句它會當真的假話。
//
// 格子逐列對應 ADR 那張表，**刻意不在這裡寫格數**：可變的計數寫死就會過期。
func TestWhitelistDenialMessagesListEffectiveEntries(t *testing.T) {
	denials := []struct {
		name string
		// checker 以 entries 當作這一段的白名單，建出校驗器。
		checker func(entries []string) *tool.SandboxChecker
		// deny 觸發這一段的拒絕，回傳那個錯誤。被拒的對象不會被下面任何一格的條目放行。
		deny func(*tool.SandboxChecker) error
	}{
		{
			name: "檔案路徑",
			checker: func(entries []string) *tool.SandboxChecker {
				return tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: entries})
			},
			deny: func(c *tool.SandboxChecker) error {
				_, _, err := c.CheckFilePath("secrets/api.txt")
				return err
			},
		},
		{
			name: "shell 命令",
			checker: func(entries []string) *tool.SandboxChecker {
				return tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: entries})
			},
			deny: func(c *tool.SandboxChecker) error {
				_, err := c.CheckShellCommand("rm")
				return err
			},
		},
		{
			name: "HTTP 域名",
			checker: func(entries []string) *tool.SandboxChecker {
				return tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: entries})
			},
			deny: func(c *tool.SandboxChecker) error {
				_, err := c.CheckHTTPURL("https://blocked.example.net/x")
				return err
			},
		},
	}

	// seq 產生 e01、e02⋯⋯共 n 條。這種條目在三段收斂下都原樣保留（相對路徑、不含分隔符、
	// 已是小寫），所以同一份清單能拿去三則訊息各跑一次。
	seq := func(n int) []string {
		entries := make([]string, n)
		for i := range entries {
			entries[i] = fmt.Sprintf("e%02d", i+1)
		}
		return entries
	}
	// renderedAs 產生一條 %q 渲染後**恰好** n bytes 的條目。tag 讓條目彼此不同，其餘以 `"`
	// 填滿：每個 `"` 渲染成 `\"`，原字串因此只有渲染結果的一半左右——門檻若量的是原字串，
	// 這種條目會被錯放進清單。長度當場量出來，不靠算術。
	renderedAs := func(tag string, n int) string {
		entry := tag
		for len(fmt.Sprintf("%q", entry)) < n-1 {
			entry += `"`
		}
		if len(fmt.Sprintf("%q", entry)) < n {
			entry += "x"
		}
		if got := len(fmt.Sprintf("%q", entry)); got != n {
			t.Fatalf("renderedAs(%q, %d) 渲染後是 %d bytes", tag, n, got)
		}
		return entry
	}

	at256 := renderedAs("edge", 256)
	longA := renderedAs("long-a", 257)
	longB := renderedAs("long-b", 257)
	// 原字串 64 bytes，遠在門檻內；每個 \x01 渲染成 4 bytes 的 `\x01`，渲染後 258 bytes。
	expands := strings.Repeat("\x01", 64)
	if len(expands) > 256 || len(fmt.Sprintf("%q", expands)) <= 256 {
		t.Fatalf("expands 必須原字串在門檻內、渲染後超出：原 %d、渲染後 %d bytes",
			len(expands), len(fmt.Sprintf("%q", expands)))
	}
	worst := make([]string, 32)
	for i := range worst {
		worst[i] = renderedAs(fmt.Sprintf("w%02d", i+1), 256)
	}
	// 每一條只靠一種控制字元入選。第五條若原樣輸出，訊息會多長出一行假提醒。C1（U+0085）
	// 與 DEL 不在 0x00–0x1F 的範圍裡，逸出規則若只認那一段就會漏掉它們。
	controls := []string{"a\nb", "c\rd", "e\tf", "g\x1bh", "notes\n提醒：偽造的一行", "i\u0085j", "k\x7fl"}
	// 兩類上限同時觸發：35 條裡 2 條過長、有效的 33 條——過長的故意插在中間，不在頭尾。
	bothLimits := append(append(append(seq(10), longA), seq(33)[10:20]...), append([]string{longB}, seq(33)[20:]...)...)

	tests := []struct {
		name string
		// only 非空時這一格只跑那一則——三段各自的去重規則本來就不同。
		only    string
		entries []string
		// wantListed 是收斂後、必須以 %q 恰好出現一次的條目。
		wantListed []string
		// wantUnlisted 是收斂後仍在白名單裡、但不得出現在訊息裡的條目。
		wantUnlisted []string
		// wantTotal 是訊息宣稱的總數；0 代表走空白名單分支。
		wantTotal   int
		wantTooLong int // 因過長未列出的條數
		wantOverCap int // 因超過 32 條上限未列出的條數
		// wantZeroDisplayable 代表有條目、但一條都顯示不出來。
		wantZeroDisplayable bool
		// fragmentBudget 非零時量清單片段的**實際** byte 數，必須不超過它。
		fragmentBudget int
	}{
		{name: "0 條走空白名單分支", entries: nil},
		{name: "1 條列出且不標示未列出", entries: seq(1), wantListed: seq(1), wantTotal: 1},
		{name: "32 條全部列出且不標示未列出", entries: seq(32), wantListed: seq(32), wantTotal: 32},
		{name: "33 條列前 32 條並標示 1 條超過上限", entries: seq(33),
			wantListed: seq(32), wantUnlisted: []string{"e33"}, wantTotal: 33, wantOverCap: 1},
		{
			// 空字串在三段收斂下都被剔除，重複的 e01 被去重：原始 slice 4 條，有效的只有 2 條。
			name: "總數取自收斂並去重後的清單", entries: []string{"e01", "", "e01", "e02"},
			wantListed: seq(2), wantTotal: 2,
		},
		{
			// 去重若排在上限之後，33 條會先被截成 32 條，並標示 1 條超過上限。
			name: "33 條含重複時去重後 30 條全部列出", entries: append(seq(30), "e01", "e02", "e03"),
			wantListed: seq(30), wantTotal: 30,
		},
		{name: "單項渲染後 256 bytes 原樣列出", entries: []string{at256}, wantListed: []string{at256}, wantTotal: 1},
		{name: "單項渲染後 257 bytes 不列出但計入總數", entries: []string{"e01", longA},
			wantListed: seq(1), wantUnlisted: []string{longA}, wantTotal: 2, wantTooLong: 1},
		{name: "原字串在門檻內但渲染後超出時不列出", entries: []string{"e01", expands},
			wantListed: seq(1), wantUnlisted: []string{expands}, wantTotal: 2, wantTooLong: 1},
		{
			// 8 KiB 是 32 × 256；另外 512 bytes 給說明文字與 31 個分隔符。
			name: "32 條各 256 bytes 時清單片段的實際長度有上界", entries: worst,
			wantListed: worst, wantTotal: 32, fragmentBudget: 8*1024 + 512,
		},
		{
			// 先取 32 再濾的話，前 32 條裡含那條過長的，只列得出 31 條，e32 被擠到上限之外。
			name: "33 條且第 1 條過長時列滿 32 條", entries: append([]string{longA}, seq(32)...),
			wantListed: seq(32), wantUnlisted: []string{longA}, wantTotal: 33, wantTooLong: 1,
		},
		{name: "兩類上限同時觸發時分開計", entries: bothLimits,
			wantListed: seq(32), wantUnlisted: []string{longA, longB, "e33"}, wantTotal: 35, wantTooLong: 2, wantOverCap: 1},
		{name: "零筆可顯示時不輸出清單的冒號", entries: []string{longA},
			wantUnlisted: []string{longA}, wantTotal: 1, wantZeroDisplayable: true},
		{name: "控制字元與換行以 %q 逸出", entries: controls, wantListed: controls, wantTotal: len(controls)},
		{name: "paths 標準化後重複收斂成一條", only: "檔案路徑", entries: []string{"notes", "notes/", "./notes"},
			wantListed: []string{"notes"}, wantTotal: 1},
		{name: "commands 字面重複收斂成一條", only: "shell 命令", entries: []string{"git", "git", "git"},
			wantListed: []string{"git"}, wantTotal: 1},
		{
			// 列的是轉小寫後的有效值，不是使用者寫的原樣：校驗器實際拿來比對的就是那一份。
			name: "domains 大小寫重複收斂成一條", only: "HTTP 域名", entries: []string{"Example.com", "example.COM"},
			wantListed: []string{"example.com"}, wantUnlisted: []string{"Example.com", "example.COM"}, wantTotal: 1,
		},
	}

	// 分支標記。一般分支長成「目前允許的⋯⋯（共 N 條⋯⋯）：清單。」
	const (
		emptyMark  = "白名單目前是空的"
		listColon  = "）："
		listHeader = "目前允許的"
	)
	// clause 查一個「N 條因⋯⋯未列出」子句：n 為零時整個子句必須省略。
	clause := func(t *testing.T, msg, suffix string, n int) {
		t.Helper()
		if n == 0 {
			if strings.Contains(msg, suffix) {
				t.Errorf("沒有條目%s，訊息卻有這個子句: %q", suffix, msg)
			}
			return
		}
		if want := fmt.Sprintf("%d %s", n, suffix); !strings.Contains(msg, want) {
			t.Errorf("訊息沒說 %q: %q", want, msg)
		}
	}

	for _, tt := range tests {
		matched := 0
		for _, d := range denials {
			if tt.only != "" && tt.only != d.name {
				continue
			}
			matched++
			t.Run(tt.name+"/"+d.name, func(t *testing.T) {
				err := d.deny(d.checker(tt.entries))
				if !errors.Is(err, tool.ErrSandboxViolation) {
					t.Fatalf("拒絕的錯誤 = %v, 期望 SandboxViolation", err)
				}
				msg := err.Error()

				// 不論哪一格，訊息裡都不得有任何控制字元：換行會偽造日誌行，ESC 會改終端機版面。
				if strings.ContainsFunc(msg, unicode.IsControl) {
					t.Errorf("訊息含未逸出的控制字元: %q", msg)
				}

				switch {
				case tt.wantTotal == 0:
					if !strings.Contains(msg, emptyMark) {
						t.Errorf("空白名單要明說是空的（期望含 %q）: %q", emptyMark, msg)
					}
					for _, forbidden := range []string{listHeader, listColon, "共 "} {
						if strings.Contains(msg, forbidden) {
							t.Errorf("空白名單走了清單的形狀（含 %q）: %q", forbidden, msg)
						}
					}
				case tt.wantZeroDisplayable:
					if want := fmt.Sprintf("共 %d 條，但全部因過長未列出", tt.wantTotal); !strings.Contains(msg, want) {
						t.Errorf("零筆可顯示要說出總數與全部未列出（期望含 %q）: %q", want, msg)
					}
					if strings.Contains(msg, listColon) {
						t.Errorf("零筆可顯示輸出了清單的冒號，後面卻什麼都沒有: %q", msg)
					}
				default:
					for _, want := range []string{fmt.Sprintf("（共 %d 條", tt.wantTotal), listColon} {
						if !strings.Contains(msg, want) {
							t.Errorf("一般分支期望含 %q: %q", want, msg)
						}
					}
					clause(t, msg, "條因過長未列出", tt.wantTooLong)
					clause(t, msg, "條因超過 32 條上限未列出", tt.wantOverCap)
				}

				for _, entry := range tt.wantListed {
					if got := strings.Count(msg, fmt.Sprintf("%q", entry)); got != 1 {
						t.Errorf("條目 %q 以 %%q 出現了 %d 次，期望恰好 1 次: %q", entry, got, msg)
					}
				}
				for _, entry := range tt.wantUnlisted {
					if strings.Contains(msg, fmt.Sprintf("%q", entry)) {
						t.Errorf("條目 %q 不該被列出: %q", entry, msg)
					}
				}

				if tt.fragmentBudget > 0 {
					last := fmt.Sprintf("%q", tt.wantListed[len(tt.wantListed)-1])
					start, end := strings.Index(msg, listHeader), strings.LastIndex(msg, last)
					if start < 0 || end < 0 {
						t.Fatalf("量不到清單片段（header 在 %d、最後一條在 %d）: %q", start, end, msg)
					}
					if got := end + len(last) - start; got > tt.fragmentBudget {
						t.Errorf("清單片段實際 %d bytes，超過宣稱的上界 %d bytes", got, tt.fragmentBudget)
					}
				}
			})
		}
		if matched == 0 {
			t.Errorf("格子 %q 的 only = %q 對不上任何一則訊息", tt.name, tt.only)
		}
	}
}
