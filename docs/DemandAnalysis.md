# 專案篇 OryxOS：需求文檔

> 本文檔定義 OryxOS 專案的功能需求和非功能需求，作為後續技術方案設計、研發實施、測試驗收的依據。本文檔回答 What，不回答 How，How 在後續的技術方案中展開。前置閱讀《專案篇 OryxOS 業界調研》，本文檔基於調研得出的領域判斷，不重複論證企業 Agent OS 領域的現狀。

**目錄**

1. 專案概述
2. 術語和概念
3. 設計目標
4. 典型場景
5. 核心功能
6. 擴展功能
7. 社區共建功能
8. 非功能需求
9. 關鍵流程
10. 資料模型
11. 里程碑規劃
12. 風險與未決事項
13. 驗收標準
14. 總結

---

## 1. 專案概述

### 1.1 OryxOS 是什麼

OryxOS 是基於 Go 實現的面向企業場景的 Agent OS。它裝在企業自己的 K8s 或伺服器上，作為統一底座，在底座上跑各種業務 Agent（運維助手、客服助手、HR 助手、銷售助手、知識管理助手等），共享一套渠道接入、模型路由、工具呼叫、記憶系統、沙箱執行能力。資料完全留在企業自己的基礎設施，不鎖任何雲生態。

業界已經有開源 Agent 專案把這套設計驗證過（OpenClaw 用 Node.js，Hermes Agent 用 Python），但寫遍了雲原生基礎設施的 Go，還沒有任何專案把「Agent OS」作為定位。Agent OS 本質上是一層基礎設施，要當企業內多個 Agent 共享的常駐底座，而過去十年雲原生基礎設施幾乎清一色是 Go 寫的（Kubernetes、Docker、containerd、Prometheus、etcd）；用基礎設施的母語寫一層新的基礎設施，是本命。至於底層 LLM 呼叫，OpenAI 兼容協議已是事實標準，go-openai 提供了成熟的客戶端，OryxOS 在其上做一層薄 Provider 抽象即可，缺的就是上面那一層「Agent OS」。OryxOS 填這個位置。詳細的領域分析見前置調研文檔。

這裡要先說清楚一個貫穿全文的分層判斷，它決定了怎麼理解後面的功能規劃。

Agent OS 跟 agent runtime（Agent 運行時）不是一回事。

agent runtime 是讓單個 Agent 跑起來的執行內核，負責 LLM 呼叫、工具執行、上下文管理、循環控制。

Agent OS 的內核包含一個 agent runtime，但它在 runtime 之上還要管多個 Agent 的生命週期、統一的對外對內接入、統一記憶、多租戶、審計這些 OS 級治理能力。

借操作系統類比，runtime 像單個進程的執行環境，Agent OS 像管理一群進程、調度資源、提供共享服務和治理的那層。一句話，runtime 讓一個 Agent 跑起來，Agent OS 讓一群 Agent 在企業裡被管起來。

理解這個分層，才能看懂 OryxOS 的交付節奏。OryxOS 區別於 OpenClaw、Hermes 的立身之本，是企業級治理能力（雲原生 / K8s 對齊、多租戶、SSO、完整審計、Tool 治理）。但這些治理能力重，做不進有限的核心階段。所以 OryxOS 的交付分兩段：

- 核心階段先把 Agent OS 的運行時內核用 Go 做扎實，這一層在能力上對齊業界開源 Agent OS 的基礎層；
- OryxOS 真正的差異化治理層，在核心內核之上、由擴展階段和社區共建陸續補齊。

> **📌 重要：** 換句話說，核心階段交付的是 Agent OS 的內核底座，而不是一個治理能力完備的企業級 Agent OS，後者是終局，核心階段是地基。後面所有「核心功能」都應放在這個語境下理解。

### 1.2 OryxOS 能幹什麼

OryxOS 優先做五個核心能力，基於這五個能力可以擴展出企業裡大量真實需求。需要說明，這五個能力都屬於「讓單個 Agent 跑得好」的運行時內核層；讓 OryxOS 成為真正「OS」的多 Agent 治理能力（多租戶、Tool Policy、審計、SSO），在擴展和社區階段補齊。

#### 1.2.1 能力一：對接 LLM

OryxOS 通過 go-openai 接 OpenAI 兼容協議，在其上做一層自實現的 Provider 抽象，對接主流大模型（DeepSeek、通義、Kimi、智譜、混元、豆包、Anthropic、OpenAI 等，主流模型多提供 OpenAI 兼容端點），Agent 不感知具體調的是哪家模型，運行時切換無 lock-in。

基於這個能力可以做的事：任意業務場景的自然語言對話助手，Agent 通過 LLM 理解使用者意圖、給出回覆；同一個 Agent 在不同任務用不同模型，簡單任務走便宜模型、複雜任務走強模型；接入企業自有的本地推理服務（Ollama、vLLM），資料完全不出企業；多 Provider 編排，做一份報告可以讓規劃用便宜模型、綜合用強模型。

#### 1.2.2 能力二：ReAct 循環

ReAct（Reason + Act）是 Agent 的核心工作機制：Agent 接到一個任務後，LLM 思考要不要調工具、調哪個工具，呼叫之後看結果，再決定下一步，直到給出最終回應。

基於這個能力可以做的事：Agent 能自主決定何時呼叫哪個工具，不需要業務方寫死流程；多步驟任務可以一次對話內連續完成（先讀檔案、再分析、再調 API、再生成報告）；Agent 出錯時能自己回滾、重試、換工具；複雜業務流程不需要預先編排，Agent 在運行時動態決定執行路徑。

#### 1.2.3 能力三：Memory 三層記憶

Agent 記得住使用者的偏好、專案、決策、對話歷史。三層記憶設計，核心階段先實現會話和長期兩層，情景記憶放擴展階段補齊：

- 會話記憶：當前對話的完整歷史，過長時自動壓縮
- 長期記憶：使用者偏好、專案背景、關鍵事實，存在 `MEMORY.md` 檔案裡，跨對話保留
- 情景記憶：每個任務過程中學到的東西，修改了什麼檔案、做了什麼決策、得到什麼結果（擴展階段補齊）

基於這個能力可以做的事：Agent 跨多次對話記住使用者偏好（“我一般用 net/http 不用重框架”、“我的專案部署在 K8s 上”）；長任務過程中狀態保持，對話中斷後能恢復繼續做；團隊內多個 Agent 共享同一個使用者的偏好記憶；歷史決策可追溯（"上次為什麼選 DeepSeek 不選 Kimi"在記憶裡能查到）。

#### 1.2.4 能力四：Plugin 自定義工具加內建工具集

Agent 能呼叫工具實際操作系統，OryxOS 提供兩類 Tool：

- 內建 Tool：OryxOS 自帶的基礎工具（讀寫檔案、執行 Shell、發起 HTTP 請求）
- Plugin Tool：業務方自己擴展的工具，按門檻從低到高有三種方式
  - 零程式碼：寫一份 `SKILL.md` 描述意圖，複用社區現成的 MCP server（GitHub、Slack、Notion 等），讓 LLM 自己理解並組合呼叫
  - 輕程式碼：用任何語言自己寫 MCP server，接入企業自有系統
  - 重程式碼：寫原生 Go Tool（實現 OryxTool 介面、顯式註冊、編進二進制），適合效能關鍵或深度集成場景

基於這個能力可以做的事：給 Agent 接入企業自己的 ERP、CRM、CMDB，讓 Agent 真正能幹企業的活；接 GitHub、Jira、Confluence 這些研發工具，做研發助手；接 Prometheus、Grafana、SSH，做運維自愈；接工商登記查詢、天氣、新聞 API，做資訊聚合助手；業務方零程式碼擴展，寫 SKILL.md 加複用 MCP，純 markdown 就能上線新場景。

#### 1.2.5 能力五：Web Service

OryxOS 通過完整的 REST API 把所有能力對外暴露，業務系統用 HTTP 調一下就能用上 Agent，不用關心內部怎麼實現。Web Service 是 OryxOS 的對外門面，是企業把 AI 能力嵌入已有業務系統的唯一通道。

API 覆蓋六類操作：會話管理（創建會話、發訊息、查歷史、歸檔會話）、Agent 呼叫（無狀態呼叫一次 Agent、流式回應擴展階段補）、Profile 管理（列 Profile、看詳情、重載）、Memory 操作（查長期記憶、手動寫入、清理）、Tool 資訊（列可用 Tool、看元資訊）、系統狀態（健康檢查、運行指標、Provider 狀態）。

基於這個能力可以做的事：業務系統通過 REST API 直接呼叫 Agent，把 AI 能力嵌入已有產品；跨語言集成，任何語言的業務系統都能調；一個 OryxOS 實例同時服務多個業務系統；監控告警、Webhook 觸發、定時任務都通過 Web Service 呼叫 Agent；第三方開發者基於 REST API 二次開發，構建上層 AI 產品。

#### 1.2.6 關於 Channel

除了上面五個核心能力，核心階段還有一個基礎模組是 Channel（訊息接入渠道）。Channel 主要解決「訊息進來、回應出去」，核心階段只內建 CLI 一種，Slack、Telegram、Discord 等 IM Channel 放擴展階段。Channel 是核心功能模組，但它不算「五大核心能力」之一，單獨說明以免編號體系混淆。

#### 1.2.7 五個能力組合起來能幹什麼

五個能力像五個齒輪，組合起來能解決企業大量真實場景：

- 全渠道客服：LLM 理解使用者問題 + ReAct 循環調知識庫 Tool + Memory 記住客戶歷史 + Plugin Tool 接 CRM + Web Service 讓客服系統 HTTP 接入
- 運維助手：LLM 分析告警 + ReAct 循環調日誌查詢和服務重啟 + Memory 記住歷史故障 + Plugin Tool 接 Prometheus 和 SSH + Web Service 讓告警系統 Webhook 觸發
- 研發助手：LLM 理解需求 + ReAct 循環讀程式碼改程式碼 + Memory 記住專案慣例 + Plugin Tool 接 GitHub 和 CI + Web Service 讓 IDE 插件接入
- 知識管理：LLM 理解問題 + ReAct 循環檢索文檔 + Memory 記住團隊約定 + Plugin Tool 接 Confluence + Web Service 讓內網門戶嵌入對話框
- 銷售助手：LLM 拼裝客戶畫像 + ReAct 循環調 CRM 和工商登記查詢 + Memory 記住客戶偏好 + Plugin Tool 接銷售系統 + Web Service 讓銷售 App 呼叫
- 資料分析：LLM 生成 SQL + ReAct 循環執行查詢和圖表生成 + Memory 記住業務表結構 + Plugin Tool 接 BI 系統 + Web Service 讓 BI 工具集成自然語言查詢

這些場景不需要 OryxOS 單獨做模組。只要五個核心能力扎實，業務方在 OryxOS 上配 Profile、寫 Plugin Tool、調 Web Service 就能落地。OryxOS 不綁定具體業務，業務方按自己的需求組合。

### 1.3 文檔定位

本文檔定義 OryxOS 的功能需求，按三檔分級。

核心功能是最短鏈路，跑通「配置一個 Agent、跟它對話、它能呼叫工具」這件事，對應 Agent OS 的運行時內核。

擴展功能是生產級使用必需但不在核心鏈路上的能力，包含讓 OryxOS 成為真正企業級 Agent OS 的治理層（多租戶、SSO、審計、Tool Policy），核心階段之後陸續推進。

社區共建功能作為長期方向開放給社區貢獻。

核心階段的實施按 4 週節奏組織，每週 3 小時實踐，合計 12 小時。這是一個極強的時間約束，意味著核心功能的範圍必須收得很緊，只覆蓋運行時內核的最短跑通鏈路，其餘一切放擴展或社區共建。核心階段之後，OryxOS 長期以開源社區的方式維護和演進。

文檔的讀者包括專案研發人員、架構師、測試人員、產品和運營、社區貢獻者。研發把這份文檔當作實施依據，架構師把它當作技術方案設計的輸入，測試照著它寫用例，社區貢獻者理解 OryxOS 的邊界，判斷在哪些方向可以貢獻。

---

## 2. 術語和概念

為避免歧義，先把核心術語統一下來。這套術語對齊業界開源 Agent OS 的事實標準（OpenClaw、Hermes Agent 都用類似命名）。

- **Agent（智能體）**：一個具象的智能體，有具體的工種（運維、客服、HR 等）、人格設定、任務範圍、可用工具、綁定渠道。一個 Agent 通過 Profile 配置出來，不是寫程式碼寫出來的。
- **Profile（配置）**：一個 Agent 的完整配置，包括系統提示詞、綁定的 LLM Provider、可用 Tool 列表、綁定 Channel、Tool Policy、引用的 Skill。一個 Profile 對應一個 Agent。
- **Provider（供應商）**：LLM API 服務的抽象，實現統一介面讓 Agent 不感知具體調的是哪家模型。OryxOS 通過 go-openai 接 OpenAI 兼容協議，在其上做一層自實現的 Provider 抽象支援主流 LLM。
- **ReAct 循環（ReAct Loop）**：Agent 的核心工作機制，Reason 加 Act 的簡稱。LLM 思考是否呼叫工具，呼叫之後看結果，再決定下一步，直到給出最終回應。這是所有 Agent 框架的底層機制。
- **Tool（工具）**：Agent 可以呼叫的外部能力，分兩類。內建 Tool 是 OryxOS 自帶的基礎工具（檔案、Shell、HTTP）；Plugin Tool 是業務方自己寫的工具，通過實現 OryxTool 介面寫原生 Go Tool（顯式註冊、編進二進制）或者通過 MCP 協議接入外部工具服務。
- **Memory（記憶）**：Agent 跨對話保留的狀態，分三層。會話記憶是當前對話的完整歷史，過長時自動壓縮；長期記憶是使用者偏好、專案背景、關鍵事實，存在 `MEMORY.md` 檔案裡跨對話保留；情景記憶是任務過程中的狀態保持（擴展階段補齊）。
- **Channel（渠道）**：Agent 對外接入的訊息入口，包括 CLI、Slack、Telegram、Discord 等。Channel 主要解決「訊息進來、回應出去」這件事。HTTP 接入歸屬於 Web Service，不算 Channel。
- **Web Service**：OryxOS 對外暴露的完整 REST API，是業務系統集成 OryxOS 的唯一通道。覆蓋會話管理、Agent 呼叫、Profile 管理、Memory 操作、Tool 資訊、系統狀態六類操作。
- **Session（會話）**：使用者和 Agent 一次對話的上下文容器，按渠道和會話 ID 劃分，包含對話歷史、當前上下文、臨時變數。
- **Sandbox（沙箱）**：工具執行的隔離環境。核心階段是應用層白名單校驗，擴展階段補 Docker、K8s pod 等容器級隔離。
- **Tool Policy（工具策略）**：控制 Agent 可用工具的允許或拒絕規則，在 Profile 級別配置。
- **Skill（技能）**：可複用的指令模板，用 `SKILL.md` 檔案描述，兼容 agentskills.io 開放標準。一個 Skill 通常是幾個 Tool 的組合加上 prompt 增強。
- **Bootstrap（引導檔案）**：加載到系統提示詞中的上下文檔案，標準命名是 `AGENTS.md`（專案級 agent 行為說明）、`SOUL.md`（agent 人格定義）、`USER.md`（使用者偏好）。
- **Workspace（工作區）**：OryxOS 實例的工作目錄，預設是 `.oryxos/`，包含配置、Bootstrap 檔案、記憶、會話、技能的子目錄。

---

## 3. 設計目標

OryxOS 的核心目標可以用四個詞概括：統一、私有、易接入、可觀測。

統一指企業內多個業務 Agent 共享同一套底座。Agent 之上只關心業務邏輯，Channel、Provider、Tool、Memory、Sandbox 這些公共能力下沉到 OryxOS。企業上一個新 Agent 不用重複造這些輪子，通過 Profile 配置一份 YAML 就能跑起來。

私有指資料完全留在企業自己的基礎設施上，部署在企業自己的 K8s、虛擬機或物理機上。模型可以接外部 API，也可以用本地 Ollama 或 vLLM。OryxOS 本身不收集任何企業資料。

易接入指企業接入 OryxOS 不需要複雜的廠商綁定關係，基於標準 Go 工程結構，透過 MCP / HTTP / gRPC 中立協議跟企業現有的 ERP、CRM、CMDB、SSO、監控系統直接對接，不預設企業是哪種語言的技術棧；運維面對齊雲原生生態（Prometheus、OTel、Operator、etcd）。業務方寫 Tool 用 MCP 協議或者直接寫原生 Go Tool，任何方式都能接入。

可觀測指 OryxOS 的運行狀態對外可觀測，標準的 Prometheus 指標、結構化 JSON 日誌、健康檢查介面、Web 儀表板，適配企業現有的監控告警體系。

---

## 4. 典型場景

三個典型場景說明 OryxOS 的真實用法。這些場景描述的是 OryxOS 完整形態（含擴展階段能力）下的目標用法，核心階段先具備其運行時內核。

第一個場景是運維助手。某中型 SaaS 公司的運維團隊基於 OryxOS 搭一個運維助手，接入 Slack。Agent 配了幾個 Tool，告警分診、日誌查詢、服務重啟、變更審批。凌晨告警通過 webhook 進 OryxOS，Agent 收到告警後呼叫日誌查詢 Tool 拉錯誤堆棧，跟歷史故障庫交叉引用發現是已知 bug，自動應用 mitigation Skill 重啟服務，在 Slack 運維群裡匯報"已自愈，詳情見附件"，值班工程師早晨起來看下記錄就行。這個場景裡 OryxOS 提供了 Channel 接入（Slack）、Provider 路由（主備 LLM）、Tool 呼叫（SSH、Prometheus、Slack 通知）、Memory（歷史故障庫）、Skill（自愈 runbook）。

第二個場景是知識管理助手。某金融企業的法務團隊基於 OryxOS 搭一個知識管理 Agent，接入 Slack。Agent 索引了內部的合同模板、法規文檔、歷史案例、諮詢記錄。員工在 Slack 裡問"上次簽 SaaS 服務協議是怎麼處理資料出境條款的"，Agent 檢索 Memory 拉出歷史案例，綜合相關法規給出建議草稿，標註引用來源。這個場景關鍵點是 Memory 檢索準確度和引用追溯（合規要求所有 Agent 回覆必須可追溯到引用源）。

第三個場景是銷售助手。某製造業企業的銷售部門基於 OryxOS 搭一個客戶洞察 Agent，接入 Slack 和 CRM。銷售跑客戶前問 Agent"明天去拜訪 A 公司，有什麼我需要知道的"，Agent 呼叫 CRM connector 拉客戶歷史交易記錄，呼叫工商登記查詢 MCP 工具查最新公司登記資訊，呼叫知識庫 Tool 提取這家公司的關鍵決策人和採購習慣，綜合輸出客戶簡報。這個場景裡 OryxOS 提供的核心能力是 MCP 集成（外部資料）、企業 IT 系統 connector（自家 CRM）、Tool 編排。

---

## 5. 核心功能

核心功能是核心階段 4 週（合計 12 小時）內必須完成的最短鏈路，對應 Agent OS 的運行時內核。目標是跑通一個完整鏈路：用 Profile 配置一個 Agent，通過 CLI 跟它對話，它能呼叫 LLM 和工具完成任務，並能通過 REST API 對外暴露。

> **📌 重要：** 需要再次強調，核心階段交付的是運行時內核，讓 OryxOS 成為真正企業級 Agent OS 的治理層（多租戶、SSO、完整審計、Tool Policy）在擴展和社區階段補齊。下面按功能模組展開。

### 5.1 工作區初始化

OryxOS 的工作目錄是 `.oryxos/`，通過 `oryxos init` 命令初始化。這是使用者使用 OryxOS 的第一個動作。

`oryxos init` 在當前目錄下創建 `.oryxos/` 目錄，包含五個子目錄和三個 Bootstrap 檔案。

五個子目錄：`profiles/` 存放 Profile 配置（每個 Agent 一個 YAML）、`sessions/` 存放會話歷史、`skills/` 存放 SKILL.md 檔案、`memory/` 存放長期記憶 MEMORY.md、`logs/` 存放結構化日誌。

三個 Bootstrap 檔案（在 Agent 啟動時被自動加載到系統提示詞，讓 Agent 知道專案背景、自己的身份、使用者偏好）：`AGENTS.md` 專案級 agent 行為說明、`SOUL.md` 預設 agent 人格定義、`USER.md` 使用者偏好。

`oryxos init` 同時生成一份預設 Profile（`profiles/default.yaml`），用最簡配置讓使用者立刻可用：一個預設 LLM Provider、幾個基礎 Tool、CLI Channel。

### 5.2 Profile 配置

Profile 是 Agent 的完整配置，用 YAML 檔案描述。一個 Profile 對應一個 Agent。這是 OryxOS 最核心的配置抽象。

Profile 檔案包含五個字段：

- `identity` 段（Agent 名稱、描述、人格 prompt，也可以引用 SOUL.md 檔案）
- `provider` 段（綁定的 LLM Provider，provider 名加模型加參數，可選 fallback 配置）
- `tools` 段（Tool 列表，每個 Tool 名，可選參數）
- `channels` 段（綁定的 Channel，channel 名加配置）
- `bootstrap` 段（引用要加載到系統提示詞的 Bootstrap 檔案列表）。

Profile 通過 `oryxos profile create <name>` 命令創建，通過 `oryxos profile list` 查看，通過 `oryxos profile show <name>` 查看詳情，通過編輯 YAML 檔案修改。Profile 修改不需要重啟 OryxOS，下次啟動 Agent 時生效。

核心階段 Profile 在檔案系統裡管理，不做 Web 管理台 UI（擴展功能）。核心階段支援創建並管理多個 Profile，多個 Agent 可以在同一個 OryxOS 實例上並存，這是「OS」在核心階段的最小體現。

### 5.3 Provider 抽象（核心能力一：對接 LLM）

Provider 是 LLM 呼叫的統一抽象。所有 LLM 呼叫通過 Provider 介面走，Agent 不感知具體調的是哪家。

核心階段通過 go-openai 接 OpenAI 兼容協議，OryxOS 在其上做一層自實現的 Provider 抽象。OpenAI 兼容協議已是事實標準，主流 LLM（DeepSeek、通義、文心、Kimi、智譜、混元、豆包、Anthropic、OpenAI 等）多提供 OpenAI 兼容端點，OryxOS 把它們包裝成 Provider，不重複造輪子。非兼容的 Provider 在擴展階段逐家補 adapter。

每個 Provider 實例配置 provider 名（deepseek、qwen、kimi 等）、模型名、API key、可選的 base URL。Profile 通過 provider 名引用具體 Provider。

核心階段不做 fallback 和 hedge racing。Provider 故障時直接報錯給 Agent，fallback 鏈路、circuit breaker、hedge racing 這些可靠性能力放在擴展功能。

成本透明在核心階段做基礎版：每次 LLM 呼叫記錄 token 使用量、Provider、模型，落到日誌，擴展功能階段做完整的成本聚合和 Web 看板。

### 5.4 ReAct 循環（核心能力二：Agent 大腦）

ReAct 循環是 Agent 的核心工作機制，也是 OryxOS 最關鍵的一段程式碼。

核心算法是 Reason 加 Act：LLM 思考（Reason）是否要呼叫工具、調哪個工具、參數是什麼；OryxOS 執行（Act）這個工具，把結果回填給 LLM；LLM 看到結果決定下一步動作。這個循環持續到 LLM 給出最終回應或者達到最大迭代次數。

ReAct 循環的執行步驟：接到使用者訊息追加到 Session 對話歷史；組裝 Prompt（system prompt 加 Bootstrap 加對話歷史加可用 Tool 列表）；呼叫 LLM Provider 獲取回應；如果回應沒有 Tool 呼叫，返回最終回應；如果回應有 Tool 呼叫，OryxOS 執行 Tool 並把結果作為 tool 訊息追加到對話歷史；回到組裝 Prompt 步驟繼續循環；達到最大迭代次數（預設 10 次）強制結束。

核心階段實現要點：

- ReAct 循環邏輯精簡，核心循環約數十行 Go 程式碼，自己實現而不採用任何框架的自動執行 Agent 抽象，讓實現者完整掌握 Agent 的工作機制；
- 最大迭代次數可在 Profile 裡覆蓋；每次 LLM 呼叫和 Tool 呼叫都記錄結構化日誌，便於排查問題；
- Tool 呼叫失敗時按可重試策略再調，重試次數限制在 Tool Result 裡返回。

核心階段不做 Tool 呼叫並行（一次回應裡有多個 Tool 呼叫時按順序執行）、上下文動態壓縮、Agent 間任務委託（spawn sub-agent）。這些放在擴展功能。

### 5.5 Memory 三層記憶（核心能力三：讓 Agent 記得住）

Agent 跨對話保留狀態的能力。三層記憶是完整設計，核心階段做極簡版的兩層（會話和長期），情景記憶放擴展。

會話記憶（已通過 Session 管理實現）：當前對話的完整歷史，按 Channel 加使用者加 Profile 聯合標識。Session 資料持久化到本地 SQLite，重啟 OryxOS 後正在進行的 Session 可以恢復。Session 上下文超過 LLM context window 上限時簡單截斷早期對話保留近期對話。

長期記憶（核心階段做極簡版）：存在 `.oryxos/memory/MEMORY.md` 一個 Markdown 檔案，跨所有對話保留。Agent 通過兩個內建 Tool 主動讀寫這個檔案，`save_memory(content)` 讓 Agent 把要長期記住的事追加到 MEMORY.md，`recall_memory(query)` 讓 Agent 按關鍵詞檢索 MEMORY.md 裡的相關內容。Agent 啟動時 MEMORY.md 整個檔案作為長期上下文注入到 system prompt。檔案超過一定大小（預設 4000 字）時簡單截斷，擴展階段做壓縮。

核心階段不做：自動從對話中抽取事實（讓 LLM 自己決定何時調 save_memory）、語義檢索（recall_memory 用關鍵詞匹配，不做向量化）、情景記憶（任務過程中的修改檔案、決策、成果，放擴展）、Memory Wiki 與 claim/evidence 結構化、矛盾檢測、新鮮度管理。

使用者視角的核心體驗：用 OryxOS 一段時間後，Agent 自然會記住使用者的偏好、專案資訊、關鍵決策，下一次對話不需要重新解釋。這是 Agent OS 區別於 chatbot 的核心體驗。

### 5.6 Tool 體系（核心能力四：讓 Agent 能幹事）

Tool 是 Agent 可以呼叫的外部能力。Agent 通過 LLM Function Calling 決定何時調哪個 Tool，OryxOS 負責 Tool 的註冊、查找、呼叫、結果回傳。Tool 分兩類，這兩類的區分是 OryxOS 讓業務方擴展的核心機制。

內建 Tool（OryxOS 自帶）。核心階段提供三類基礎內建 Tool：檔案操作 Tool（`read_file`、`write_file`、`list_dir`，在沙箱裡執行，有路徑白名單限制）、Shell Tool（執行 bash 命令，有超時和命令白名單限制）、HTTP Tool（發起 HTTP 請求 GET、POST，有域名白名單限制）。加上 Memory 用到的兩個內建 Tool：`save_memory`（把內容追加到 MEMORY.md）、`recall_memory`（按關鍵詞檢索 MEMORY.md）。這五個內建 Tool 是「讓 Agent 能讀寫檔案、跑命令、調外部 API、記事」的最短鏈路，足以演示運行時內核的核心價值。

Plugin Tool（業務方自己擴展）。業務方擴展 OryxOS 的能力，按門檻從低到高有三種方式。OryxOS 主推方式一，因為這是 LLM 時代最優雅的寫法：業務方只描述意圖，讓 LLM 自己組合現成能力。

從使用的角度，有三種方式：

- **方式一（零程式碼）：寫 SKILL.md 加複用現成 MCP server。** 業務方寫一份 `.oryxos/skills/<name>.md` 描述要做的事，Profile 引用這個 Skill 和需要的幾個 MCP server（GitHub、Slack、Notion 這些社區已經有大量現成的開源 MCP server），LLM 讀到 SKILL.md 後自己理解任務、自己決定呼叫哪個 MCP 工具、自己組合完成任務。這種方式的核心是把意圖交給 LLM，基礎設施提供能力，業務方寫一份 markdown 描述零程式碼就能上線一個新場景。舉個例子，業務方想做"每天早上推送昨日 GitHub PR 評審進度到 Slack"：寫一份 daily-pr-digest.md 描述任務和觸發時機，複用社區現成的 github-mcp 和 slack-mcp，配置一個 Profile 引用這個 Skill 和兩個 MCP server，整個過程不寫一行程式碼。
- **方式二（輕程式碼）：自己寫 MCP server。** 業務方用任何語言（Python、Shell、Go、Java 等）寫 MCP server，通過標準 MCP 協議暴露工具，OryxOS 作為 MCP Client 連接進來。這種方式適合接入企業自己的系統（自家 ERP、CRM、CMDB），社區沒有現成 MCP server 時業務方自己寫一個。MCP 協議本身是 JSON-RPC，寫一個 MCP server 工程量不大。
- **方式三（重程式碼）：寫原生 Go Tool。** 實現 OryxOS 的 `OryxTool` 介面、顯式註冊、編進二進制，OryxOS 啟動時把它註冊到 Tool 池。這種方式適合效能關鍵或深度集成場景（直接在進程內執行、完整掌控參數與資源訪問），性能和集成深度最好，但工程量最大。要深度對接企業既有的 Java 服務，改走 MCP / gRPC / HTTP（即方式二）——集成與實現語言解耦，反而更乾淨。

三種方式的選擇標準：能用方式一就不用方式二，能用方式二就不用方式三。Plugin Tool 是 OryxOS 讓業務方落地真實場景的關鍵，OryxOS 本身只提供基礎內建 Tool，企業要做運維助手、客服助手、銷售助手，靠的是業務方組合 SKILL.md 加 MCP server。

核心階段 MCP Client 集成。OryxOS 實現一個最小 MCP Client，能連接外部 MCP server 並呼叫其工具。具體配置是在 `.oryxos/mcp_servers.yaml` 裡聲明 MCP server 的 URL 或啟動命令，OryxOS 啟動時連上 server 把它的工具註冊到 Tool 池，Profile 通過 Tool 名引用。MCP Server 暴露、Tool Policy、Tool LRU 加載、Tool 憑證管理這些放在擴展功能。

沙箱是 Tool 呼叫的安全隔離，核心階段用應用層白名單校驗實現，在 Tool 執行入口對參數和資源訪問做校驗：檔案操作有路徑白名單、Shell 有命令白名單、HTTP 有域名白名單，並對執行超時和資源佔用做限制。完整的容器級隔離（Linux namespaces / cgroups、gVisor，或 Docker、K8s pod）放在擴展功能——對 Go 而言，容器級隔離正是其本命的部署形態。

### 5.7 Channel 接入

Channel 是 Agent 對外的訊息接入入口，主要解決「訊息進來、回應出去」這件事。HTTP 接入歸 Web Service（核心能力五），不在 Channel 範疇內。

核心階段只內建一種 Channel：CLI Channel，通過 `oryxos chat` 命令啟動，是開發調試時的主要互動方式，支援多輪對話、查看上下文、查看 Tool 呼叫記錄。

Slack、Telegram、Discord 這些 IM Channel 放在擴展功能。它們的實現複雜度高（webhook、卡片、媒體、組織架構），且需要單獨的 OAuth 流程和企業資質，不在 12 小時核心階段能完成的範圍。擴展階段的 IM Channel 底層呼叫 Web Service，不重複實現 Agent 邏輯。

### 5.8 Web Service（核心能力五：對外介面暴露）

Web Service 是 OryxOS 的對外完整門面，業務系統通過 REST API 接入 OryxOS 的所有能力。這是 OryxOS 區別於個人助手專案（OpenClaw、Hermes 偏個人定位）的關鍵能力，企業要把 AI 能力嵌入已有產品，靠的就是 Web Service。

API 覆蓋六類操作：會話管理（創建會話、發訊息、查歷史、歸檔會話）、Agent 呼叫（無狀態呼叫一次 Agent、流式回應擴展階段補）、Profile 管理（列 Profile、看詳情、重載）、Memory 操作（查長期記憶、手動寫入、清理）、Tool 資訊（列可用 Tool、看元資訊）、系統狀態（健康檢查、運行指標、Provider 狀態）。

核心階段 10 個關鍵端點。 12 小時核心階段做最關鍵的 10 個端點跑通，其他放擴展階段：

| 端點 | 說明 | 分類 |
| --- | --- | --- |
| `POST /api/v1/sessions` | 創建會話 | 會話管理 |
| `POST /api/v1/sessions/{id}/messages` | 發訊息 | 會話管理 |
| `GET /api/v1/sessions/{id}` | 查詢會話歷史 | 會話管理 |
| `DELETE /api/v1/sessions/{id}` | 歸檔會話 | 會話管理 |
| `POST /api/v1/agents/{name}/invoke` | Agent 無狀態呼叫 | Agent 呼叫 |
| `GET /api/v1/profiles` | 列 Profile | 資訊查詢 |
| `GET /api/v1/memory` | 查長期記憶 | 資訊查詢 |
| `GET /api/v1/tools` | 列可用 Tool | 資訊查詢 |
| `GET /api/v1/health` | 健康檢查 | 系統狀態 |
| `GET /api/v1/info` | 系統資訊 | 系統狀態 |

擴展階段補齊的 15 個端點：Profile 的 show/reload/create/update/delete；Memory 的 append/clear/search；Tool describe 和呼叫歷史查詢；LLM call 歷史查詢、token 使用統計；Webhook 觸發、流式 SSE 回應；Prometheus metrics、OpenAPI spec。

核心階段不做的部分：認證機制（無認證假設內網，擴展補 API Key 加 JWT）、流式回應 SSE（核心同步阻塞返回，擴展加 SSE）、WebSocket（擴展補齊）、RBAC 權限（擴展加）。

業務系統集成場景：

- 同步呼叫：調 `POST /agents/{name}/invoke` 等返回，適合 stateless 短任務
- 會話保持：先創建 Session，後續多次發訊息保持上下文，適合連續對話
- Webhook 觸發：告警系統、CI/CD、定時任務通過 Webhook 調 Agent
- 跨語言集成：任何能發 HTTP 請求的語言都能接入

### 5.9 Session 管理

Session 是使用者和 Agent 一次對話的上下文容器。Session 包含起止時間、使用者身份、Agent 標識、對話歷史、當前上下文、臨時變數。Session 標識由 Channel、使用者、Agent 聯合生成。

核心階段 Session 資料持久化到本地 SQLite（`.oryxos/sessions/` 下）。重啟 OryxOS 後，正在進行的 Session 可以恢復。

跨 Session 的長期記憶、上下文壓縮、Memory Wiki 這些放在擴展功能。核心階段 Session 上下文超過 LLM 的 context window 時，簡單截斷早期對話保留近期對話。對外提供的 Session API 包括創建 Session、追加訊息、獲取歷史、結束 Session。

### 5.10 三種運行模式

OryxOS 提供三種運行模式，在核心階段全部實現。這三種模式是使用者跟 OryxOS 互動的全部入口：

- `oryxos chat`：互動式多輪對話模式。啟動後使用者在終端跟 Agent 對話，Agent 呼叫 LLM 和 Tool，實時返回結果。也可以用 `--message “xxx”` 發送單條訊息後退出。這是開發調試和日常使用的主要方式
- `oryxos server`：HTTP API 模式。啟動後 OryxOS 在指定端口（預設 8080）開放 RESTful 介面，業務系統通過 HTTP 呼叫 OryxOS 的 Agent
- `oryxos gateway`：常駐守護進程模式。啟動後 OryxOS 同時服務多個接入點（擴展功能補齊多 Channel 後才有完整用途，核心階段只掛 CLI Channel 和 Web Service）

三種模式共享同一份 Profile 配置和 Session 儲存。

### 5.11 命令行工具

OryxOS 通過命令行工具完成主要操作。核心階段實現 12 個命令，這一組命令是使用者跟 OryxOS 互動的全部入口。

啟動和狀態：`oryxos init` 初始化工作區、`oryxos status` 查看配置和運行狀態、`oryxos chat` 互動對話（可選 `--profile` 指定 Profile，預設用 default）、`oryxos server` 啟動 HTTP API 服務、`oryxos gateway` 啟動多渠道守護進程。

Profile 管理：`oryxos profile list` 列出所有 Profile、`oryxos profile create <name>` 創建新 Profile、`oryxos profile show <name>` 查看 Profile 詳情、`oryxos profile delete <name>` 刪除 Profile。

查詢：`oryxos provider list` 列出已配置的 Provider、`oryxos tool list` 列出已註冊的 Tool、`oryxos session list` 列出會話歷史。

命令行工具是 OryxOS 跟使用者最直接的互動界面。核心階段必須做到命令行體驗流暢，有清晰的錯誤提示和幫助資訊。

### 5.12 配置與密鑰加載

OryxOS 需要加載 LLM API key、Provider 憑證、MCP server 憑證等敏感配置。

核心階段做基礎版：敏感配置通過環境變數注入或獨立的本地配置檔案加載，不明文寫死在 Profile YAML 裡；配置加載時做基礎校驗（必填項、格式），缺失或非法時給出清晰報錯。完整的加密儲存、密鑰輪轉、對接企業密鑰管理系統（KMS、Vault）放在擴展階段。這一節單列，是因為對一個企業級底座，配置和密鑰的加載校驗是 day one 該有的，不能散落在各模組裡無人負責。

### 5.13 專案主頁

OryxOS 作為開源專案，需要一個獨立的主頁作為對外門面，講清楚 OryxOS 是什麼、能幹嘛、怎麼用，引導開發者快速上手。

主頁是專案方的統一交付物（不是社區共建），在核心階段做出來。技術棧和具體內容不在本文檔展開，常見選擇是用 VitePress、Astro 或 Docusaurus 之類的靜態站點生成器，把核心理念和快速開始呈現清楚就行。主頁跟核心程式碼同期發布，作為 OryxOS 1.0 對外亮相的一部分。

---

## 6. 擴展功能

擴展功能在核心功能完成後推進，補齊生產級使用必需但不在最短鏈路上的能力，其中包含讓 OryxOS 成為真正企業級 Agent OS 的治理層。這一檔以開源社區方式陸續補齊，具體節奏看社區需求和貢獻者投入。

### 6.1 渠道和模型層

- 多 Channel 接入：補齊 Slack、Telegram、Discord、郵件這幾個核心渠道。每個 Channel 通過 Channel Adapter 插件機制擴展。IM 渠道的深度功能（複雜卡片、審批回調、企業組織架構同步、多媒體訊息）在這一階段補齊
- Provider Fallback 和可靠性：三層 failover（hedge racing、circuit breaker、自動切換），Provider 故障時自動切換備用，業務不感知
- Adaptive Routing：LLM 路由從靜態配置升級為動態決策，根據任務類型、歷史呼叫質量、當前 Provider 負載，自動選擇合適的 Provider 和模型

### 6.2 記憶和能力層

- Memory 自動抽取：擴展階段加自動抽取機制，讓 LLM 在對話結束時自動提取值得長期保留的事實寫入 MEMORY.md
- Memory 語義檢索：集成向量資料庫（Milvus、Qdrant、Weaviate、PostgreSQL pgvector），Memory 寫入時生成 embedding，檢索按語義相似度匹配
- 情景記憶：補齊 Memory 第三層，記錄任務過程中修改的檔案、做出的決策、得到的成果，可按關鍵詞或語義搜索
- Memory Wiki：結構化 claim/evidence、矛盾檢測、新鮮度管理，讓長期記憶不只是流水賬
- Skill 體系：完整支援 SKILL.md 檔案，兼容 agentskills.io 開放標準。OpenClaw 的 ClawHub 上數以萬計的 skill 和 Hermes 社區的 skill 通過 SKILL.md 標準可以直接複用

### 6.3 工具和安全層

- MCP Server 暴露：OryxOS 自己作為 MCP server，把內部 Agent 的能力暴露給其他系統使用
- Tool Policy：Profile 級別的 Tool 允許或拒絕規則，控制每個 Agent 能用哪些 Tool。不能讓客服 Agent 拿到能 rm -rf 的 Shell Tool。這是 Agent OS 治理能力裡最輕、最能體現 OS 管控的一項，擴展階段優先做
- Tool LRU 加載：工具數量多時，同時只加載一部分，根據 Agent 當前任務動態加載，避免把所有工具塞進 LLM context 消耗 token
- 完整 Sandbox 隔離：補齊 Docker 容器和 K8s pod 兩種 sandbox 實現，WebAssembly Sandbox 作為高性能選項

### 6.4 治理和運維層

這一層是 OryxOS 區別於個人級 Agent OS 的核心差異化所在:

- Web 儀表板：提供 Web 儀表板做 Profile 管理、Session 查看、監控看板、審計日誌查詢
- SSO 和多租戶：補齊 SAML 和 OIDC 標準協議接入，對接企業 AD、Okta、Entra ID、Google Workspace。三級租戶模型（組織、部門、專案），RBAC 權限粒度到 Agent、Tool、Skill 級別
- 審計與可追溯：完整審計事件記錄、JSON 結構化輸出、trace ID 串聯、敏感資訊脫敏、SIEM 導出
- 可觀測性：Prometheus 指標、結構化日誌、健康檢查介面、Grafana Dashboard 模板
- 集群化部署與高可用：多節點協同通過 etcd 完成，Controller 角色通過選舉產生，節點故障自動遷移負載，API 請求不中斷

### 6.5 企業集成層

企業 IT 系統 connector：ERP（SAP、Oracle、Microsoft Dynamics）、CRM（Salesforce、HubSpot、Microsoft Dynamics）、CMDB、監控系統、內網知識庫這些系統的現成 connector。這是 OryxOS 真實落地時工程量最大的一塊，擴展階段先做最高頻的幾個，長尾的留給社區貢獻

---

## 7. 社區共建功能

社區共建功能不在 OryxOS 主線開發計劃內，作為長期方向開放給社區貢獻。這一檔不規定時間表，有人貢獻就推進，沒人貢獻就先放著。

- 剩餘專案文檔：核心階段專案方只交付需求文檔、技術方案文檔、業界調研。其他文檔（API 參考文檔、部署運維手冊、貢獻者指南 CONTRIBUTING.md、典型場景使用手冊）作為社區共建專案，通過 PR 貢獻
- Skills Marketplace：一個社區貢獻的 Skill 共享平台，Skill 用 SKILL.md 描述，符合 agentskills.io 開放標準，跟 OpenClaw 和 Hermes Agent 兼容。Marketplace 讓企業可以一鍵安裝別人貢獻的運維 Skill、客服 Skill、銷售 Skill
- SDK 多語言支援：優先級是 Go（OryxOS 同語言）、Python、TypeScript、Java，其他長尾語言看社區訴求
- 可視化 Profile 編輯器：讓非工程師也能配置和調整 Agent。編輯器輸出標準的 Profile YAML，OryxOS 直接讀取。產品形態接近 Dify 的 Agent 配置界面
- Native 檔案生成：不依賴 LibreOffice 直接生成 pptx、docx、xlsx 的能力，用 Go 原生庫實現，免去額外的 LibreOffice 進程依賴
- 多區域部署：企業在不同地域部署 OryxOS 集群，集群之間的 Agent、Memory、Session 可以跨區域協同。涉及時鐘同步、網路分區處理，實現複雜度高
- Kubernetes Operator：把 OryxOS 的部署、擴縮容、配置變更、版本升級工程化，跟 Helm、ArgoCD 集成，做到一鍵部署、聲明式配置、GitOps 工作流
- 移動端管理台：運維場景下用手機隨時查集群狀態、處理告警。工程量小、價值清晰，適合社區貢獻者起步貢獻
- Voice Channel：語音喚醒和連續語音對話，適配會議室、車載、智能家居場景
- RISC-V 和邊緣部署：OryxOS 跑在 Raspberry Pi、邊緣網關、嵌入式設備。Go 原生交叉編譯就能為多種架構產出單一靜態二進制，天然適合邊緣部署

---

## 8. 非功能需求

### 8.1 性能層面

核心階段單節點支援的 Agent 數不低於 10 個，單節點支援的並發 Session 數不低於 100 個，Session 創建 P99 延遲控制在 200 毫秒以內。LLM 呼叫本身的延遲取決於 Provider，OryxOS 內部的轉發開銷控制在 50 毫秒以內。集群規模通過水平擴展支撐更大規模（擴展功能階段）。

### 8.2 可靠性層面

已註冊的 Profile 配置和已寫入的 Session 資料保證不丟，這是和企業使用方的基本契約。LLM Provider 故障時核心階段直接報錯給上層，完整 failover 在擴展階段實現。Tool 呼叫失敗時按重試策略再調，預設指數退避最多三次。

### 8.3 可運維性層面

配置變更通過 Profile YAML 檔案修改，核心階段重啟服務生效；ETCD 動態下發不重啟生效在擴展階段。部署方式上支援物理機、虛擬機、Docker、Kubernetes，適配企業各種現有的部署體系。

### 8.4 兼容性層面

Go 1.24 及以上，操作系統支援 Linux 主流發行版（Ubuntu 22.04+、CentOS 8+、Debian 11+、Alibaba Cloud Linux 3、Rocky Linux）。LLM Provider 協議兼容性上，OpenAI 兼容協議是事實標準，只要 Provider 實現這套協議，OryxOS 就能直接接，不需要專門適配。

### 8.5 安全方面

核心階段做基礎。API 呼叫支援 HTTPS。敏感配置（LLM API key、資料庫密碼、Tool 憑證）支援加密儲存，不能明文寫在配置檔案裡。Tool 呼叫通過應用層白名單校驗（路徑、命令、域名）做基礎隔離，保證不能越權訪問主進程資源。完整的鑑權機制、Docker Sandbox 隔離、SSO 集成放在擴展階段。

### 8.6 合規方面

資料駐留保證 OryxOS 不主動外發任何資料，所有資料留在企業自己的基礎設施上。完整的審計日誌覆蓋、SIEM 導出、SOC 2、GDPR、HIPAA、等保三級的對接放在擴展階段。OryxOS 專案本身不背書認證，但提供合規所需的所有技術能力（審計、加密、隔離、留痕）。

---

## 9. 關鍵流程

幾個核心流程的步驟化描述，作為後續技術方案設計的輸入。

工作區初始化流程是使用者第一次使用 OryxOS 的標準動作。使用者在自己的專案目錄下執行 `oryxos init`，OryxOS 創建 `.oryxos/` 目錄、五個子目錄、三個 Bootstrap 檔案、一份預設 Profile。使用者編輯 Bootstrap 檔案填入專案背景、Agent 人格、使用者偏好，編輯 default.yaml 配置 LLM Provider 的 API key 和模型。

Profile 創建和 Agent 啟動流程是使用者加一個新業務 Agent 的標準動作。使用者執行 `oryxos profile create <name>` 命令，OryxOS 在 `.oryxos/profiles/` 下創建新的 YAML 檔案，使用者編輯配置 Agent 人格、Provider、Tool 列表、Channel。然後通過 `oryxos chat --profile <name>` 啟動 Agent，OryxOS 加載 Profile，初始化 Provider 連接、註冊 Tool 到 Agent 工具池、把 Bootstrap 檔案加載到系統提示詞，Agent 進入待對話狀態。

訊息處理流程是 OryxOS 最高頻的鏈路。訊息從接入層進來（CLI Channel 輸入、Web Service 的 HTTP API 呼叫、或擴展階段的 IM webhook）：CLI／IM 訊息由 Channel Adapter 轉換成 OryxOS 內部統一格式並帶上使用者身份，HTTP API 呼叫則由 Web Service 直接受理。Agent 接到訊息後查詢 Session 上下文，組裝 LLM prompt（包括 Bootstrap、對話歷史、可用 Tool 列表），呼叫 LLM Provider 獲取回應。回應裡如果包含 Tool 呼叫，OryxOS 執行 Tool，把結果回傳給 LLM 繼續生成。最終回應由原接入層送回（CLI／IM 經 Channel Adapter 轉成渠道特定格式，HTTP 呼叫由 Web Service 回傳 JSON），發回給使用者。整個過程中所有動作落結構化日誌。

Tool 呼叫流程是 Agent 執行業務動作的鏈路。LLM 在生成回應時通過 Function Calling 指明要調哪個 Tool 和參數。OryxOS 接到 Tool 呼叫請求後，從 Agent 的 Tool 池找到對應 Tool，做參數校驗和白名單校驗，然後執行。內建 Tool 直接在 OryxOS 進程內執行（在應用層白名單約束下），MCP Tool 通過 MCP 協議轉發給對應的 MCP server 執行。執行結果帶上成功失敗標識、錯誤資訊、可重試標識，回傳給 Agent。Agent 把 Tool 結果作為新一輪 LLM 輸入繼續生成最終回應。

Session 上下文管理流程是 Agent 處理一段對話的內部鏈路。使用者第一次跟 Agent 說話時，OryxOS 用 Channel 加使用者加 Agent 聯合 ID 查詢是否有活躍 Session。沒有則創建新 Session，初始化對話歷史為空。後續訊息追加到 Session 的對話歷史。Session 上下文超過 LLM 的 context window 上限時，核心階段簡單截斷早期對話保留近期對話（擴展階段做總結壓縮）。Session 在配置的超時時間內無訊息則結束，對話歷史歸檔可查。

---

## 10. 資料模型

幾個核心實體的字段描述，具體儲存結構在技術方案中細化。

Profile（YAML 檔案）：

| 字段 | 說明 |
| --- | --- |
| name | Profile 名，全局唯一 |
| description | 描述 |
| identity | 身份段：agent_name、prompt 或 prompt_file |
| provider | Provider 段：name、model、temperature、可選 fallback |
| tools | Tool 列表 |
| channels | Channel 列表 |
| bootstrap | 引用的 Bootstrap 檔案列表 |
| created_at / updated_at | 時間戳 |

Session（持久化到 SQLite）：

| 字段 | 說明 |
| --- | --- |
| session_id | 全局唯一 |
| profile_name | 關聯 Profile |
| channel | 來源 Channel |
| user_id | 使用者標識 |
| messages | 對話歷史 JSON 數組，每條有 role、content、timestamp、tool_calls |
| context_state | 當前上下文狀態 JSON |
| status | active、archived |
| created_at / last_active_at / archived_at | 時間戳 |

Memory（核心階段為檔案形態，非資料庫表）：長期記憶是 `.oryxos/memory/MEMORY.md` 一個 Markdown 檔案，按追加方式寫入，無結構化 schema。這一點跟其他持久化實體不同，特此說明。擴展階段引入向量庫後，Memory 才有結構化的 embedding 儲存。

Tool Invocation（記錄每次 Tool 呼叫）：

| 字段 | 說明 |
| --- | --- |
| invocation_id / session_id / profile_name | 標識與關聯 |
| tool_name | Tool 名 |
| parameters | 參數 JSON |
| status | running、completed、failed、timeout |
| result / error | 結果或錯誤（可選） |
| started_at / completed_at | 時間戳 |
| token_cost | 關聯的 LLM token 消耗 |

LLM Call（記錄每次 LLM 呼叫）：

| 字段 | 說明 |
| --- | --- |
| call_id / session_id | 標識與關聯 |
| provider / model | 呼叫的 Provider 和模型 |
| prompt_tokens / completion_tokens / total_tokens | token 用量 |
| latency_ms | 延遲 |
| status | 呼叫狀態 |
| started_at / completed_at | 時間戳 |

---

## 11. 里程碑規劃

OryxOS 核心功能的實施按 4 週節奏組織。

每一週圍繞一個或多個核心能力展開，每週末有可演示成果。核心階段完成後，OryxOS 轉入長期的開源社區維護。

四週的能力主線和可演示成果如下表：

| 週次 | 核心能力 | 週末可演示成果 |
| --- | --- | --- |
| 第一週 | 對接 LLM 加 ReAct 循環（能力一加二） | Agent 能多輪對話並調 HTTP Tool 完成簡單任務 |
| 第二週 | Memory 加 Tool 體系（能力三加四） | Agent 能記住偏好、調檔案讀寫、調外部 MCP 工具 |
| 第三週 | Web Service（能力五） | 外部系統能通過 10 個 REST 端點呼叫 OryxOS |
| 第四週 | 多 Agent 演示加工程化收尾 | 多 Agent 並存、CLI 完整、Session 跨重啟恢復、主頁可訪問 |

### 第一週：對接 LLM 加 ReAct 循環（核心能力一加二）

實施內容：

- `oryxos init` 工作區初始化、Profile YAML 解析
- Provider 抽象（基於 go-openai 接 OpenAI 兼容協議，先跑通 DeepSeek 或 Kimi）
- ReAct 循環（核心循環約數十行 Go，含 LLM 呼叫、Tool 呼叫解析、訊息累積）
- 一個基礎內建 Tool（HTTP）、CLI Channel
- Session 管理（記憶體版，第四週加 SQLite 持久化，用純 Go 的 modernc.org/sqlite 驅動）

驗收：`oryxos chat` 能跟一個 Agent 多輪對話，Agent 能通過 ReAct 循環呼叫 HTTP Tool 完成簡單任務（比如"查一下北京天氣並告訴我穿什麼"）。

### 第二週：Memory 加 Tool 體系（核心能力三加四）

實施內容：

- Memory 長期記憶極簡版（MEMORY.md 檔案、save_memory 和 recall_memory 兩個內建 Tool、啟動時整個檔案注入 system prompt）
- 檔案操作 Tool（read_file、write_file、list_dir）、Shell Tool（帶白名單校驗）
- MCP Client 集成（連接外部 MCP server）

驗收：Agent 能記住使用者偏好（“我用 Go”）並在後續對話用到，Agent 能調本地檔案讀寫、調外部 MCP server 的工具，完成一個跨工具的任務。

### 第三週：Web Service 加 API 端點（核心能力五）

實施內容：

- Web Service 核心 10 個 REST 端點（會話管理 4 個、Agent 呼叫 1 個、Profile/Memory/Tool 列表 3 個、health/info 2 個）
- 通過 `oryxos server` 啟動 net/http + chi 服務
- 配置與密鑰加載（環境變數注入加基礎校驗）

驗收：外部系統能通過 10 個 REST 端點呼叫 OryxOS（創建會話、發訊息、查 Profile、查 Memory、查 Tool、查健康狀態），API 呼叫鏈路完整。

### 第四週：多 Agent 演示加工程化收尾

實施內容：

- 多 Agent 演示（配置兩個不同 Profile 的 Agent 在同一實例並存，驗證「OS」的多 Agent 形態）
- 命令行工具完整 12 個命令、Session 持久化到 SQLite（跨重啟恢復）
- Bootstrap 檔案機制（AGENTS.md、SOUL.md、USER.md 加載到系統提示詞）
- 結構化日誌、專案主頁（VitePress 或類似靜態站點工具）

驗收：同一實例上多個 Agent 並存可用，完整的命令行工具體驗流暢，Bootstrap 檔案能影響 Agent 行為，Session 跨重啟能恢復，專案主頁可訪問。

核心階段結束後：OryxOS 1.0 是一個可演示的最小完整 Agent OS 運行時內核，五個核心能力（對接 LLM、ReAct 循環、Memory、Tool、Web Service）全部跑通，具備配置 Agent、CLI 對話、多 Agent 並存、REST API 接入、MCP 工具生態對接的能力。

社區接力階段：擴展功能（多 Channel、Memory 自動抽取和語義檢索、情景記憶、Skill 體系、MCP Server、Tool Policy、完整 Sandbox、Web Service 剩餘 15 個端點加 SSE 流式加認證、Web 儀表板、SSO 和多租戶、完整審計、集群高可用）以及讓 OryxOS 成為真正企業級 Agent OS 的治理層，由社區貢獻者陸續推進。

OryxOS 主倉庫提供清晰的 issue 標註和貢獻者指南，標註哪些是 good-first-issue、哪些是 feature-request、哪些是 long-term-goal。

---

## 12. 風險與未決事項

幾個已識別的風險和應對思路。

**核心功能範圍風險**

> **⚠️ 風險：** 4 週 12 小時是很緊的時間約束，可能實施過程中發現某些核心功能比預期複雜。應對是核心功能範圍卡得很緊，如果某一週完不成，立刻把當週末段功能挪到擴展功能，保證每週有可演示成果。優先級是"跑通"而不是"做完美"，後續社區接力可以慢慢完善。

**LLM 框架不集中風險**

Go 生態沒有「一個框架送十幾個現成 connector」的集中方案，LLM 接入需要自己收斂。應對是 OpenAI 兼容協議已是事實標準，核心階段用 go-openai 做薄包裝、在其上做一層自實現的 Provider 抽象，先把 OpenAI 兼容協議（DeepSeek、Kimi 支援）跑穩；非兼容的 Provider 在擴展階段逐家補 adapter，每接入一家做完整回歸測試。

**Tool 執行安全風險**

核心階段 Tool 呼叫用應用層白名單校驗做基礎隔離，不是完整 Sandbox。Tool 呼叫如果有 bug 或被惡意構造可能影響 OryxOS 進程。應對是核心階段嚴格限制內建 Tool 的能力範圍，檔案操作有路徑白名單、Shell 有命令白名單、HTTP 有域名白名單，不開放任意 Shell 執行。這意味著核心階段不建議在生產環境跑高敏感場景，真正的生產部署在擴展階段補齊 Docker Sandbox 之後。

**cgo 依賴破壞單二進制風險（Go 特有）**

Go 的一大價值是單一靜態二進制、裝好就跑。但一旦引入依賴 cgo 的庫（典型如 `mattn/go-sqlite3`），就會破壞純靜態編譯、把 C 工具鏈與動態連結帶回來，單二進制的優勢就沒了。應對是強制使用純 Go 驅動（SQLite 用 `modernc.org/sqlite`），並以 `CGO_ENABLED=0` 靜態編譯，把「無 cgo」當作 day one 的硬約束。

**goroutine 洩漏與 context 取消紀律風險（Go 特有）**

OryxOS 大量用 goroutine 承載並發 Session 和阻塞在 LLM IO 上的請求，若阻塞路徑沒有正確接受 context 取消，容易累積 goroutine 洩漏，長跑後吃滿記憶體。應對是所有阻塞路徑（LLM 呼叫、Tool 執行、MCP 通信、HTTP 請求）一律透過 `context.Context` 傳遞取消與超時，統一在請求邊界做取消，並在測試中檢查 goroutine 數不隨請求累積。

**社區接力的不確定性**

擴展功能依賴社區貢獻者，可能某些功能長期沒人推進。應對是專案維護方對核心擴展功能（多 Channel、Memory、Tool Policy）保持基本投入，社區共建功能（Marketplace、可視化編輯器、移動端）純粹靠社區，即使沒人做也不影響主線。

**定位被誤讀的風險**

核心階段交付的是運行時內核，能力上對齊業界開源 Agent OS 的基礎層，企業級治理差異化在擴展階段才顯現。社區可能會問"核心階段的 OryxOS 跟 OpenClaw、Hermes 有什麼區別"。應對是文檔明確說明核心階段是地基（Go 實現的運行時內核）、差異化是終局（企業級治理層），不把核心階段包裝成完整的企業級 Agent OS。

**和 OpenClaw、Hermes Agent 生態的關係**

OryxOS 兼容 agentskills.io 標準，但跟它們是不同的專案，設計哲學和產品形態有差異。應對是專案文檔明確說明定位差異：OpenClaw 偏個人（Node.js），Hermes 偏個人到小團隊（Python），OryxOS 直接定位企業場景（Go），三者通過 SKILL.md 互通，生態互補不競爭。

幾個未決事項，在技術方案階段或後續迭代決議。

- Provider 抽象介面設計，是直接用 go-openai 的型別，還是在其上加一層 OryxOS 自己的抽象。前者最省力，後者更可控。技術方案階段決議。
- Bootstrap 檔案加載順序和優先級，AGENTS.md、SOUL.md、USER.md 怎麼組合進系統提示詞，有不同方案。技術方案階段決議。
- LLM 客戶端核心走 go-openai 薄包裝，未來是否引入 Eino 一類框架作可選適配。核心階段已定走薄包裝加自實現 Provider 抽象，Eino 僅列為擴展期的可選項，後續迭代決議。

---

## 13. 驗收標準

驗收分四檔：功能、性能、可運維性、場景。

功能驗收：核心功能（第 5 章）全部完成，每個功能模組至少有一個端到端測試用例覆蓋。具體包括：

- `oryxos init` 工作區初始化
- Profile 配置和管理（支援多 Profile 並存）
- Provider 抽象（基於 go-openai 接 OpenAI 兼容協議，至少跑通 DeepSeek 和 Kimi 兩個）
- ReAct 循環（多輪 Tool 呼叫、正確累積訊息歷史、達到最大迭代次數時正確終止）
- Memory 長期記憶（save_memory 寫入、recall_memory 關鍵詞檢索、啟動時注入 system prompt）
- 內建 Tool（檔案、HTTP、Shell、save_memory、recall_memory）
- Plugin Tool 接入（方式一零程式碼 SKILL.md 加 MCP 跑通；方式三原生 Go Tool 示例跑通）
- MCP Client 集成、CLI Channel
- Web Service 核心 10 個 REST 端點全部跑通
- Session 持久化（SQLite，跨重啟恢復）、12 個命令行工具、配置與密鑰加載

性能驗收：通過壓力測試驗證單節點 10 個 Agent 穩定運行 4 小時、單節點 100 個並發 Session、Session 創建 P99 延遲低於 200 毫秒、內部轉發開銷低於 50 毫秒。這些是核心階段的目標，不達標不影響發布但需在擴展階段優化。

可運維性驗收：完整的部署文檔（新手 30 分鐘內完成單節點部署）；命令行工具有清晰的幫助和錯誤提示；專案主頁可訪問，講清楚 OryxOS 是什麼、怎麼快速開始。

場景驗收：通過五個 demo Agent 驗證五個核心能力，五個 demo 跑通是核心功能發布的硬條件。

| Demo | 驗證能力 | 內容 |
| --- | --- | --- |
| Demo 一 | 對接 LLM 加 ReAct | “查天氣並寫日報”，Agent 調天氣 API、用檔案 Tool 寫日報到本地 |
| Demo 二 | Memory | 第一次對話告訴偏好（Go、K8s），Agent 調 save_memory；第二次對話能引用記憶回答 |
| Demo 三 | Plugin Tool 加 MCP | Agent 通過 MCP Client 調外部 server 的工具完成跨工具任務 |
| Demo 四 | Web Service 同步呼叫 | 外部系統創建 Session、發訊息、獲取回應、歸檔，鏈路跑通 |
| Demo 五 | Web Service 多端點聯動 | 外部系統先後調 info、profiles、tools、invoke、memory 完成一次業務流程 |

---

## 14. 總結

OryxOS 是基於 Go 實現的面向企業場景的 Agent OS，裝在企業自己的 K8s 或伺服器上，作為統一底座跑各種業務 Agent，共享一套渠道接入、模型路由、工具呼叫、記憶系統、沙箱執行能力。

OryxOS 的交付分兩段。

- 核心階段先用 Go 把 Agent OS 的運行時內核做扎實，這一層在能力上對齊業界開源 Agent OS 的基礎層；
- OryxOS 真正的差異化治理層（多租戶、SSO、完整審計、Tool 治理），在核心內核之上由擴展階段和社區共建陸續補齊。核心階段是地基，企業級治理是終局。

核心階段優先做五個核心能力，基於這五個能力可以擴展出企業裡大量真實需求：

- 對接 LLM（Provider 抽象，讓 Agent 能調任意主流大模型，運行時切換無 lock-in）
- ReAct 循環（Agent 大腦，LLM 思考加工具執行，多步驟任務自主完成）
- Memory 三層記憶（核心階段會話加長期 MEMORY.md，跨對話記住使用者偏好和專案背景）
- Plugin 自定義工具加內建工具集（內建檔案、Shell、HTTP，業務方通過 SKILL.md 加 MCP 零程式碼擴展、MCP server 輕程式碼擴展、原生 Go Tool 重程式碼擴展）
- Web Service（REST API 覆蓋會話管理、Agent 呼叫、Profile/Memory/Tool 資訊查詢、系統狀態，業務系統通過 HTTP 接入）。

核心階段按 4 週組織，每週 3 小時實踐，合計 12 小時：

- 第一週做對接 LLM 加 ReAct 循環
- 第二週做 Memory 加 Tool 體系
- 第三週做 Web Service 加 API 端點
- 第四週做多 Agent 演示加工程化收尾。

完成之後是一個能跑通真實 demo 的最小完整 Agent OS 運行時內核。

核心階段之後，OryxOS 以開源社區方式長期維護，陸續推進擴展功能：多 Channel、Memory 自動抽取和語義檢索、情景記憶、Skill 體系、MCP Server 暴露、Tool Policy、完整 Sandbox 隔離、Provider Fallback 和 Adaptive Routing、Web Service 剩餘端點加 SSE 流式加認證、Web 儀表板、SSO 和多租戶、完整審計、可觀測性、集群高可用、企業 IT 系統 connector。其中治理層是 OryxOS 成為真正企業級 Agent OS 的關鍵。

更長期的方向（Skills Marketplace、SDK 多語言支援、可視化 Profile 編輯器、Native 檔案生成、多區域部署、Kubernetes Operator、移動端管理台、Voice Channel、RISC-V 和邊緣部署）開放給社區共建。

核心理念：OryxOS 核心階段把運行時內核做扎實，擴展階段補齊企業級治理形成差異化，業務方在 OryxOS 上配 Profile、寫 Plugin Tool、調 Web Service 就能解決自己的業務問題。OryxOS 不綁定具體業務，業務方按自己的需求組合。
