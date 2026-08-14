package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rexshen5913/oryxos/internal/core"
)

// mcpServersFile 是 mcp_servers.yaml 的檔案形狀。
//
// 頂層留一個 mcp_servers key（而不是讓 server 直接掛在根上）與 config.yaml 的
// providers 段同形：日後要在同一份檔案加別的頂層設定時不必改既有寫法。
type mcpServersFile struct {
	McpServers map[string]mcpServerEntry `yaml:"mcp_servers"`
}

// mcpServerEntry 是單一 MCP server 的宣告。server 名是它在 map 裡的 key，不重複
// 寫成欄位——YAML 的 map key 天生唯一，同名重複宣告在解析階段就過不了。
type mcpServerEntry struct {
	Transport string            `yaml:"transport"`
	Command   []string          `yaml:"command"`
	Env       map[string]string `yaml:"env"`
}

// LoadMcpServers 讀取並解析 Workspace 的 MCP server 宣告檔。
//
// **回傳的 env 是原始字串，${ENV_VAR} 佔位還沒展開**——展開由 ExpandMcpServerEnv 在
// Profile 過濾**之後**做。這個分工不是為了分層漂亮，是隔離語義的一部分：宣告檔描述
// 「這個 Workspace 有哪些 server 可用」，一份宣告檔常同時服務多個 Agent。若在這裡就
// 展開全部，一個**沒被當前 Profile 引用**的 server 缺憑證就會擋下啟動——只接 Slack 的
// Agent 會因為機器上沒有 GitHub token 而起不來，連 mcp_servers 省略（完全不用 MCP）的
// Profile 也一樣。那正是 mcp_servers 這一層要消除的耦合。
//
// **檔案不存在時回 nil、不算錯誤**：那是明確的「這個 Workspace 不用 MCP」。`oryxos
// init` 自本票起才建這個模板，spec #1／#2 建立的既有 Workspace 沒有它，照常啟動即
// 免遷移。（檔案存在但解析失敗則相反——那是壞掉的宣告，靜默忽略會讓某個 server 無聲
// 消失，比擋下啟動更難查。錯誤原樣上拋，由組裝點決定怎麼報。**這條與上面那條不衝突**：
// 語法壞掉是整份檔案的問題，缺一個環境變數只是某個 server 的問題。）
func LoadMcpServers(path string) (map[string]core.McpServerSpec, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("讀取 MCP server 宣告檔 %s: %w", path, err)
	}

	var file mcpServersFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("解析 MCP server 宣告檔 %s: %w", path, err)
	}
	if len(file.McpServers) == 0 {
		return nil, nil
	}

	specs := make(map[string]core.McpServerSpec, len(file.McpServers))
	// 名字先排序再逐一校驗：map 的迭代順序是隨機的，不排序的話一份有兩個缺陷的宣告檔
	// 每次會報不同的那一個，使用者修完一個又冒出另一個、還以為自己改壞了。
	for _, name := range slices.Sorted(maps.Keys(file.McpServers)) {
		entry := file.McpServers[name]
		if err := validateMcpServerEntry(name, entry); err != nil {
			return nil, fmt.Errorf("MCP server 宣告檔 %s: %w", path, err)
		}
		specs[name] = core.McpServerSpec{
			Name:      name,
			Transport: entry.Transport,
			Command:   entry.Command,
			Env:       entry.Env,
		}
	}
	return specs, nil
}

// validateMcpServerEntry 校驗單一宣告的**靜態缺陷**。
//
// 這裡與連線失敗的分界是本票的核心判斷，要記明白：
//
//	靜態宣告缺陷（transport 不支援、沒有 command、YAML 壞掉）→ 啟動即報錯
//	環境／執行期問題（子進程起不來、交握逾時、server 中途死掉）→ 降級並警示
//
// 判準是「這個缺陷會不會因為換一台機器、換一個時間就消失」。非 stdio 的 transport 與
// 空的 command 對**任何** Agent 在**任何**環境都不會 work，那是使用者打錯字；而連不上
// 可能只是那台機器上沒裝 node。前者靜默忽略會讓那個 server 無聲消失（spec #3 的
// Failure and Fallback 明列這條），後者擋下啟動則會讓一個外部依賴掛掉就癱瘓整個 Agent。
//
// **校驗在載入端、對宣告檔裡的每一份做，不管當前 Profile 有沒有引用它**。與憑證展開
// （ticket #21 特意搬到 Profile 過濾之後）刻意相反：缺一個環境變數只是「這台機器上還
// 沒設定」，一份宣告檔常同時服務多個 Agent，不該讓只接 Slack 的 Agent 因為缺 GitHub
// token 起不來；而 transport: sse 是宣告檔自己壞了，那是所有 Agent 共用的那份東西。
func validateMcpServerEntry(name string, entry mcpServerEntry) error {
	if entry.Transport != "" && entry.Transport != core.McpTransportStdio {
		return fmt.Errorf("server %q 宣告的 transport %q 不支援：核心階段只支援 %s"+
			"（省略這個欄位即為 %s）",
			name, entry.Transport, core.McpTransportStdio, core.McpTransportStdio)
	}
	if len(entry.Command) == 0 {
		return fmt.Errorf("server %q 沒有宣告 command，無法啟動它的子進程"+
			"（command 是 argv 陣列，例如 [npx, -y, some-mcp-server]）", name)
	}
	// **只看長度不夠**：`command: [""]` 的長度是 1，會通過長度檢查、然後在 exec.Start
	// 失敗，被降級成一句「環境問題」的警示。但空白的 argv0 跟 command 整個沒寫是同一種
	// 筆誤——它換幾台機器都一樣壞，屬於靜態宣告缺陷那一類，該在這裡擋。
	if strings.TrimSpace(entry.Command[0]) == "" {
		return fmt.Errorf("server %q 的 command 第一個元素是空的，沒有可執行的程式"+
			"（第一個元素是執行檔，其餘是它的參數）", name)
	}
	return nil
}

// ExpandMcpServerEnv 把 specs 的 env 值裡的 ${ENV_VAR} 佔位以環境變數展開，回傳展開後
// 的新 spec（需求 §5.12、技術方案 §8.7：憑證統一載入、不明文寫死在設定檔裡）。
//
// **只展開傳進來的那幾份**，所以呼叫端要先用 Profile 的 mcp_servers 過濾過（見
// core.ResolveMcpServers）：一個沒被引用的 server 缺憑證不該影響這個 Agent 能不能啟動。
//
// 佔位解析沿用 providers 段既有的同一份 resolveEnv：兩處對「什麼是佔位、未設定時怎麼
// 報」不該各有一套。錯誤訊息指名到 mcp_servers.<server>.env.<KEY>，使用者不必自己去猜
// 是哪一行。
//
// **只有 env 走佔位展開，command 不展開**：command 是要執行的程式與參數，讓環境變數
// 決定執行什麼會把「設定檔說了什麼」與「當下環境是什麼」混在一起——那是最不該有歧義
// 的地方。需求文檔點名的也只有憑證（憲法 3.1）。
//
// 回傳新的 map 而不是原地改：spec 是從宣告檔那份 map 複製出來的值，但 Env 是 map、
// 底層是共用的。原地展開會把展開後的憑證寫回宣告來源，讓「原始宣告」與「展開結果」
// 變成同一份東西——日後任何想重讀原始佔位的人都會拿到已展開的值。
func ExpandMcpServerEnv(specs []core.McpServerSpec) ([]core.McpServerSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	expanded := make([]core.McpServerSpec, 0, len(specs))
	for _, spec := range specs {
		if len(spec.Env) > 0 {
			env := make(map[string]string, len(spec.Env))
			for key, val := range spec.Env {
				resolved, err := resolveEnv(val)
				if err != nil {
					return nil, fmt.Errorf("mcp_servers.%s.env.%s: %w", spec.Name, key, err)
				}
				env[key] = resolved
			}
			spec.Env = env
		}
		expanded = append(expanded, spec)
	}
	return expanded, nil
}
