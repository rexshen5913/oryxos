package tool

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// maxSuggestedTools 是「未註冊」的錯誤訊息裡最多列幾個可用名字。
//
// 需要上限是因為工具很多：一台 `@modelcontextprotocol/server-github` 就 26 個，接兩台
// 就是 50 多個名字。訊息長到讀不完等於沒列，反而把真正的錯誤沖淡。超出的部分只報總數，
// 完整清單交給 `oryxos tools`。
const maxSuggestedTools = 20

// ToolInfo 是一個已註冊 Tool 對外可見的資訊。
//
// 刻意不是 OryxTool：列出來是給人看的，呼叫端不需要（也不該順手拿到）Execute。
// 能不能執行由 Subset 的產物 Executor 決定，那條白名單語義不因為多了查詢途徑而放寬。
type ToolInfo struct {
	Name        string
	Description string
	// Server 是這個 Tool 來自哪一台 MCP server；內建 Tool 與原生 Go Tool 為空字串。
	//
	// **來源是註冊時記下來的，不是從名字反推的。** server 名沒有任何字元限制（見
	// config.validateMcpServerEntry：只擋 transport 與 command），所以 `foo` 與
	// `foo__bar` 可以同時宣告——此時 `foo__bar__echo` 這個註冊名用雙底線去切會得到
	// 兩種都說得通的答案。猜錯的後果在輸出上看不出來：使用者會以為某個工具是別台
	// server 給的，照著抄前綴然後繼續失敗。
	Server string
}

// registeredTool 是 Registry 內部的一筆註冊：Tool 本身，加上它從哪裡來。
type registeredTool struct {
	tool   OryxTool
	server string
}

// Registry 統一管理所有 Tool：啟動時顯式註冊（憲法 2.3），Profile 啟動 Agent 時
// 按 tools 欄位過濾出可用子集。
type Registry struct {
	tools map[string]registeredTool
}

// NewRegistry 建立空的 ToolRegistry。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]registeredTool)}
}

// Register 顯式註冊一個 Tool；名稱重複時報錯。
func (r *Registry) Register(t OryxTool) error {
	return r.register(t, "")
}

// RegisterMcpTool 註冊一個來自 MCP server 的 Tool，並記下它是哪一台給的。
//
// 與 Register 分開而不是加一個參數：內建 Tool 與原生 Go Tool 沒有「來源 server」這回事，
// 讓每個既有呼叫點多傳一個空字串只會讓那些地方看起來像漏填了什麼。
func (r *Registry) RegisterMcpTool(t OryxTool, server string) error {
	return r.register(t, server)
}

func (r *Registry) register(t OryxTool, server string) error {
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("Tool %q 已註冊，名稱不得重複", name)
	}
	r.tools[name] = registeredTool{tool: t, server: server}
	return nil
}

// Subset 按 Profile 的 tools 欄位過濾出可用子集（欄位過濾，不是 Tool Policy）。
// logger 用於落每次 Tool 呼叫的結構化日誌。
//
// names 是**使用者手寫**的 tools 欄位：引用未註冊的 Tool、或在同一份 tools 裡重複
// 列出，都報清晰錯誤（配置筆誤不靜默）。
//
// autoIncluded 是**由其他配置推導出來**的 Tool，合併進子集且**冪等**——重複不報錯。
// 目前唯一的來源是 load_skill：Profile 的 skills 非空時它必須可用，否則宣告了
// `skills:` 卻忘記帶 load_skill 的 Profile 會安靜退化成「LLM 看得到描述、永遠載不到
// 正文」，正是漸進揭露這條鏈路最該避免的失敗形態。
//
// 兩者的重複語義刻意相反，因為來源不同：使用者在同一份 tools 裡寫兩次是筆誤，該
// 報錯；而「使用者顯式列了 load_skill」與「系統依 skills 推導出 load_skill」撞在
// 一起是**必然會發生**的正常情況，報錯等於懲罰把設定寫清楚的人。
//
// 這仍然是顯式註冊（憲法 2.3）：觸發條件是使用者自己寫的 skills 欄位，不是反射或
// 型別掃描。Tool 本身也仍要先 Register 進 Registry 才推導得到。
func (r *Registry) Subset(names, autoIncluded []string, logger *slog.Logger) (*Executor, error) {
	sub := make(map[string]OryxTool, len(names)+len(autoIncluded))
	ordered := make([]string, 0, len(names)+len(autoIncluded))
	for _, name := range names {
		t, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("Profile 引用的 Tool %q 未註冊%s", name, r.suggest(name))
		}
		if _, dup := sub[name]; dup {
			return nil, fmt.Errorf("Profile 的 tools 重複列出 %q", name)
		}
		sub[name] = t.tool
		ordered = append(ordered, name)
	}
	for _, name := range autoIncluded {
		if _, already := sub[name]; already {
			continue // 冪等合併：使用者也列了同一個，不是錯誤
		}
		t, ok := r.tools[name]
		if !ok {
			// 推導出來的 Tool 沒註冊是**組裝點的 bug**，不是使用者的設定錯誤——
			// 訊息要指向前者，免得使用者去 tools 欄位找一個他沒寫過的名字。
			return nil, fmt.Errorf("自動加入的 Tool %q 未註冊（組裝點漏了 Register）", name)
		}
		sub[name] = t.tool
		ordered = append(ordered, name)
	}
	return &Executor{names: ordered, tools: sub, logger: logger}, nil
}

// All 回傳全部已註冊的 Tool，按名字排序。
//
// 排序而不是照註冊順序：這是給人讀的清單，找一個名字在不在裡面比「誰先註冊」有用得多。
//
// 它不放寬任何東西——Registry 本來就持有全部 Tool，能不能執行仍由 Subset 決定。補這個
// 方法是因為「Profile 的 tools 該寫什麼」在此之前**沒有任何查詢途徑**（issue #27）：
// 使用者只能翻該 server 的文件、自己寫腳本探、或亂寫一個等報錯。
func (r *Registry) All() []ToolInfo {
	all := make([]ToolInfo, 0, len(r.tools))
	for name, t := range r.tools {
		all = append(all, ToolInfo{Name: name, Description: t.tool.Description(), Server: t.server})
	}
	slices.SortFunc(all, func(a, b ToolInfo) int { return strings.Compare(a.Name, b.Name) })
	return all
}

// suggest 為一個未註冊的名字產出「目前可用的是這些」的線索，接在錯誤訊息後面。
//
// **範圍跟著來源走**：MCP 工具的註冊名是 `<server>__<tool>`，所以打錯工具名時只列同一台
// server 的——接兩台 26 工具的 server 就是 50 多個名字，全倒出來讀不完，等於沒列。
// 認不出屬於哪一台（server 名打錯或記錯）才列全部，那時使用者需要看到的正是「有哪些
// server」。
//
// **歸屬用已註冊的 server 名去比、取最長的那個，不是把名字切在第一個雙底線上。**
// server 名可以含雙底線，`foo` 與 `foo__bar` 能同時存在；切第一個的話 `foo__bar__echo`
// 會被算成 foo 的，於是建議清單裡混進另一台的工具——使用者照抄之後繼續失敗，而訊息
// 看起來還很篤定。已註冊的 server 名是一個已知集合，拿它來比就沒有歧義。
//
// 沒有任何 Tool 註冊時回空字串：硬接一句「目前可用：」後面空空的，比不說更讓人困惑。
func (r *Registry) suggest(missing string) string {
	all := r.All()
	if len(all) == 0 {
		return ""
	}

	var server string
	for _, info := range all {
		if info.Server == "" || !strings.HasPrefix(missing, info.Server+McpToolSeparator) {
			continue
		}
		if len(info.Server) > len(server) {
			server = info.Server
		}
	}

	scope := "目前已註冊"
	candidates := all
	if server != "" {
		sameServer := make([]ToolInfo, 0, len(all))
		for _, info := range all {
			if info.Server == server {
				sameServer = append(sameServer, info)
			}
		}
		scope = "MCP server " + server + " 目前提供"
		candidates = sameServer
	}

	names := make([]string, 0, min(len(candidates), maxSuggestedTools))
	for _, info := range candidates[:min(len(candidates), maxSuggestedTools)] {
		names = append(names, info.Name)
	}
	listed := strings.Join(names, "、")
	if len(candidates) > len(names) {
		listed += fmt.Sprintf("…等共 %d 個", len(candidates))
	}
	return fmt.Sprintf("；%s：%s", scope, listed)
}

// RegisterBuiltins 顯式註冊本 package 自帶的內建 Tool：HttpTools（http_get、
// http_post），File／Shell Tool 隨其模組於後續 ticket 加入。
//
// Memory Tool（save_memory 等）不在此註冊：它們住在 internal/memory，且需要
// Workspace 的 MEMORY.md 路徑，而 internal/tool 不該知道 Workspace 的檔案佈局。
// 由組裝點顯式 Register 進同一個 Registry（憲法 2.3 要的是顯式，不是單一函式）。
func RegisterBuiltins(r *Registry, checker *SandboxChecker) error {
	for _, t := range []OryxTool{NewHTTPGet(checker), NewHTTPPost(checker)} {
		if err := r.Register(t); err != nil {
			return fmt.Errorf("註冊內建 Tool: %w", err)
		}
	}
	return nil
}
