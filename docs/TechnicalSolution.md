# 專案篇 OryxOS：技術方案文檔

> 本文檔定義 OryxOS 的技術方案，回答 How 的問題。前置閱讀《專案篇 OryxOS 業界調研》和《OryxOS 需求文檔》。本文檔以需求文檔定義的五大核心能力（對接 LLM、ReAct 循環、Memory 記憶、Plugin Tool、Web Service）為骨架展開，每個模組只給職責和功能說明，不展開程式碼細節。程式碼層面的實現細節在研發階段補充。

> 承接需求文檔的定位判斷：核心階段交付的是 Agent OS 的運行時內核，能力上對齊業界開源 Agent OS 的基礎層；讓 OryxOS 成為真正企業級 Agent OS 的治理層（多租戶、SSO、完整審計、Tool 治理）在擴展和社區階段補齊。本技術方案只覆蓋核心階段的運行時內核，並在架構上為治理層預留擴展點。

**目錄**

1. 方案概述
2. 整體架構
3. 核心能力一：對接 LLM（Provider 抽象）
4. 核心能力二：ReAct 循環
5. 核心能力三：Memory 三層記憶
6. 核心能力四：Tool 體系
7. 核心能力五：Web Service
8. 支撐模組
9. 資料持久化
10. 專案工程結構
11. 關鍵流程
12. 實施節奏（4 週）
13. 性能和可擴展性考慮
14. 總結

---

## 1. 方案概述

OryxOS 是一個 Go 1.24+ 的單一二進制服務，基於 go-openai 接 OpenAI 兼容協議做 LLM 呼叫，自己實現 ReAct loop 作為 Agent 核心。整個 OryxOS 編譯成單一靜態二進制（`CGO_ENABLED=0`），單二進制部署是 day-one 預設。

技術棧選型一句話總結：Go 1.24+ + go-openai + 自實現 ReAct loop + net/http+chi + modernc SQLite + cobra 命令行。

### 1.1 關鍵技術決策

需求文檔定義了五大核心能力，下面統一列出 7 個關鍵決策的取捨。先用一張表速覽，再逐條展開。

| 決策 | 選擇 | 一句話理由 |
| --- | --- | --- |
| 一 ReAct 循環 | 自己實現，不用任何框架的 Agent 抽象 | Agent 核心完全可控 |
| 二 LLM 客戶端邊界 | go-openai 只做協議轉換和 tool schema，循環自己寫 | raw tool_calls 自己執行，循環完全可控 |
| 三 執行模型 | 同步直觀加 goroutine，用 context.Context 統一取消/超時/追蹤 | 程式碼直觀又能扛並發 |
| 四 Tool 註冊 | OryxTool 介面加顯式註冊（無反射掃描） | ReAct 不感知 Tool 來源 |
| 五 HTTP 層 | net/http 加 chi，goroutine-per-request | 單機撐幾千並發 |
| 六 Sandbox | Path/Pattern 白名單 | 應用層校驗，擴展階段容器隔離 |
| 七 持久化 | SQLite（modernc 純 Go 驅動）加 MEMORY.md，審計表 day one 落庫 | 可審計地基從一開始立起來，守單二進制 |

**決策一：自己實現 ReAct loop。** go-openai 負責 LLM 呼叫和 Function Calling 的協議格式轉換這些底層工作，Provider 抽象由 OryxOS 自己實現，ReAct loop 自己寫，保證 Agent 核心完全可控，也保留未來定制循環行為的空間。Go 裡沒有框架的 Agent 抽象要繞開，自實現循環反而更自然。

**決策二：明確劃清 LLM 客戶端的使用邊界。** 這是最容易埋 bug 的地方，單列為一條決策。go-openai 是 OpenAI 兼容協議的薄客戶端，OryxOS 只用它兩件事：

- 一是向 OpenAI 兼容協議的請求/回應轉換，
- 二是 tool 定義的 JSON Schema 組裝。

ReAct 循環由 OryxOS 自己寫，不採用任何框架的自動執行機制。

> **⚠️ 注意：** go-openai 天然把模型回傳的 raw `tool_calls` 直接交回給呼叫方，由 OryxOS 自己的 ReActLoop 加 ToolExecutor 決定調度和執行，因此「tool 被重複調度」的風險自然消除。原則仍然守住：不採用任何框架的自動執行 Agent 抽象，go-openai 在 OryxOS 裡只做協議適配器和 schema 生成器，不做循環引擎。

**決策三：同步直觀的執行模型加 goroutine。** 核心階段採用同步直觀的執行模型。一次訊息從進來、ReAct loop 執行、Tool 呼叫、Provider 呼叫到最終回應返回，全程同步、程式碼一路讀下來。Go 的 goroutine 讓每個並發請求跑在獨立輕量執行緒上，單節點支撐高並發不需要響應式編程，並用 `context.Context` 統一做取消、超時和追蹤傳遞。擴展階段引入流式輸出（SSE）和異步 Tool 呼叫。

**決策四：Tool 註冊機制用 OryxTool 介面加顯式註冊。** 每個 Tool 實現 OryxTool 介面，啟動時顯式註冊到 ToolRegistry，不靠反射掃描自動裝配。OryxTool 抽象統一內建 Tool 和 MCP Tool 的介面形式，讓 ReAct loop 不感知 Tool 來源。顯式註冊比魔法掃描更可控，也對齊 Go「顯式優於魔法」的取向。

**決策五：HTTP 服務層用 `net/http` 加 `chi` 路由。** 同步直觀的程式碼加 goroutine-per-request 的高並發能力（這本就是 `net/http` 的預設模型），單機輕鬆撐住幾千並發。擴展階段要 SSE 流式返回時，`net/http` 的 `http.Flusher` 也能支援。

**決策六：Sandbox 策略用 Path 和 Pattern 白名單。** 檔案操作限制工作目錄、Shell 命令白名單、HTTP 域名白名單，在應用層做校驗。擴展階段引入子進程加 bwrap 或容器隔離做完整 sandbox——OryxOS 本就跑在雲原生環境裡，容器隔離對 Go 版是本命手段。

**決策七：持久化用 SQLite 加 `database/sql`，Memory 長期記憶用 MEMORY.md 檔案加關鍵詞檢索。** SQLite 驅動採用純 Go 的 `modernc.org/sqlite`，避免 cgo 依賴以守住單一靜態二進制。Profile YAML 放 `.oryxos/profiles/`，Session、Tool Invocation、LLM Call 落 SQLite。其中審計相關的 `tool_invocations` 和 `llm_calls` 兩張表在核心階段就做寫入（不做查詢介面），讓可審計這個差異化能力的資料地基在 day one 就立起來，避免後期從日誌反解析返工。完整的向量檢索方案在擴展階段升級（詳見第 9 章）。

### 1.2 整體技術棧

OryxOS 的完整技術棧：

- Go 1.24+，`CGO_ENABLED=0` 靜態編譯（goroutine 加 `context.Context` 處理高並發）
- go-openai 加自實現 Provider 抽象（接 OpenAI 兼容協議做 LLM 呼叫）
- 自實現 ReAct loop（Agent 核心循環）
- `net/http` 加 `chi`（HTTP API 服務層）
- cobra（命令行工具，kubectl/docker/gh 同款）
- `gopkg.in/yaml.v3`（Profile YAML 解析）
- SQLite（`modernc.org/sqlite` 純 Go 驅動）加 `database/sql`（Session、審計和元資料持久化）
- 官方 Go MCP SDK 加 `mark3labs/mcp-go`（MCP Client 集成，生態成熟）
- `log/slog`（標準庫，結構化日誌）
- `prometheus/client_golang`（指標採集，client_golang 是 Prometheus 參考實現，擴展階段）。

---

## 2. 整體架構

OryxOS 的整體架構按"五大核心能力加支撐模組"組織。五大核心能力是 Agent OS 運行時內核的主體，支撐模組是讓這些能力跑起來需要的工程基礎設施。

整體上，OryxOS 是一個 Go 單一二進制服務，對外只有兩個入口：

CLI Channel 用於本地互動和調試，Web Service 用於業務系統通過 REST API 集成，兩個入口的訊息最終都匯入同一個引擎。

引擎是 ReAct 循環，它是整個系統的中樞，負責把"接收訊息、組裝 Prompt、呼叫 LLM、執行 Tool、回填結果、繼續推理"這條鏈路驅動起來。引擎自己不直接幹活，而是調度三塊能力：

- Provider 負責 LLM 呼叫並向外對接各家大模型 API
- Memory 負責會話和長期記憶並讀寫本地檔案
- Tool 負責工具執行並通過 MCP Client 向外對接外部 MCP server。

這三塊能力之下是儲存層，Session 和審計資料落 SQLite，Profile、Bootstrap、Memory、Skill 這些使用者可維護的資料落檔案系統。

這個架構有兩個要點。

第一，所有能力收斂到一個引擎、一套儲存、一個進程內，符合"單二進制、裝好就跑"的定位，外部依賴（LLM 廠商 API、外部 MCP server）都在應用邊界之外，OryxOS 自身不綁定任何一家。

第二，引擎和能力之間、能力和外部之間都通過抽象介面解耦，這讓擴展階段加新 Channel、新 Provider、新 Tool 時只需在邊緣擴展，不動核心引擎。下面分層和按能力兩個視角分別展開。

### 2.1 分層視圖

從上到下分四層。

- 最上是接入層（CLI Channel、Web Service 的 REST API），負責訊息進出。
- 中間是引擎層（ReActLoop、PromptBuilder、ToolExecutor），是 Agent 的大腦。
- 再下是能力層（Provider、Memory、Tool），給引擎提供 LLM 呼叫、上下文、執行能力。
- 最底是基礎層（Profile/Bootstrap/Skill 加載、Session 儲存、SQLite、配置與密鑰加載），是工程地基。

### 2.2 五大能力之間的關係

五大能力不是平行的功能模組，它們之間有明確的協作關係。

- ReAct 循環（能力二）是引擎，負責把"使用者訊息到 LLM 思考到 Tool 執行到結果回填到繼續"這件事跑起來。
- Provider（能力一）給 ReAct 循環提供 LLM 呼叫能力，每次迭代都要調一次。
- Memory（能力三）給 ReAct 循環提供上下文：長期記憶在每個 turn 開始時載入一次、注入 system prompt；會話歷史則每次組裝 prompt 時從當前 Session 重新取，兩者更新頻率不同。
- Tool（能力四）給 ReAct 循環提供執行能力，LLM 決定調哪個 Tool 後由 ReAct 循環負責執行。
- Web Service（能力五）是這套內部能力的對外出口，把前四個能力包裝成 REST API 供業務系統集成，它不參與 Agent 內部循環，而是循環的觸發入口和結果出口之一（另一個入口是 CLI Channel）。

簡化成一句話：Provider、Memory、Tool 三個能力供養 ReAct 循環這個引擎，引擎跑出的能力通過 CLI 和 Web Service 兩個入口對外提供。

---

## 3. 核心能力一：對接 LLM（Provider 抽象）

LLM 呼叫走 OpenAI 兼容協議，由 go-openai 客戶端承接底層 HTTP 與協議細節。OryxOS 在其上做一層自實現的抽象，把 go-openai 客戶端封裝成 OryxOS 內部的 ProviderService 抽象。

### 3.1 模組組成

ProviderService 模組。職責是統一管理所有 LLM Provider，對 ReAct 循環屏蔽不同 LLM 廠商的差異。ReAct 循環調 LLM 時傳入 Profile 和 Prompt，ProviderService 按 Profile 配置選對應的底層 go-openai 客戶端實例完成呼叫。

Function Calling 適配模組。職責是把 OryxOS 內部的 OryxTool 抽象轉成 OpenAI 協議的 tools 呼叫格式。go-openai 已經處理好 OpenAI 兼容協議的 tools 格式，核心階段對接的 Provider 都走 OpenAI 兼容協議，OryxOS 不需要逐一處理協議差異；對非 OpenAI 兼容的 provider（如 Anthropic、Gemini 原生協議），擴展階段再逐家補 adapter。注意這裡只用協議的格式轉換，不用任何框架的自動執行（見決策二）。

Provider 配置模組。通過 `.oryxos/config.yaml` 配置 Provider 的 API key 和 base URL，ProviderService 根據配置為每個 Provider 建立對應的 go-openai 客戶端實例。

### 3.2 Provider 名到客戶端實例的顯式映射

這是一個需要講清楚的關鍵點。配多個 Provider 時，會有多個結構相同的 go-openai 客戶端實例（都是 OpenAI 兼容協議，只是 base URL 和 key 不同）。僅靠"掃描所有客戶端實例"無法可靠區分哪個是 deepseek、哪個是 kimi，因為型別相同、變數名未必等於 provider name。

OryxOS 的做法是維護一份顯式的 provider name 到客戶端實例的映射（一張顯式註冊表），而不是靠型別掃描自動來。

具體是在 Provider 配置裡為每個 Provider 聲明唯一的 provider name（deepseek、qwen、kimi 等），ProviderService 啟動時按這個 name 建立一張顯式的 map 註冊表，Profile 通過 provider name 引用。這樣多 Provider 並存時不會有歧義。這正是 Go"顯式優於魔法"的體現：不靠型別掃描或自動裝配，就用一張顯式的 map 註冊表——這個原則要守住，否則多 Provider 跑不起來。

### 3.3 關鍵設計點

核心階段不做 fallback 和 hedge racing。Provider 故障時直接報錯給 Agent。fallback 鏈路、circuit breaker、hedge racing 放擴展階段，通過 Profile 的 fallback 字段聲明備用 Provider。

成本透明在核心階段做基礎版。每次 LLM 呼叫記錄 token 使用量、Provider、模型，寫入 `llm_calls` 表（見第 9 章）。擴展階段做完整的成本聚合和 Web 看板。

---

## 4. 核心能力二：ReAct 循環

ReAct 循環是 OryxOS 最核心的一段程式碼。輸入一條使用者訊息，輸出 Agent 的最終回應，中間可能呼叫若干次 LLM 和若干次 Tool。

### 4.1 ReAct loop 算法

ReAct 是 Reason 加 Act 的簡稱。算法步驟：

1. 接到使用者訊息追加到 Session 對話歷史；
2. 載入本 turn 的長期記憶（整份 MEMORY.md，**每個 turn 只載入一次**，見 5.3）；
3. 組裝 Prompt（system prompt 加 Bootstrap 加 Skill 加 Memory 加對話歷史加可用 Tool 列表）；
4. 呼叫 LLM Provider 獲取回應；
5. 如果回應沒有 Tool 呼叫，返回最終回應；
6. 如果有 Tool 呼叫，OryxOS 執行 Tool 並把結果作為 tool 訊息追加到對話歷史；
7. 回到組裝 Prompt 步驟（第 3 步，**不回到第 2 步**）繼續循環；
8. 達到最大迭代次數（預設 10 次）強制結束。

第 2 步刻意在循環之外：一個 turn 內 system prompt 保持固定，長期記憶不隨迭代變動。若 Agent 在同一 turn 內需要看到剛剛 `save_memory` 寫入的內容，走 `recall_memory`（直接讀檔，必然最新），不靠重新注入。術語見 `CONTEXT.md` 的 turn 與 iteration。

### 4.2 模組組成

ReActLoop 模組。Agent 的核心循環引擎。輸入 Session 和使用者訊息，輸出最終回應。

內部維護當前迭代次數，呼叫 ProviderService 調 LLM，呼叫 ToolExecutor 執行 Tool，把每次迭代的回應和工具結果累積到 Session 對話歷史。核心循環邏輯精簡，約數十行 Go，不依賴任何框架的 Agent 抽象，讓實現者完整掌握 Agent 的工作機制。

PromptBuilder 模組。組裝每次 LLM 呼叫的 Prompt。按四部分順序拼接：

- 第一部分 system prompt（人格加 Bootstrap 檔案加 Skill 檔案內容，統一由下面講的 ContextLoader 提供，內部拼接順序與覆蓋語義見 8.3 與 ADR-0003）；
- 第二部分 Memory 注入（**只有長期記憶**，由 MemoryService 提供，並由呼叫端在 turn 開始時載入後傳入，PromptBuilder 本身不碰檔案，見 5.3）；
- 第三部分對話歷史（按 maxHistoryTurns 截斷後的 Session messages，**每次組裝都從當前 Session 重新取**，含本 turn 內剛追加的 assistant 與 tool 訊息）；
- 第四部分當前 Profile 可用的 Tool 列表（按 Function Calling 格式）。

ToolExecutor 模組。執行 LLM 返回的 Tool 呼叫請求。從 ToolRegistry 找到對應 Tool，做 Sandbox 檢查，執行 Tool，把結果包裝成 ToolResult 返回給 ReAct 循環，並寫入 `tool_invocations` 表。失敗時按可重試策略返回錯誤資訊。

### 4.3 關鍵設計點

MAX_ITERATIONS 限制。核心階段預設 10 次，防止 Agent 陷入 Tool 呼叫死循環，可在 Profile 裡覆蓋。

訊息累積。每次迭代都把 LLM 回應和 Tool 結果追加到 Session 的 messages 列表。這意味着 Session 的對話歷史包含完整的 LLM 呼叫鏈和 Tool 呼叫鏈，對外可查可審計。

上下文長度管理。核心階段策略簡單：保留 system prompt 和最近 N 輪對話，超出部分丟棄，N 由 Profile 配置預設 20 輪。擴展階段引入總結壓縮。

核心階段不做：Tool 呼叫並行（一次回應裡多個 Tool 呼叫按順序執行）、Agent 間任務委託、流式回應。這些放擴展階段。

---

## 5. 核心能力三：Memory 三層記憶

Memory 是 Agent OS 區別於普通 chatbot 的核心能力。三層記憶是完整設計，核心階段做會話和長期兩層，情景記憶放擴展。

Memory 做成**統一門面**，對 ReAct 循環只暴露一個 MemoryService 介面，內部再分 Session 和長期記憶。ReAct 循環不需要分別去問 Session 和 MEMORY.md 兩個地方。

門面**不叫「三層門面」**——核心階段只實作兩層，門面內部也只有兩個委託對象，多算一層會誘導實作者去補刻意未實作的情景記憶。門面的職責與內部層數無關。

### 5.1 模組組成

MemoryService 模組（統一門面）。對 ReAct 循環暴露統一的記憶讀寫介面。內部把會話記憶委託給 SessionManager（底層是 SQLite 的 Session 儲存），把長期記憶委託給 LongTermMemory（底層是 MEMORY.md 檔案）。ReAct 循環組裝 prompt 時只調 MemoryService 一個介面拿到完整上下文，避免 Memory 概念橫跨兩個模組卻沒有統一入口。

LongTermMemory 子模組。長期記憶的核心讀寫，底層操作 `.oryxos/memory/MEMORY.md` 一個 Markdown 檔案。對外提供四個方法：`append`（追加內容，自動加日期 header；單條超過 1000 rune 拒絕寫入並回可操作的錯誤）、`load`（加載整個檔案，超閾值截斷）、`recallByKeyword`（按關鍵詞檢索返回匹配行，回傳總量同樣有 4000 rune 上限）、`truncateIfNeeded`（超過 4000 字保留最近內容）。介面預留向量檢索升級空間：`recallByKeyword` 設計成可升級為 `recall`（帶 mode 參數支援 keyword 加 semantic），切換底層實現不影響上層。

兩條進 LLM 的輸入路徑都要有上限：prompt 注入走 `load`、tool 結果回填走 `recallByKeyword`。單靠寫入側的單條上限不足以守住 `recallByKeyword`——MEMORY.md 是使用者可直接編輯、git 追蹤的純文字檔（見 9.3 檔案系統資料），手改或 spec #2 之前遺留的超長行繞得過寫入校驗。

MemoryTools 子模組。把長期記憶暴露給 Agent 呼叫，包含 `save_memory` 和 `recall_memory` 兩個內建 Tool，實現 OryxTool 介面後顯式註冊到 ToolRegistry，跟其他內建 Tool 一視同仁。

會話記憶。由 SessionManager 實現（見第 9 章），通過 SQLite 持久化，按 Channel 加使用者加 Profile 聯合標識管理。MemoryService 把它作為三層之一統一對外。

### 5.2 MEMORY.md 檔案設計

檔案位置 `.oryxos/memory/MEMORY.md`，內容是簡單的 markdown 列表，每條記憶帶日期 header。格式不規定嚴格，Agent 寫什麼 LLM 自己理解就行，簡單但有效。

### 5.3 Memory 注入到 system prompt

ReAct 循環在**每個 turn 開始時**（進入迭代迴圈之前）向 MemoryService 取一次**長期記憶快照**（整份 MEMORY.md 內容）傳給 PromptBuilder；同一 turn 內的後續迭代重用這份快照，不重讀檔案。

**被快照的只有長期記憶，對話歷史絕不快照。** 每次 PromptBuilder 呼叫都要從**當前** Session messages 重新組裝對話歷史，必須含本 turn 內剛追加的 assistant 與 tool 訊息——否則第 2 次 LLM 呼叫看不到 4.1 第 6 步回填的 tool 結果，ReAct 循環直接壞掉。兩者更新頻率不同是刻意的：長期記憶是 turn 級的穩定前提（進 system prompt），對話歷史是 iteration 級的即時累積（進 messages 序列）。MemoryService 作為統一門面內部仍委託 SessionManager 管理 Session，但那不是 PromptBuilder「Memory 注入」這一部分的內容。

長期記憶**每個 turn 重新讀、不做緩存**，這樣 Agent 呼叫 `save_memory` 後下一個 turn 立刻能看到，使用者手動編輯 MEMORY.md 也是下一個 turn 生效，讀一個小檔案性能可接受。擴展階段加 in-memory cache 加檔案 watch 自動失效。

載入頻率取 turn 而非 iteration 有三個理由：一個 turn 內 system prompt 保持固定，LLM 在第二次迭代看到的前提與它第一次迭代決策時一致；prompt 組裝函式維持無檔案 I/O，好測；同一 turn 內剛寫入的內容 LLM 本來就在對話歷史裡看得到，重複注入是冗餘，真要精準取用有 `recall_memory`。次要好處是 system prompt 前綴在 turn 內穩定，對有前綴快取的 Provider 較友善——這是效能補充理由，不作為架構依據。

### 5.4 MEMORY.md 跟 USER.md 的區別

需求文檔 Bootstrap 檔案有 USER.md，跟 MEMORY.md 角色容易混淆，明確區分：

- USER.md 是 Bootstrap 檔案，由使用者手寫、OryxOS 只讀不寫，是使用者的"初始設定"；
- MEMORY.md 是長期記憶，由 Agent 通過 `save_memory` 寫入、OryxOS 讀寫，是 Agent 的"成長記錄"。

兩者都進 system prompt，但來源和生命週期不同。

### 5.5 核心階段不做的部分

自動抽取（由 LLM 自己決定何時調 `save_memory`，不自動從對話提取）、語義檢索（recall 用關鍵詞不引入向量庫）、情景記憶（放擴展）、Memory Wiki（結構化 claim/evidence、矛盾檢測）、壓縮（超長簡單截斷）。

---

## 6. 核心能力四：Tool 體系

Tool 是 Agent 可以呼叫的外部能力。OryxOS 的 Tool 分兩類：內建 Tool 由 OryxOS 提供，Plugin Tool 由業務方擴展。Plugin Tool 有三種接入方式，按門檻從低到高排。

核心階段 Tool 相關合併為一個 `internal/tool` package（內建 Tool、MCP Client、ToolRegistry、Sandbox 都在裡面），**不拆成 builtin/skill/mcp 三個 package**——它們共享同一個 OryxTool 抽象和 ToolRegistry，耦合度高，拆細沒有收益。

另外，SKILL.md 嚴格說不是一種 Tool，而是注入 system prompt 的指令模板，因此 SkillLoader 不放在 Tool 體系裡，歸到上下文加載那一層（見 8.3），跟 Bootstrap 檔案一類。這樣概念更齊整。

### 6.1 OryxTool 抽象

OryxOS 內部統一的 Tool 抽象介面。內建 Tool、原生 Go 的 Plugin Tool、MCP Tool 都被包裝成 OryxTool 實例註冊到 ToolRegistry，ReAct 循環不感知具體 Tool 的來源。

OryxTool 介面約定四個核心方法：`getName`、`getDescription`、`getInputSchema`（JSON Schema）、`execute`（接收 JSON 輸入返回 ToolResult）。ToolResult 包含成功標識、結果內容、錯誤資訊、是否可重試。

### 6.2 內建 Tool（五個）

核心階段提供五個內建 Tool，分三組。FileTools：`read_file`、`write_file`、`list_dir`，執行前調 SandboxChecker 做路徑白名單檢查。

ShellTools：`shell` Tool 執行 bash 命令，帶超時和命令白名單。

HttpTools：`http_get`、`http_post`，帶域名白名單。

加上 MemoryTools 的 `save_memory`、`recall_memory`（歸 Memory 模組，但作為內建 Tool 註冊）。

這五個覆蓋"讓 Agent 能讀寫檔案、跑命令、調外部 API、記事"的最短鏈路。

### 6.3 Plugin Tool 方式一：零程式碼 SKILL.md 加複用 MCP

OryxOS 主推的接入方式。業務方不寫程式碼，只寫一份 markdown 描述要做的事，LLM 自己理解任務、自己組合呼叫 MCP 工具。

SKILL.md 是一份帶 frontmatter（name、description、trigger、required_tools）加任務說明正文的 markdown 檔案。Profile 通過 skills 字段引用它，通過 mcp_servers 字段引用需要的 MCP server。

OryxOS 把 SKILL.md 內容加載進 system prompt，LLM 讀到後自己理解任務、自己決定調哪個 MCP 工具、自己組合完成。OryxOS 不解析任務步驟、不做工作流引擎，所有邏輯交給 LLM。

注意 SKILL.md 的加載由 ContextLoader（8.3）負責，不在 Tool 模組裡。它本質是 prompt 的輸入源，跟 Bootstrap 檔案同類。

### 6.4 Plugin Tool 方式二：自己寫 MCP server

業務方用任何語言寫 MCP server，通過 MCP 協議暴露工具，OryxOS 作為 MCP Client 連接進來。MCP server 配置在 `.oryxos/mcp_servers.yaml`，聲明 name、transport、command、env。

McpClientService 子模組。MCP server 的連接維護和工具註冊。OryxOS 啟動時連接所有配置的 MCP server，調 tools/list 拿工具列表，把每個 MCP 工具包裝成 OryxTool 註冊到 ToolRegistry，處理 server 失聯、超時、錯誤恢復。

McpToolAdapter 子模組。把 MCP Tool 適配成 OryxTool 介面。Tool 呼叫時通過 MCP 協議（JSON-RPC over stdio 或 SSE）轉發給對應 MCP server 執行，結果包裝成 ToolResult 返回。

### 6.5 Plugin Tool 方式三：寫原生 Go Tool

業務方寫原生 Go Tool：實現 OryxTool 介面、在啟動時顯式註冊，編進同一個二進制。工程量最大但集成深度最好，適合需要直接在進程內呼叫企業內部 Go 服務、複用現有 Go 程式碼的場景。寫法跟 OryxOS 內建 Tool 完全一樣，直接在進程內呼叫 Go 函式，不走 MCP 協議、不起獨立進程、不序列化，效能最好。深度對接企業既有的 Java 或其他語言系統，則走方式二的 MCP/gRPC/HTTP——集成與實現語言解耦，反而更乾淨。

### 6.6 ToolRegistry

統一管理所有 Tool。啟動時顯式註冊內建 Tool 和方式三的原生 Go Tool（都實現 OryxTool 介面），加上 MCP Client 註冊的工具（方式二），全部包裝成 OryxTool 實例。Profile 啟動 Agent 時按 tools 字段從 Registry 過濾出該 Profile 可用的 Tool 子集。

### 6.7 Sandbox 檢查

核心階段 Sandbox 用 Path 和 Pattern 白名單做基礎檢查，配置在 `.oryxos/config.yaml`（`file.allowed_paths`、`shell.allowed_commands`、`http.allowed_domains`）。

SandboxChecker 子模組。Tool 執行前做白名單校驗，三個核心方法：

- `checkFilePath`（路徑標準化後比對白名單）
- `checkShellCommand`（拆出命令首個 token 比對白名單）
- `checkHttpUrl`（解析 host 後做通配符匹配）。

任意校驗失敗回傳 SandboxViolation 錯誤，Tool 執行終止。擴展階段用子進程加 bwrap 或容器隔離做完整 sandbox。

這裡要點明：Sandbox 加白名單是核心階段唯一的 Tool 治理手段，而 Profile 級的 Tool Policy（哪個 Agent 能用哪些 Tool）放在擴展階段。

核心階段 Profile 的 tools 字段已經能限定 Agent 可用 Tool 子集，算是 Tool 治理的雛形，完整的 allow/deny 策略擴展階段補。

---

## 7. 核心能力五：Web Service

Web Service 是 OryxOS 的對外完整門面，業務系統通過 REST API 接入。前面四大能力是 OryxOS 的內部能力，Web Service 是對外暴露。沒有 Web Service，OryxOS 只是一個 CLI 工具，無法跟企業現有業務系統集成。這也是 OryxOS 區別於偏個人定位的 OpenClaw、Hermes 的關鍵能力。

### 7.1 模組組成

WebServer 模組。啟動 `net/http` 加 `chi` 路由的 HTTP 伺服器，`oryxos server` 命令觸發，預設端口 8080，goroutine-per-request 本就是 `net/http` 的預設模型。

ApiHandler 集合。按資源分六組 handler：SessionApiHandler（會話管理）、AgentApiHandler（無狀態呼叫）、ProfileApiHandler（Profile 查詢）、MemoryApiHandler（Memory 查詢）、ToolApiHandler（Tool 資訊）、SystemApiHandler（系統狀態）。每組 handler 只做參數校驗、回應包裝、錯誤處理，實際邏輯委託給核心層的服務。

錯誤處理中介層。統一處理各 handler 回傳的錯誤，把錯誤轉成標準 JSON 錯誤回應（errorCode、message、timestamp）。

OpenAPI 文檔模組。提供 OpenAPI 3.0 文檔，暴露在 `/swagger-ui`；核心階段可手寫 spec 或用程式碼生成，不綁定特定工具。

### 7.2 核心階段 10 個端點

會話管理 4 個：

- `POST /api/v1/sessions`（創建）
- `POST /api/v1/sessions/{id}/messages`（發訊息）
- `GET /api/v1/sessions/{id}`（查歷史）
- `DELETE /api/v1/sessions/{id}`（歸檔）。

Agent 呼叫 1 個：

- `POST /api/v1/agents/{name}/invoke`（無狀態呼叫）。

Profile/Memory/Tool 資訊 3 個：

- `GET /api/v1/profiles`
- `GET /api/v1/memory`
- `GET /api/v1/tools`。

系統狀態 2 個：

- `GET /api/v1/health`
- `GET /api/v1/info`。

### 7.3 擴展階段補齊的端點

Profile 的 show/reload/create/update/delete；Memory 的 append/clear/search；Tool describe 和呼叫歷史；LLM call 歷史和 token 統計；Webhook 觸發；SSE 流式回應；Prometheus metrics；OpenAPI spec。

### 7.4 關鍵設計點

- 錯誤碼規範：標準 HTTP 狀態碼加內部錯誤碼（400 參數錯誤、404 資源不存在、500 內部錯誤、503 Provider 故障）。
- CORS：核心階段開放所有源方便調試，擴展階段加白名單。
- 請求大小限制：單條訊息最大 32KB，Session 歷史返回最多最近 100 條。
- 超時：Agent 呼叫最長 60 秒超時返回 504。

### 7.5 核心階段不做的部分

認證機制（無認證假設內網，擴展補 API Key 加 JWT）、流式回應 SSE、WebSocket、RBAC 權限、限流。這些放擴展階段。

### 7.6 業務系統集成場景

同步呼叫（最常用，業務系統調 invoke 等返回，適合 stateless 短任務）、會話保持（先創建 Session 再多次發訊息，適合連續對話）、Webhook 觸發（告警系統、CI/CD、定時任務調 Agent，打通監控的感知到分析到行動閉環）、跨語言集成（任何能發 HTTP 請求的語言都能接，核心階段不出 SDK，擴展階段才出）。

---

## 8. 支撐模組

五大核心能力之外，OryxOS 還有幾個支撐模組讓整個系統跑起來。這些不是運行時內核的核心能力，但缺一不可。

### 8.1 工作區初始化

InitCommand 模組。`oryxos init` 命令的執行邏輯，創建 `.oryxos/` 工作目錄及完整結構：`profiles/`（Profile YAML）、`memory/MEMORY.md`（長期記憶）、`skills/`（SKILL.md）、`mcp_servers.yaml`（MCP 配置）、`sessions/`（Session 資料）、`logs/`（日誌）、`AGENTS.md`/`SOUL.md`/`USER.md`（Bootstrap）、`oryxos.db`（SQLite）。創建目錄、寫預設模板、生成預設 Profile。

### 8.2 Profile 配置

ProfileLoader 模組。從 `.oryxos/profiles/` 加載所有 YAML，解析後註冊到 ProfileRegistry。啟動時做合法性校驗：Provider 是否存在、Tool 是否註冊、Channel 是否支援、Bootstrap 檔案是否存在。

校驗失敗的處理**依載入形態而定**（spec #3 定案）：

- **單一 Profile（`oryxos chat`，核心階段）—— fail fast，啟動即報錯。** 與本專案既有的一致語義對齊：`Registry.Subset` 對未註冊的 Tool、組裝點對未配置的 Provider 都是啟動即報錯。一次只跑一個 Profile，它壞了就沒有「其餘 Agent」可保；讓它半殘地啟動、使用者要到對話中途才發現 Agent 少了半個腦袋，比啟動就報錯更難查。
- **多個 Profile 同時載入（Web Service，後續 spec）—— 不阻斷啟動、記錄錯誤日誌。** 一份 Profile 壞掉不該讓其餘 Agent 都起不來。這個形態尚未實作，契約隨那份 spec 定案。

`bootstrap` 欄位的校驗分三處，各自回答不同的問題：

| 問題 | 落點 | 時機 |
| --- | --- | --- |
| 名稱是否為已知的 Bootstrap 檔名、有無重複 | `core.Profile.BootstrapSelection`（`LoadProfile` 也呼叫同一個校驗） | 載入 Profile 時，以及每個 turn |
| 明確列出的檔案是否存在 | `config.BootstrapLoader` 的讀取路徑 | **每個 turn**（權威） |
| 同上，提前回報 | `config.ValidateBootstrapFiles`，由組裝點呼叫 | 啟動時 |

存在性的權威把關在**讀取路徑**而不是啟動校驗：Bootstrap 是每個 turn 重讀的（§5.3），啟動後才被刪掉的檔案若在讀取端被當成「該層為空」，Agent 就會安靜地少掉一段明確要求的上下文。啟動校驗是同一條規則的提前回報，讓使用者連一句話都還沒打就知道設定錯了。

兩處校驗看的都是**同一組 selection**（`Profile.BootstrapSelection` 的產物），不是欄位的字面清單——否則一份被 ADR-0003 互斥排除的 `SOUL.md` 會出現「壞掉可以跑、缺檔卻起不來」這種說不通的不對稱。欄位省略時不做存在性校驗：那是「載入預設三檔」，缺檔視為該層為空。

ProfileRegistry 模組。Profile 的記憶體索引，按 name 提供快速查找。Channel 接收訊息時通過它拿到具體 Profile。Profile 的 YAML 包含 name、description、identity（agent_name、prompt）、provider（name、model、temperature）、tools、skills、mcp_servers、channels、bootstrap、settings（max_iterations、max_history_turns）。核心階段支援多個 Profile 並存，多個 Agent 在同一實例上同時可用，這是"OS"在核心階段的最小體現。

### 8.3 上下文加載（Bootstrap 加 Skill 統一）

Bootstrap 檔案加載和 Skill 檔案加載合併到一個 ContextLoader 模組——兩者本質相同，都是注入 system prompt 的 markdown 上下文，只是來源不同。

ContextLoader 模組。按 Profile 的 bootstrap 字段和 skills 字段，從 `.oryxos/` 讀取 AGENTS.md、SOUL.md、USER.md（Bootstrap）和 `.oryxos/skills/` 下引用的 SKILL.md（Skill），拼接成 system prompt 的上下文部分，提供給 PromptBuilder。**每個 turn 重新加載一次、不緩存**，使用者修改後下一個 turn 立即生效。

> 載入粒度為 **turn** 而非 iteration（spec #3 定案，原文的「每次組裝 prompt 時」是 iteration 級）。載入點在 ReAct 迭代迴圈**之外**取一次快照：同一個 turn 內 system prompt 保持固定，LLM 第二次迭代看到的前提與它第一次決策時一致，組裝函式也維持無檔案 I/O。「不緩存、使用者修改後生效」的意圖完全保留，只是生效粒度是下一個 turn。與長期記憶（spec #2 §5.3）同一條規則。

**拼接順序與覆蓋語義（ADR-0003）**。按「最穩定普遍 → 最具體當下」排列，衝突時後者勝出：

1. `SOUL.md` **或** Profile 的 `identity.prompt`（人格）
2. `AGENTS.md`（專案約定）
3. `USER.md`（使用者偏好）
4. `SKILL.md`（當前任務）

Profile 的 `identity.prompt` 與 `SOUL.md` **互斥、前者優先**。此語義須以表格驅動測試固定，至少斷言三條：`USER.md` 與 `SOUL.md` 衝突時 `USER.md` 勝；`identity.prompt` 存在時 `SOUL.md` 完全不進 prompt；四層皆存在時的拼接順序。把 SKILL.md 歸到這裡而不是 Tool 模組，是因為它是 prompt 的輸入、不是可執行的 Tool。

### 8.4 Channel 接入

Channel 是 Agent 對外的訊息接入入口，主要解決"訊息進來、回應出去"。HTTP 接入歸 Web Service，不在 Channel 範疇內。

CliChannel 模組。`oryxos chat` 命令的實現，讀 stdin 寫 stdout 實現互動式對話，維護當前 Session，每次輸入調 AgentService.process，支援 `/quit` 退出。擴展階段補 Slack、Telegram、Discord 等 IM Channel，每個通過 Channel Adapter 插件機制擴展，所有 IM Channel 底層都調 Web Service 的 Agent 介面，不重複實現 Agent 邏輯。

### 8.5 兩種運行模式

`oryxos chat`（互動對話）、`oryxos server`（啟動 Web Service）。兩種模式共享同一份 Profile 配置和 Session 儲存，差異只是接入層。`oryxos gateway`（守護進程同時掛多個 Channel）屬擴展階段（ADR-0004）。

### 8.6 命令行工具

OryxOsCli 模組。cobra 命令行入口，整個 OryxOS 的 main 函數（位於 `cmd/oryxos`），註冊 11 個子命令：init、status、chat、server、profile list/create/show/delete、provider list、tool list、session list。每個子命令對應一個 cobra `Command`。Go 單一二進制啟動本就在 ~10ms 量級，所有命令都無啟動負擔。

### 8.7 配置與密鑰加載

承接需求文檔 5.12。ConfigLoader 模組負責統一加載 LLM API key、Provider 憑證、MCP server 憑證等敏感配置。核心階段做基礎版：敏感配置通過環境變數注入或獨立的本地配置檔案加載，不明文寫死在 Profile YAML 裡（Profile 裡用 `${ENV_VAR}` 佔位，加載時從環境變數解析）；配置加載時做必填項和格式的基礎校驗，缺失或非法時給清晰報錯。完整的加密儲存、密鑰輪轉、對接企業 KMS/Vault 放擴展階段。單列這個模組，是因為對企業級底座，配置和密鑰的加載校驗是 day one 該有的，不能散落各模組無人負責。

---

## 9. 資料持久化

### 9.1 持久化選型說明

核心階段選 SQLite（`modernc.org/sqlite` 純 Go 驅動）加 `database/sql` 做關係型持久化，MEMORY.md 檔案加關鍵詞檢索做長期記憶。這個選擇跟"業界一些 Agent OS 用向量資料庫做 Memory"不同，說明取捨。

為什麼核心階段不用向量資料庫。語義檢索是 Memory 自然的升級方向，但核心階段先不引入，把最短鏈路跑通。Go 有純 Go 的嵌入式向量方案 `chromem-go`，能直接編進二進制、不破壞單二進制部署；也有給 SQLite 加向量的 `sqlite-vec`，保單二進制做語義檢索很順手。其他需要外部進程的向量庫（如 pgvector 要外部 PostgreSQL）則屬於另一條擴展路線。

核心階段的判斷是先用 SQLite 加 MEMORY.md 跑通最短鏈路，讓實現者先掌握 Agent OS 的核心機制，向量檢索這種檢索體驗優化放擴展階段。

擴展階段升級路徑。

- 方案 A 用純 Go 嵌入式向量庫 `chromem-go`，直接編進二進制、保持單二進制；
- 方案 B 給 SQLite 加向量擴展 `sqlite-vec`，沿用既有 SQLite 儲存、仍是單二進制；
- 方案 C 接 PostgreSQL pgvector，企業部署多起一個 PG 服務，社區最成熟。

值得點明的是：Go 的純 Go 嵌入式向量方案很現成，保單二進制做語義檢索沒有障礙。具體選哪個擴展階段決議。核心階段 LongTermMemory 介面已預留升級空間（recallByKeyword 可升級為帶 mode 的 recall），切換底層不影響上層 Tool。

### 9.2 SQLite 關係型資料

通過 `database/sql` 加 `modernc.org/sqlite` 純 Go 驅動集成，配置資料源指向 `.oryxos/oryxos.db`。

> **⚠️ 注意：** 這裡有一個要提醒的工程風險：**SQLite 本身 ALTER TABLE 能力有限，對表結構演進的支援較弱。** OryxOS 不採用任何自動 DDL / ORM 自動遷移機制，因此沒有"依賴自動遷移把表結構改壞"的隱患。核心階段首次建表直接用手寫 schema，表結構後續演進用 `goose` 或 `golang-migrate` 一類遷移工具顯式管理，不要依賴任何自動遷移。這一點研發階段要注意，免得後期表結構改不動。

核心表三張。

- `sessions`：Session 元資料加 JSON 序列化的對話歷史。
- `tool_invocations`：每次 Tool 呼叫記錄。
- `llm_calls`：每次 LLM 呼叫記錄。

`tool_invocations` 和 `llm_calls` 在核心階段就做寫入（不一定做查詢介面）。「可審計」是 OryxOS 的差異化賣點之一，資料地基應該 day one 就立起來——純靠日誌後期要做審計還得反解析返工。查詢介面和審計報表放擴展階段，但寫入核心階段就有。

Session 實體字段：

```
session_id（主鍵，channel 加 user 加 profile 聯合生成）
profile_name
channel、user_id
messages_json
status（active/archived）
created_at
last_active_at
archived_at
```

### 9.3 檔案系統資料

`.oryxos/` 裡幾類資料放檔案系統不放 SQLite：Profile YAML、Bootstrap 檔案、Memory（MEMORY.md）、SKILL.md、MCP 配置、日誌。檔案系統的優勢是使用者可以直接編輯、git 跟蹤、備份。Profile 和 Bootstrap 這種使用者主動維護的資料放檔案系統比放資料庫友好。

---

## 10. 專案工程結構

OryxOS 是一個 Go 單一 module 專案，用 `internal/` 分包組織，不做多 module（Go 的多 module 是給獨立發布的庫用的）。由 8 個 internal package 加 `cmd/oryxos` 組成：

```
oryxos/
  cmd/oryxos/            # main 加 cobra 命令樹（取代原 oryxos-cli 加 oryxos-boot）
  internal/
    core/                # ReActLoop、PromptBuilder、ToolExecutor、ContextLoader、
                         #   Session、Profile、OryxTool 介面（所有 package 都依賴它）
    provider/            # ProviderService、OpenAI 兼容 adapter、provider name 顯式註冊
    memory/              # MemoryService 統一門面、LongTermMemory、MemoryTools
    tool/                # 內建 Tool（File/Shell/Http）、MCP Client、ToolRegistry、SandboxChecker
    web/                 # HTTP server（net/http 加 chi）、六組 handler、錯誤處理、OpenAPI
    channel/cli/         # CLI Channel
    storage/             # SQLite（modernc）、sessions / tool_invocations / llm_calls 三張表
    config/              # ConfigLoader 配置與密鑰加載
  go.mod
```

職責說明如下：

| package | 對應 | 職責 |
| --- | --- | --- |
| `internal/core` | 核心引擎 | ReActLoop、PromptBuilder、ToolExecutor、ContextLoader、Session、Profile、OryxTool 等抽象。所有 package 都依賴它 |
| `internal/provider` | 能力一 | ProviderService、OpenAI 兼容 adapter、provider name 顯式註冊 |
| `internal/memory` | 能力三 | MemoryService（統一門面）、LongTermMemory、MemoryTools |
| `internal/tool` | 能力四 | 內建 Tool（File/Shell/Http）、MCP Client、ToolRegistry、SandboxChecker（三合一） |
| `internal/web` | 能力五 | HTTP server（net/http 加 chi）、六組 handler、錯誤處理、OpenAPI 文檔 |
| `internal/channel/cli` | 支撐 | CLI Channel 實現 |
| `internal/storage` | 支撐 | SQLite（modernc）儲存層，含 sessions、tool_invocations、llm_calls 三張表 |
| `internal/config` | 支撐 | ConfigLoader 配置與密鑰加載 |
| `cmd/oryxos` | 支撐 | main 函數加 cobra 命令樹（11 個子命令），`go build` 直接編成單一二進制 |

映射與變化：

- 工程結構為 **8 個 internal package 加 `cmd/oryxos`**（單一 Go module，`internal/` 分包）。
- **無獨立啟動模組。** `cmd/oryxos` 的 main package 直接 `go build` 就編成單一靜態二進制，不需要額外的打包或啟動層。
- package 之間通過介面解耦。擴展階段加新 Channel 或新 Tool 實現只加新 package 不改 `core`，所有 Channel package 底層都調 `internal/web` 的 Agent 介面。打包 `go build`（`CGO_ENABLED=0`）生成單一靜態二進制，直接執行、單檔部署。

---

## 11. 關鍵流程

五大核心能力組合起來怎麼跑通，用五個端到端流程展開，對應需求文檔第 13 章驗收的五個 demo。

### 11.1 demo 一：對接 LLM 加 ReAct 循環

場景"查一下北京天氣並告訴我穿什麼"。使用者在 `oryxos chat` 輸入，CliChannel 調 AgentService.process；ReActLoop 第一輪，PromptBuilder 通過 ContextLoader 和 MemoryService 加載上下文組裝 Prompt；ProviderService 調 DeepSeek，返回包含 http_get 的 Tool 呼叫；ToolExecutor 執行，SandboxChecker 檢查 URL 通過，HttpTools 拿到天氣 JSON，並寫 tool_invocations；結果追加到 Session 進入第二輪；DeepSeek 看到天氣給出建議；無更多 Tool 呼叫，循環結束返回。涉及能力一加二加四。

### 11.2 demo 二：Memory 跨對話記偏好

第一次對話使用者說後端專案用 Go 部署在 K8s 上，DeepSeek 判斷值得長期記，調 save_memory 追加到 MEMORY.md。第二次對話（甚至重啟後），ReActLoop 組裝 Prompt 時 MemoryService 把 MEMORY.md 注入 system prompt，DeepSeek 看到偏好自然用上，不需要使用者重新解釋。涉及能力三。

### 11.3 demo 三：Plugin Tool 零程式碼

業務方寫 `.oryxos/skills/daily-pr-digest.md` 描述任務，配 mcp_servers.yaml 聲明 github-mcp 和 slack-mcp，創建 Profile 引用 skill 和 mcp_servers。OryxOS 啟動時 ContextLoader 加載 SKILL.md，McpClientService 連接兩個 MCP server 把工具註冊到 ToolRegistry。觸發後 ReActLoop 組裝 Prompt 把 SKILL.md 注入 system prompt，LLM 自己決定先調 github-mcp 拉 PR，McpToolAdapter 通過 MCP 協議轉發，結果回填，LLM 再調 slack-mcp 推送。業務方零程式碼。涉及能力四方式一加方式二。

### 11.4 demo 四：Web Service 同步呼叫

外部系統 POST /sessions 創建 Session，POST /sessions/{id}/messages 發訊息，AgentApiHandler 調 AgentService.process 走完整 ReActLoop，回應返回，GET 查歷史、DELETE 歸檔。goroutine 讓每個請求跑在獨立 goroutine，單機撐住幾千並發。驗證能力五。

### 11.5 demo 五：Web Service 多端點聯動

模擬 AI 嵌入業務系統。外部系統先 GET /info 查健康和 Provider，GET /profiles 選 Agent，GET /tools 確認能力，GET /memory 了解背景，POST /agents/{name}/invoke 無狀態呼叫一次 Agent 做分析，Agent 調 Plugin Tool 接 CRM、HTTP Tool 查物流，回應返回展示。全程 5 個端點完成，驗證 Web Service 多端點協同。

---

## 12. 實施節奏（4 週）

實施按需求文檔第 11 章的 4 週節奏組織，每週約 3 小時。**這是節奏參考，不是驗收條件**（ADR-0004）。每週對應一組核心能力，每週末有可演示成果。

### 第一週（3 小時）：核心能力一加二（對接 LLM 加 ReAct 循環）

- 搭 Go module 加 `internal/` 分包骨架、oryxos init、Profile YAML 解析
- ProviderService 包裝 go-openai 客戶端（先跑通 DeepSeek 或 Kimi，含 provider name 顯式註冊）
- ReActLoop 加 PromptBuilder 加 ToolExecutor、一個內建 HTTP Tool、CliChannel
- Session 記憶體版（第四週加 SQLite）

可演示：`oryxos chat` 多輪對話，Agent 調 HTTP Tool 完成簡單任務。

### 第二週（3 小時）：核心能力三加四（Memory 加 Tool）

- MemoryService 統一門面加 LongTermMemory（MEMORY.md 讀寫）、save_memory 加 recall_memory
- PromptBuilder 加 Memory 注入
- 檔案 Tool 加 Shell Tool（帶白名單）、McpClientService（連接外部 MCP server）
- ContextLoader 加載 SKILL.md

可演示：Agent 記住偏好並後續用到，調本地檔案讀寫、調外部 MCP server 完成跨工具任務。

### 第三週（3 小時）：核心能力五 Web Service

- WebServer（`net/http` 加 `chi`）、六組 handler 的核心 10 個端點
- 錯誤處理中介層、ConfigLoader（配置與密鑰加載）

可演示：外部系統通過 10 個 REST 端點完整呼叫 OryxOS。

### 第四週（3 小時）：多 Agent 演示加工程化收尾

- 多 Agent 演示（兩個不同 Profile 的 Agent 在同一實例並存）
- Session 持久化到 SQLite（含 tool_invocations、llm_calls 寫入）
- ContextLoader 的 Bootstrap 加載（載入順序見 ADR-0003）、cobra 11 個命令完整、結構化日誌
- 專案主頁（VitePress 或類似）

可演示：多 Agent 並存可用，CLI 體驗流暢，Bootstrap 影響 Agent 行為，Session 跨重啟恢復，主頁可訪問。

核心階段結束後 OryxOS 1.0 是一個可演示的最小完整 Agent OS 運行時內核，五大核心能力全部跑通。之後轉入開源社區維護，擴展功能（多 Channel、Memory 向量檢索、情景記憶、Skill 體系、MCP Server 暴露、Tool Policy、完整 Sandbox、Web Service 剩餘端點、Web 儀表板、SSO 和多租戶、完整審計、集群高可用）以及讓 OryxOS 成為真正企業級 Agent OS 的治理層由社區陸續推進。

---

## 13. 性能和可擴展性考慮

性能目標在需求文檔第 8 章已定義，這裡說明怎麼達到。

goroutine 撐高並發。每個 Agent 是記憶體裡的 Profile 對象加 Session 列表佔用極少，goroutine 讓每個並發請求跑在獨立輕量執行緒，Go runtime 用少量 OS 執行緒就能調度成千上萬個 goroutine，LLM 呼叫 IO 阻塞時 goroutine 自動讓出底層 OS 執行緒。這正是"大量並發 Session 卡在 LLM IO"這類場景的本命，原生成熟，不需要響應式編程。

1000 個並發 Session 記憶體可控。1000 個 Session 平均 50KB 共 50MB 沒問題。SQLite 寫入主要由 Session 追加訊息和審計表寫入觸發，核心階段每次都寫，壓測發現瓶頸再優化成批量落盤。

Memory 檔案 IO。**每個 turn 讀一次 MEMORY.md**，同一 turn 內多次 PromptBuilder 呼叫重用該長期記憶快照（見 5.3），檔案幾 KB 到幾十 KB 每次讀 1 到 2 ms，1000 並發可接受。擴展階段加 cache 加檔案 watch。

啟動時間。Go 單一靜態二進制啟動本就在 ~10ms 量級，對常駐服務和 CLI 工具都沒有啟動負擔，所有命令一律快速啟動，無需任何啟動優化 workaround。

---

## 14. 總結

OryxOS 技術方案核心：Go 1.24+ 單一靜態二進制服務，自實現 ReAct loop，基於 go-openai 接 OpenAI 兼容協議做 LLM 呼叫（只用其協議轉換和 tool schema，不用任何框架的自動 tool 執行），SQLite（modernc 純 Go 驅動）持久化加 MEMORY.md 檔案，cobra 命令行。

方案圍繞五大核心能力展開：

- 能力一對接 LLM（Provider 抽象加顯式 provider name 映射）；
- 能力二 ReAct 循環（Agent 的大腦，引擎約數十行 Go）；
- 能力三 Memory 三層記憶（統一門面，核心階段 MEMORY.md 加兩個內建 Tool，向量檢索放擴展，介面預留升級空間）；
- 能力四 Tool 體系（內建 5 個 Tool 加 Plugin Tool 三檔接入，主推 SKILL.md 加 MCP 零程式碼，核心階段 Tool 相關三合一為一個 `internal/tool` package）；
- 能力五 Web Service（REST API 六類操作核心 10 個端點，業務系統集成的唯一通道）。

實施按 4 週組織每週 3 小時：

- 第一週對接 LLM 加 ReAct
- 第二週 Memory 加 Tool
- 第三週 Web Service
- 第四週多 Agent 演示加工程化收尾。

每週末有可演示成果，對應需求文檔第 13 章五個驗收 demo。

儲存選型：核心階段 SQLite 加 MEMORY.md 加關鍵詞檢索跑通最短鏈路，向量檢索放擴展（chromem-go、sqlite-vec、pgvector 三選一），MemoryService 介面預留升級空間。

承接定位：核心階段交付運行時內核，能力上對齊業界開源 Agent OS 基礎層，企業級治理差異化在擴展階段補齊。架構上為治理層預留擴展點（Tool Policy、多租戶、審計查詢、SSO 都有對應的預留位置）。

核心理念不變：OryxOS 五大能力扎實落地，業務方組合 SKILL.md 加 MCP server 就能解決業務問題，通過 Web Service 接入已有系統，不需要寫 Agent 後端程式碼。
