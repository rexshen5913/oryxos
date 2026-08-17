// init.go 實作 `oryxos init`：在當前目錄建立 Workspace（.oryxos/）。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/core"
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
		{core.McpServersFile, mcpServersTemplate},
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
  mcp_servers.yaml（外部 MCP server 宣告，預設不宣告任何一個）

三份 Bootstrap 檔案刻意留空——它們的內容會逐字注入 Agent 的系統提示詞，寫什麼
Agent 就照什麼做（載入順序與覆蓋語義見 docs/adr/0003）：

  AGENTS.md  這個專案怎麼做事：慣例、流程、禁忌
  USER.md    你的偏好：語言、輸出風格、常用約定
  SOUL.md    Agent 的人格與語氣（Profile 設了 identity.prompt 時本檔不載入，兩者互斥）

留空即代表該層沒有內容，對話一切照常。

下一步：設定環境變數 OPENROUTER_API_KEY，並依需求編輯 config.yaml 與 profiles/default.yaml。
`, workspaceDir)
	return nil
}

// Bootstrap 檔案模板：由使用者手寫、OryxOS 只讀不寫。
//
// **三份一律建成空檔**，這是刻意的：它們的內容會被**逐字注入每個 turn 的
// system prompt**（ADR-0003），所以檔案裡放任何說明文字，LLM 都會當成真的專案
// 慣例／使用者偏好／人格定義來遵循。最糟的形態是 Profile 沒設 identity.prompt
// 時，SOUL.md 的說明文字會直接變成 Agent 的整個人格。
//
// 「這幾份檔案是做什麼的」屬於**給人看的說明**，因此放在 init 的輸出訊息裡
// （見 initWorkspace 的下一步提示），不放進會被送往 Provider 的檔案。
const (
	agentsTemplate = ""
	soulTemplate   = ""
	userTemplate   = ""
)

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

// mcpServersExample 是宣告檔模板裡那段範例的**原始 YAML**。
//
// 它與模板分開存在，是為了讓測試能真的把它餵給載入器（見 TestInitMcpServersTemplate）。
// 模板裡那段是由它逐行加 `# ` 產生的，所以「註解掉的範例」與「測得到的 YAML」保證是
// 同一份東西——模板叫使用者「拿掉註解就能用」，那句話因此有測試背書。
//
// 注意 `@modelcontextprotocol/...` 一定要加引號：`@` 是 YAML 的保留指示字元，不能當
// 純量的開頭，不加引號整份檔案會解析失敗。這正是需要測試盯著的那類細節。
const mcpServersExample = `mcp_servers:
  github:
    transport: stdio
    # command 是 argv：第一個是執行檔，其餘是它的參數（不經 shell，不必處理引號）
    command: [npx, -y, "@modelcontextprotocol/server-github"]
    env:
      GITHUB_TOKEN: ${GITHUB_TOKEN}
`

// mcpServersTemplate 是外部 MCP server 的宣告檔模板。
//
// **範例一律留在註解裡、實際宣告留空**：這個檔案宣告的每個 server 都會被 OryxOS 起成
// 子進程，模板裡放一個「看起來可以跑」的宣告，等於預設幫使用者執行一個他沒讀過的
// 命令。空宣告加註解則是零副作用的引導。
//
// 空宣告與「檔案不存在」的效果相同（都是沒有任何 MCP server），所以既有 Workspace
// 不補這個檔也照常跑——這裡建它是為了讓使用者知道這個能力存在、以及格式長什麼樣。
var mcpServersTemplate = `# OryxOS 外部 MCP server 宣告檔（Plugin Tool 方式二）。
# 這裡宣告的是「這個 Workspace 有哪些 server 可用」；某個 Agent 要接哪幾個，由它的
# Profile 以 mcp_servers 欄位引用（沒被引用的宣告不會被連線、也不會起子進程）。
#
# 工具會以 <server>__<tool>（雙底線）註冊，Profile 的 tools 欄位就用這個名字引用。
# 例如下面的 github 宣告了 search_pr 工具，Profile 要寫 github__search_pr。
#
# 不知道某台 server 提供哪些 <tool>？宣告好之後執行 oryxos tools：它會連上 Profile
# 引用的 server、把工具名與用途列出來，直接照抄進 tools 欄位即可。
#
# 敏感值一律以 ${ENV_VAR} 佔位，載入時從環境變數解析，不明文落檔。
# 核心階段只支援 transport: stdio。
#
# 範例（拿掉每行開頭的「# 」並改成你自己的 server）：
#
` + commentOut(mcpServersExample) + `
mcp_servers: {}
`

// commentOut 把每一行加上 `# ` 前綴（空行只加 `#`，不留尾隨空白）。
func commentOut(yaml string) string {
	lines := strings.Split(strings.TrimRight(yaml, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = "#"
			continue
		}
		lines[i] = "# " + line
	}
	return strings.Join(lines, "\n") + "\n"
}
