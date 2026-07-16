# 專案篇 OryxOS：AI 編程指南

> 本文檔定義 OryxOS 的 AI 編程實施思路。主體思路是用 Spec-Kit 完成主體開發，把已有的需求文檔和技術方案餵給 Spec-Kit，按五大核心能力拆成 5 個 user story 逐步實施；後續增量階段切換到手動提示詞配合 Claude Code。前置閱讀《專案篇 OryxOS 業界調研》《OryxOS 需求文檔》《OryxOS 技術方案》。本文檔講思路和拆解方法，不綁定具體時間安排，也不展開提示詞細節。
>
> 本文檔以最新技術方案為準：核心階段交付的是 Agent OS 的執行時內核，工程結構為 Go 單一 module + `internal/` 分包（8 個 package + `cmd/oryxos`，技術方案第 10 章），五大核心能力（對接 LLM、ReAct、Memory、Tool、Web Service）作為 5 個 user story 的骨架。

**目錄**

1. 實施總覽
2. Spec-Kit 跟 OryxOS 的匹配度評估
3. 準備階段
4. 基於 Spec-Kit 的實施拆解
5. 專案交付物
6. 增量階段：手動提示詞模式
7. 風險和注意事項
8. 總結

---

## 1. 實施總覽

### 1.1 主體思路：Spec-Kit 加手動提示詞的混合模式

OryxOS 的 AI 編程實施分兩個階段，兩個階段用不同的協作工具。

主體開發階段：從零開發 OryxOS 1.0 的五大核心能力。整個專案是 Go 單一 module + `internal/` 分包（8 個 package + `cmd/oryxos`）、package 邊界清晰、需求文檔和技術方案完整。用 Spec-Kit 跑完整的 spec-driven 流程，constitution 加 specify 加 plan 加 tasks 加 implement，保證產出的程式碼跟需求文檔對齊，避免 vibe coding 跑偏。

增量開發階段：擴展功能、修 bug、加 Plugin Tool 這些都是小顆粒度增量，每個增量 1 到 3 個檔案改動。切換到手動提示詞配合 Claude Code，因為 Spec-Kit 跑一次完整流程對小增量來說工程開銷過大。

這兩個階段的邊界很清晰：Spec-Kit 適合大顆粒度 greenfield，手動提示詞適合小顆粒度增量。OryxOS 主體開發是前者，社區接力是後者，工具選擇跟工作性質匹配。

### 1.2 跟需求文檔加技術方案的關係

實施指引不重寫需求和技術方案，而是把已有文檔餵給 Spec-Kit。具體對應關係：

- 需求文檔是 Spec-Kit 的 `/speckit.specify` 輸入。Spec-Kit 把需求文檔轉成 5 個 user story 的 spec（對應五大核心能力）
- 技術方案是 Spec-Kit 的 `/speckit.plan` 輸入。Spec-Kit 把技術方案轉成模組化的實施 plan
- 需求文檔第 3 章設計目標加技術方案第 1.1 節關鍵技術決策一起組成 `constitution.md`（非協商原則）
- 需求文檔第 13 章 5 個驗收 demo 直接作為 5 個 user story 的 acceptance criteria

已有文檔的投入不浪費，Spec-Kit 只是把它們轉換成 AI agent 能直接消費的格式。這裡有一個要點：技術方案是 `/speckit.plan` 的輸入，所以 plan 裡的 package 佈局必須跟技術方案第 10 章的 Go 工程結構（8 個 internal package + `cmd/oryxos`）一致，餵文檔時確保用的是最新版技術方案，否則生成的 plan 會按錯誤的分包結構拆分。

### 1.3 拆解策略：按 user story 拆，不按時間拆

整個 OryxOS 主體開發按 5 個 user story 組織，每個對應一個核心能力。

5 個 user story 不是平行的，它們之間有明確的依賴關係，依賴關係決定推進順序：

| User Story | 核心能力 | 依賴 | 對應驗收 demo |
| --- | --- | --- | --- |
| US-1 | 對接 LLM | 無（基礎） | （與 US-2 合併）Demo 一 |
| US-2 | ReAct 循環 | US-1 | Demo 一（查天氣穿衣） |
| US-3 | Memory 三層記憶 | US-2 | Demo 二（跨對話記偏好） |
| US-4 | Plugin Tool 體系 | US-2（與 US-3 並行） | Demo 三（零程式碼 PR digest） |
| US-5 | Web Service | 前 4 個 | Demo 四加五（同步呼叫、多端點聯動） |

依賴關係展開：

- US-1 是基礎，沒有 LLM 呼叫所有 Agent 能力都跑不起來；
- US-2 依賴 US-1，ReAct 循環每輪都要調 LLM；
- US-3 加 US-4 並行依賴 US-2（Memory 注入 ReAct 的 prompt，Tool 被 ReAct 呼叫）；
- US-5 依賴前 4 個，對外暴露所有能力。

推進順序：US-1 到 US-2 到（US-3 加 US-4 並行）到 US-5。具體推進的時間投入由專案方根據團隊情況決定。本文檔按依賴順序拆，不規定時長。需求文檔和技術方案定的核心階段節奏是 4 週每週 3 小時，5 個 user story 跟這個節奏的對應關係見技術方案第 12 章，本文檔不重複。這裡只規定推進的邏輯順序。

需要說明，這套 user story 拆法跟 Spec-Kit 的機制天然契合。Spec-Kit 的 `/speckit.tasks` 命令本身就是按 user story 組織任務的，每個 user story 成為一個獨立的實施 phase，任務之間按依賴排序、可並行的標記出來。OryxOS 按五大核心能力拆成 5 個 user story，正好順著 Spec-Kit 的工作方式。

---

## 2. Spec-Kit 跟 OryxOS 的匹配度評估

寫實施計劃之前先回答一個根本問題：Spec-Kit 真的適合 OryxOS 專案嗎？這個判斷決定整個實施路徑，必須先講清楚。

### 2.1 Spec-Kit 適合什麼場景

Spec-Kit 是 GitHub 開源的 spec-driven development 工具鏈，是 2026 年增長最快的開發者工具之一，社區採用度很高，支援二十多種 AI coding agent。它把 AI 輔助編碼結構化成可重複的 specify 到 plan 到 tasks 到 implement 流程，核心理念是讓 spec 成為程式碼行為的契約和單一事實來源，把 AI agent 從"程式碼生成器"變成"按規格幹活的協作者"，對治 vibe coding。

社區共識它適合的場景：

- medium 到 large greenfield 專案（從零開發，工程量中到大，模組跨多個檔案夾）；
- 需求清晰（上游有明確的需求文檔或產品決策）；
- AI agent 協作（用 Claude Code、Copilot、Cursor 等做主體開發）；
- 方法論場景（強制 spec-driven 流程，團隊能學到工程方法論）。

不適合的場景：小 feature、快速原型、單檔案改動（流程開銷大於收益）；大型 brownfield 專案改造（legacy 程式碼上下文太複雜，超出 LLM context limit）；探索性研究專案（需求未定就跑 spec 會反覆返工）。

### 2.2 OryxOS 的匹配度判斷

對照 Spec-Kit 適合的場景逐條評估 OryxOS：

- greenfield 上，OryxOS 是從零開發的全新專案不是改造現有程式碼，完全匹配；
- 規模上，Go 單一 module + `internal/` 分包（8 個 package + `cmd/oryxos`）、五大核心能力清晰，是典型的 medium 規模，完全匹配；
- 需求清晰上，已有完整的需求文檔加技術方案，五大核心能力都有 user story 級別的描述，完全匹配；
- AI agent 協作上，OryxOS 本來就是用 Claude Code 做主體開發，完全匹配；
- 方法論場景上，Spec-Kit 的強制流程讓產出對齊需求，對開發者掌握工程方法論很有價值。

結論：Spec-Kit 是 OryxOS 主體開發的最佳工具選擇。社區在 brownfield 專案上對 Spec-Kit 有爭議，但 OryxOS 是純 greenfield，這些爭議不適用。

### 2.3 Spec-Kit 的局限和應對

Spec-Kit 有幾個公認的局限，要提前知道應對方案。

局限一，流程對小增量過重。Spec-Kit 的完整流程對小改動開銷過大，業內有些團隊為此在小增量上轉向更輕量的方式。應對：OryxOS 主體開發是大顆粒度（8 個 package 加 cmd/oryxos 同時建），開銷分攤到所有 package 上是合理的，增量階段的小改動切換到手動提示詞，這正是本文檔第 6 章講的兩階段工具切換的核心理由。

局限二，spec 不會自動跟實現同步。如果 AI agent 在 implement 階段偏離了 spec，spec 檔案本身不會更新。應對：每個 user story 實施完成後跑 `/speckit.analyze` 做跨 artifact 一致性檢查，發現漂移立刻修正。analyze 是 Spec-Kit 專門用來防漂移的命令，官方建議在 tasks 之後、implement 之前跑。

局限三，context limit 在大型 brownfield 上失效。十萬級檔案的 legacy 專案 LLM 看不全。應對：OryxOS 是純 greenfield，整個專案所有程式碼加起來還在 LLM context window 內，這個局限不適用。

局限四，Spec-Kit 本身在快速迭代。命令名、artifacts 格式、集成方式都還在變（比如 Claude Code 的集成已經從早期形態演進到 skills 模式）。應對：本文檔不鎖定 Spec-Kit 具體版本的細節，主線講思路加節奏，具體命令和安裝方式以實施時官方文檔為準。

---

## 3. 準備階段

準備階段是正式實施前的腳手架工作，由專案方完成，產出三份 Spec-Kit artifacts（constitution、spec、plan），讓後續每個 user story 的實施都有清晰的依據。

### 3.1 Spec-Kit 安裝加 Claude Code 配置

Specify CLI 是 Spec-Kit 的入口工具（Python 實現，需要 Python 3.11 以上，推薦用 uv 安裝）。安裝後通過 specify init 初始化 OryxOS 專案的 Spec-Kit 工作區，工作區裡有 .specify/memory/constitution.md 以及 spec、plan、tasks 等 artifacts 的目錄結構。

Claude Code 是主推的 AI agent，Spec-Kit 官方支援 Claude Code。具體集成方式（早期是 slash 命令，現在 Claude Code 走 skills 模式，初始化時通過參數指定）以官方文檔為準。本文檔不展開安裝步驟細節（隨版本變化），實施前給一份環境準備 checklist 即可。

### 3.2 `/speckit.constitution`：寫 OryxOS 專案憲章

`constitution.md` 是專案的 non-negotiable principles，所有後續 spec、plan、tasks、implement 都要遵守。OryxOS 的 constitution 從需求文檔第 3 章設計目標加技術方案第 1.1 節的關鍵技術決策提煉，定為以下原則：

- 原則一：Go 1.24+，單一 module + `internal/` 分包（8 個 package + `cmd/oryxos`），`CGO_ENABLED=0` 靜態編譯成單一二進制部署
- 原則二：五大核心能力（LLM、ReAct、Memory、Tool、Web Service）優先，支撐 package 次之；核心階段交付執行時內核，企業級治理層放擴展階段
- 原則三：自實現 ReAct loop，不直接用任何框架的 Agent 抽象
- 原則四：LLM 客戶端只用 go-openai 做協議轉換與 tool schema，tool 調度完全由 ReActLoop 加 ToolExecutor 控制；不採用任何框架（如 Eino）的自動執行 Agent 抽象。go-openai 天然回傳 raw tool_calls，本就該由自己執行。這條單列，因為它是最容易被 AI agent 寫錯的地方
- 原則五：Plugin Tool 三檔接入，主推 SKILL.md 加 MCP 零程式碼方式；重程式碼方式改為原生 Go Tool（實現介面、顯式註冊、編進二進制）
- 原則六：核心階段 SQLite 加 MEMORY.md 檔案儲存，向量檢索放擴展階段；審計相關的 tool_invocations 和 llm_calls 核心階段就寫入落庫
- 原則七（Go 特有）：SQLite 用純 Go 的 `modernc.org/sqlite` 驅動，`CGO_ENABLED=0`，避免任何 cgo 依賴以守住單一靜態二進制
- 原則八：每個 user story 完成後有可演示 demo，優先級是跑通而非完美

這些原則會在每次 specify 加 plan 加 implement 時被 AI agent 主動引用，保證整個開發過程不偏離 OryxOS 的方向。其中原則四（go-openai 只做協議轉換與 tool schema、tool 調度自己控制的 LLM 客戶端邊界）、原則六（審計 day one 落庫）和原則七（避免 cgo 依賴守單二進制）是相對容易被 AI agent 寫錯、又必須守住的幾條，寫進 constitution 是為了讓 AI agent 每次都看到。

`constitution.md` 寫一次定下來，整個主體開發期間不改。如果中途發現某條原則不對，停下來重新討論，不允許 AI agent 自己修改 constitution。

### 3.3 `/speckit.specify`：把需求文檔轉成 5 個 user story

`/speckit.specify` 命令的輸入是需求文檔，輸出是 5 個 user story 的 spec，每個 user story 對應一個核心能力。

5 個 user story 按依賴關係排推進順序，而不是按重要性。這裡要特別說明：US-5 Web Service 排在最後實施，是因為它依賴前四個能力都就緒，不是因為它不重要。恰恰相反，Web Service 是 OryxOS 區別於個人助手專案的關鍵能力，重要性很高。本文檔不用 P1/P2/P3 這種優先級標記，避免被誤讀成"靠後的可以不做"，只講依賴順序：US-1 到 US-2 是基礎，US-3 和 US-4 可並行，US-5 收口。

每個 user story 的 acceptance criteria 直接複用需求文檔第 13 章 5 個驗收 demo，不重新設計：

- US-1 加 US-2 對應 Demo 一（查天氣穿衣）；
- US-3 對應 Demo 二（跨對話記偏好）；
- US-4 對應 Demo 三（零程式碼 PR digest）；
- US-5 對應 Demo 四加 Demo 五（Web Service 同步呼叫加多端點聯動）。

`/speckit.specify` 執行後生成 `spec.md`，AI agent 據此理解 OryxOS 整體要做什麼。跑完後建議跑一次 `/speckit.clarify`，AI agent 會問幾個澄清問題（比如 max iterations 預設值、對話歷史截斷策略等），這一步可選但推薦。

### 3.4 `/speckit.plan`：把技術方案轉成實施 plan

`/speckit.plan` 命令的輸入是技術方案加上一步的 `spec.md` 加 `constitution.md`，輸出是實施 plan。Plan 包含技術棧選型（Go 1.24+ 加 go-openai 加 SQLite（modernc）加 cobra）、Go 工程結構（8 個 internal package + `cmd/oryxos`）各自的職責（對照技術方案第 10 章）、關鍵技術決策的展開（自實現 ReAct、go-openai 只做協議轉換與 tool schema 的邊界、Plugin Tool 三檔、SQLite 加 MEMORY.md、審計 day one 落庫）、資料流和 package 間協作（PromptBuilder 加 ProviderService 加 ToolExecutor 加 MemoryService 三層門面）。

Plan 生成後人工 review 是必要環節。AI agent 可能根據自己對技術方案的理解做了不該做的取捨，幾個要重點檢查的點：有沒有把 Memory 簡化成跟 Session 合併（應該是 MemoryService 三層統一門面）；有沒有把 Tool 又拆成多個 package（應該是合併的 `internal/tool` 一個 package）；有沒有把 SkillLoader 當成 Tool（它應該歸 core 的 ContextLoader）；有沒有採用框架的自動執行 Agent 抽象（必須自己控制 tool 調度）。Review 通過後 `plan.md` 鎖定。

### 3.5 準備階段交付物清單

準備階段結束時，OryxOS 專案倉庫裡應該有 .specify/memory/constitution.md（原則集）、spec.md（5 個 user story）、plan.md（技術棧加 Go 工程結構（8 個 package + `cmd/oryxos`）加技術決策）、專案原始需求文檔加技術方案文檔（一併放倉庫作為來源參考）、Claude Code 加 Specify CLI 配置說明。準備階段完成後，5 個 user story 的實施依據全部就緒，可以按依賴關係順序推進。

---

## 4. 基於 Spec-Kit 的實施拆解

準備階段把整體 spec 和 plan 都準備好了，下面按 5 個 user story 拆解具體實施。每個 user story 的拆解結構一致：核心目標、涉及的 package、Spec-Kit 任務拆分思路、關鍵 task 顆粒度、驗收 demo。package 名以技術方案第 10 章的 Go 工程結構為準。

### 4.1 US-1：對接 LLM（核心能力一）

核心目標：讓 OryxOS 能調任意主流 LLM，Agent 不感知具體調的是哪家。LLM 呼叫透過 go-openai 接 OpenAI 兼容協議，OryxOS 在它之上自實現一層薄的 Provider 抽象。

涉及的 package：internal/core（OryxTool 介面、Session、Profile、ContextLoader 等核心抽象）、internal/provider（核心能力一）、cmd/oryxos（main 入口與 module 骨架，取代原啟動模組）。

Spec-Kit 任務拆分思路。`/speckit.tasks` 針對 US-1 拆任務，按依賴關係排序，標記可並行任務。預期產出的 task 大類：

- 環境搭建類（Go module 加 internal 分包骨架、go-openai 依賴）；
- 核心抽象類（OryxTool 介面、Profile 資料結構、Message 資料結構）；
- Provider 實現類（ProviderService 實現、provider name 到 adapter 的顯式註冊映射、Function Calling / tool schema 適配）；
- 配置類（YAML 配置檔至少跑通 DeepSeek 或 Kimi 一個 Provider，配合 ConfigLoader 從環境變數加載 API key）。

這裡有一個關鍵點要寫進 task 注意事項：ProviderService 不能靠"掃描所有已註冊 adapter 的型別"來區分 Provider，多 Provider 並存時型別相同會有歧義，必須維護 provider name 到 adapter 的顯式註冊表（技術方案 3.2）。AI agent 很容易寫成型別掃描，要在 task 裡點明——Go 這邊一律用顯式註冊表，不靠反射或型別掃描。

關鍵 task 。Spec-Kit 傾向 1 到 2 個檔案每 task。US-1 大部分 task 符合：各種資料結構定義每個 task 較小，ProviderService 實現可以拆幾個子 task（核心服務加 name 註冊加 Function Calling 適配加配置加載）。US-1 實施完成後不立刻有 demo，因為它沒有使用者可見的入口，下一步 US-2 完成後跟 US-1 一起跑 Demo 一。

### 4.2 US-2：ReAct 循環（核心能力二）

核心目標：實現 Agent 的核心工作機制。即：LLM 思考是否呼叫工具，呼叫之後看結果，再決定下一步，直到給出最終回應。ReAct 循環是 OryxOS 最關鍵的一段程式碼。

涉及的 package：

- internal/core（ReActLoop、PromptBuilder、ToolExecutor、ContextLoader）
- internal/tool（一個 HTTP Tool 加 SandboxChecker 簡化版，Demo 一需要）
- internal/channel/cli（CLI Channel，Demo 一需要）
- cmd/oryxos（oryxos init 加 oryxos chat 命令）。

注意這裡 Tool 相關只有一個 `internal/tool` package（技術方案已把 builtin/skill/mcp 合併），不再是舊版的多個 tool 模組。

Spec-Kit 任務拆分思路。預期產出的 task 大類：

- ReAct 循環類（ReActLoop 主循環、PromptBuilder、ToolExecutor、MAX_ITERATIONS 控制）；
- CLI Channel 類（CliChannel、oryxos chat 命令、oryxos init 工作區初始化）；
- 基礎 Tool 類（HTTP Tool、SandboxChecker 簡化版只校驗 URL 白名單）；
- Profile YAML 解析類（yaml.v3、Profile 校驗）；
- Session 類（Session 資料結構、SessionManager 記憶體版，持久化放 US-5）。

關鍵 task 顆粒度。US-2 是 Spec-Kit 拆分的重點。幾個需要拆細的複雜 task：

- ReActLoop 主循環（核心循環邏輯精簡、約數十行 Go，但工程化部分如錯誤處理、日誌、訊息累積、迭代次數控制建議拆 2 到 3 個子 task，阻塞路徑一律走 context.Context 做取消與超時）；
- PromptBuilder 組裝（四部分內容即 system prompt 加 Bootstrap 加 Memory 加對話歷史加 Tool 列表，建議拆成幾個子 task 逐步加入）。

> **📌 關鍵：** 這裡再強調一次關鍵邊界（constitution 原則四）：呼叫 LLM 時 go-openai 只做協議轉換與 tool schema，tool 的實際調度由 ReActLoop 加 ToolExecutor 控制，不採用任何框架（如 Eino）的自動執行 Agent 抽象。go-openai 天然回傳 raw tool_calls 由你自己執行，「tool 被調兩次」的風險基本消失；但 AI agent 仍可能順手引入某個框架的 Agent 抽象幫忙跑循環，task 裡要明確排除。

US-1 加 US-2 完成後跑 `/speckit.analyze` 檢查 spec 跟程式碼一致性。

驗收 Demo 一：查天氣穿衣。oryxos chat 啟動 CLI，使用者輸入"查一下北京天氣並告訴我穿什麼"，Agent 通過 ReAct 循環呼叫 HTTP Tool 拉天氣 JSON，根據資料回覆穿衣建議，完整對話日誌正確累積到 Session，至少跑通一個 Provider（DeepSeek 或 Kimi）。

### 4.3 US-3：Memory 三層記憶（核心能力三）

核心目標：讓 Agent 跨對話保留狀態。核心階段做極簡版的兩層（會話和長期），用一份 MEMORY.md 檔案加兩個內建 Tool 實現，讓 Agent 主動寫入和讀取。

涉及的 package：internal/memory（核心能力三，含 MemoryService 三層門面、LongTermMemory、MemoryTools）。

Spec-Kit 任務拆分思路。US-3 相對獨立，依賴 US-2 但不影響 US-4。預期產出的 task 大類：

- MemoryService 門面類（三層統一門面，對 ReAct 循環只暴露一個介面，內部把會話記憶委託給 SessionManager、長期記憶委託給 LongTermMemory）；
- LongTermMemory 類（append、load、recallByKeyword、truncateIfNeeded 四個方法，介面預留 recall 帶 mode 參數的向量檢索升級空間）；
- MemoryTools 類（save_memory 加 recall_memory 兩個內建 Tool，實現 OryxTool 介面並顯式註冊）；
- PromptBuilder 集成類（在 PromptBuilder 裡通過 MemoryService 注入記憶，確保不破壞 US-2 跑通的 ReAct 循環）；
- MEMORY.md 檔案管理類（檔案位置、格式約定、超長截斷策略）。

關鍵 task 顆粒度。US-3 的 task 顆粒度較小，整體工程量不大。MemoryService 門面和 LongTermMemory 的方法每個較小，兩個 Tool 每個稍大，PromptBuilder 集成是改動型 task 要小心不破壞已有邏輯。US-3 實施完成後跑 `/speckit.analyze`。

驗收 Demo 二：跨對話記偏好。第一次對話告訴 Agent"我後端專案用 Go，部署在 K8s 上"，Agent 主動調 save_memory 追加到 MEMORY.md；重啟 OryxOS 或新開會話；第二次對話問"幫我看看我的專案能用什麼資料庫"，Agent 在回應裡引用之前記的偏好給出建議。

### 4.4 US-4：Plugin Tool 體系（核心能力四）

核心目標。讓業務方擴展 OryxOS 的能力。Plugin Tool 三檔接入：

- 零程式碼 SKILL.md 加 MCP 主推
- 輕程式碼自寫 MCP server
- 重程式碼原生 Go Tool（實現 OryxTool 介面、顯式註冊、編進二進制）。

核心階段做完三檔基礎設施加內建 Tool 補齊。

涉及的 package：

- internal/tool（補齊檔案 Tool 加 Shell Tool、MCP Client、SandboxChecker 完整版、ToolRegistry，三合一 package）；
- internal/core（SKILL.md 的加載歸 ContextLoader，不在 tool package）。

Spec-Kit 任務拆分思路。US-4 跟 US-3 可以並行（都依賴 US-2 但互不依賴）。預期產出的 task 大類：

- 內建 Tool 補齊類（檔案 Tool read_file、write_file、list_dir，Shell Tool 帶白名單，SandboxChecker 完整實現）；
- MCP Client 類（mcp_servers.yaml 解析、McpClientService 啟動時連接、tools/list 拉工具、McpToolAdapter 包裝成 OryxTool）；
- SKILL.md 類（ContextLoader 加載 .oryxos/skills/ 下引用的 SKILL.md 拼接到 system prompt，這部分歸 core 不歸 tool）；
- Profile 升級類（Profile 增加 skills 字段加 mcp_servers 字段）。

關鍵 task 顆粒度。US-4 的 task 數量較多。幾個需要重點拆解的複雜 task：

- MCP Client 集成（MCP 協議是 JSON-RPC over stdio 或 SSE，Go 已有官方 MCP SDK 與成熟社群實作如 mark3labs/mcp-go，工具生態齊備；建議先實現最常用的 stdio transport，SSE 放擴展，但 US-4 前仍先用最簡 MCP server 測 stdio 連通性，stdio MCP Client 建議拆幾個子 task：連接管理、tools/list、tool/call、錯誤恢復）；
- SandboxChecker 完整版（從 US-2 的簡化版只校驗 URL 擴展到完整版即檔案路徑白名單加 Shell 命令白名單加 HTTP 域名白名單，建議拆 3 個子 task）。

US-4 實施完成後跑 `/speckit.analyze`。

驗收 Demo 三：零程式碼 PR digest。業務方寫 .oryxos/skills/daily-pr-digest.md 描述任務，在 mcp_servers.yaml 配置 github-mcp（用社區現成的 MCP server），配置一個 Profile 引用這個 Skill 加 MCP server，Agent 啟動後能讀 SKILL.md 描述、調 github-mcp 拉 PR、匯總成簡報，整個過程業務方零程式碼只寫了一份 markdown 加配置。

### 4.5 US-5：Web Service（核心能力五）

核心目標。把 OryxOS 的所有能力通過 REST API 對外暴露，業務系統通過 HTTP 接入。這是 OryxOS 區別於個人助手專案的關鍵能力。

涉及的 package：

- internal/web（核心能力五）
- internal/storage（SQLite 持久化層，Session 持久化從記憶體版升級，並落 tool_invocations 和 llm_calls 審計表）
- cmd/oryxos（cobra 12 個命令補全）
- internal/core（ConfigLoader、ContextLoader 的 Bootstrap 加載補全）。

Spec-Kit 任務拆分思路。US-5 依賴前 4 個 user story 都完成，是最後實施的 user story，Spec-Kit 拆解的任務密度最高。預期產出的 task 大類：

- Web Service 基礎類（WebServer 啟動 net/http 加 chi、goroutine-per-request 本就是預設、統一錯誤處理 middleware、OpenAPI 文檔）；
- 6 組 handler 類（Session 加 Agent 加 Profile 加 Memory 加 Tool 加 System，每組 handler 一組端點，可並行實現）；
- 核心 10 個 REST 端點類（會話管理 4 個、Agent 呼叫 1 個、Profile/Memory/Tool 列表 3 個、health/info 2 個）；
- 持久化升級類（Session 從記憶體版升級到 SQLite（modernc.org/sqlite 純 Go 驅動），SessionRepository（database/sql），跨重啟恢復，以及 tool_invocations 和 llm_calls 審計表的寫入）；
- 配置與上下文類（ConfigLoader 配置密鑰加載，ContextLoader 的 Bootstrap 檔案加載補全並跟 PromptBuilder 集成）；
- CLI 完整版（cobra 12 個命令全部實現）；
- 工程化類（log/slog 結構化日誌加錯誤處理）。

> **📌 關鍵：** 注意審計表的寫入放在 US-5（constitution 原則六）：tool_invocations 和 llm_calls 核心階段就落庫，不是只放日誌，這樣可審計的資料地基 day one 就立起來。這一點 AI agent 容易漏掉（覺得日誌夠了），task 裡要明確。

關鍵 task 顆粒度。US-5 工程量最大。6 組 handler 可以並行實現（互不依賴），每組 1 到 2 個端點；Session SQLite 升級主要是 SessionRepository 加 messages_json 序列化，要小心 Session 狀態的遷移；Bootstrap 加載（ContextLoader）跟 PromptBuilder 集成時確保不破壞之前跑通的 ReAct 循環。US-5 完成後跑最後一次 `/speckit.analyze`，整個主體開發完成。

驗收 Demo 四：Web Service 同步呼叫。外部系統 POST /api/v1/sessions 創建 Session，POST /api/v1/sessions/{id}/messages 發訊息，GET 查歷史，DELETE 歸檔，完整鏈路跑通。

驗收 Demo 五：Web Service 多端點聯動。外部系統調 GET /info 查健康加 Provider 列表、GET /profiles 列可用 Agent、GET /tools 查可用 Tool、POST /agents/{name}/invoke 無狀態呼叫 Agent、GET /memory 查長期記憶，5 個不同端點協同完成一次業務流程。

### 4.6 實施過程中的協作模式

5 個 user story 的實施過程中有幾個跨 user story 的協作要點。

- `/speckit.analyze` 每個 user story 結束後跑一次，檢查 constitution 跟 spec 跟 plan 跟 tasks 跟程式碼是否一致，發現漂移立刻修正，這是 Spec-Kit 防漂移的核心命令。
- AI agent 跑偏 constitution 時主動糾正。看到 Claude Code 生成的程式碼不符合 constitution（比如用了 cgo 依賴、改了 ReAct 實現方式、採用框架的自動執行 Agent 抽象、把 Tool 又拆成多 package、Provider 沒顯式註冊、goroutine 洩漏沒走 context），主動讓 AI agent 重讀 constitution 改正。這幾個正是 OryxOS 最容易被寫錯的點。
- 跨 task 上下文丟失時回到 spec。Spec-Kit 把程式碼拆成多個 task 後，AI agent 實施每個 task 時可能不知道前面任務做了什麼，定期讓它讀 spec.md 加 plan.md 加最近的程式碼。
- git commit 標記每個 user story 完成，方便隨時回退到穩定狀態。

---

## 5. 專案交付物

主體開發完成後 OryxOS 1.0 是一個可演示的最小完整 Agent OS 執行時內核，五大核心能力全部跑通。除了核心程式碼本身，還有幾個交付物。

- 專案主頁。OryxOS 作為開源專案需要一個獨立的主頁作為對外門面，技術棧用 VitePress 或類似靜態站點工具，內容講清楚 OryxOS 是什麼、五大核心能力是什麼、怎麼快速開始。
- Spec-Kit artifacts 保留。.specify/ 目錄下的 constitution、spec、plan 在主體開發結束後仍然保留在倉庫裡，作為社區接力的長期參考。
- 社區文檔。API 參考文檔、部署運維手冊、貢獻者指南這些剩餘文檔作為社區共建專案，由社區貢獻者通過 PR 完成。

---

## 6. 增量階段：手動提示詞模式

### 6.1 為什麼從 Spec-Kit 切換到手動提示詞

主體開發完成後 OryxOS 進入增量階段。這個階段的工作性質跟主體開發完全不同：

- 單次任務顆粒度小（加一個 Channel、補一個 Bug、加一個 Plugin Tool）；
- 涉及檔案少（通常 1 到 3 個）；
- 不涉及跨 package 協作；
- 上下文是已經存在的程式碼而非從零設計。

這種工作性質下 Spec-Kit 流程過重，跑一次完整的 constitution 加 specify 加 plan 加 tasks 加 implement 流程，開銷大於單次任務的工作量本身。手動提示詞配合 Claude Code 更適合：直接打開 Claude Code 描述要做的事，Claude Code 在已有程式碼上下文裡直接修改，改完跑測試沒問題就提 PR，不需要正式的 spec 和 plan artifacts。

### 6.2 增量開發的工作流

增量階段的典型工作流：

- 社區貢獻者認領一個 issue（主倉庫標注 good-first-issue、feature-request、long-term-goal）；
- 本地 fork 加 clone OryxOS；
- 用 Claude Code 打開專案跟 Claude 描述要做的改動；
- Claude 在已有程式碼基礎上修改、加測試、跑通；提 PR 到主倉庫；
- 專案方 review 加 merge。

這個流程不強制走 Spec-Kit，每個貢獻者按自己習慣做就行。對要求嚴格的大 feature 可以選擇走 Spec-Kit，但不強制。

### 6.3 跟主體階段 Spec-Kit artifacts 的對接

主體階段產出的 `constitution.md` 和 `spec.md` 在增量階段仍作為參考文檔保留在倉庫裡：

- constitution.md 仍然是非協商原則，社區貢獻的程式碼必須遵守（Go 加單一靜態二進制、自實現 ReAct、go-openai 薄包裝、避免 cgo 依賴、Plugin Tool 三檔等）；
- spec.md 是核心能力的契約，社區貢獻者改某個核心能力時要保證不破壞 spec 裡的 acceptance criteria
- plan.md 在主體階段後基本不再更新，技術方案文檔作為社區參考保留。

新加 user story 的處理方式：

- 小 feature 直接手動提示詞加 PR；
- 大 feature（涉及新增 package、改 constitution、跨多個核心能力）由專案方決定是否單獨跑一次 Spec-Kit specify 到 plan 到 tasks 流程。

---

## 7. 風險和注意事項

### 7.1 Spec-Kit 當前局限

Spec-Kit 還在快速迭代，工具本身變化頻繁，使用時幾個注意點：

- 版本鎖定（實施前鎖定 Specify CLI 一個具體版本號，整個主體開發期間不升級，命令名、artifacts 格式、集成方式可能在版本之間變化）；
- 官方文檔隨時查（本文檔講思路加節奏，具體命令和安裝方式以實施時官方文檔為準）；
- community extension 謹慎用（Spec-Kit 有 70 多個社區擴展，主體開發期間只用官方核心命令，不引入 extension 增加不確定性）。

### 7.2 實施過程中的常見挑戰

AI agent 跑偏 constitution。AI agent 可能走捷徑生成不符合 constitution 的程式碼。對策：每次跑完 implement 後人工檢查，發現偏離立刻讓 AI agent 重讀 constitution 修正。OryxOS 最容易被寫錯的幾處是採用框架的自動執行 Agent 抽象、Provider 沒顯式註冊（用了反射/型別掃描）、Tool 被拆成多 package、SkillLoader 當成 Tool、審計表沒落庫、引入 cgo 依賴破壞單二進制、goroutine 洩漏沒走 context，檢查時重點看這幾個。

跨 user story 的上下文斷裂。AI agent 可能忘記前面 user story 實施時的具體決策。對策：每個 user story 開始前讓 AI agent 重讀 spec.md 加 plan.md 加最近程式碼。

`/speckit.analyze` 被跳過。analyze 是跨 artifact 一致性檢查的核心命令，被跳過會導致 spec 跟程式碼漂移。對策：把 analyze 作為每個 user story 結束的硬性環節，不能省。

MCP server 集成踩坑。Go MCP 生態已有官方 SDK 與成熟社群實作（如 mark3labs/mcp-go），工具生態齊備；但 stdio transport 仍可能遇到 process 啟動失敗、stdin/stdout 編碼問題。對策：US-4 實施 MCP 前先用一個最簡的 MCP server 測試 stdio 連通性。

Go 工程基礎是前提。Go modules 加 net/http 加 database/sql 不熟會顯著拖慢節奏。對策：實施前確保團隊成員對 Go modules、net/http、database/sql 有基本掌握。

---

## 8. 總結

OryxOS 的 AI 編程實施分兩個階段。

主體開發階段用 Spec-Kit。已有的需求文檔加技術方案餵給 Spec-Kit，轉成 constitution 加 spec 加 plan 加 tasks 等 artifacts。準備階段一次性把 constitution、spec、plan 準備好，然後按 5 個 user story 的依賴關係順序實施：US-1 加 US-2 是基礎，US-3 跟 US-4 可並行，US-5 在前 4 個完成後收口。每個 user story 完成後有可演示 demo，對應需求文檔第 13 章的 5 個驗收 demo。

增量階段切換到手動提示詞配合 Claude Code。小顆粒度增量不適合 Spec-Kit 完整流程，社區貢獻者用 Claude Code 直接在已有程式碼上做改動，主體階段產出的 constitution 加 spec 作為長期參考保留。

Spec-Kit 跟 OryxOS 的契合度很高：純 greenfield、medium 規模（8 個 package 加 cmd/oryxos）、需求清晰、AI agent 協作、方法論場景，每條都對得上。而且 Spec-Kit 的 `/speckit.tasks` 本來就按 user story 組織任務，OryxOS 按五大核心能力拆 5 個 user story 順著它的工作方式。社區裡對 Spec-Kit 在 brownfield 上的批評不適用純 greenfield 的 OryxOS。

> **📌 關鍵：** 核心策略是已有文檔餵給 Spec-Kit，不重寫。OryxOS 已經投入了完整的業界調研加需求文檔加技術方案，這些是 Spec-Kit 的最佳輸入，比從零生成 spec 質量好得多。關鍵是餵的是最新版文檔：工程結構是 Go 單一 module + internal 分包（8 個 package 加 cmd/oryxos，不是舊版多模組），constitution 要包含 go-openai 薄包裝、審計 day one 落庫、避免 cgo 依賴這些新決策，否則 Spec-Kit 生成的 plan 會按舊結構走偏。

按 user story 拆而不按時間拆，推進順序是 US-1 到 US-2 到（US-3 加 US-4 並行）到 US-5。具體時間投入由專案方根據團隊情況決定，對應的 4 週節奏見技術方案第 12 章。
