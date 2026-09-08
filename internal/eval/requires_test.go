package eval_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// TestCheckPreconditions 是前置條件校驗的主要測試面（issue #59 的 requires ＋
// issue #62 的 forbids）。
//
// **它是純函式，這是刻意的。** 校驗的時機在 RunCase 裡——而 run.go 沒有自動化測試
// （憲法 4.4，它會呼叫真實 Provider）。判斷邏輯全部搬到這一側，那邊只剩一行沒有分支
// 的呼叫，與 ticket #50 把判卷切成純函式是同一條理由。
//
// **兩個方向合在同一支函式而不是各自一支**，也是為了這條理由：兩類落差要一次說完，
// 而合併的邏輯若放進 run.go 就落在測不到的那一側。函式因此改名——它不再只檢查
// requires。
//
// 三類前置條件的比對語義刻意不同，各自有格：tools 與 commands 是字面完全相等，
// paths 是**子樹涵蓋**——見「allowed_paths 是 . 時涵蓋一切」那一格。反向的 paths
// 尤其需要子樹涵蓋，見「forbids.paths 被 . 涵蓋」那一格。
func TestCheckPreconditions(t *testing.T) {
	tests := []struct {
		name     string
		req      eval.Requires
		forb     eval.Forbids
		env      eval.Environment
		wantErr  bool
		wantMsgs []string // 錯誤訊息裡都該出現的片段
	}{
		{
			// requires 是選填的（issue #59 定案）。沒宣告的用例照原本的方式跑，
			// 既有用例零遷移——代價是它得不到保護，那是選填本來就換來的。
			name:    "沒有宣告任何前置條件時通過",
			req:     eval.Requires{},
			env:     eval.Environment{ProfileName: "default"},
			wantErr: false,
		},
		{
			name: "tools 被 Profile 涵蓋時通過",
			req:  eval.Requires{Tools: []string{"read_file"}},
			env: eval.Environment{
				ProfileName:  "eval",
				ProfileTools: []string{"read_file", "write_file"},
			},
			wantErr: false,
		},
		{
			// 錯誤要指名**是哪一項、在哪個檔案的哪一段**（issue #59 明訂）。只說
			// 「前置條件不滿足」的話，看的人還得自己去比對 Workspace——而那正是這個
			// 校驗要替人做的事。
			name: "tools 沒被涵蓋時指名 Profile 與該改的檔案",
			req:  eval.Requires{Tools: []string{"read_file"}},
			env: eval.Environment{
				ProfileName:  "eval",
				ProfileTools: []string{"shell"},
			},
			wantErr:  true,
			wantMsgs: []string{"read_file", "eval", "tools"},
		},
		{
			// tools 是**字面完全相等**，與 grade.go 的 tool_called 同一條理由：read 不得
			// 因為 Profile 列了 read_file 就算滿足，否則一個手誤的 requires 會安靜地
			// 一直放行。（突變測試補的一格——原本這條規則沒有測試守著。）
			name: "tools 比對是完全相等，不是前綴",
			req:  eval.Requires{Tools: []string{"read"}},
			env: eval.Environment{
				ProfileName:  "eval",
				ProfileTools: []string{"read_file"},
			},
			wantErr:  true,
			wantMsgs: []string{"read", "eval"},
		},
		{
			name: "paths 字面命中時通過",
			req:  eval.Requires{Paths: []string{"notes"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes"},
			},
			wantErr: false,
		},
		{
			// **這一格是 paths 不能用字面比對的理由。**
			//
			// allowed_paths: ["."] 涵蓋整個 Workspace，notes 當然可讀；字面比對會在這裡
			// 誤報「缺 notes」，把一個完全正常的 Workspace 判成配置錯誤。比對因此走
			// SandboxChecker.CheckFilePath——與 File Tool 執行時**同一段邏輯**，校驗說
			// 可以就是真的可以。
			name: "allowed_paths 是 . 時涵蓋一切",
			req:  eval.Requires{Paths: []string{"notes"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"."},
			},
			wantErr: false,
		},
		{
			// 子樹涵蓋的另一面：notes 放行 notes/sub，但**不放行** notes-secrets。
			// 這條規則來自 CheckFilePath 的「比對是子樹包含，不是字串前綴」。
			name: "paths 的子目錄被父目錄涵蓋",
			req:  eval.Requires{Paths: []string{"notes/sub"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes"},
			},
			wantErr: false,
		},
		{
			name: "paths 前綴相同但不同子樹時不通過",
			req:  eval.Requires{Paths: []string{"notes-secrets"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes"},
			},
			wantErr:  true,
			wantMsgs: []string{"notes-secrets", "file.allowed_paths", "config.yaml"},
		},
		{
			name: "paths 沒被涵蓋時指名該改的段",
			req:  eval.Requires{Paths: []string{"notes"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{},
			},
			wantErr:  true,
			wantMsgs: []string{"notes", "file.allowed_paths", "config.yaml"},
		},
		{
			name: "commands 被涵蓋時通過",
			req:  eval.Requires{Commands: []string{"wc"}},
			env: eval.Environment{
				ProfileName:     "eval",
				AllowedCommands: []string{"wc", "ls"},
			},
			wantErr: false,
		},
		{
			// commands 是**字面完全相等**，不是子樹也不是前綴：config.yaml 的註解明訂
			// 「比對的是程式名的字面完全匹配」，w 不得因為 wc 在白名單就算通過。
			name: "commands 比對是完全相等，不是前綴",
			req:  eval.Requires{Commands: []string{"w"}},
			env: eval.Environment{
				ProfileName:     "eval",
				AllowedCommands: []string{"wc"},
			},
			wantErr:  true,
			wantMsgs: []string{"shell.allowed_commands", "config.yaml"},
		},
		{
			// 比對是**字面**完全相等，對空白敏感：白名單寫 "wc " 不等於宣告 "wc"。
			// EffectiveAllowedCommands 只剔除條目、不 trim 保留下來的那些。
			name: "commands 比對對空白敏感，wc 不等於 wc 加空格",
			req:  eval.Requires{Commands: []string{"wc"}},
			env: eval.Environment{
				ProfileName:     "eval",
				AllowedCommands: []string{"   ", "wc "},
			},
			wantErr:  true,
			wantMsgs: []string{"wc", "shell.allowed_commands"},
		},
		{
			// **一次說出全部落差，不在第一條就停**（與 Grade 同一條理由）。這個校驗
			// 雖然發生在花錢之前，但修一項、再跑一次、才看到第二項一樣消耗人。
			name: "多項不滿足時全部列出",
			req: eval.Requires{
				Tools:    []string{"read_file"},
				Paths:    []string{"notes"},
				Commands: []string{"wc"},
			},
			env:      eval.Environment{ProfileName: "eval"},
			wantErr:  true,
			wantMsgs: []string{"read_file", "notes", "wc"},
		},
		{
			// 空白條目在 EffectiveAllowedPaths 那一層就被收斂掉了（「寫了一條等於沒寫
			// 的設定」）。校驗必須看到同一份收斂結果，否則它會說可以、實際被拒。
			name: "allowed_paths 的空白條目不算涵蓋",
			req:  eval.Requires{Paths: []string{"notes"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"   "},
			},
			wantErr:  true,
			wantMsgs: []string{"notes"},
		},

		// ── forbids：反向前置條件（issue #62）──
		//
		// 它治的是與 requires 相同形態、方向相反的失敗：測「被拒之後怎麼收斂」的用例，
		// 在白名單被**放寬**時會安靜地量錯東西——Tool 成功了、回應自然不提白名單，
		// 一切看起來都正常，只是這條用例量的東西已經不存在了。
		{
			name:    "沒有宣告任何 forbids 時通過",
			forb:    eval.Forbids{},
			env:     eval.Environment{ProfileName: "eval", AllowedCommands: []string{"df"}},
			wantErr: false,
		},
		{
			name: "commands 不在白名單時通過（這正是用例要的前提）",
			forb: eval.Forbids{Commands: []string{"df"}},
			env: eval.Environment{
				ProfileName:     "eval",
				AllowedCommands: []string{"wc", "ls"},
			},
			wantErr: false,
		},
		{
			// 錯誤要指名**是哪一項、在哪個檔案的哪一段、以及該往哪個方向改**。
			// 反向這一側的修法是「移除」而不是「加進」，訊息不能沿用正向那句。
			name: "commands 出現在白名單時指名該從哪裡移除",
			forb: eval.Forbids{Commands: []string{"df"}},
			env: eval.Environment{
				ProfileName:     "eval",
				AllowedCommands: []string{"wc", "df"},
			},
			wantErr:  true,
			wantMsgs: []string{"df", "shell.allowed_commands", "移除"},
		},
		{
			name: "paths 不在白名單涵蓋範圍時通過",
			forb: eval.Forbids{Paths: []string{"config.yaml"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes"},
			},
			wantErr: false,
		},
		{
			name: "paths 字面出現在白名單時不通過",
			forb: eval.Forbids{Paths: []string{"config.yaml"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes", "config.yaml"},
			},
			wantErr:  true,
			wantMsgs: []string{"config.yaml", "file.allowed_paths", "移除"},
		},
		{
			// **這一格是 forbids.paths 非走子樹涵蓋不可的理由。**
			//
			// 白名單寫 `["."]` 時 config.yaml 是被放行的，但字面比對看不出來——
			// 清單裡沒有 "config.yaml" 這個字串。字面比對會判成「不在白名單，前提
			// 成立」，而那條用例接著會量到一個**成功讀到檔案**的執行，判卷全紅卻不
			// 指向原因。這正是 issue #62 要擋的形態。
			name: "paths 被 allowed_paths 的 . 涵蓋時不通過（字面比對看不出來）",
			forb: eval.Forbids{Paths: []string{"config.yaml"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"."},
			},
			wantErr:  true,
			wantMsgs: []string{"config.yaml", "file.allowed_paths"},
		},
		{
			// 上層目錄被放行也算涵蓋，與 requires.paths 的子樹語義同源。
			name: "paths 的上層目錄在白名單時不通過",
			forb: eval.Forbids{Paths: []string{"notes/todo.md"}},
			env: eval.Environment{
				ProfileName:  "eval",
				AllowedPaths: []string{"notes"},
			},
			wantErr:  true,
			wantMsgs: []string{"notes/todo.md"},
		},
		{
			// **兩個方向的落差要一次說完**（與「每一項都檢查」同一條理由）。分兩支
			// 函式各自回錯誤的話，使用者修完 requires、再跑一次、才看到 forbids ——
			// 而這個校驗存在的目的正是省下那一次。
			name: "requires 與 forbids 同時不滿足時一次列出兩者",
			req:  eval.Requires{Tools: []string{"shell"}},
			forb: eval.Forbids{Commands: []string{"df"}},
			env: eval.Environment{
				ProfileName:     "eval",
				ProfileTools:    []string{"read_file"},
				AllowedCommands: []string{"df"},
			},
			wantErr:  true,
			wantMsgs: []string{"shell", "df", "requires.tools", "forbids.commands"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := eval.CheckPreconditions(tt.req, tt.forb, tt.env)
			if tt.wantErr && err == nil {
				t.Fatalf("期望錯誤含 %v，卻通過了", tt.wantMsgs)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("期望通過，得到錯誤: %v", err)
				}
				return
			}
			for _, want := range tt.wantMsgs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("錯誤訊息 = %v，期望含 %q", err, want)
				}
			}
		})
	}
}

// TestCheckPreconditionsImpossibleDeclarationPointsAtTheCase 守的是診斷的**方向**。
//
// 一個本身就不合法的 requires 條目——空值、絕對路徑、穿越出 Workspace 的路徑、含路徑
// 分隔符的程式名——**永遠不會被任何 Workspace 配置滿足**。診斷若說「請加進
// file.allowed_paths」，使用者照做也修不好，下一次執行看到同一則訊息，那是一個無法
// 收斂的迴圈（Codex 審查抓到）。
//
// **每一格的 env 都給到最寬鬆**：allowed_paths 是 `["."]`（涵蓋整個 Workspace）、
// allowed_commands 直接含著宣告的那一項。連這樣都不通過，就證明問題不在配置——這是
// 「無法收斂」最直接的證據，也是這支測試選擇這種 env 的理由。
func TestCheckPreconditionsImpossibleDeclarationPointsAtTheCase(t *testing.T) {
	tests := []struct {
		name string
		req  eval.Requires
		forb eval.Forbids
		env  eval.Environment
	}{
		{
			// **這一格的 env 是最刺眼的一種**：Profile 的 tools 剛好也含著同一個空白值，
			// 所以字面比對會命中、校驗會直接放行——一個根本不是 Tool 名的宣告被當成
			// 滿足了（Codex 審查抓到）。空白 Tool 名不論 Profile 怎麼寫都不該算數。
			name: "tools 是空白，且 Profile 剛好也含同樣的空白值",
			req:  eval.Requires{Tools: []string{"   "}},
			env:  eval.Environment{ProfileName: "eval", ProfileTools: []string{"   "}},
		},
		{
			name: "tools 是空白，Profile 不含它",
			req:  eval.Requires{Tools: []string{"  "}},
			env:  eval.Environment{ProfileName: "eval", ProfileTools: []string{"read_file"}},
		},
		{
			name: "paths 是絕對路徑",
			req:  eval.Requires{Paths: []string{"/etc/passwd"}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "paths 穿越出 Workspace",
			req:  eval.Requires{Paths: []string{"../outside"}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "paths 是空字串",
			req:  eval.Requires{Paths: []string{""}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "commands 含路徑分隔符",
			req:  eval.Requires{Commands: []string{"/usr/bin/wc"}},
			env:  eval.Environment{ProfileName: "eval", AllowedCommands: []string{"/usr/bin/wc"}},
		},
		{
			name: "commands 是純空白",
			req:  eval.Requires{Commands: []string{"   "}},
			env:  eval.Environment{ProfileName: "eval", AllowedCommands: []string{"   "}},
		},

		// ── forbids 這一側：不合法條目的後果是**恆滿足**，方向與 requires 相反 ──
		//
		// requires 的不合法條目永遠不被滿足，失敗是大聲的；forbids 的不合法條目永遠
		// 滿足（絕對路徑本來就不會被白名單放行），於是一條什麼都沒檢查的宣告會安靜
		// 地一直綠燈。**這一側更需要在解析層擋下**，理由比正向那側強。
		//
		// 每一格的 env 一樣給到最寬鬆：allowed_paths 是 `["."]`、allowed_commands
		// 直接含著宣告的那一項——連這樣都要判成宣告有問題，才證明它與配置無關。
		{
			name: "forbids.paths 是絕對路徑",
			forb: eval.Forbids{Paths: []string{"/etc/passwd"}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "forbids.paths 穿越出 Workspace",
			forb: eval.Forbids{Paths: []string{"../outside"}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "forbids.paths 是空字串",
			forb: eval.Forbids{Paths: []string{""}},
			env:  eval.Environment{ProfileName: "eval", AllowedPaths: []string{"."}},
		},
		{
			name: "forbids.commands 含路徑分隔符",
			forb: eval.Forbids{Commands: []string{"/usr/bin/df"}},
			env:  eval.Environment{ProfileName: "eval", AllowedCommands: []string{"/usr/bin/df"}},
		},
		{
			name: "forbids.commands 是純空白",
			forb: eval.Forbids{Commands: []string{"   "}},
			env:  eval.Environment{ProfileName: "eval", AllowedCommands: []string{"   "}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := eval.CheckPreconditions(tt.req, tt.forb, tt.env)
			if err == nil {
				t.Fatal("期望不通過，卻通過了")
			}
			msg := err.Error()
			// **正向判準：訊息必須說出這是宣告本身的問題。**
			//
			// 負向判準（「不得含 X」）在這裡連漏兩次：第一版查「請加進」，而 paths 那
			// 三則寫的是「請把它或它的上層目錄加進」；第二版改查 config.yaml，而 tools
			// 那則指向的是 profiles/<name>.yaml。每一種措辭都要另外列一次，漏一種就假
			// 通過一格。
			//
			// 正向的判準沒有這個問題：一則說得出「不論 Workspace 怎麼配置都不會滿足」
			// 的訊息，不可能同時叫使用者去改配置。
			if !strings.Contains(msg, "不論 Workspace 怎麼配置") {
				t.Errorf("診斷沒有說出這是宣告本身的問題（照著它改配置永遠修不好）: %s", msg)
			}
			// 仍然保留這一道：指向 config.yaml 是最常見的錯誤方向。
			if strings.Contains(msg, "config.yaml") {
				t.Errorf("診斷指向 Workspace 配置檔，但那個修法永遠不會奏效: %s", msg)
			}
			if !strings.Contains(msg, "用例") {
				t.Errorf("診斷沒有指向用例宣告: %s", msg)
			}
		})
	}
}

// TestRequiresUnmarshalNullNode 守住 UnmarshalYAML 對 null 節點的處理：等同沒寫。
//
// **這條路徑經 ParseCase 目前走不到**——實測 yaml.v3 對空的 `requires:` 直接給零值、
// 不呼叫 UnmarshalYAML，所以突變測試把那一段拿掉時沒有任何用例層的測試轉紅。
//
// 保留那一段並用這支直接守住它，是因為「走不到」取決於**第三方庫的行為**而不是本程式
// 的結構：UnmarshalYAML 是匯出的方法，換 yaml 版本或換解析路徑都可能開始傳 null 進來，
// 而那時沒有這一段會把一個合法的空宣告判成「必須是一組欄位」。
//
// 這與「移除沒有測試守得住的分支」不衝突：那條原則針對的是**結構上等價**的程式碼
// （例如 commands 那次多餘的收斂），這裡的分支有明確且不同的行為，只是觸發它需要繞過
// 目前這條解析路徑。
func TestRequiresUnmarshalNullNode(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("null"), &node); err != nil {
		t.Fatalf("準備 null 節點: %v", err)
	}
	// 文件節點的內容才是那個 null 純量。
	if len(node.Content) != 1 {
		t.Fatalf("期望一個內容節點，得到 %d", len(node.Content))
	}

	var req eval.Requires
	if err := req.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("null 節點應等同沒寫，卻回錯誤: %v", err)
	}
	if !req.IsZero() {
		t.Errorf("null 節點應解出空宣告，得到 %+v", req)
	}

	// **receiver 已有值時，null 節點要把它清空**（Codex 審查抓到）：只 return nil 的話
	// 舊條件會殘留，而 null 的語義是「沒有前置條件」。解碼進一個重用的結構體時，殘留
	// 的條件會讓校驗檢查一份根本不屬於這份用例的宣告。
	stale := eval.Requires{
		Tools:    []string{"read_file"},
		Paths:    []string{"notes"},
		Commands: []string{"wc"},
	}
	if err := stale.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("null 節點應等同沒寫，卻回錯誤: %v", err)
	}
	if !stale.IsZero() {
		t.Errorf("null 節點應清空 receiver，卻殘留 %+v", stale)
	}
}

// TestForbidsUnmarshalNullNode 守住 Forbids 對 null 節點的處理：等同沒寫。
//
// 理由與 TestRequiresUnmarshalNullNode 完全相同（那支的說明是這條規則的完整推導），
// 這裡不重複。**但後果在這一側更重**：null 若沒有清空 receiver，殘留的反向條件會
// 讓校驗去檢查一份不屬於這份用例的宣告——而 forbids 的滿足態是「不在白名單」，
// 一份殘留的宣告很容易剛好滿足，於是靜默放行。
func TestForbidsUnmarshalNullNode(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("null"), &node); err != nil {
		t.Fatalf("準備 null 節點: %v", err)
	}
	if len(node.Content) != 1 {
		t.Fatalf("期望一個內容節點，得到 %d", len(node.Content))
	}

	var forb eval.Forbids
	if err := forb.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("null 節點應等同沒寫，卻回錯誤: %v", err)
	}
	if !forb.IsZero() {
		t.Errorf("null 節點應解出空宣告，得到 %+v", forb)
	}

	stale := eval.Forbids{Paths: []string{"config.yaml"}, Commands: []string{"df"}}
	if err := stale.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("null 節點應等同沒寫，卻回錯誤: %v", err)
	}
	if !stale.IsZero() {
		t.Errorf("null 節點應清空 receiver，卻殘留 %+v", stale)
	}
}
