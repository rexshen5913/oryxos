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

// whitelistNoListingMark 是三則白名單拒絕訊息共同要帶的那句話。
//
// 它面對的是一個很難察覺的推論：**資訊的缺席被讀成了資訊**。訊息說「X 不在白名單」
// 而完全沒提白名單裡有什麼，模型就把它讀成「白名單是空的」，然後對使用者講出一句
// 聽起來合理的錯誤事實（issue #58 的實測：`file.allowed_paths` 實際是 [notes]，
// 模型卻告訴使用者「未設定任何允許路徑」）。
//
// **但這半句自己擋不住那個推論，它只陳述了「你看不到」。** 真正擋住的是接在它後面
// 那句「所以⋯⋯」——每一則各自不同，見表格的 wantConsequence 欄。兩半要一起釘，
// 否則刪掉下半句測試照樣綠，而缺陷原封不動回來（外部審查抓到本輪第一版的漏洞：
// 突變測試只試了「整段刪掉」，沒試「只留前半句」）。
const whitelistNoListingMark = "不會在這裡列出"

// TestWhitelistDenialMessagesShareTheSameContract 是三則白名單拒絕訊息的**共同契約**
// （issue #58）。
//
// **為什麼要一張橫跨三則的表，而不是各自補一支測試**：這個 bug 的根因不是某一則訊息
// 寫錯了，是三則各寫各的、沒有任何東西在比對它們，於是它們漂成了三種品質——shell 那則
// （issue #36 改過）三項齊備、路徑那則缺反推論句、HTTP 那則連「該往哪加」都沒有。
// 逐則補測試修得掉這一次的症狀，修不掉「下一則又漂掉」的成因。表格驅動讓五項契約在
// 三則上一次成立，日後新增第四種白名單時，加一列就得同時滿足全部五項。
//
// 五項契約：
//
//  1. **指名被拒的那一個**——沒有它，使用者不知道是什麼被擋了
//  2. **說出要往 config.yaml 的哪一段加**——沒有它，使用者知道被擋了卻不知道怎麼放行
//  3. **說出白名單的內容不會在這裡列出**——沒有它，模型會把缺席讀成「白名單是空的」
//  4. **那句話必須接上「所以⋯⋯」**（每列的 wantConsequence，措辭各自不同）
//  5. **不洩漏白名單其餘條目**（issue #33 定案，不得回退）——訊息會落日誌、也會回填
//     給 LLM，把其餘條目倒出來等於交出這個 Workspace 還允許什麼
//
// 第 3、4 項與第 5 項是**同一個張力的兩端**：不揭露內容（5）正是模型無從得知白名單
// 狀態的原因，所以必須明說「看不到不等於沒有」（3），並說出因此該怎麼辦（4）。少了
// 第 3 項，第 5 項會被模型自行「補完」成一個錯誤的結論；少了第 4 項，第 3 項只是一句
// 沒有出口的陳述，模型會自己補一個出口出來。
//
// **第 4 項鎖的是每列一個「最低語意標記」，不是整句措辭。** 三則的下半句各自不同——
// 路徑與 HTTP 擋推論（標記「不代表白名單是空的」），shell 擋行為（標記「不要逐一
// 嘗試」）——契約要求的是那個標記在、且排在 no-listing 那句之後，其餘用字自由。
//
// 這與 TestSandboxShellCommandErrorIsActionable 的分工要說清楚，因為兩邊都會查
// shell 那則：那一支是 issue #36 的成果測試，查的是那則訊息**整組**性質（含「告訴
// 使用者」與不洩漏基數，是真實 API 量出來的，10 次 iteration 變 1 次）；這裡只查
// 「no-listing 之後有沒有接上結論」這一件三則共有的事。同一個字串被兩支測試查到，
// 但它們問的是不同的問題。
//
// 這也是 issue #58「只把『內容不會列出』搬進路徑那則、不搬防猜句」的正確讀法：不搬的
// 是**措辭**，因為路徑那條路上模型本來就會 2 次後轉向使用者，加防猜是修沒壞的東西；
// 但「要有下半句」這個結構是三則共有的，路徑那則的下半句是反推論句，不是防猜句。
func TestWhitelistDenialMessagesShareTheSameContract(t *testing.T) {
	tests := []struct {
		name string
		// deny 觸發這一種白名單的拒絕，回傳那個錯誤。
		deny func(*tool.SandboxChecker) error
		// checker 帶**兩條**白名單條目：一條與被拒對象無關，用來驗第 5 項不洩漏。
		checker *tool.SandboxChecker
		// wantSubject 是被拒的那一個，必須出現在訊息裡。
		wantSubject string
		// wantSetting 是 config.yaml 裡該改的那一段。
		wantSetting string
		// otherEntry 是白名單裡的另一條，絕不可出現。
		otherEntry string
		// wantConsequence 是接在「內容不會列出」後面那句「所以⋯⋯」的關鍵字。
		//
		// **每一則不同，但都必須有**：這半句才是真正起作用的部分。路徑與 HTTP 擋的是
		// 推論（別以為白名單是空的），shell 擋的是行為（別逐一猜命令名）——兩者都在
		// 回答同一個問題：「既然我看不到清單，那我該怎麼辦」。少了它，前半句只是一句
		// 沒有出口的陳述，而模型會自己補一個出口出來。
		wantConsequence string
	}{
		{
			name: "檔案路徑",
			checker: tool.NewSandboxChecker(tool.SandboxConfig{
				AllowedPaths: []string{"notes", "internal/private-notes"}}),
			deny: func(c *tool.SandboxChecker) error {
				_, _, err := c.CheckFilePath("secrets/api.txt")
				return err
			},
			wantSubject:     "secrets/api.txt",
			wantSetting:     "file.allowed_paths",
			otherEntry:      "internal/private-notes",
			wantConsequence: "不代表白名單是空的",
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
			otherEntry:  "internal-deploy-tool",
			// shell 這句由 issue #36 量出來（10 次 iteration 變 1 次），
			// TestSandboxShellCommandErrorIsActionable 另有專屬斷言；這裡收的是
			// 「no-listing 那句話必須有下半句」這個共通性質，不是 #36 的措辭本身。
			wantConsequence: "不要逐一嘗試",
		},
		{
			name: "HTTP 域名",
			checker: tool.NewSandboxChecker(tool.SandboxConfig{
				AllowedDomains: []string{"trusted.example.com", "internal.example.org"}}),
			deny: func(c *tool.SandboxChecker) error {
				_, err := c.CheckHTTPURL("https://blocked.example.net/x")
				return err
			},
			wantSubject:     "blocked.example.net",
			wantSetting:     "http.allowed_domains",
			otherEntry:      "internal.example.org",
			wantConsequence: "不代表白名單是空的",
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
			if !strings.Contains(msg, whitelistNoListingMark) {
				t.Errorf("訊息 %q 未說明白名單的內容不會列出——"+
					"模型會把這個缺席讀成「白名單是空的」，然後對使用者講出一句"+
					"聽起來合理的錯誤事實（issue #58）", msg)
			}
			// **下半句要單獨查。** 只查上面那半句的話，把「所以⋯⋯」刪掉測試照樣綠，
			// 而缺陷原封不動回來——「你看不到清單」本身不構成任何指示，模型會自己
			// 補一個出口。
			//
			// **而且要查它排在後面**，不只是「出現在訊息裡某處」。下半句是個結論子句
			// （「所以⋯⋯」），放到 no-listing 之前就不成話——而 strings.Contains 對
			// 順序一無所知，光用它，這段註解說的「必須接上」就只是一句沒有測試支持的
			// 宣稱（外部審查抓到）。
			atConsequence := strings.Index(msg, tt.wantConsequence)
			if atConsequence < 0 {
				t.Errorf("訊息 %q 說了內容不會列出，卻沒接上 %q——"+
					"一句沒有出口的陳述，模型會自己補一個出口出來", msg, tt.wantConsequence)
			} else if atNoListing := strings.Index(msg, whitelistNoListingMark); atConsequence < atNoListing {
				t.Errorf("訊息 %q 的 %q 出現在 %q **之前**——"+
					"下半句是結論子句，排到前面就不成話",
					msg, tt.wantConsequence, whitelistNoListingMark)
			}
			if strings.Contains(msg, tt.otherEntry) {
				t.Errorf("訊息 %q 洩漏了白名單的其他條目 %q（issue #33 定案不得回退）",
					msg, tt.otherEntry)
			}
		})
	}
}
