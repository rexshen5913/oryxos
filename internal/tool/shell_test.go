// Shell Tool 的行為矩陣。**真的跑程式，不 mock exec**（憲法 4.3）：這條鏈路要驗的
// 東西——「args 裡的 metacharacter 不會被任何人重新解析」「argv[0] 是白名單那個名字」
// 「Dir 固定在 Workspace 根」「Env 只有三個變數」——沒有一個在假的 exec 上驗得出來。
package tool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// shellOutput 是 shell 回填給 LLM 的結果形狀，測試據此解回來斷言。
type shellOutput struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// argv0ProbeName 是 TestShellArgv0IsWhitelistName 用的探針名字。
//
// **這是 mcp_server_test.go 那個同名常數的副本**：TestMain 每個測試二進制只能有一個，
// 而它住在 `package tool`（內部測試），這裡是 `package tool_test`，看不到那邊的常數。
// 兩邊對不上時這一格會直接失敗，不會假綠。
const argv0ProbeName = "oryxos-argv0-probe"

// probeStderrFloodArg 同樣是 mcp_server_test.go 那邊的副本（見上一段的理由）：
// `<probe> stderr-flood <n>` 會往 **stderr** 灌 n 個位元組。
const probeStderrFloodArg = "stderr-flood"

// maxShellOutput 是 stdout／stderr 各自的上限，以字面值釘住 spec #4 定案的對外契約。
const maxShellOutput = 256 << 10

// shellTestTimeout 是這些測試給命令的超時上限。取得比任何一格的預期執行時間都寬，
// 讓「命令被逾時砍掉」不會假冒成別的失敗。
const shellTestTimeout = 20 * time.Second

// testShellRuntime 是 RegisterBuiltins 的 shell 參數在**與 shell 無關**的測試裡要
// 給的值。Timeout 一定要是正數：非正數是接線失誤，RegisterBuiltins 會擋下來。
func testShellRuntime(t *testing.T) tool.ShellRuntime {
	t.Helper()
	return tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: shellTestTimeout}
}

// newShell 以指定命令白名單組出 shell Tool，子進程的工作目錄是 dir。
// PATH 用**真實的**父進程 PATH（過濾後），因為這些測試真的要跑 echo、ls、pwd。
func newShell(t *testing.T, dir string, allowedCommands []string) tool.OryxTool {
	t.Helper()
	return tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: allowedCommands}),
		tool.ShellRuntime{Dir: dir, PathDirs: tool.ParentPathDirs(), Timeout: shellTestTimeout},
	)
}

// decodeShell 把回填內容解成 shellOutput。
func decodeShell(t *testing.T, content string) shellOutput {
	t.Helper()
	var out shellOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("回填結果不是合法 JSON（%q）: %v", content, err)
	}
	return out
}

// shellInputJSON 把 command ＋ args 組成 Tool 的輸入 JSON。
func shellInputJSON(t *testing.T, command string, args ...string) string {
	t.Helper()
	payload := map[string]any{"command": command}
	if args != nil {
		payload["args"] = args
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("組 shell 輸入: %v", err)
	}
	return string(raw)
}

// TestShellRunsRealCommand 是 happy path：白名單內的程式真的被執行，stdout 與
// exit code 都回填得回來。
func TestShellRunsRealCommand(t *testing.T) {
	result := newShell(t, t.TempDir(), []string{"echo"}).
		Execute(context.Background(), shellInputJSON(t, "echo", "hello", "oryxos"))
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	out := decodeShell(t, result.Content)
	if out.Stdout != "hello oryxos\n" {
		t.Errorf("stdout = %q, 期望 %q", out.Stdout, "hello oryxos\n")
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, 期望 0", out.ExitCode)
	}
	if out.StdoutTruncated || out.StderrTruncated {
		t.Errorf("短輸出不該標示截斷: %+v", out)
	}
}

// TestShellRunsWithoutArgs 驗證 args 省略等於無參數（spec 定案 args 是選填）。
func TestShellRunsWithoutArgs(t *testing.T) {
	result := newShell(t, t.TempDir(), []string{"true"}).
		Execute(context.Background(), `{"command":"true"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}
	if out := decodeShell(t, result.Content); out.ExitCode != 0 {
		t.Errorf("exit_code = %d, 期望 0", out.ExitCode)
	}
}

// TestShellNonZeroExitCodeIsNotToolFailure 釘住一條與 HTTP Tool 對非 2xx **同構**的
// 語義（`internal/tool/http.go` 對任何狀態碼都回 OK:true）：命令回非零 exit code
// **不算 Tool 失敗**，exit code ＋ stdout ＋ stderr 照樣回填，由 LLM 決定下一步。
//
// 這不是寬鬆，是必要的：「測試失敗了」正是 Agent 最需要知道的那個事實，把它報成
// Tool 壞掉會讓 ReAct 循環退避重試一件重跑幾次都一樣的事。
func TestShellNonZeroExitCodeIsNotToolFailure(t *testing.T) {
	result := newShell(t, t.TempDir(), []string{"false"}).
		Execute(context.Background(), `{"command":"false"}`)
	if !result.OK {
		t.Fatalf("非零 exit code 不算 Tool 失敗，實際錯誤: %s", result.Error)
	}
	if out := decodeShell(t, result.Content); out.ExitCode != 1 {
		t.Errorf("exit_code = %d, 期望 1", out.ExitCode)
	}
}

// TestShellCapturesStderr 驗證 stderr 分開回填（不與 stdout 合流）——LLM 要據
// 「錯誤訊息說了什麼」決定下一步，混在一起它分不出哪句是輸出、哪句是錯誤。
func TestShellCapturesStderr(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	result := newShell(t, t.TempDir(), []string{"ls"}).
		Execute(context.Background(), shellInputJSON(t, "ls", missing))
	if !result.OK {
		t.Fatalf("非零 exit code 不算 Tool 失敗，實際錯誤: %s", result.Error)
	}

	out := decodeShell(t, result.Content)
	if out.ExitCode == 0 {
		t.Errorf("exit_code = 0, 期望非零: %+v", out)
	}
	if out.Stderr == "" {
		t.Errorf("stderr 是空的, 期望帶 ls 的錯誤訊息: %+v", out)
	}
	if out.Stdout != "" {
		t.Errorf("stdout = %q, 期望空（錯誤訊息不該混進 stdout）", out.Stdout)
	}
}

// TestShellInjectionIsStructurallyImpossible 是本票**最重要的一格**，也是 ADR-0005
// 那個實測反例在測試層的固化。
//
// ADR 記錄的事實是：`bash -c 'echo "result: $(rm -f victim.txt)"'` **真的把檔案刪了**，
// 而首個 token 是 `echo`、逐段校驗全數通過。結構化 exec 之下同一段文字只是普通參數。
//
// **斷言兩件事，缺一不可**：
//
//  1. 呼叫成功、stdout 是那段**字面文字**。
//  2. **`rm` 沒有被執行**——形狀是「若被解析就會留下可觀測痕跡」：工作目錄裡先放一個
//     victim 檔，那段文字若被任何人重新解析就會刪掉它（或用 `>` 生出新檔），然後比對
//     **整棵目錄樹前後一致**。
//
// **只斷言「原樣成為參數」或只比對 stdout 不算通過**：那個較弱的形狀連被推翻的
// `bash -c` 模型都可能矇混過去（`echo` 印出的東西與 `rm` 有沒有跑是兩件事）。
func TestShellInjectionIsStructurallyImpossible(t *testing.T) {
	tests := []struct {
		name string
		// arg 是丟給 echo 的**單一參數**；<victim> 會被換成 victim 檔的絕對路徑。
		arg string
		// wantStdout 是 stdout 應該原樣含有的字面文字（同樣支援 <victim> 佔位）。
		wantStdout string
		// forbidStdout 是 stdout **不得**出現的字串（展開發生時才會出現的東西）。
		forbidStdout string
	}{
		{name: "管線", arg: "hi | rm -rf <victim>", wantStdout: "hi | rm -rf <victim>"},
		{name: "分號", arg: "hi; rm -rf <victim>", wantStdout: "hi; rm -rf <victim>"},
		{name: "AND 串接", arg: "hi && rm -rf <victim>", wantStdout: "hi && rm -rf <victim>"},
		{name: "命令替換", arg: "$(rm -rf <victim>)", wantStdout: "$(rm -rf <victim>)"},
		{name: "反引號", arg: "`rm -rf <victim>`", wantStdout: "`rm -rf <victim>`"},
		{name: "輸出重導向", arg: "hi > redirected.txt", wantStdout: "hi > redirected.txt"},
		{name: "glob 不展開", arg: "*.txt", wantStdout: "*.txt", forbidStdout: "seed.txt"},
		{name: "變數展開不發生", arg: "$HOME", wantStdout: "$HOME", forbidStdout: os.Getenv("HOME")},
		{name: "上層路徑只是字面文字", arg: "../secret", wantStdout: "../secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(dir, "victim.txt")
			if err := os.WriteFile(victim, []byte("這個檔案若消失，代表有第二個解析器"), 0o644); err != nil {
				t.Fatalf("建立 victim 檔: %v", err)
			}
			// glob 那一格需要工作目錄裡真的有 .txt 檔才驗得到「沒有展開」。
			if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("x"), 0o644); err != nil {
				t.Fatalf("建立 seed 檔: %v", err)
			}
			before := treeSnapshot(t, dir)

			arg := strings.ReplaceAll(tt.arg, "<victim>", victim)
			result := newShell(t, dir, []string{"echo"}).
				Execute(context.Background(), shellInputJSON(t, "echo", arg))

			// (1) 呼叫成功，且 stdout 是那段字面文字。
			if !result.OK {
				t.Fatalf("期望成功（那只是一個普通參數），實際錯誤: %s", result.Error)
			}
			out := decodeShell(t, result.Content)
			want := strings.ReplaceAll(tt.wantStdout, "<victim>", victim)
			if !strings.Contains(out.Stdout, want) {
				t.Errorf("stdout = %q, 期望原樣含 %q", out.Stdout, want)
			}
			if tt.forbidStdout != "" && strings.Contains(out.Stdout, tt.forbidStdout) {
				t.Errorf("stdout = %q 含 %q——那代表發生了展開", out.Stdout, tt.forbidStdout)
			}

			// (2) 沒有第二個解析器：整棵目錄樹前後一致。victim 還在（`rm` 沒跑）、
			// 也沒有多出 redirected.txt（`>` 沒生效）。
			if _, err := os.Stat(victim); err != nil {
				t.Fatalf("victim 檔不見了——那段文字被重新解析並執行了: %v", err)
			}
			if after := treeSnapshot(t, dir); !slices.Equal(before, after) {
				t.Errorf("目錄樹變了:\n前 %v\n後 %v——那代表有第二個解析器", before, after)
			}
		})
	}
}

// TestShellArgv0IsWhitelistName 驗證 `argv[0]` 是**白名單裡那個裸名字**，不是解析出的
// 絕對路徑（US 64）。
//
// 為什麼要管：官方文件寫明「**Args[0] is always name**, not the possibly resolved
// Path」——以絕對路徑當 `name` 建構，`argv[0]` 就變成那個絕對路徑。後果有二：「白名單
// 決定 `argv[0]`」在字面上不再成立；busybox／git 這類**依 `argv[0]` 改變行為的
// multicall 程式**會看到不同的名字。
//
// 手法是把測試二進制自己做成一條符號連結當作待執行的程式：它一被啟動就從 `os.Args[0]`
// 認出自己在 probe 模式（見 TestMain），把 `argv[0]` 原樣印出來。**用 argv[0] 當訊號
// 而不是環境變數**是刻意的——`Env` 是白名單式的，測試自訂的變數傳不進去。
//
// 這一格同時證明了另一半：程式**真的被執行了**，代表 `Cmd.Path` 是我們解析出的絕對
// 路徑（裸名字不可能被 execve 找到，因為我們根本不呼叫 LookPath）。
func TestShellArgv0IsWhitelistName(t *testing.T) {
	binDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("此環境取不到測試二進制路徑: %v", err)
	}
	probe := filepath.Join(binDir, argv0ProbeName)
	if err := os.Symlink(self, probe); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}

	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{argv0ProbeName}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: []string{binDir}, Timeout: shellTestTimeout},
	)
	result := shell.Execute(context.Background(), shellInputJSON(t, argv0ProbeName))
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	got := strings.TrimSpace(decodeShell(t, result.Content).Stdout)
	if got != argv0ProbeName {
		t.Errorf("子進程看到的 argv[0] = %q, 期望白名單裡那個裸名字 %q（不是解析出的絕對路徑）",
			got, argv0ProbeName)
	}
}

// TestShellDirIsWorkspaceRoot 驗證子進程的工作目錄**固定在 Workspace 根**（US 21）。
//
// 不固定的話 `ls`、`wc -l foo`、`git status` 的可觀測行為會隨使用者從哪個目錄啟動
// oryxos 而變——而 `oryxos chat` 是在 Workspace 的**父目錄**執行的。
func TestShellDirIsWorkspaceRoot(t *testing.T) {
	ws := t.TempDir()
	result := newShell(t, ws, []string{"pwd"}).Execute(context.Background(), `{"command":"pwd"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	got := strings.TrimSpace(decodeShell(t, result.Content).Stdout)
	// macOS 的 t.TempDir() 在 /var/folders/…，而 /var 是指向 /private/var 的連結；
	// pwd 印的是化開後的路徑，所以兩邊都化開再比。
	wantReal, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", ws, err)
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", got, err)
	}
	if gotReal != wantReal {
		t.Errorf("子進程的工作目錄 = %q, 期望 Workspace 根 %q", gotReal, wantReal)
	}
}

// TestShellEnvIsWhitelistedAndPathMatchesResolution 一次驗兩件事，因為它們是同一個
// 決定的兩半：
//
//  1. **Env 白名單式傳遞**（US 22）：只有 PATH／HOME／LANG 進得去，其餘一律不傳。
//     防的是具體的事——Provider 憑證（OPENROUTER_API_KEY 這類）不進子進程，`env`
//     即使被列進白名單也回填不出密鑰。
//  2. **子進程的 PATH 與解析所用的是同一份**（US 55）：兩邊不只字串相同、語義也相同
//     （都與工作目錄無關）。少了這條會出現「環境已收窄，但實際執行的檔案仍由繼承的
//     PATH 決定」的落差。
func TestShellEnvIsWhitelistedAndPathMatchesResolution(t *testing.T) {
	t.Setenv("ORYXOS_TEST_SECRET", "must-not-leak")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LANG", "en_US.UTF-8")

	pathDirs := tool.ParentPathDirs()
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"env"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: pathDirs, Timeout: shellTestTimeout},
	)
	result := shell.Execute(context.Background(), `{"command":"env"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	env := map[string]string{}
	for _, line := range strings.Split(decodeShell(t, result.Content).Stdout, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	if _, leaked := env["ORYXOS_TEST_SECRET"]; leaked {
		t.Errorf("子進程拿到了父進程的自訂環境變數: %v", env)
	}
	for _, key := range []string{"PATH", "HOME", "LANG"} {
		if _, ok := env[key]; !ok {
			t.Errorf("子進程沒拿到 %s: %v", key, env)
		}
	}
	if len(env) != 3 {
		t.Errorf("子進程的環境變數 = %v, 期望只有 PATH／HOME／LANG 三個", env)
	}
	if want := strings.Join(pathDirs, string(os.PathListSeparator)); env["PATH"] != want {
		t.Errorf("子進程的 PATH = %q, 期望與解析所用的同一份 %q", env["PATH"], want)
	}
}

// TestShellPathEnvDropsRelativeAndEmptySegments 是上一格在「PATH 有髒段」時的延伸：
// 相對段與空段既不進解析、也不進子進程的 Env（US 61）。
func TestShellPathEnvDropsRelativeAndEmptySegments(t *testing.T) {
	sep := string(os.PathListSeparator)
	dirty := "./bin" + sep + sep + strings.Join(tool.ParentPathDirs(), sep) + sep + "relative/seg" + sep
	pathDirs := tool.EffectivePathDirs(dirty)

	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"env"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: pathDirs, Timeout: shellTestTimeout},
	)
	result := shell.Execute(context.Background(), `{"command":"env"}`)
	if !result.OK {
		t.Fatalf("期望成功，實際錯誤: %s", result.Error)
	}

	var childPath string
	for _, line := range strings.Split(decodeShell(t, result.Content).Stdout, "\n") {
		if value, ok := strings.CutPrefix(line, "PATH="); ok {
			childPath = value
		}
	}
	for _, dropped := range []string{"./bin", "relative/seg"} {
		if strings.Contains(childPath, dropped) {
			t.Errorf("子進程的 PATH %q 仍含被丟掉的段 %q", childPath, dropped)
		}
	}
	for _, seg := range filepath.SplitList(childPath) {
		if !filepath.IsAbs(seg) {
			t.Errorf("子進程的 PATH 含非絕對段 %q: %q", seg, childPath)
		}
	}
}

// TestShellCommandOnlyInDroppedSegmentIsNotFound 是「**拒絕不是忽略**」的實質斷言
// （US 61）：命令只在被丟掉的那個相對段裡找得到時，結果是明確的**找不到該程式**，
// 而不是悄悄從別處執行。
//
// 斷言兩件事：回的是「找不到」的錯誤，**而且那個程式真的沒被跑起來**（跑起來會留下
// 一個 marker 檔）。少了第二條，一個「其實有跑、只是回了錯誤」的實作也會全綠。
func TestShellCommandOnlyInDroppedSegmentIsNotFound(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("建立 bin/: %v", err)
	}
	marker := filepath.Join(dir, "executed.marker")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "mytool"), []byte(script), 0o755); err != nil {
		t.Fatalf("建立 mytool: %v", err)
	}

	// PATH 只有一個相對段——它指的正是上面那個 bin/（工作目錄就是 dir）。
	pathDirs := tool.EffectivePathDirs("./bin")
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"mytool"}}),
		tool.ShellRuntime{Dir: dir, PathDirs: pathDirs, Timeout: shellTestTimeout},
	)
	result := shell.Execute(context.Background(), `{"command":"mytool"}`)

	if result.OK {
		t.Fatalf("相對段已被丟掉，不該找得到 mytool: %s", result.Content)
	}
	if !strings.Contains(result.Error, "找不到") {
		t.Errorf("錯誤 %q 未說明是「找不到該程式」", result.Error)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("mytool 真的被執行了——相對段應該是拒絕，不是換個方式照用")
	}
}

// TestShellNotFoundDiffersFromNotWhitelisted 驗證兩種失敗**分得出來**：「白名單裡有
// 但機器上沒裝」與「根本不在白名單」。
//
// 這兩件事使用者的下一步完全不同——前者去裝那個程式，後者去改 config.yaml。混成
// 同一句話會讓人在一個沒有 git 的最小容器裡找錯方向（US 37）。
func TestShellNotFoundDiffersFromNotWhitelisted(t *testing.T) {
	const absent = "oryxos-definitely-not-installed"
	shell := newShell(t, t.TempDir(), []string{absent})

	notFound := shell.Execute(context.Background(), shellInputJSON(t, absent))
	if notFound.OK {
		t.Fatalf("這個程式不該存在: %s", notFound.Content)
	}
	if !strings.Contains(notFound.Error, "找不到") {
		t.Errorf("「沒裝」的錯誤 %q 未說明是找不到程式", notFound.Error)
	}
	if strings.Contains(notFound.Error, "SandboxViolation") {
		t.Errorf("「沒裝」被報成白名單違規，兩者要分得出來: %q", notFound.Error)
	}

	notAllowed := shell.Execute(context.Background(), `{"command":"rm"}`)
	if notAllowed.OK {
		t.Fatalf("rm 不在白名單: %s", notAllowed.Content)
	}
	if !strings.Contains(notAllowed.Error, "SandboxViolation") {
		t.Errorf("「不在白名單」的錯誤 %q 未標 SandboxViolation", notAllowed.Error)
	}
}

// TestShellRejectionMatrix 是白名單拒絕在 **Tool 這條路徑**上的斷言。
//
// sandbox_test.go 已經驗過 CheckShellCommand 本身；這裡驗的是它**確實被套用在
// Execute 上**，而且拒絕一律不標 Retryable（重跑一次結果都一樣）。
func TestShellRejectionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		allowed    []string
		input      string
		wantErrSub string
	}{
		{name: "不在白名單", allowed: []string{"echo"}, input: `{"command":"rm"}`, wantErrSub: "SandboxViolation"},
		{name: "空白名單全部拒絕", allowed: nil, input: `{"command":"echo"}`, wantErrSub: "SandboxViolation"},
		{name: "command 為空字串", allowed: []string{"echo"}, input: `{"command":""}`, wantErrSub: "command"},
		{name: "缺 command 參數", allowed: []string{"echo"}, input: `{}`, wantErrSub: "command"},
		{name: "command 是相對路徑", allowed: []string{"echo"}, input: `{"command":"./echo"}`, wantErrSub: "SandboxViolation"},
		{name: "command 是絕對路徑", allowed: []string{"echo"}, input: `{"command":"/bin/echo"}`, wantErrSub: "SandboxViolation"},
		{name: "command 含 metacharacter", allowed: []string{"echo", "rm"}, input: `{"command":"echo;rm"}`, wantErrSub: "SandboxViolation"},
		{name: "輸入非 JSON", allowed: []string{"echo"}, input: `not-json`, wantErrSub: "解析"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newShell(t, t.TempDir(), tt.allowed).Execute(context.Background(), tt.input)
			if result.OK {
				t.Fatalf("期望失敗，實際成功: %s", result.Content)
			}
			if !strings.Contains(result.Error, tt.wantErrSub) {
				t.Errorf("錯誤 %q 未含 %q", result.Error, tt.wantErrSub)
			}
			if result.Retryable {
				t.Errorf("Retryable = true, 期望 false（重跑不會改變結果）: %q", result.Error)
			}
		})
	}
}

// TestShellArgsWithMetacharactersAreAllowed 是拒絕矩陣的**反向那一格**：`args` 裡的
// metacharacter 一律放行，因為它們是字面參數，不是命令文字。
//
// 這一格與「command 含 metacharacter 拒絕」合起來，說明白名單比對的對象是**一個
// 程式名**——而不是「有沒有出現危險字元」。
func TestShellArgsWithMetacharactersAreAllowed(t *testing.T) {
	result := newShell(t, t.TempDir(), []string{"echo"}).
		Execute(context.Background(), shellInputJSON(t, "echo", ";", "&&", "|", "$(id)", ">"))
	if !result.OK {
		t.Fatalf("args 裡的 metacharacter 是字面參數，應放行，實際錯誤: %s", result.Error)
	}
	if got := decodeShell(t, result.Content).Stdout; !strings.Contains(got, "$(id)") {
		t.Errorf("stdout = %q, 期望原樣含 $(id)", got)
	}
}

// TestShellOutputLimitMatrix 是 stdout 上限的**三態**矩陣：超過上限截斷並標示、
// **剛好等於上限不算截斷**、低於上限不算截斷。
//
// 只有「超過」那一格是不夠的：截斷旗標算錯邊界的實作在那裡照樣全綠，而它的實際
// 後果是把一份**完整**的輸出標成 truncated——LLM 會據「我只看到一部分」下結論，
// 而且 text() 只在標了截斷時才退到 rune 邊界，於是完整輸出的尾端還會被無故切掉
// 最多 3 個位元組。上限以字面值 256 KiB 釘住那個對外契約。
func TestShellOutputLimitMatrix(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		wantTruncated bool
	}{
		{name: "超過上限截斷並標示", size: maxShellOutput + 4096, wantTruncated: true},
		{name: "剛好等於上限不算截斷", size: maxShellOutput},
		{name: "低於上限不算截斷", size: maxShellOutput - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			big := filepath.Join(dir, "out.txt")
			if err := os.WriteFile(big, []byte(strings.Repeat("a", tt.size)), 0o644); err != nil {
				t.Fatalf("建立 %d bytes 的檔案: %v", tt.size, err)
			}

			result := newShell(t, dir, []string{"cat"}).Execute(context.Background(), shellInputJSON(t, "cat", big))
			if !result.OK {
				t.Fatalf("截斷不算失敗，實際錯誤: %s", result.Error)
			}

			out := decodeShell(t, result.Content)
			if out.StdoutTruncated != tt.wantTruncated {
				t.Errorf("stdout_truncated = %v, 期望 %v（輸出 %d bytes、上限 %d）",
					out.StdoutTruncated, tt.wantTruncated, tt.size, maxShellOutput)
			}
			if len(out.Stdout) > maxShellOutput {
				t.Errorf("stdout 長度 = %d, 期望不超過上限 %d", len(out.Stdout), maxShellOutput)
			}
			// 沒截斷時內容必須**一個位元組都不少**——這是上面那個邊界錯誤的第二個
			// 後果：誤標截斷會連帶讓尾端被退到 rune 邊界而無故短少。
			if !tt.wantTruncated && len(out.Stdout) != tt.size {
				t.Errorf("stdout 長度 = %d, 期望原樣的 %d bytes", len(out.Stdout), tt.size)
			}
			if out.StderrTruncated {
				t.Errorf("stderr 沒有輸出，不該標示截斷: %+v", out)
			}
		})
	}
}

// TestShellStderrHasItsOwnLimit 釘住「stdout 與 stderr **各自** 256 KiB，不是合計」。
//
// 上一格只驗了 stdout 那一半：一個把兩者共用同一個額度的實作在那裡照樣全綠（stdout
// 用滿、stderr 是空的）。這一格反過來——**stdout 空、stderr 灌爆**——兩格合起來才
// 說得出「各自」。
//
// 分開算是刻意的：一個把診斷訊息全寫進 stderr 的命令，不該因為 stdout 很長就看不到
// 自己的錯誤訊息，而 LLM 的下一步常常只靠 stderr。
func TestShellStderrHasItsOwnLimit(t *testing.T) {
	binDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("此環境取不到測試二進制路徑: %v", err)
	}
	if err := os.Symlink(self, filepath.Join(binDir, argv0ProbeName)); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{argv0ProbeName}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: []string{binDir}, Timeout: shellTestTimeout},
	)

	tests := []struct {
		name          string
		size          int
		wantTruncated bool
	}{
		{name: "超過上限截斷並標示", size: maxShellOutput + 4096, wantTruncated: true},
		{name: "剛好等於上限不算截斷", size: maxShellOutput},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shell.Execute(context.Background(),
				shellInputJSON(t, argv0ProbeName, probeStderrFloodArg, strconv.Itoa(tt.size)))
			if !result.OK {
				t.Fatalf("截斷不算失敗，實際錯誤: %s", result.Error)
			}

			out := decodeShell(t, result.Content)
			if out.StderrTruncated != tt.wantTruncated {
				t.Errorf("stderr_truncated = %v, 期望 %v（輸出 %d bytes、上限 %d）",
					out.StderrTruncated, tt.wantTruncated, tt.size, maxShellOutput)
			}
			if len(out.Stderr) > maxShellOutput {
				t.Errorf("stderr 長度 = %d, 期望不超過上限 %d", len(out.Stderr), maxShellOutput)
			}
			if !tt.wantTruncated && len(out.Stderr) != tt.size {
				t.Errorf("stderr 長度 = %d, 期望原樣的 %d bytes", len(out.Stderr), tt.size)
			}
			// stdout 一個字都沒有卻被動到，代表兩者共用了同一個額度。
			if out.Stdout != "" || out.StdoutTruncated {
				t.Errorf("stdout 沒有輸出，不該被動到: stdout=%q truncated=%v", out.Stdout, out.StdoutTruncated)
			}
		})
	}
}

// TestShellTimeoutAbortsCommand 驗證跑太久的命令**會被中止**、回明確錯誤。
//
// **這一格刻意不斷言「一定在期限內返回」。** 本票的逾時就是 exec.CommandContext 的
// 原生行為（只 Kill 直接子進程），沒有 process group 終止、也沒有 bounded wait——
// 後代若持有 stdout／stderr，Wait 可能不返回。那個保證由 ticket #35 補；在這裡先斷言
// 它，等於替一個還沒實作的契約背書。
//
// 訊息斷言只比對**語意關鍵字**、不綁死整句：#35 會把「可能有殘留」換成它的三句保證，
// 綁死字串會讓那張票連帶改一批與它無關的測試。
func TestShellTimeoutAbortsCommand(t *testing.T) {
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"sleep"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: 100 * time.Millisecond},
	)

	// 這段計時**不是**在斷言 bounded return，而是在斷言「取消訊號真的接上了」。
	// 兩者是不同的宣稱：#35 要保證的是連「後代抓著 pipe」「子進程卡在 D state」都
	// 仍然按期限返回；這裡只要求一個乖乖睡覺的子進程真的被砍掉，而不是跑到自己
	// 結束。上限取得比 sleep 的秒數小一個數量級，那個差距本身就說明了它在區分什麼。
	//
	// **沒有這一格，一個手動建構 `&exec.Cmd{Path:..., Args:...}` 的實作會全綠**——
	// argv[0] 正確、Dir／Env 正確、最後也回了逾時錯誤，只是它整整等了 30 秒才回，
	// 因為 exec.Cmd 存放 context 的欄位未匯出、只有 CommandContext 設得了，手動建構
	// 等於**完全沒有取消監看**。那正是 #35 第一道防線要掛上去的地方。
	const sleepSeconds = "30"
	const abortWithin = 10 * time.Second
	started := time.Now()
	result := shell.Execute(context.Background(), shellInputJSON(t, "sleep", sleepSeconds))
	if elapsed := time.Since(started); elapsed >= abortWithin {
		t.Errorf("Execute 花了 %v 才返回（sleep %s 秒跑完了），代表命令沒有被取消訊號中止——"+
			"Cmd 是不是沒有用 exec.CommandContext 建構？", elapsed, sleepSeconds)
	}
	if result.OK {
		t.Fatalf("逾時的命令不該回成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "逾時") {
		t.Errorf("錯誤 %q 未說明是逾時", result.Error)
	}
	// 本票就要如實揭露，不能等 #35：使用者不該以為每次逾時都清乾淨了。
	if !strings.Contains(result.Error, "殘留") {
		t.Errorf("錯誤 %q 未如實說明可能有殘留進程", result.Error)
	}
	if result.Retryable {
		t.Errorf("Retryable = true, 期望 false: %q", result.Error)
	}
}

// TestShellParentDeadlineIsNotReportedAsShellTimeout 驗證**呼叫端自己的期限**與
// **shell 的上限**分得出來。
//
// 兩者都會讓 runCtx.Err() 變成 DeadlineExceeded（context 的錯誤沿父子傳下來），所以
// 只比對 runCtx 的實作會把前者報成後者，並附上一個**根本沒被觸及的秒數**——使用者
// 於是跑去調 timeout_seconds，怎麼調都沒用。
//
// 目前的呼叫端只帶取消不帶期限，所以這條現在走不到；但那是**呼叫端當下的性質**，
// 不是這段程式的性質。這一格把「分得出來」釘成程式自己的性質。
func TestShellParentDeadlineIsNotReportedAsShellTimeout(t *testing.T) {
	// shell 自己的上限給得很寬，讓觸發的一定是呼叫端那個。
	const shellLimit = 30 * time.Second
	shell := tool.NewShell(
		tool.NewSandboxChecker(tool.SandboxConfig{AllowedCommands: []string{"sleep"}}),
		tool.ShellRuntime{Dir: t.TempDir(), PathDirs: tool.ParentPathDirs(), Timeout: shellLimit},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := shell.Execute(ctx, shellInputJSON(t, "sleep", "30"))

	if result.OK {
		t.Fatalf("呼叫端期限已到，不該回成功: %s", result.Content)
	}
	if strings.Contains(result.Error, shellLimit.String()) {
		t.Errorf("錯誤 %q 提了 shell 自己的上限 %s，但觸發的是呼叫端的期限——"+
			"這會讓使用者跑去調一個沒被觸及的 timeout_seconds", result.Error, shellLimit)
	}
	if !strings.Contains(result.Error, "呼叫端") {
		t.Errorf("錯誤 %q 未說明是呼叫端的期限先到", result.Error)
	}
	// 本票就要如實揭露，這條路徑同樣不例外。
	if !strings.Contains(result.Error, "殘留") {
		t.Errorf("錯誤 %q 未如實說明可能有殘留進程", result.Error)
	}
}

// TestShellContextCancelled 驗證取消當下就中止（憲法 5.3）。
func TestShellContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := newShell(t, t.TempDir(), []string{"echo"}).Execute(ctx, shellInputJSON(t, "echo", "hi"))
	if result.OK {
		t.Fatalf("context 已取消，不該成功: %s", result.Content)
	}
	if !strings.Contains(result.Error, "取消") {
		t.Errorf("錯誤 %q 未說明是取消", result.Error)
	}
}

// TestShellInputSchemaTeachesTheModel 釘住 InputSchema 的三件事（本票 AC）。
//
// **InputSchema 是「事前教 LLM」唯一的落點**——LLM 的訓練分佈裡 shell tool 預設吃
// shell 語法，不在描述裡講清楚，它很可能仍然生出 `ls | wc -l`。這段描述與「管線寫法
// fixture」是同一件事的兩面：描述負責事前教，錯誤訊息負責事後教。
func TestShellInputSchemaTeachesTheModel(t *testing.T) {
	shell := newShell(t, t.TempDir(), []string{"echo"})

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(shell.InputSchema(), &schema); err != nil {
		t.Fatalf("InputSchema 不是合法 JSON: %v", err)
	}
	// 必填參數名照 spec 定案：command 必填、args 選填。
	if !slices.Equal(schema.Required, []string{"command"}) {
		t.Errorf("required = %v, 期望只有 command（args 是選填）", schema.Required)
	}
	for _, key := range []string{"command", "args"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("InputSchema 缺參數 %q: %v", key, schema.Properties)
		}
	}

	// 三件事都要講到。比對的是**語意關鍵字**而不是整句，措辭本來就留給本票決定。
	text := shell.Description() + string(shell.InputSchema())
	for _, want := range []string{
		"參數",         // (1) 輸入是一個程式加參數陣列
		"管線",         // (2) 不支援管線
		"重導向",        // (2) 不支援重導向
		"write_file", // (2) 要寫檔請用 write_file
		"展開",         // (3) 參數不做 shell 展開
		"*.txt",      // (3) glob 不展開的具體例子
		"$HOME",      // (3) 變數不展開的具體例子
	} {
		if !strings.Contains(text, want) {
			t.Errorf("InputSchema／Description 未提到 %q:\n%s", want, text)
		}
	}
}

// TestBuiltinToolRequiredParamNames 釘住**四個新 Tool 的必填參數名**（spec #4 定案）。
//
// 這是對外契約，不是實作細節：參數名寫進 InputSchema 送給 LLM，改名等於讓所有既有的
// Profile 與錄製回應一起失效。四個放同一格是刻意的——散在各自的測試裡，下一個加 Tool
// 的人看不到別人用了什麼名字，於是 `path` 與 `file_path` 會同時存在。
func TestBuiltinToolRequiredParamNames(t *testing.T) {
	root, dir := newWorkspace(t)
	checker := tool.NewSandboxChecker(tool.SandboxConfig{})

	tests := []struct {
		tl           tool.OryxTool
		wantRequired []string
		// wantOptional 是宣告了但**不必填**的參數（shell 的 args）。
		wantOptional []string
	}{
		{tl: tool.NewReadFile(checker, root), wantRequired: []string{"path"}},
		{tl: tool.NewWriteFile(checker, root), wantRequired: []string{"path", "content"}},
		{tl: tool.NewListDir(checker, root), wantRequired: []string{"path"}},
		{
			tl:           tool.NewShell(checker, tool.ShellRuntime{Dir: dir, Timeout: shellTestTimeout}),
			wantRequired: []string{"command"},
			wantOptional: []string{"args"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.tl.Name(), func(t *testing.T) {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(tt.tl.InputSchema(), &schema); err != nil {
				t.Fatalf("InputSchema 不是合法 JSON: %v", err)
			}
			if !slices.Equal(schema.Required, tt.wantRequired) {
				t.Errorf("required = %v, 期望 %v", schema.Required, tt.wantRequired)
			}
			for _, name := range slices.Concat(tt.wantRequired, tt.wantOptional) {
				if _, ok := schema.Properties[name]; !ok {
					t.Errorf("InputSchema 沒宣告參數 %q: %v", name, schema.Properties)
				}
			}
			// 選填的那些**不得**混進 required——LLM 會被逼著每次都給 args。
			for _, name := range tt.wantOptional {
				if slices.Contains(schema.Required, name) {
					t.Errorf("%q 被列為必填，期望選填", name)
				}
			}
		})
	}
}
