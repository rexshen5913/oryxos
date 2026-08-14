package core

import "fmt"

// McpServersFile 是 MCP server 宣告檔在 Workspace 裡的檔名。
//
// 定義在 core 而不是 config：它出現在**使用者看得到的錯誤訊息**裡（「你引用的 server
// 沒有在這個檔案宣告」），而報那個錯的是 core。檔案實際住在 Workspace 的哪個位置仍然
// 只有 config 與組裝點知道，依賴方向不變——同 Bootstrap 檔名常數的位置與理由。
const McpServersFile = "mcp_servers.yaml"

// McpTransportStdio 是核心階段唯一支援的 transport（技術方案 §6.4；SSE 屬後續，
// Demo 三 的場景是本地子進程，沒有驅動它的用例——憲法 3.1）。
//
// 與 McpServersFile 同理放在 core：宣告檔的載入端（internal/config，校驗宣告值）與
// MCP Client（internal/tool，撥號前的前提檢查）都要用它，而那兩個 package 互不依賴。
// 兩邊各寫一份字面字串的話，日後支援 SSE 時會改一邊漏一邊。
const McpTransportStdio = "stdio"

// McpServerSpec 是連上一個外部 MCP server 所需的宣告，由 mcp_servers.yaml 解出
// （internal/config），由 MCP Client 消費（internal/tool）。
//
// 型別放在 core 而不是任一端：它是 config 與 tool **共用的詞彙**，而那兩個 package
// 互不依賴（config → core、tool → core，沒有 config ↔ tool 這條邊）。同 SkillMeta
// 與 Bootstrap 檔名常數的位置與理由——跨 package 共用的詞彙放 core，避免兩邊各寫
// 一份、日後改一邊漏一邊。
//
// core 自己不使用這個型別：MCP 工具抵達 ReAct 循環時已經是 OryxTool，循環不感知
// 工具來自哪裡（憲法 2.1、技術方案 §6.1）。
type McpServerSpec struct {
	// Name 是這個 server 的宣告名。它同時是三樣東西：Profile mcp_servers 欄位要寫
	// 的值、其工具註冊名的前綴（<server>__<tool>）、以及日誌與審計裡的來源標識。
	Name string
	// Transport 是連線方式。核心階段只支援 stdio（技術方案 §6.4；SSE 屬後續，
	// Demo 三 的場景是本地子進程，沒有驅動它的用例——憲法 3.1）。
	Transport string
	// Command 是啟動 server 子進程的 argv：第一個元素是執行檔，其餘是它的參數。
	//
	// 刻意是**字串陣列**而不是一整行命令：一整行就得決定由誰做分詞，而交給 shell
	// 分詞會把引號、含空白的路徑與變數展開的規則一起帶進來，那是命令注入最常見的
	// 入口。argv 沒有這層歧義，OryxOS 也因此不必起 shell。
	Command []string
	// Env 是要額外注入子進程的環境變數，值支援 ${ENV_VAR} 佔位（憑證不明文落檔，
	// 需求 §5.12、技術方案 §8.7）。展開在載入端（internal/config），到這裡已經是
	// 實際值。
	Env map[string]string
}

// McpServerRefs 回傳這個 Profile 引用的 MCP server 名單，並校驗沒有重複。
//
// **語義與 skills 相同、與 bootstrap 刻意不同**：省略或空清單都是「這個 Agent 不接
// 任何 MCP server」，沒有「省略即載入全部」這回事。mcp_servers.yaml 宣告的是**這個
// Workspace 有哪些 server 可用**，不是每個 Agent 都該吃到全部——那一層正是多 Agent
// 隔離的所在，預設放行等於把隔離責任推給使用者的書寫紀律。
//
// 重複列出報錯而不是去重：那是設定筆誤，沿 Registry.Subset 對 tools、validateBootstrap
// 對 bootstrap 的既有語義。靜默去重的代價不只是不一致——重複引用會讓同一個 server 被
// 連兩次、其工具第二次註冊時撞名，錯誤訊息會指向「Tool 已註冊」這個與筆誤無關的地方。
func (p *Profile) McpServerRefs() ([]string, error) {
	seen := make(map[string]struct{}, len(p.McpServers))
	for _, name := range p.McpServers {
		if name == "" {
			return nil, fmt.Errorf("mcp_servers 列出了空的 server 名")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("mcp_servers 重複列出 %q", name)
		}
		seen[name] = struct{}{}
	}
	return p.McpServers, nil
}

// ResolveMcpServers 把 Profile 引用的 server 名對上 mcp_servers.yaml 的宣告，回傳
// **只有被引用到的**那幾份 spec，順序等於引用順序。
//
// 「只回被引用到的」是這條鏈路的關鍵，不是效率上的小聰明：組裝點據此只 spawn 這個
// Agent 真的要用的子進程。`oryxos chat` 一次只跑一個 Profile，連無關的 server 不只
// 多開進程，更糟的是會對**與這個 Agent 無關**的連線失敗發出警示，把真正該看的那條
// 淹掉。（Web Service 同時服務多個 Profile 時要重新定義連線池語義，屬那份 spec。）
//
// 引用了未宣告的 server 是設定錯誤，啟動即報錯——沿本專案既有的一致語義（Subset 對
// 未註冊的 Tool、組裝點對未配置的 Provider 都是 fail fast）。與「server 連不上」的
// 差別是：後者是環境問題（降級並警示），這裡是使用者打錯字。
func ResolveMcpServers(refs []string, declared map[string]McpServerSpec) ([]McpServerSpec, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	specs := make([]McpServerSpec, 0, len(refs))
	for _, name := range refs {
		spec, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("mcp_servers 引用的 %q 未在 %s 宣告", name, McpServersFile)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
