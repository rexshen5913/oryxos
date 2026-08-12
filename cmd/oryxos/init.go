// init.go 實作 `oryxos init`：在當前目錄建立 Workspace（.oryxos/）。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// workspaceDir 是 Workspace 的目錄名，建立於執行 init 的當前目錄下。
const workspaceDir = ".oryxos"

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "在當前目錄建立 OryxOS Workspace（.oryxos/）",
		Long: "建立 OryxOS 的標準 Workspace：五個子目錄（profiles/、sessions/、skills/、\n" +
			"memory/、logs/）、三個 Bootstrap 檔案模板（AGENTS.md、SOUL.md、USER.md）、\n" +
			"預設 Profile 與 Workspace 設定檔。偵測到既有 .oryxos/ 時不覆蓋任何檔案。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("取得當前目錄: %w", err)
			}
			return initWorkspace(cmd.OutOrStdout(), cwd)
		},
	}
}

// initWorkspace 在 baseDir 下建立完整的 Workspace；若 .oryxos 已存在（不論目錄或檔案），
// 提示後直接返回，不覆蓋任何既有內容。
func initWorkspace(out io.Writer, baseDir string) error {
	ws := filepath.Join(baseDir, workspaceDir)
	if _, err := os.Lstat(ws); err == nil {
		fmt.Fprintf(out, "%s 已存在，未變更任何檔案；如要重新初始化，請先自行移除它。\n", workspaceDir)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("檢查 %s: %w", workspaceDir, err)
	}

	for _, sub := range []string{"profiles", "sessions", "skills", "memory", "logs"} {
		if err := os.MkdirAll(filepath.Join(ws, sub), 0o755); err != nil {
			return fmt.Errorf("建立 Workspace 目錄 %s: %w", filepath.Join(workspaceDir, sub), err)
		}
	}

	files := []struct {
		path    string
		content string
	}{
		{"AGENTS.md", agentsTemplate},
		{"SOUL.md", soulTemplate},
		{"USER.md", userTemplate},
		{filepath.Join("profiles", "default.yaml"), defaultProfileTemplate},
		{"config.yaml", workspaceConfigTemplate},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(ws, f.path), []byte(f.content), 0o644); err != nil {
			return fmt.Errorf("寫入 %s: %w", filepath.Join(workspaceDir, f.path), err)
		}
	}

	fmt.Fprintf(out, `已在 %s/ 建立 OryxOS Workspace：

  profiles/ sessions/ skills/ memory/ logs/
  AGENTS.md、SOUL.md、USER.md（Bootstrap 模板，由你手寫、OryxOS 只讀不寫）
  profiles/default.yaml（預設 Profile）
  config.yaml（Workspace 設定檔）

下一步：設定環境變數 OPENROUTER_API_KEY，並依需求編輯 config.yaml 與 profiles/default.yaml。
`, workspaceDir)
	return nil
}

// Bootstrap 檔案模板：由使用者手寫、OryxOS 只讀不寫（本切片只建立、不載入）。
const agentsTemplate = `# AGENTS.md — 專案級行為說明

由你手寫、OryxOS 只讀不寫。描述這個專案怎麼做事：慣例、流程、禁忌。
內容之後會載入 Agent 的系統提示詞；留空亦可。
`

const soulTemplate = `# SOUL.md — 預設 Agent 人格定義

由你手寫、OryxOS 只讀不寫。定義 Agent 的人格與語氣。
注意：若 Profile 已設定 identity.prompt，則以其為準，本檔不載入（兩者互斥）。
`

const userTemplate = `# USER.md — 使用者偏好

由你手寫、OryxOS 只讀不寫。記錄你的偏好：語言、輸出風格、常用約定等。
`

// defaultProfileTemplate 是最簡可用的預設 Profile：填入 API key（見 config.yaml）即可對話。
const defaultProfileTemplate = `# OryxOS 預設 Profile。Agent 由 Profile 配置出來，一個 Profile 對應一個 Agent。
# 敏感值（API key 等）不寫在這裡，統一放 config.yaml 並以環境變數佔位。
name: default
description: OryxOS 預設 Agent
identity:
  agent_name: Oryx
  prompt: 你是 Oryx，一個樂於助人的通用助理。回答力求精確、直接。
provider:
  name: openrouter                   # 引用 config.yaml providers 段的 provider name
  model: deepseek/deepseek-v4-flash  # 模板預設值；可改為 OpenRouter 上任何模型，或改 config.yaml 的 base_url 接別的 OpenAI 兼容端點
  temperature: 0.7
tools:
  - http_get
  - http_post
  - save_memory       # 長期記憶：Agent 自主把值得記住的偏好或事實寫進 memory/MEMORY.md
  - recall_memory     # 長期記憶：以關鍵詞檢索 memory/MEMORY.md，取回匹配的記憶行
settings:
  max_iterations: 10      # ReAct 循環最大迭代次數
  max_history_turns: 20   # 對話歷史保留的近期輪數
`

// workspaceConfigTemplate 是 Workspace 設定檔：Provider 憑證與 HTTP Tool 域名白名單。
const workspaceConfigTemplate = `# OryxOS Workspace 設定檔。
# 敏感值一律以 ${ENV_VAR} 佔位，載入時從環境變數解析，不明文落檔。
providers:
  openrouter:
    api_key: ${OPENROUTER_API_KEY}
    # OpenRouter 的 OpenAI 兼容端點。改接 DeepSeek、Kimi 等其他端點時要一起改
    # 三處：上面的 provider 名（openrouter:）、api_key、以及這行 base_url，
    # 並讓 profiles/default.yaml 的 provider.name 與那個 provider 名一致。
    base_url: https://openrouter.ai/api/v1

http:
  # HTTP Tool（http_get、http_post）只能存取白名單內的域名，預設全部拒絕。
  # 範例： - api.example.com
  allowed_domains: []
`
