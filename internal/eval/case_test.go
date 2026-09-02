package eval_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// TestParseCase 涵蓋用例宣告解析的合法與不合法形狀。
//
// 斷言對象是**解析出來的用例宣告**與**錯誤訊息**，不是解析器內部走了哪些分支：
// 換一套 YAML 函式庫、把校驗拆成幾個函式，這張表都該保持綠色。
func TestParseCase(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // 空字串代表期望成功
		check   func(t *testing.T, got eval.Case)
	}{
		{
			name: "完整的合法宣告",
			yaml: `
name: 讀檔案並回答
profile: default
setup:
  files:
    notes/todo.md: |
      買牛奶
task: 讀 notes/todo.md，告訴我裡面寫什麼
assert:
  - kind: reply_contains
    value: 牛奶
  - kind: tool_called
    value: read_file
`,
			check: func(t *testing.T, got eval.Case) {
				if got.Name != "讀檔案並回答" {
					t.Errorf("Name = %q，期望 %q", got.Name, "讀檔案並回答")
				}
				if got.Profile != "default" {
					t.Errorf("Profile = %q，期望 %q", got.Profile, "default")
				}
				if got.Task != "讀 notes/todo.md，告訴我裡面寫什麼" {
					t.Errorf("Task = %q", got.Task)
				}
				if got.Setup.Files["notes/todo.md"] != "買牛奶\n" {
					t.Errorf("setup.files[notes/todo.md] = %q，期望 %q",
						got.Setup.Files["notes/todo.md"], "買牛奶\n")
				}
				if len(got.Assert) != 2 {
					t.Fatalf("斷言數 = %d，期望 2", len(got.Assert))
				}
				if got.Assert[0].Kind != eval.AssertReplyContains || got.Assert[0].Value != "牛奶" {
					t.Errorf("assert[0] = %+v", got.Assert[0])
				}
				if got.Assert[1].Kind != eval.AssertToolCalled || got.Assert[1].Value != "read_file" {
					t.Errorf("assert[1] = %+v", got.Assert[1])
				}
			},
		},
		{
			name: "沒有 setup 段也合法",
			yaml: `
name: 純對話
profile: default
task: 說一句你好
assert:
  - kind: reply_contains
    value: 你好
`,
			check: func(t *testing.T, got eval.Case) {
				if len(got.Setup.Files) != 0 {
					t.Errorf("setup.files = %v，期望空", got.Setup.Files)
				}
			},
		},
		{
			// 哨兵用一個**永遠不會成真**的種類名（ticket #53）：這格原本寫
			// `max_iterations`，而本票正好把它落地了——繼續用它會讓這條斷言在種類被
			// 支援之後安靜地失去意義，測試照樣綠，守的東西卻沒了。
			name: "未知的斷言種類要明確報錯",
			yaml: `
name: 不存在的斷言
profile: default
task: 隨便
assert:
  - kind: no_such_kind
    value: "3"
`,
			wantErr: "no_such_kind",
		},
		{
			// 四種斷言種類都要解析得出來（ticket #53 驗收條件）。指標型的值在 YAML 裡
			// 不加引號寫成整數也要收得下——使用者不會想到 `value: 3` 與 `value: "3"`
			// 有什麼差別，而多數人會寫前者。
			name: "四種斷言種類都收得下",
			yaml: `
name: 四種斷言
profile: default
task: 隨便
assert:
  - kind: reply_contains
    value: 牛奶
  - kind: tool_called
    value: read_file
  - kind: max_iterations
    value: 3
  - kind: max_tool_failures
    value: 0
`,
			check: func(t *testing.T, got eval.Case) {
				want := []eval.Assertion{
					{Kind: eval.AssertReplyContains, Value: "牛奶"},
					{Kind: eval.AssertToolCalled, Value: "read_file"},
					{Kind: eval.AssertMaxIterations, Value: "3"},
					{Kind: eval.AssertMaxToolFailures, Value: "0"},
				}
				if len(got.Assert) != len(want) {
					t.Fatalf("斷言數 = %d，期望 %d", len(got.Assert), len(want))
				}
				for i, w := range want {
					if got.Assert[i] != w {
						t.Errorf("assert[%d] = %+v，期望 %+v", i, got.Assert[i], w)
					}
				}
			},
		},
		{
			// 指標型斷言的值必須是整數：`value: 三次` 解析得過的話，這條斷言要嘛在
			// 判卷時安靜失敗、要嘛安靜通過，兩種都是在**花完錢之後**才發現宣告寫錯。
			// 校驗一律在送出任何請求之前（與其餘欄位同一條理由）。
			name: "max_iterations 的值不是數字要被拒",
			yaml: `
name: 值不是數字
profile: default
task: 隨便
assert:
  - kind: max_iterations
    value: 三次
`,
			wantErr: "整數",
		},
		{
			name: "max_tool_failures 的值是小數要被拒",
			yaml: `
name: 值是小數
profile: default
task: 隨便
assert:
  - kind: max_tool_failures
    value: 1.5
`,
			wantErr: "整數",
		},
		{
			// 負的上限沒有任何實際值：Tool 失敗數與 iteration 數都不可能小於 0，這種
			// 宣告必然永遠不通過。與「值是純空白」相反——那種永遠通過，這種永遠不通過，
			// 但兩者都代表使用者寫錯了，都該在解析時就說出來。
			name: "上限是負數要被拒",
			yaml: `
name: 負的上限
profile: default
task: 隨便
assert:
  - kind: max_iterations
    value: -1
`,
			wantErr: "不得為負",
		},
		{
			name: "缺 name",
			yaml: `
profile: default
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "name",
		},
		{
			name: "缺 profile",
			yaml: `
name: 沒有 profile
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "profile",
		},
		{
			name: "缺 task",
			yaml: `
name: 沒有 task
profile: default
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "task",
		},
		{
			name: "一條斷言都沒有",
			yaml: `
name: 沒有斷言
profile: default
task: 隨便
`,
			wantErr: "assert",
		},
		{
			name: "斷言缺 value",
			yaml: `
name: 斷言沒有值
profile: default
task: 隨便
assert:
  - kind: reply_contains
`,
			wantErr: "value",
		},
		{
			// value: " " 配 reply_contains 幾乎對任何回應都成立——一條什麼都沒檢查的
			// 斷言會一直綠燈。判空一律用 TrimSpace，與 name／profile／task／setup.files
			// 的路徑同一條規則（那四處本來就是這樣寫的，這裡原本漏掉了）。
			name: "reply_contains 的 value 是純空白要被拒",
			yaml: `
name: 空白斷言
profile: default
task: 隨便
assert:
  - kind: reply_contains
    value: "   "
`,
			wantErr: "value",
		},
		{
			name: "tool_called 的 value 是純空白要被拒",
			yaml: `
name: 空白 Tool 名
profile: default
task: 隨便
assert:
  - kind: tool_called
    value: "\t"
`,
			wantErr: "value",
		},
		{
			// **判空用 TrimSpace，比對用原值**，這兩件事必須分開。使用者寫的前後空白
			// 可能是刻意的（回應裡「答案： 42」的那個空格），解析時順手 trim 掉會讓
			// 斷言比的東西與他寫的不是同一個，而且從輸出完全看不出來。
			name: "value 的前後空白要原樣保留，不得被 trim",
			yaml: `
name: 空白有語義
profile: default
task: 隨便
assert:
  - kind: reply_contains
    value: " 牛奶 "
`,
			check: func(t *testing.T, got eval.Case) {
				if got.Assert[0].Value != " 牛奶 " {
					t.Errorf("value = %q，期望原樣保留 %q", got.Assert[0].Value, " 牛奶 ")
				}
			},
		},
		{
			name: "斷言缺 kind",
			yaml: `
name: 斷言沒有種類
profile: default
task: 隨便
assert:
  - value: x
`,
			wantErr: "kind",
		},
		{
			name: "setup.files 用 ../ 逃出 Workspace 要被拒",
			yaml: `
name: 逃逸
profile: default
setup:
  files:
    ../outside.md: x
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "../outside.md",
		},
		{
			name: "setup.files 標準化後才逃出 Workspace 也要被拒",
			yaml: `
name: 繞路逃逸
profile: default
setup:
  files:
    notes/../../outside.md: x
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "notes/../../outside.md",
		},
		{
			name: "setup.files 用絕對路徑要被拒",
			yaml: `
name: 絕對路徑
profile: default
setup:
  files:
    /etc/passwd: x
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "/etc/passwd",
		},
		{
			name: "setup.files 的路徑不得為空",
			yaml: `
name: 空路徑
profile: default
setup:
  files:
    "": x
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "路徑",
		},
		{
			name: "setup.files 標準化後留在 Workspace 內就放行",
			yaml: `
name: 繞路但沒逃出去
profile: default
setup:
  files:
    notes/../todo.md: x
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			check: func(t *testing.T, got eval.Case) {
				if len(got.Setup.Files) != 1 {
					t.Fatalf("setup.files = %v，期望一條", got.Setup.Files)
				}
			},
		},
		{
			// 兩條路徑指向同一個檔案，兩條都寫得出去，最後留下哪一份內容取決於 map
			// 的走訪順序——那在 Go 裡是隨機的。一個結果不確定的評測沒有價值，而確定性
			// 正是這份 schema 選宣告式而非 shell 腳本的理由。
			name: "兩條 setup.files 指向同一個檔案要被拒",
			yaml: `
name: 同一個檔案兩種寫法
profile: default
setup:
  files:
    todo.md: 第一份
    ./todo.md: 第二份
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "同一個檔案",
		},
		{
			// profile 會被拼進 `<Workspace>/profiles/<name>.yaml`，所以它跟 setup.files
			// 一樣是一個進路徑的欄位。放行的話評測會載入一份**沒有被複製進乾淨
			// Workspace** 的 Profile——那份可能是上一次執行留下的，也可能根本在
			// Workspace 之外，兩者都讓「每個用例一份乾淨的 Workspace」失效。
			name: "profile 用 ../ 逃出 profiles/ 要被拒",
			yaml: `
name: Profile 逃逸
profile: ../../outside
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "../../outside",
		},
		{
			name: "profile 帶路徑分隔符要被拒",
			yaml: `
name: Profile 帶子目錄
profile: sub/other
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "sub/other",
		},
		{
			name: "profile 是 .. 要被拒",
			yaml: `
name: Profile 是點點
profile: ".."
task: 隨便
assert:
  - kind: reply_contains
    value: x
`,
			wantErr: "..",
		},
		{
			name:    "不是合法的 YAML",
			yaml:    "name: [未閉合",
			wantErr: "解析",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval.ParseCase([]byte(tt.yaml), "用例.yaml")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，卻成功解析出 %+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("錯誤訊息 = %v，期望含 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望解析成功，得到錯誤: %v", err)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// TestParseCaseErrorNamesSourceFile 釘住一條實際使用時最有感的性質：解析失敗的訊息
// 要說得出是哪一份檔案壞了。評測用例目錄裡放十份 YAML 時，一句沒有檔名的「缺 task」
// 等於要人一份一份翻。
func TestParseCaseErrorNamesSourceFile(t *testing.T) {
	_, err := eval.ParseCase([]byte("profile: default\n"), "evals/壞掉的.yaml")
	if err == nil {
		t.Fatal("期望解析失敗")
	}
	if !strings.Contains(err.Error(), "evals/壞掉的.yaml") {
		t.Errorf("錯誤訊息 = %v，期望含來源檔名", err)
	}
}

// TestCaseProfilePath 釘住「用例的 Profile 一定落在這份 Workspace 的 profiles/ 之內」。
//
// 這是 ParseCase 那道校驗的**第二道防線**，理由與 PrepareWorkspace 再驗一次路徑相同：
// RunCase 是匯出的函式，一個繞過解析直接建 Case 值的呼叫端（下一張票、或日後的 Web
// 觸發）不該有辦法讓評測去讀 Workspace 外面的 YAML。
func TestCaseProfilePath(t *testing.T) {
	const ws = "/tmp/案例/.oryxos"
	tests := []struct {
		name    string
		profile string
		want    string
		wantErr bool
	}{
		{name: "一般的 Profile 名", profile: "default", want: ws + "/profiles/default.yaml"},
		{name: "帶連字號的名字", profile: "code-reviewer", want: ws + "/profiles/code-reviewer.yaml"},
		{name: "往上跳出 Workspace", profile: "../../outside", wantErr: true},
		{name: "帶子目錄", profile: "sub/other", wantErr: true},
		{name: "當前目錄", profile: ".", wantErr: true},
		{name: "上層目錄", profile: "..", wantErr: true},
		{name: "空字串", profile: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval.Case{Profile: tt.profile}.ProfilePath(ws)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望拒絕 profile %q，卻得到路徑 %q", tt.profile, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProfilePath(%q): %v", tt.profile, err)
			}
			if got != tt.want {
				t.Errorf("ProfilePath(%q) = %q，期望 %q", tt.profile, got, tt.want)
			}
		})
	}
}
