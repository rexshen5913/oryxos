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
}

const (
	defaultMaxIterations   = 10
	defaultMaxHistoryTurns = 20
	// defaultMaxRepeatedToolFailures 是死循環守衛的預設門檻。
	//
	// 3 是在兩個方向之間取的：**大到**讓 LLM 有機會自行修正（換個路徑、補個欄位
	// 常常第二次就對了，門檻 2 會在正常的試錯上誤觸發），**小到**在預設的 10 次
	// iteration 上限內還留得下七次讓它換路——提示來得太晚就沒有意義了。
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
	return &p, nil
}
