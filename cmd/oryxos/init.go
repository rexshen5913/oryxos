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

// workspaceConfigTemplate 是 Workspace 設定檔：Provider 憑證，加上 Sandbox 的域名與
// 路徑白名單。
//
// **三段白名單都預填 `[]`**（全部拒絕），彼此完全同構：模板給範例註解，程式碼沒有
// 隱含放行。
//
// shell 那段的註解**必須寫滿五句**（ticket #33 的 AC），因為它要擋掉兩種會出事的
// 反推：「只列 echo／cat／ls 就安全」（第 3 句擋它——白名單管的是跑哪個程式，不是
// 那個程式能碰什麼），以及「只要不列直譯器就安全」（第 2 句擋它——find 與 git 都
// **不是**直譯器，卻都能啟動清單外的程式）。
//
// 揭露以**性質**表述，**不寫成一份危險命令清單**：列清單的副作用是讓使用者反推
// 「不在清單上就安全」，而那份清單在定義上窮舉不完。
//
// 考慮過把 file.allowed_paths 預填成整個 Workspace 取「開箱可用」，**不採納**：那之下
// Agent 讀得到 config.yaml（含 base_url 與 ${ENV_VAR} 佔位）、profiles/*.yaml（自己的
// 配置）與 memory/MEMORY.md，而後續的 write_file 更能改寫它們——**Agent 能改自己的
// Profile** 是一個不該預設開啟的性質。收益則近乎零：預設 Profile 根本不列 File Tool，
// 預填的白名單在使用者主動加 Tool 之前完全用不到。
var workspaceConfigTemplate = `# OryxOS Workspace 設定檔。
# 敏感值一律以 ${ENV_VAR} 佔位，載入時從環境變數解析，不明文落檔。
providers:
  openrouter:
    api_key: ${OPENROUTER_API_KEY}
    # OpenRouter 的 OpenAI 兼容端點。改接 DeepSeek、Kimi 等其他端點時要一起改
    # 三處：上面的 provider 名（openrouter:）、api_key、以及這行 base_url，
    # 並讓 profiles/default.yaml 的 provider.name 與那個 provider 名一致。
    base_url: https://openrouter.ai/api/v1

    # 各模型的單價，用來把 token 用量換算成成本寫進審計表（llm_calls.cost_micro_usd）。
    # 三個數字都是「每百萬 token 幾美元」，與各家定價頁上的寫法一致，照抄即可：
    # input 是未命中快取的輸入、output 是輸出、cached_input 是命中提示詞快取的輸入。
    # cached_input 省略時那些 token 按 input 計價（沒有快取折扣的 Provider 就是如此）。
    #
    # **整段省略即不計價**，成本欄位會是空值而不是零——「沒算」與「不用錢」在報表上
    # 必須分得開。這裡刻意不預填任何價格：價格會變，填一個沒查證過的數字，成本報表
    # 會錯得無聲無息，而那比沒有數字更難發現。
    #
    # 要啟用就拿掉每行開頭的「# 」，把模型名與價格改成你實際用的：
    #
` + commentOut(pricingExample) + `
http:
  # HTTP Tool（http_get、http_post）只能存取白名單內的域名，預設全部拒絕。
  # 範例： - api.example.com
  allowed_domains: []

file:
  # File Tool（read_file、write_file、list_dir）只能存取白名單內的路徑，預設全部拒絕。
  #
  # 每條都是**相對這個 Workspace（.oryxos/）根**的路徑，不是相對你執行 oryxos 的目錄
  # ——同一份設定在哪裡跑，允許範圍都一樣。標準化後必須落在其中一條的子樹之內：
  # 白名單寫 work 不會放行 workspace-secrets。
  #
  # 一律拒絕的三種：絕對路徑、用 ../ 穿越出白名單或 Workspace、路徑上任何一段是
  # 符號連結（不跟隨，也不解析後比對）。能力界定在 Workspace 之內。
  #
  # write_file 會**覆寫**目標檔案的全部內容（不是追加），父目錄不存在時報錯而不自動
  # 建立；新建的檔案一律不含執行位，覆寫既有檔案則不改變它原有的權限。
  #
  # list_dir 只列出一層（不遞迴），每個項目給名稱、是否為目錄與大小；條目過多時截斷。
  #
  # 範例： - notes
  allowed_paths: []

shell:
  # shell Tool 只能執行白名單內的程式，預設全部拒絕。比對的是**程式名的字面完全
  # 匹配**（寫 git，不是 /usr/bin/git，也不做萬用字元）。
  #
  # 執行的是**單一程式加參數**，不經 shell 直譯器：沒有管線、沒有重導向、沒有命令
  # 替換，參數裡的 | ; && $() 只是普通字元。要組合多個命令請讓 Agent 分多次呼叫，
  # 要把輸出寫進檔案請用 write_file。
  #
  # 讀這一段之前，先讀懂這五句——它們是這層防護的**實際**邊界：
  #
  # 1. 這一層擋的是「LLM 選錯程式」，不是惡意使用者。核心階段沒有進程級隔離，
  #    不要拿它跑不受信任的內容。
  # 2. 白名單決定 OryxOS **直接啟動哪個程式**，不決定那個程式接下來做什麼。被列入的
  #    程式可以依它自己的參數或配置啟動清單外的程式——find -exec、xargs、git -c、
  #    make 的 recipe 都做得到，而**它們都不是直譯器**。這裡只舉例，不試圖窮舉。
  # 3. 白名單管的是「跑哪個程式」，不是「那個程式能碰什麼」：cat 讀得到任何路徑、
  #    tee 寫得到任何路徑。
  # 4. **file.allowed_paths 不約束 shell。** 上面那段管的是 OryxOS 自己開檔，對子進程
  #    完全無效。要真隔離，把 oryxos 跑在容器裡。
  # 5. **PATH 上的目錄不要與 file.allowed_paths 重疊。** 重疊時 write_file 等於「能新增
  #    或改掉 shell 跑得到的程式」，兩個分開看都合理的授權合起來就是提權。啟動時會就
  #    此印一行警告。
  #
  # 範例： - git
  allowed_commands: []

  # 單次命令的超時上限（秒）。省略、填 0 或負數都回退預設的 30 秒。
  timeout_seconds: 30
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

// pricingExample 是定價段那段範例的**原始 YAML**，與 mcpServersExample 同一個手法：
// 模板裡註解掉的那段由它逐行加前綴產生，所以「拿掉註解就能用」那句話有測試背書
// （見 TestInitPricingTemplate）。
//
// **刻意帶四個空格的前導縮排**：它住在 providers.<name> 之下，使用者拿掉「# 」之後
// 得到的正是該有的縮排，不必自己數。模型名加引號則因為 anthropic/claude-sonnet-4
// 這種帶斜線的字串雖然合法，加了引號才不會在使用者換成含冒號的模型名時突然解析失敗。
const pricingExample = `    pricing:
      "anthropic/claude-sonnet-4":
        input: 3
        output: 15
        cached_input: 0.3
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
