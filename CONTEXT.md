# OryxOS

面向企業場景的 Agent OS，裝在企業自己的基礎設施上，作為企業內多個業務 Agent 共享的常駐底座。

本檔是術語表，只收 OryxOS 特有的概念，不收一般程式設計概念，也不寫實作細節。當輸出提到下列概念時（issue 標題、ticket、假設、測試名稱），一律使用左欄的正式術語，不要漂移到 `_Avoid_` 列出的同義詞。

## 分層

**Agent OS**：
管理一群 Agent 的那層基礎設施，除了讓單個 Agent 跑起來，還負責多 Agent 的生命週期、統一接入、統一記憶與治理。OryxOS 的終局形態。

**執行時內核**：
Agent OS 之中讓**單個** Agent 跑起來的執行核心，涵蓋 LLM 呼叫、工具執行、上下文管理、循環控制。核心階段交付的就是這一層，不含治理能力。
_Avoid_: agent runtime、運行時內核（同一物，統一用「執行時內核」）

## Agent 與配置

**Agent**：
一個具象的智能體，有具體工種、人格設定、任務範圍、可用 Tool 與綁定 Channel。Agent 由 Profile 配置出來，不是寫程式碼寫出來的。
_Avoid_: bot、機器人

**Profile**：
一個 Agent 的完整配置，以 YAML 檔案描述。與 Agent 是一對一關係；凡是需要持久化標識時一律用 Profile（如 Session 主鍵），因為它是實體，Agent 是概念。
_Avoid_: agent 定義、配置檔

**Bootstrap**：
由使用者手寫、OryxOS 只讀不寫的上下文檔案，載入系統提示詞，標準命名為 `AGENTS.md`（專案級行為說明）、`SOUL.md`（人格定義）、`USER.md`（使用者偏好）。是使用者給 Agent 的「初始設定」。

**Workspace**：
一個 OryxOS 實例的工作目錄，預設 `.oryxos/`。

## 執行

**ReAct 循環**：
Agent 的核心工作機制。LLM 思考是否呼叫 Tool、呼叫後看結果、再決定下一步，直到給出最終回應或達到最大迭代次數。OryxOS 自行實作此循環，不採用任何框架的自動執行 Agent 抽象。
_Avoid_: agent loop、思考循環

**turn（輪）**：
一條使用者訊息啟動的一次 Agent 完整處理，從訊息進入到 Agent 給出最終回應為止。是失敗 rollback、對話歷史截斷（`max_history_turns`）與長期記憶載入的單位。
_Avoid_: 回合、對話輪次

**iteration（迭代）**：
一個 turn 之內 ReAct 循環的一次 LLM 呼叫。一個 turn 含一到多個 iteration，上限由 `max_iterations` 控制。與 turn 是不同量級的概念，文檔與程式碼中不可互稱——說「每輪」一律指 turn。
_Avoid_: 輪（「輪」專指 turn）

**Provider**：
LLM API 服務的抽象，讓 Agent 不感知具體呼叫的是哪一家。一個 Provider 有唯一的 provider name，Profile 透過該 name 引用。注意 Provider 是服務的抽象，不是模型本身。
_Avoid_: 模型、廠商、LLM 客戶端

**Session**：
使用者與 Agent 一次對話的上下文容器，含對話歷史與當前狀態，由 Channel、使用者、Profile 聯合標識。同時也是 Memory 的第一層。
_Avoid_: 會話記憶（同一物，統一用 Session）

## 記憶

**Memory**：
Agent 跨對話保留的狀態，完整設計分三層 —— **Session**（當前對話歷史）、**長期記憶**（跨對話保留的偏好與關鍵事實）、**情景記憶**（任務過程中的修改、決策與成果）。核心階段只實作前兩層；**情景記憶為擴展階段功能，刻意未實作，不是遺漏**。

**長期記憶**：
Memory 的第二層，跨所有對話保留的使用者偏好、專案背景與關鍵事實，由 Agent 自己主動寫入與檢索。是 Agent 的「成長記錄」，與 Bootstrap 的「初始設定」來源和生命週期都不同。

## 能力擴展

**Tool**：
Agent 可呼叫的外部能力。分兩類 —— **內建 Tool** 由 OryxOS 自帶，**Plugin Tool** 由業務方擴展。
_Avoid_: function、外掛

**Skill**：
可複用的指令模板，以 `SKILL.md` 描述，兼容 agentskills.io 開放標準。Skill 是注入系統提示詞的**指令**，不是可執行的 Tool——這個區分要守住。

**Sandbox**：
Tool 執行的隔離環境。核心階段是應用層白名單校驗（路徑、命令、域名），容器級隔離屬擴展階段。

**Tool Policy**：
Profile 級別控制 Agent 可用哪些 Tool 的允許或拒絕規則。屬擴展階段；核心階段僅以 Profile 的 tools 欄位限定可用子集，那不是 Tool Policy。

## 對外

**Channel**：
Agent 對外的訊息接入入口，解決「訊息進來、回應出去」。核心階段只有 CLI 一種。**HTTP 接入歸 Web Service，不算 Channel**。

**Web Service**：
OryxOS 對外暴露的 REST API，是業務系統集成 OryxOS 的唯一通道。
_Avoid_: API 層、HTTP Channel
