package tool

import (
	"fmt"
	"log/slog"
)

// Registry 統一管理所有 Tool：啟動時顯式註冊（憲法 2.3），Profile 啟動 Agent 時
// 按 tools 欄位過濾出可用子集。
type Registry struct {
	tools map[string]OryxTool
}

// NewRegistry 建立空的 ToolRegistry。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]OryxTool)}
}

// Register 顯式註冊一個 Tool；名稱重複時報錯。
func (r *Registry) Register(t OryxTool) error {
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("Tool %q 已註冊，名稱不得重複", name)
	}
	r.tools[name] = t
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
			return nil, fmt.Errorf("Profile 引用的 Tool %q 未註冊", name)
		}
		if _, dup := sub[name]; dup {
			return nil, fmt.Errorf("Profile 的 tools 重複列出 %q", name)
		}
		sub[name] = t
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
		sub[name] = t
		ordered = append(ordered, name)
	}
	return &Executor{names: ordered, tools: sub, logger: logger}, nil
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
