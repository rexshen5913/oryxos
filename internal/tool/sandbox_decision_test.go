package tool_test

import (
	"errors"
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// sandboxProbe 是一次校驗呼叫：怎麼跑，以及期望得到哪一種決策。
//
// 三個校驗方法的回傳形狀不完全相同（CheckFilePath 多回一個標準化路徑），用一個
// closure 把那點差異吸收掉，下面幾張表就能對三者用同一組斷言——「決策 ＋ 錯誤」
// 這個一致的形狀正是本票要立起來的東西，測試自己先照著它寫。
type sandboxProbe struct {
	name string
	run  func() (tool.SandboxDecision, error)
	want tool.SandboxDecision
}

// sandboxProbes 涵蓋三個校驗方法的放行與拒絕兩態。
//
// 格子刻意取既有 sandbox 矩陣的代表值而不是重抄整份：完整的白名單語義由
// TestSandboxCheckerCheckHTTPURL、TestSandboxCheckerCheckFilePath 與
// TestSandboxCheckerCheckShellCommand 覆蓋，這裡要證的是**決策這一維**跟得上它們。
func sandboxProbes() []sandboxProbe {
	httpChecker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedDomains: []string{"api.example.com"}})
	fileChecker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: []string{"notes"}})
	shellChecker := tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"echo"}})
	empty := tool.NewSandboxChecker(tool.SandboxConfig{})

	checkURL := func(c *tool.SandboxChecker, url string) func() (tool.SandboxDecision, error) {
		return func() (tool.SandboxDecision, error) { return c.CheckHTTPURL(url) }
	}
	checkPath := func(c *tool.SandboxChecker, path string) func() (tool.SandboxDecision, error) {
		return func() (tool.SandboxDecision, error) {
			decision, _, err := c.CheckFilePath(path)
			return decision, err
		}
	}
	checkCommand := func(c *tool.SandboxChecker, command string) func() (tool.SandboxDecision, error) {
		return func() (tool.SandboxDecision, error) { return c.CheckShellCommand(command) }
	}

	return []sandboxProbe{
		{name: "HTTP 白名單內放行", run: checkURL(httpChecker, "https://api.example.com/x"), want: tool.SandboxAllow},
		{name: "HTTP host 不在白名單拒絕", run: checkURL(httpChecker, "https://evil.com/x"), want: tool.SandboxDeny},
		{name: "HTTP 非 http/https 拒絕", run: checkURL(httpChecker, "ftp://api.example.com/x"), want: tool.SandboxDeny},
		{name: "HTTP 無法解析的 URL 拒絕", run: checkURL(httpChecker, "://bad-url"), want: tool.SandboxDeny},
		{name: "HTTP 空白名單拒絕", run: checkURL(empty, "https://api.example.com/x"), want: tool.SandboxDeny},

		{name: "路徑白名單內放行", run: checkPath(fileChecker, "notes/todo.md"), want: tool.SandboxAllow},
		{name: "路徑不在白名單拒絕", run: checkPath(fileChecker, "secrets/api.txt"), want: tool.SandboxDeny},
		{name: "路徑穿越出白名單拒絕", run: checkPath(fileChecker, "notes/../secrets/api.txt"), want: tool.SandboxDeny},
		{name: "絕對路徑拒絕", run: checkPath(fileChecker, "/etc/passwd"), want: tool.SandboxDeny},
		{name: "空路徑拒絕", run: checkPath(fileChecker, ""), want: tool.SandboxDeny},
		{name: "路徑空白名單拒絕", run: checkPath(empty, "notes/todo.md"), want: tool.SandboxDeny},

		{name: "命令白名單內放行", run: checkCommand(shellChecker, "echo"), want: tool.SandboxAllow},
		{name: "命令不在白名單拒絕", run: checkCommand(shellChecker, "rm"), want: tool.SandboxDeny},
		{name: "命令含路徑分隔符拒絕", run: checkCommand(shellChecker, "./echo"), want: tool.SandboxDeny},
		{name: "空命令拒絕", run: checkCommand(shellChecker, ""), want: tool.SandboxDeny},
		{name: "命令空白名單拒絕", run: checkCommand(empty, "echo"), want: tool.SandboxDeny},
	}
}

// TestSandboxDecisionZeroValueIsDeny 釘住 fail closed：決策列舉的**零值是拒絕**。
//
// 這不是型別的內部細節，是呼叫端安全性的前提。一個沒被賦值的 SandboxDecision——結構體
// 零值、未來多一條 return 分支忘了填、測試裡的 var 宣告——必須落在「擋下來」而不是
// 「放行」。反過來寫（零值是放行）的話，每一次漏填都是一個安靜的繞過。
func TestSandboxDecisionZeroValueIsDeny(t *testing.T) {
	var zero tool.SandboxDecision
	if zero != tool.SandboxDeny {
		t.Errorf("SandboxDecision 的零值 = %d, 期望等於 SandboxDeny(%d)", zero, tool.SandboxDeny)
	}
	if zero == tool.SandboxAllow {
		t.Error("SandboxDecision 的零值等於 SandboxAllow：漏填的呼叫點會被放行，這是 fail open")
	}
}

// TestSandboxCheckDecisions 是三個校驗方法在**決策這一維**的行為矩陣：放行與拒絕
// 兩態，形狀沿用既有的 sandbox 表格驅動測試。
//
// 一併釘住「決策 ＋ 錯誤」這一對的契約，因為呼叫端要靠它才敢直接用錯誤訊息回填：
//
//   - 放行時錯誤是 nil——放行不該附帶任何要回填給 LLM 的話。
//   - 非放行時錯誤**一定非 nil**，且可被 errors.Is 認成 SandboxViolation。少了這條，
//     回填點取 err.Error() 會在對話中途對 nil 解參考。
func TestSandboxCheckDecisions(t *testing.T) {
	for _, tt := range sandboxProbes() {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := tt.run()
			if decision != tt.want {
				t.Fatalf("決策 = %d, 期望 %d（err=%v）", decision, tt.want, err)
			}
			if tt.want == tool.SandboxAllow {
				if err != nil {
					t.Errorf("放行卻附了錯誤: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("非放行卻沒附錯誤：回填點會拿不到拒絕理由")
			}
			if !errors.Is(err, tool.ErrSandboxViolation) {
				t.Errorf("拒絕的錯誤 = %v, 期望可被 errors.Is 認成 SandboxViolation", err)
			}
		})
	}
}

// TestSandboxCheckNeverAsks 釘住「第三態本階段不產生」。
//
// SandboxAsk 是為擴展階段的 Tool Policy（issue #39）與人工審批（issue #40）在型別上
// 預留的位置，本階段的白名單校驗只有放行與拒絕兩態——這正是「行為與現況完全等價」
// 那句話在型別層的意思。
//
// 它與上面那格分開寫，因為斷言的是不同的東西：上面問「這個輸入的決策對不對」，這裡
// 問「有沒有任何輸入會走到第三態」。哪天真的開始產生 ask，該轉紅的是這一格，而它轉紅
// 就是在提醒「呼叫端要先學會處理它」。
func TestSandboxCheckNeverAsks(t *testing.T) {
	for _, tt := range sandboxProbes() {
		t.Run(tt.name, func(t *testing.T) {
			if decision, err := tt.run(); decision == tool.SandboxAsk {
				t.Errorf("決策 = SandboxAsk，但本階段不該產生第三態（err=%v）", err)
			}
		})
	}
}
