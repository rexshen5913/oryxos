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
// 引用未註冊或重複列出的 Tool 都報清晰錯誤（配置筆誤不靜默）；logger 用於落
// 每次 Tool 呼叫的結構化日誌。
func (r *Registry) Subset(names []string, logger *slog.Logger) (*Executor, error) {
	sub := make(map[string]OryxTool, len(names))
	ordered := make([]string, 0, len(names))
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
