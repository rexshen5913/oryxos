package core

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile 是 Agent 的完整配置。channels 等欄位隨其模組於後續 ticket 加入。
type Profile struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Identity    Identity    `yaml:"identity"`
	Provider    ProviderRef `yaml:"provider"`
	Tools       []string    `yaml:"tools"`
	// Bootstrap 決定這個 Agent 載入**哪些** Bootstrap 檔案，三態刻意分開：
	//
	//   nil（欄位省略）  沒意見，載入預設三檔——既有 Profile 因此免遷移
	//   非 nil 零長      明確表達「我不要」，一份都不載入
	//   有元素           只載入列到的那些（覆寫，不是疊加）
	//
	// 「省略」與「空清單」必須分得出來，所以型別是切片而非別的東西：yaml.v3 對
	// 未寫的欄位留 nil、對 `bootstrap: []` 給非 nil 的零長切片。裸 key 無值
	// （`bootstrap:`）是 null、同樣留 nil，歸「省略」——那是寫了一半的設定，
	// 不是明確的拒絕。
	//
	// **欄位決定載入哪些，不決定誰蓋過誰。** 拼接順序恆由 ADR-0003 決定，與這裡的
	// 書寫順序無關（見 composeSystemPrompt）；讓每份 Profile 自己改順序會把
	// ADR-0003 從固定契約降級成預設值，它 Consequences 要求的可測性也跟著失效。
	Bootstrap []string `yaml:"bootstrap"`
	// Skills 引用這個 Agent 可用的 Skill，值為不帶副檔名的 Skill 名
	// （`skills: [daily-pr-digest]` → `.oryxos/skills/daily-pr-digest.md`）。
	//
	// **語義與 Bootstrap 刻意不同、不可照抄**：省略或空清單都是「這個 Agent 沒有
	// Skill」，沒有「省略即載入全部」這回事——Workspace 的 skills/ 底下放著的檔案
	// 不該因為 Profile 沒提就自動生效。所以這裡不需要區分 nil 與零長切片。
	//
	// 每個值都必須是合法的 Skill 名稱（見 ValidateSkillName），這順帶讓 `../` 一類
	// 的路徑逃逸結構上不可能。
	Skills []string `yaml:"skills"`
	// McpServers 宣告這個 Agent 要接哪幾個外部 MCP server（Agent 級隔離），值是
	// mcp_servers.yaml 裡的宣告名。
	//
	// 這是**兩層過濾的第一層**：先由這個欄位決定這個 Agent 看得到哪些 server 的工具，
	// 再由 tools 欄位從中挑出具體工具（工具級控制），兩層都要通過。沒有第一層的話每個
	// Agent 都吃到所有 server 的工具，只能靠逐個列長名字收斂——那是把隔離責任推給
	// 使用者的書寫紀律。
	//
	// 語義同 Skills、與 Bootstrap 刻意不同：省略或空清單都是「不接任何 server」，
	// 沒有「省略即全部」（見 McpServerRefs）。既有 Profile 因此免遷移——沒寫這個欄位
	// 的 Profile 行為與本票之前完全相同。
	McpServers []string `yaml:"mcp_servers"`
	Settings   Settings `yaml:"settings"`
}

// Bootstrap 檔案的正式名稱，也就是 Profile bootstrap 欄位的合法值。
//
// 定義在 core 而不是 config：這三個字串是**使用者寫在 Profile 裡的欄位值**（配置
// 詞彙），core 需要它們才能把 bootstrap 欄位映射成 BootstrapSelection。檔案實際
// 住在 Workspace 的哪個位置仍然只有 config 知道，依賴方向不變。單一來源是為了
// 避免兩個 package 各寫一份字串、日後改一邊漏一邊。
const (
	BootstrapAgentsFile = "AGENTS.md"
	BootstrapUserFile   = "USER.md"
	BootstrapSoulFile   = "SOUL.md"
)

// BootstrapSelection 把 bootstrap 欄位的三態解成「要讀哪幾份檔案、缺檔算不算錯」，
// 並套上 ADR-0003 的互斥：identity.prompt 存在時 SOUL.md 完全不進 prompt，那份
// 檔案因此連讀都不必讀。
//
// 互斥在**這裡**而不是在載入端：它是架構決策，要留在能被測試釘住的那一層。
// 列了 SOUL.md 的 Profile 也推翻不了它——欄位決定載入哪些，不決定覆蓋語義。
//
// 欄位有未知檔名時回錯誤而不是靜默略過。LoadProfile 已經在載入時擋過一次，但
// **不是每個 Profile 都經過 LoadProfile**（測試手組的、日後 ProfileRegistry 走的
// 路徑都不是），少一道就會把 `Agents.md` 這種筆誤變成一個安靜的空 prompt——那正是
// 這份設計其餘部分都在避免的失敗形態。校驗共用同一個 validateBootstrap，不另立
// 第二份「什麼算合法檔名」的定義。
func (p *Profile) BootstrapSelection() (BootstrapSelection, error) {
	if err := validateBootstrap(p.Bootstrap); err != nil {
		return BootstrapSelection{}, err
	}

	sel := BootstrapSelection{Soul: true, Agents: true, User: true}
	if p.Bootstrap != nil {
		sel = BootstrapSelection{Explicit: true}
		for _, name := range p.Bootstrap {
			switch name {
			case BootstrapSoulFile:
				sel.Soul = true
			case BootstrapAgentsFile:
				sel.Agents = true
			case BootstrapUserFile:
				sel.User = true
			}
		}
	}
	if strings.TrimSpace(p.Identity.Prompt) != "" {
		sel.Soul = false
	}
	return sel, nil
}

// validateBootstrap 校驗 bootstrap 欄位的每個值都是已知的 Bootstrap 檔名、且沒有
// 重複。兩者都是設定筆誤，一律報錯不靜默（沿 Registry.Subset 對 tools 的既有語義）。
//
// 只認完全相同的檔名，`agents.md` 不算數：欄位值對應的是實體檔案，而 macOS 的
// 檔案系統預設不分大小寫、Linux 分——放寬會讓同一份 Profile 在不同平台有不同行為。
//
// 這裡只管**名稱**，不碰檔案系統。「列出的檔案是否真的存在」需要 Workspace 的
// root，由組裝點在啟動時另行校驗（config.ValidateBootstrapFiles）。
func validateBootstrap(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		switch name {
		case BootstrapAgentsFile, BootstrapUserFile, BootstrapSoulFile:
		default:
			return fmt.Errorf("bootstrap 列出的 %q 不是 Bootstrap 檔案（只能是 %s、%s、%s）",
				name, BootstrapAgentsFile, BootstrapUserFile, BootstrapSoulFile)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("bootstrap 重複列出 %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// Identity 是 Profile 的身份段。Prompt 是 system prompt 的**人格層**，與
// Workspace 級的 SOUL.md **互斥、前者優先**（ADR-0003）；其餘各層（AGENTS.md、
// USER.md、長期記憶）由 ContextLoader 與 MemoryService 各自供給。
type Identity struct {
	AgentName string `yaml:"agent_name"`
	Prompt    string `yaml:"prompt"`
}

// ProviderRef 以 provider name 引用 Provider 並指定模型與參數。
type ProviderRef struct {
	Name        string  `yaml:"name"`
	Model       string  `yaml:"model"`
	Temperature float32 `yaml:"temperature"`
}

// Settings 是 ReAct 循環的執行參數。
type Settings struct {
	MaxIterations   int `yaml:"max_iterations"`
	MaxHistoryTurns int `yaml:"max_history_turns"`
	// MaxRepeatedToolFailures 是死循環守衛的門檻：同一個 Tool 帶等價參數連續失敗
	// 幾次之後，回填內容要多帶一段要求改變策略的提示（ticket #54）。
	MaxRepeatedToolFailures int `yaml:"max_repeated_tool_failures"`
	// MaxContextRunes 是一次 LLM 呼叫的上下文預算，以 rune 計。超出時久遠的 Tool
	// 結果會被壓縮，好讓讀過的大檔案不在後續每個 iteration 重送（ticket #48）。
	MaxContextRunes int `yaml:"max_context_runes"`
}

const (
	defaultMaxIterations   = 10
	defaultMaxHistoryTurns = 20
	// defaultMaxRepeatedToolFailures 是死循環守衛的預設門檻。
	//
	// **3 有實測依據**（issue #57，證據取自 ticket #55 的真實 API 驗收）。這裡原本
	// 寫的是一段推導——「大到讓 LLM 有機會自行修正、小到還留得下七次讓它換路」——
	// 方向對，但兩端都沒有量過。以下是量過之後的版本。
	//
	// **驗收時一度得到「守衛幾乎觸發不到」的印象**，理由是 ToolErrorKind.Guidance 的
	// 分類指引讓模型第 1 次失敗就換了策略。那個印象來自四次都拿 read_file 讀不存在
	// 檔案的嘗試——而 not_found 是 internal/tool 全部 85 個失敗結果建構點裡的 3 個。
	// 逐點數過（括號配對取 ToolResult 字面，只算帶 Error 欄位的）：
	//
	//	16 個（19%）Guidance 非空，回填時由型別自動附上（invalid_args 13、not_found 3）
	//	13 個（15%）是 sandbox，Guidance 刻意留空、下一步由每則訊息自帶
	//	56 個（66%）仍是零值未分類，多數只陳述失敗、不說下一步
	//
	// **這 81% 缺的是「集中式、由型別決定」的指引，不是缺所有下一步訊號。** sandbox
	// 那 13 個刻意由每則訊息自帶（見 ToolErrorSandbox，那是一條要人工守住的維護
	// 契約），未分類那 56 個裡也有少數帶了建議（file.go 抽 31 個，3 個有）。
	//
	// **所以守衛的定位是「共同 fallback」，不是「唯一機制」。** 它根本不看 ErrorKind，
	// 對所有 Tool、所有失敗類型一視同仁，因此不依賴任何一則訊息的措辭品質——而那
	// 正好補在維護契約最容易鬆掉的地方。下面 A／B 的對照組（守衛等於關閉）第 3 場
	// 模型自己也在 3 次後改了途徑，正是「不唯一」的直接證據。
	//
	// 這個數字量的是**廣度**（有多少種失敗形態沒有集中式指引），不是各類在真實使用
	// 中的**發生頻率**——那份資料目前沒有，別把它當成頻率讀。
	//
	// **門檻 N 的意思是「第 N 次等價失敗**之後**，回填內容多帶一段提示」。** 那一次
	// 失敗已經執行完了——Tool 先跑，loopGuard.observe 才在它的回填內容上附提示（見
	// react.go 的呼叫順序）。
	//
	// 所以守衛擋不下**觸發門檻的那第 N 次**呼叫；它作用在模型對**其後**呼叫的決定
	// 上。而那正是它有效的地方，不是它的限制——下面 A／B 那組 9、5、3 → 3、3、3
	// 就是這個作用的量測結果。調低門檻等於讓那句「請改變策略」早一輪出現，因此
	// **可能**進一步減少其後的重複呼叫；擋不下的始終只有第 N 次本身。
	//
	// 而計數的 key 是「Tool ＋ 規範化後的參數」：模型換了路徑、補了欄位就是另一個
	// key、從 1 重新數。**真正的試錯本來就累積不到門檻**，累積得到的只有原封不動
	// 再送一次。往下調到 2 的意思因此是「送了兩次相同呼叫就要求收手」——那不見得
	// 更差，但沒有任何 A／B 資料支持，而 3 有、且沒有量到需要更早介入的缺口。
	// **維持 3 是「沒有理由改」，不是「2 一定更糟」。**
	//
	// **A／B 實測**（兩份 Profile 只差這個欄位，任務刻意不指定重試次數，失敗類型選
	// sandbox 好讓停止點歸不到分類指引上；每組三場）：
	//
	//	                 開頭連續等價呼叫    iteration    撞 max_iterations
	//	門檻 99（等於關閉）    9、5、3       10、10、6        1／3 場
	//	門檻 3                 3、3、3        4、4、6         0／3 場
	//
	// 「開頭連續等價呼叫」是 turn 起頭那一段**原封不動重送的長度**，不是總 Tool 呼叫
	// 數，也不是守衛計數的最大值（實驗組第 3 場在中間隔了一次別的失敗呼叫之後又送了
	// 一次等價參數，計數到 4，見 loopguard.go）。這三個量在 issue #57 的第一版留言裡
	// 被混為一談過，引用時別再混。
	//
	// **每組三場，樣本小；而且對照組第 3 場沒有守衛也在 3 次之後自己改了途徑。**
	// 這組數字說得出守衛有可測量的影響，說不出它是模型收手的唯一原因。
	//
	// 與 maxToolRetries、maxEmptyResponses 取同一個數字，本專案「重試幾次算夠」的
	// 答案維持一致。
	defaultMaxRepeatedToolFailures = 3
)

// effectiveMaxIterations 回傳迭代上限，零值（含負值）回退預設。預設在讀取點
// 也成立：手組（未經 LoadProfile）的 Profile 不得零輪終止。
func (s Settings) effectiveMaxIterations() int {
	if s.MaxIterations <= 0 {
		return defaultMaxIterations
	}
	return s.MaxIterations
}

// effectiveMaxHistoryTurns 回傳截斷輪數上限，零值（含負值）回退預設。
func (s Settings) effectiveMaxHistoryTurns() int {
	if s.MaxHistoryTurns <= 0 {
		return defaultMaxHistoryTurns
	}
	return s.MaxHistoryTurns
}

// effectiveMaxRepeatedToolFailures 回傳死循環守衛的門檻，零值（含負值）回退預設。
//
// 形狀刻意與上面兩個一模一樣（ticket #54 明訂）：**沒有寫這個欄位的既有 Profile
// 因此完全不必遷移**——零值就是預設值，不是「守衛關閉」。手組、未經 LoadProfile 的
// Profile 也一樣拿得到門檻，理由同 effectiveMaxIterations。
func (s Settings) effectiveMaxRepeatedToolFailures() int {
	if s.MaxRepeatedToolFailures <= 0 {
		return defaultMaxRepeatedToolFailures
	}
	return s.MaxRepeatedToolFailures
}

// effectiveMaxContextRunes 回傳上下文預算，零值（含負值）回退預設。
//
// 形狀與上面三個一模一樣（ticket #48 明訂）：**沒有寫這個欄位的既有 Profile 完全
// 不必遷移**——零值就是預設值，不是「壓縮關閉」。手組、未經 LoadProfile 的 Profile
// 也一樣拿得到預算，理由同 effectiveMaxIterations。
func (s Settings) effectiveMaxContextRunes() int {
	if s.MaxContextRunes <= 0 {
		return defaultMaxContextRunes
	}
	return s.MaxContextRunes
}

// LoadProfile 從 path 讀取並解析 Profile YAML，套用 Settings 預設值並做基礎校驗
// （provider.name、provider.model 必填）。
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取 Profile %s: %w", path, err)
	}

	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("解析 Profile %s: %w", path, err)
	}

	if p.Provider.Name == "" {
		return nil, fmt.Errorf("Profile %s 校驗失敗: provider.name 必填", path)
	}
	if p.Provider.Model == "" {
		return nil, fmt.Errorf("Profile %s 校驗失敗: provider.model 必填", path)
	}
	if err := validateBootstrap(p.Bootstrap); err != nil {
		return nil, fmt.Errorf("Profile %s 校驗失敗: %w", path, err)
	}
	if err := validateSkills(p.Skills); err != nil {
		return nil, fmt.Errorf("Profile %s 校驗失敗: %w", path, err)
	}

	p.Settings.MaxIterations = p.Settings.effectiveMaxIterations()
	p.Settings.MaxHistoryTurns = p.Settings.effectiveMaxHistoryTurns()
	p.Settings.MaxRepeatedToolFailures = p.Settings.effectiveMaxRepeatedToolFailures()
	p.Settings.MaxContextRunes = p.Settings.effectiveMaxContextRunes()
	return &p, nil
}
