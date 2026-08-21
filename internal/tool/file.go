package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/core"
)

// ReadFileToolName 是內建 Tool read_file 的註冊名，也是 Profile 的 tools 欄位要
// 引用的那個字串。
const ReadFileToolName = "read_file"

// readFileTool 是內建 Tool read_file：讀 Workspace 內、白名單路徑下的檔案內容。
//
// **兩道防線分工明確、不互相取代**：
//
//   - checker 是**應用層**白名單（CheckFilePath），回 ErrSandboxViolation，訊息
//     告訴使用者要改哪一段設定。
//   - root 是**開檔層**的把關：os.Root 保證開不出 Workspace 之外的東西，加上這裡
//     的「拒絕符號連結」與「Lstat 型別檢查」，擋下任何漏網的逃逸。
//
// os.Root 保證的是「Workspace 之外開不出來」，**不是**「不跟隨符號連結」——
// Workspace 之內的連結它照樣跟隨。所以後兩項是應用層的獨立檢查，不是 os.Root 的
// 附帶效果（internal/config/skill.go 與 internal/memory/longterm.go 對這條分工
// 已有先例，這裡沿用同一個論述）。
type readFileTool struct {
	checker *SandboxChecker
	root    *os.Root
}

// NewReadFile 建立內建 Tool read_file。依賴顯式注入（憲法 5.2）：白名單校驗器與
// Workspace 的 *os.Root 都由組裝點給，這個 package 不知道 Workspace 在磁碟哪裡。
func NewReadFile(checker *SandboxChecker, root *os.Root) OryxTool {
	return &readFileTool{checker: checker, root: root}
}

func (t *readFileTool) Name() string { return ReadFileToolName }

// Description 是 LLM 判斷「什麼時候該用這個 Tool」的唯一依據。白名單與路徑基準
// 都要講明白——那是 LLM 唯一看得到的提示，沿用 http.go 既有 schema 的做法。
func (t *readFileTool) Description() string {
	return "讀取 Workspace 內、路徑白名單允許範圍中的檔案內容，回傳內容與是否被截斷。" +
		"路徑相對 Workspace 根，不接受絕對路徑；內容過長時會截斷並標示。"
}

func (t *readFileTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "要讀取的檔案路徑（相對 Workspace 根，須在 file.allowed_paths 白名單內）"}
		},
		"required": ["path"]
	}`)
}

// readFileInput 是 read_file 的輸入參數。
type readFileInput struct {
	Path string `json:"path"`
}

// readFileOutput 是回填給 LLM 的結果內容：檔案內容與是否被截斷。
//
// Truncated 要明確標示出來，讓 LLM 知道自己只看到一部分、不會據殘缺內容下結論
// （形狀沿用 httpOutput）。
type readFileOutput struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Execute 校驗路徑、經 Workspace 的 os.Root 開檔並讀回內容。
//
// 任何失敗都以錯誤 ToolResult 回填給 LLM，不 panic、不中斷 turn。**沒有一條路徑
// 標 Retryable**：這裡的失敗（白名單拒絕、連結、型別、不存在、沒權限）重跑一次
// 結果都一樣，讓 ReAct 循環退避重試只是白白多燒兩輪。
func (t *readFileTool) Execute(ctx context.Context, input string) core.ToolResult {
	var in readFileInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("解析 %s 輸入參數: %v", ReadFileToolName, err)}
	}
	if in.Path == "" {
		return core.ToolResult{Error: fmt.Sprintf("%s 缺必填參數 path", ReadFileToolName)}
	}
	// 取消在這裡收：底下的 os.Root 開檔與讀取都**不吃 context**，走進去之後就沒有
	// 中止點了（憲法 5.3）。真正會無限期阻塞的那一種（具名管道）由下面的型別檢查
	// 在開檔前擋掉，兩者合起來這條路徑才是有界的。
	if err := ctx.Err(); err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 被取消: %v", ReadFileToolName, err)}
	}

	rel, err := t.checker.CheckFilePath(in.Path)
	if err != nil {
		return core.ToolResult{Error: err.Error()}
	}

	info, err := statNoSymlink(t.root, rel)
	switch {
	case errors.Is(err, ErrSandboxViolation):
		return core.ToolResult{Error: err.Error()}
	case errors.Is(err, os.ErrNotExist):
		// 「不存在」與「沒權限」要分得出來：使用者（與 LLM）對這兩件事的下一步不同。
		return core.ToolResult{Error: fmt.Sprintf("%s 找不到 %s：這個檔案在 Workspace 內不存在", ReadFileToolName, rel)}
	case errors.Is(err, os.ErrPermission):
		return core.ToolResult{Error: fmt.Sprintf("%s 讀不到 %s：權限不足", ReadFileToolName, rel)}
	case err != nil:
		return core.ToolResult{Error: fmt.Sprintf("%s 檢查 %s: %v", ReadFileToolName, rel, err)}
	}

	// 目錄與「非普通檔」刻意分成兩種回報。目錄是使用者給錯了對象，是可以換一個做法
	// 的正常情況；裝置檔／具名管道／socket 則是 Sandbox 層面的拒絕。
	//
	// 措辭這裡不提「改用 list_dir」：那個 Tool 還沒落地（ticket #32），叫 LLM 去用
	// 一個它的工具清單裡沒有的名字只會讓它多繞一圈。等 list_dir 進來再補這句。
	if info.IsDir() {
		return core.ToolResult{Error: fmt.Sprintf("%s 的目標 %s 是目錄，不是檔案", ReadFileToolName, rel)}
	}
	if !info.Mode().IsRegular() {
		// 以 %w 包裝而不是把哨兵字串插進去：這條與 sandbox.go 那幾條是同一類拒絕，
		// 日後若把這段抽成回 error 的函式，errors.Is 才仍然認得它（憲法 5.1）。
		violation := fmt.Errorf("%w: %s 的目標 %s 不是普通檔（實際為 %s）；裝置檔、具名管道與 socket 一律拒絕",
			ErrSandboxViolation, ReadFileToolName, rel, info.Mode().Type())
		return core.ToolResult{Error: violation.Error()}
	}

	// 權限不足在這裡才浮出來：Lstat 讀的是 metadata，一個 0o000 的檔案 stat 得到、
	// 開不起來。訊息要與上面那兩條同樣分得出來，別讓「沒權限」混進「不存在」。
	f, err := t.root.Open(rel)
	if errors.Is(err, os.ErrPermission) {
		return core.ToolResult{Error: fmt.Sprintf("%s 讀不到 %s：權限不足", ReadFileToolName, rel)}
	}
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 開啟 %s: %v", ReadFileToolName, rel, err)}
	}
	defer func() { _ = f.Close() }()

	// 多讀一個位元組才分得出「剛好等於上限」與「超過上限」。
	data, err := io.ReadAll(io.LimitReader(f, maxResponseBytes+1))
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("%s 讀取 %s: %v", ReadFileToolName, rel, err)}
	}
	out := readFileOutput{Content: string(data)}
	if len(data) > maxResponseBytes {
		out.Content = string(trimPartialRune(data[:maxResponseBytes]))
		out.Truncated = true
	}
	content, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{Error: fmt.Sprintf("編碼 %s 結果: %v", ReadFileToolName, err)}
	}
	return core.ToolResult{OK: true, Content: string(content)}
}

// statNoSymlink 逐段檢查 rel 的每一個路徑元件（含最終目標）都不是符號連結，
// 並回傳最終目標的 FileInfo。
//
// **必須檢查每一段，只看最終元件是不夠的**：a/link-dir/b.txt 這種把連結放在中間的
// 寫法會整個漏掉，而 os.Root 補不上這個洞——它只擋指到 Workspace **之外**的連結，
// Workspace 之內的照樣跟隨。形狀沿用 LongTermMemory.ensureNoSymlink。
//
// 連結一律拒絕、不解析後比對：與 internal/memory、internal/config 同形，也不必去猜
// 使用者放那條連結的意圖。
//
// 這是 TOCTOU-racy 的檢查——它與底下的實際開檔之間有空隙。要防的是**靜態植入**的
// 連結（隨專案進 git 的那種），不是即時競爭的攻擊者；os.Root 把殘留風險界定在
// Workspace 之內，而攻擊者得先能在使用者自己的工作區裡寫檔。
func statNoSymlink(root *os.Root, rel string) (os.FileInfo, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	var info os.FileInfo
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		var err error
		if info, err = root.Lstat(component); err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: 路徑 %s 是符號連結，一律拒絕不跟隨（只接受 Workspace 內的實體檔案）",
				ErrSandboxViolation, component)
		}
	}
	return info, nil
}

// trimPartialRune 把截斷位置往回退到一個完整的 UTF-8 字元邊界。
//
// 直接切在第 N 個位元組會把一個多位元組字元切成兩半，json.Marshal 接著把那半個字元
// 換成 U+FFFD——LLM 讀到的最後一個字是壞的。中日韓文字每個 3 個位元組，而上限
// 1 MiB 不是 3 的倍數，所以這不是邊角案例，是常態。
//
// 最多退 utf8.UTFMax-1 個位元組（一個字元最長 4 個位元組，尾端不完整的序列因此最多
// 3 個位元組）。有了這個上限，一份非 UTF-8 的二進位檔最多少 3 個位元組，不會被
// 一路往回砍。
func trimPartialRune(b []byte) []byte {
	for range utf8.UTFMax - 1 {
		if len(b) == 0 {
			break
		}
		// size > 1 代表那是一個真的 U+FFFD 字元（檔案裡本來就有），不是被切半的殘餘。
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
