package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AssertionKind 是一種斷言的種類。
//
// 這是一份**封閉的白名單**：解析時不認得的種類一律報錯，不當成「以後再說」放過去。
// 一個被安靜忽略的斷言，會讓評測宣稱一個它根本沒檢查的性質成立——那比沒有評測更糟，
// 因為報表上是綠的。
type AssertionKind string

const (
	// AssertReplyContains 斷言最終回應含有某段文字。
	AssertReplyContains AssertionKind = "reply_contains"
	// AssertReplyNotContains 斷言最終回應**不含**某段文字。
	//
	// 它治的是 reply_contains 治不了的一類缺陷：回應說了該說的、指標也全在上限內，
	// 但裡面夾著一句錯誤的事實陳述（issue #58）。那種回應對既有四種斷言全綠。
	AssertReplyNotContains AssertionKind = "reply_not_contains"
	// AssertToolCalled 斷言某個 Tool 被呼叫過（不論那次執行成功與否）。
	AssertToolCalled AssertionKind = "tool_called"
	// AssertMaxIterations 斷言 iteration 數不超過某個上限。
	AssertMaxIterations AssertionKind = "max_iterations"
	// AssertMaxToolFailures 斷言 Tool 失敗次數不超過某個上限。
	AssertMaxToolFailures AssertionKind = "max_tool_failures"
)

// knownAssertionKinds 是目前支援的全部斷言種類。
//
// **前三種是布林問題（有沒有），後兩種是程度問題（多少）。**
//
// 後兩種由 ticket #53 加入，理由是一次退步的典型形態是「答案還是對的，只是繞了比較
// 久」——issue #36 那個 10→1 的改善若被改回去，當時僅有的兩種布林斷言一條都不會轉紅。
//
// reply_not_contains 是第三種布林斷言，理由是另一種同樣不轉紅的形態：**答案的形狀
// 對、指標也在上限內，但裡面夾著一句錯誤的事實陳述**（issue #58）。它與 reply_contains
// 不是互為反面而已——2026-09-04 的 A／B 取樣量到兩組各 3 輪「判卷通過但同時在謊稱」，
// 那些回應正是因為提到了正確的欄位名而滿足 reply_contains。
var knownAssertionKinds = []AssertionKind{
	AssertReplyContains, AssertReplyNotContains, AssertToolCalled, AssertMaxIterations, AssertMaxToolFailures,
}

// isMetricKind 回答這一種斷言的值是不是一個數字上限。
//
// 這個判斷同時被解析層（校驗值寫得對不對）與判卷層（比對）用到，寫成一處是必要的：
// 兩邊對「哪些種類是指標型」的認知一旦分岔，一種斷言會在解析時被當文字放行、在判卷
// 時被當數字比對，而那條用例的結果從輸出完全看不出是這裡出的問題。
func isMetricKind(kind AssertionKind) bool {
	return kind == AssertMaxIterations || kind == AssertMaxToolFailures
}

// parseLimit 把指標型斷言的值解析成上限。
//
// **TrimSpace 只用在這裡，不用在文字型斷言的比對上。** 數字的前後空白沒有語義——
// `value: "3 "` 與 `value: 3` 講的是同一個上限；而文字的前後空白可能是使用者刻意寫的
// （見 Assertion.validate 那段說明），順手 trim 掉會讓斷言比的東西與他寫的不是同一個。
func parseLimit(value string) (int, error) {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("上限必須是整數，實際是 %q", value)
	}
	// 上限 0 是合法的（「一次 Tool 失敗都不准有」），負數則不是：iteration 數與 Tool
	// 失敗數都不可能小於 0，這種宣告必然永遠不通過。
	if limit < 0 {
		return 0, fmt.Errorf("上限 %d 不得為負（iteration 數與 Tool 失敗數都不可能小於 0，這條斷言永遠不會通過）", limit)
	}
	return limit, nil
}

// Assertion 是用例的一條斷言。
type Assertion struct {
	Kind  AssertionKind `yaml:"kind"`
	Value string        `yaml:"value"`
}

// Setup 是執行前要在乾淨的 Workspace 裡佈置的初始狀態。
//
// **宣告式而非 shell 腳本**（spec #5 定案，理由於 ADR-0006 定案當日更正）：宣告式是
// 確定性的、不依賴目標系統上存在哪一種 shell、沒有引號與跳脫的解析坑，且讓佈置與判卷
// 都能寫成可表格驅動測試的純函式。
type Setup struct {
	// Files 是「相對 Workspace 根的路徑 → 檔案內容」。父目錄不存在時自動建立。
	Files map[string]string `yaml:"files"`
}

// Case 是一份評測用例的宣告，一個 YAML 檔一份。
type Case struct {
	Name    string `yaml:"name"`
	Profile string `yaml:"profile"`
	// Requires 是對 Workspace 環境的前置條件，**選填**（issue #59）。
	//
	// 欄位位置緊接 Profile，因為它校驗的正是 Profile 的 tools 與 config.yaml 的兩份
	// 白名單。**這不是執行順序**（Codex 審查抓到原本的說法與接線不符）：RunCase 先
	// PrepareWorkspace——連同 Setup.Files 的佈置——才校驗 requires。這樣接是因為校驗
	// 需要的 cfg 與 Profile 都從**複製後**的 Workspace 載入，而複製檔案不花錢；
	// issue #59 要的那條線是「在送出任何請求之前」，它在更後面。
	Requires Requires    `yaml:"requires"`
	Setup    Setup       `yaml:"setup"`
	Task     string      `yaml:"task"`
	Assert   []Assertion `yaml:"assert"`
}

// caseFields 是用例支援的全部頂層欄位，**與 Case 的 yaml tag 一一對應**，只用於錯誤
// 訊息。實際的把關由 KnownFields 做，這裡漏列只會讓訊息少一項、不會放行任何東西
// ——與 requiresFields 那份（漏列會擋下合法欄位）的風險方向不同。
var caseFields = []string{"name", "profile", "requires", "setup", "task", "assert"}

// ParseCase 解析並校驗一份用例宣告。source 只用於錯誤訊息——用例目錄裡放十份 YAML
// 時，一句沒有檔名的「缺 task」等於要人一份一份翻。
//
// 校驗全部集中在這裡，而不是散在執行的各個階段：一份寫壞的用例應該在**送出任何請求
// 之前**就被擋下。跑到一半才發現斷言種類不認得，那次真實 Provider 呼叫的錢已經花了。
func ParseCase(data []byte, source string) (Case, error) {
	var c Case
	// **頂層欄位也是封閉白名單**（Codex 審查抓到）。yaml.Unmarshal 對不認得的欄位一律
	// 靜默忽略，於是 `require:`（少一個 s）會被當成整段省略——requires 的內層白名單
	// 一個字都攔不到。**那是同一個 fail-open，只是往上一層**：修內層而不修頂層，等於
	// 修了症狀沒修根。
	//
	// KnownFields 對整棵樹生效，所以 assert 的欄位拼錯也一併擋得住。它與 AssertionKind、
	// requiresFields 是同一條規則：被安靜忽略的宣告會讓評測宣稱一個它根本沒檢查的性質
	// 成立，而報表上是綠的。
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		// **空輸入回 io.EOF，那不是語法錯誤**：yaml.Unmarshal 對空輸入是回 nil 加一個
		// 零值，行為要保持一致。讓它往下走到 validate，由「name 必填」給出對使用者更
		// 有意義的訊息——一句「EOF」不會告訴任何人該補什麼。
		if !errors.Is(err, io.EOF) {
			// KnownFields 的拒絕是 *yaml.TypeError。它說得出行號與欄位名，但夾著
			// 「type eval.Case」這種 Go 型別名——對寫 YAML 的人是實作細節——而且不說
			// 支援哪些欄位。補一句，與 requires 那一層的訊息品質看齊。
			var typeErr *yaml.TypeError
			if errors.As(err, &typeErr) {
				return Case{}, fmt.Errorf("解析用例 %s: %w\n        用例支援的欄位：%s（拼錯的欄位會被靜默忽略，所以一律擋下）",
					source, err, strings.Join(caseFields, "、"))
			}
			return Case{}, fmt.Errorf("解析用例 %s: %w", source, err)
		}
	}
	if err := c.validate(); err != nil {
		return Case{}, fmt.Errorf("用例 %s 校驗失敗: %w", source, err)
	}
	return c, nil
}

// LoadCases 讀入一個用例檔，或一個目錄下的全部 `.yaml` 用例檔。
//
// 目錄的走訪不遞迴、依檔名排序：執行順序必須恆定，否則同一批用例兩次執行的輸出順序
// 不同，人要比對兩次結果就得先自己排一遍。
func LoadCases(path string) ([]Case, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("讀取用例路徑 %s: %w", path, err)
	}
	if !info.IsDir() {
		c, err := loadCaseFile(path)
		if err != nil {
			return nil, err
		}
		return []Case{c}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("讀取用例目錄 %s: %w", path, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		files = append(files, filepath.Join(path, entry.Name()))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("用例目錄 %s 裡沒有任何 .yaml 用例檔", path)
	}

	cases := make([]Case, 0, len(files))
	for _, file := range files {
		c, err := loadCaseFile(file)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	return cases, nil
}

func loadCaseFile(path string) (Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("讀取用例 %s: %w", path, err)
	}
	return ParseCase(data, path)
}

// validate 校驗一份用例宣告的必填欄位、斷言種類與佈置路徑。
func (c Case) validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("name 必填（用例名會出現在輸出裡，用來指認是哪一條）")
	}
	if err := validateProfileName(c.Profile); err != nil {
		return err
	}
	if err := c.Requires.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Task) == "" {
		return fmt.Errorf("task 必填（送給 Agent 的訊息）")
	}
	if len(c.Assert) == 0 {
		return fmt.Errorf("assert 至少要有一條斷言（沒有斷言的用例永遠會通過，等於沒測）")
	}
	for i, a := range c.Assert {
		if err := a.validate(); err != nil {
			return fmt.Errorf("assert[%d]: %w", i, err)
		}
	}
	return validateSetupFiles(c.Setup.Files)
}

func (a Assertion) validate() error {
	if a.Kind == "" {
		return fmt.Errorf("kind 必填（目前支援 %s）", joinKinds())
	}
	if !isKnownKind(a.Kind) {
		return fmt.Errorf("不認得的斷言種類 %q（目前支援 %s）", a.Kind, joinKinds())
	}
	// **判空用 TrimSpace，比對用原值**，這兩件事要分開（Codex 審查抓到）。
	//
	// 判空這一面：純空白比對不到任何實質內容，而它**壞的方向隨斷言而相反**——
	// `value: " "` 配 reply_contains 幾乎永遠成立（假綠燈），配 reply_not_contains
	// 則幾乎永遠不成立（假紅燈）。`value: ""` 更極端：strings.Contains 對空字串恆真，
	// 前者恆過、後者恆敗。
	//
	// **所以診斷不能寫成任何一個方向**（Codex 審查抓到）。它也不能退一步描述「取決於
	// 回應裡有沒有空白字元」——那對兩種輸入都不成立：`""` 的結果與任何字元無關，
	// `"  "` 取決於的是**精確的連續空白**而非「有沒有空白」（Codex 第二輪抓到）。
	//
	// 唯一對所有種類、所有空白值都成立的說法是「這個值不表達有效的比對條件，結果與
	// 想檢查的性質無關」，訊息只講這一點。TestParseCase 的三格空白案例（正向一格、
	// 負向兩格）都斷言這句中性片段，方向寫回去任何一邊都會轉紅。這與 name、profile、
	// task 與 setup.files 的路徑是同一條規則，那四處本來就這樣寫。
	//
	// 比對那一面：**這裡不動 a.Value**。使用者寫的前後空白可能是刻意的（回應裡
	// 「答案： 42」的那個空格），解析時順手 trim 掉會讓斷言比的東西與他寫的不是同一個，
	// 而且從輸出完全看不出來。
	if strings.TrimSpace(a.Value) == "" {
		return fmt.Errorf("value 必填且不得只有空白（%s 要比對的內容；空白值不表達有效的比對條件，判卷結果與想檢查的性質無關，等於沒測）", a.Kind)
	}
	// 指標型斷言的值要在**送出任何請求之前**確認是個數字。跑到判卷才發現 `value: 三次`
	// 比對不了的話，那一次真實 Provider 呼叫的錢已經花了——與其餘欄位同一條理由。
	if isMetricKind(a.Kind) {
		if _, err := parseLimit(a.Value); err != nil {
			return fmt.Errorf("%s: %w", a.Kind, err)
		}
	}
	return nil
}

func isKnownKind(kind AssertionKind) bool {
	for _, known := range knownAssertionKinds {
		if kind == known {
			return true
		}
	}
	return false
}

func joinKinds() string {
	names := make([]string, len(knownAssertionKinds))
	for i, kind := range knownAssertionKinds {
		names[i] = string(kind)
	}
	return strings.Join(names, "、")
}

// ProfilePath 回傳這份用例要載入的 Profile 檔路徑，並確認它落在這份 Workspace 的
// profiles/ 之內。
//
// **profile 是一個會進路徑的欄位，跟 setup.files 的鍵同一類。** 這一點在第一版被漏掉了
// （Codex 審查抓到）：只檢查非空的話，`profile: ../../outside` 會讓評測載入一份**沒有被
// 複製進乾淨 Workspace** 的 Profile——可能是上一次執行留下的，也可能根本在 Workspace
// 之外，兩者都讓「每個用例一份乾淨的 Workspace」這條驗收條件失效。
//
// 校驗在這裡**再做一次**，儘管 ParseCase 已經驗過，理由與 PrepareWorkspace 重驗佈置
// 路徑相同：RunCase 是匯出的函式，一個繞過解析直接建 Case 值的呼叫端不該有辦法讓評測
// 去讀 Workspace 外面的 YAML。
func (c Case) ProfilePath(ws string) (string, error) {
	if err := validateProfileName(c.Profile); err != nil {
		return "", err
	}
	return filepath.Join(ws, "profiles", c.Profile+".yaml"), nil
}

// validateProfileName 要求 profile 是一個單純的檔名 stem。
//
// 判準是 filepath.Base(name) == name：`../../outside` 的 Base 是 `outside`、`sub/other`
// 的 Base 是 `other`，兩者都與原字串不同，因此都被擋下。`.` 與 `..` 的 Base 等於自己，
// 所以要另外列出來——那是這條規則唯一漏得掉的兩個值。
func validateProfileName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("profile 必填（要用哪份 Profile 跑這個用例）")
	}
	if name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("profile %q 不是單純的 Profile 名：它會被拼進 <Workspace>/profiles/<名字>.yaml，"+
			"不得含路徑分隔符、也不得是 . 或 ..（評測只能用複製進乾淨 Workspace 的那幾份 Profile）", name)
	}
	return nil
}

// validateSetupFiles 校驗每條佈置路徑，並確認兩條路徑不會寫到同一個檔案。
//
// **不同寫法指向同一個檔案是要擋的**（`todo.md` 與 `./todo.md`）：map 的走訪順序在 Go
// 裡是隨機的，兩條都寫得出去，最後留下哪一份內容每次執行都可能不同。一個結果不確定的
// 評測沒有價值——確定性正是這份 schema 選宣告式而非 shell 腳本的理由，這裡不能自己
// 開一個破口。
func validateSetupFiles(files map[string]string) error {
	seen := make(map[string]string, len(files))
	for raw := range files {
		clean, err := validateSetupPath(raw)
		if err != nil {
			return err
		}
		if other, dup := seen[clean]; dup {
			return fmt.Errorf("setup.files 的 %q 與 %q 指向同一個檔案 %q，寫入順序不確定；請只留一條",
				raw, other, clean)
		}
		seen[clean] = raw
	}
	return nil
}

// validateSetupPath 校驗一條佈置路徑並回傳標準化後的形式。
//
// 三條規則與 File Tool 的路徑校驗同源（見 internal/tool 的 SandboxChecker.CheckFilePath）：
// 基準一律是 Workspace 根、拒絕絕對路徑、`../` 在比對之前先解掉。刻意各寫一份而不是
// 共用：那一份回答的是「Agent 執行時能不能碰這個路徑」，受 config.yaml 的白名單約束；
// 這一份回答的是「佈置階段能不能寫這個路徑」，只受 Workspace 邊界約束。兩者的答案本來
// 就不同——用例常要在白名單之外的地方放檔案（例如一份給 Agent 讀的 AGENTS.md）。
func validateSetupPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("setup.files 的路徑不得為空")
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("setup.files 的路徑 %q 是絕對路徑；基準是 Workspace 根，請改用相對路徑", raw)
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup.files 的路徑 %q 標準化後穿越出 Workspace 根，一律拒絕", raw)
	}
	return clean, nil
}
