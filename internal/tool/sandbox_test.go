package tool_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

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

// TestSandboxFilePathErrorIsActionableAndNarrow 驗證拒絕訊息**可行動**又**不多話**：
// 它要指出使用者該改哪一段設定（沒有這句，使用者只會看到「被拒絕」卻不知往哪加），
// 但不得把白名單其餘條目一起倒出來——訊息會落日誌、也會回填給 LLM，那等於把這個
// Workspace 允許的其他路徑一併交出去。形狀沿用 TestSandboxViolationErrorOmitsQuery。
func TestSandboxFilePathErrorIsActionableAndNarrow(t *testing.T) {
	const otherEntry = "internal/private-notes"
	checker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: []string{"notes", otherEntry}})

	_, _, err := checker.CheckFilePath("secrets/api.txt")
	if !errors.Is(err, tool.ErrSandboxViolation) {
		t.Fatalf("CheckFilePath = %v, 期望 SandboxViolation", err)
	}
	if !strings.Contains(err.Error(), "file.allowed_paths") {
		t.Errorf("錯誤訊息沒告訴使用者要改哪段設定: %q", err.Error())
	}
	if strings.Contains(err.Error(), otherEntry) {
		t.Errorf("錯誤訊息洩漏了白名單的其他條目: %q", err.Error())
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
// 同時釘住訊息**不把白名單其餘條目倒出來**——那等於交出這個 Workspace 還允許跑哪些
// 程式（形狀沿用 TestSandboxViolationErrorOmitsQuery）。
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
	if strings.Contains(err.Error(), "internal-deploy-tool") {
		t.Errorf("訊息 %q 洩漏了白名單其餘條目", err)
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
	// **同一個模型、同一句 prompt 重驗過：10 次變 1 次。** 這一格與下面那格反向斷言合起來
	// 擋的就是回退——把引導拿掉（10 次那個形態回來），或「順便把白名單列出來」這種看似
	// 更有幫助、實際上退回 #33 定案的改法。
	for _, want := range []string{"逐一", "告訴使用者"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("訊息 %q 未提到 %q——少了對 LLM 的引導，它會改猜下一個命令名而不是轉向使用者", err, want)
		}
	}

	// **反向：引導不得以洩漏白名單為代價**（#33 定案不得回退）。
	// 連「有幾個」都不說——基數本身就是這個 Workspace 的資訊。
	for _, leak := range []string{"echo", "2 個", "兩個"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("訊息 %q 洩漏了白名單的內容或基數 %q", err, leak)
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
