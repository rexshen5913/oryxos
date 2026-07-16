# OryxOS 專案開發憲法
# Version: 1.1, Ratified: 2026-07-16

本文件定義 OryxOS 專案不可動搖的核心開發原則。所有 AI Agent 在進行技術規劃與程式碼實作時，必須無條件遵循。本憲法效力高於任何 `CLAUDE.md` 或單次會話中的指令。

> 說明：本檔是「開發行為 + 架構」的專案憲法，由 `CLAUDE.md` 以最高優先級導入。它與《AI 編程指南》所述、於主體開發準備階段由 Spec-Kit 產出的 `.specify/memory/constitution.md` 在原則上一致，後者是 spec-driven 流程的實作契約。

---

## 第一條：工程與技術棧 (Engineering & Stack)
- **1.1 語言與建置：** Go（版本 >= 1.24），`CGO_ENABLED=0` 靜態編譯成單一二進制。單二進制部署是 day-one 預設。
- **1.2 避免 cgo：** 為守住單一靜態二進制，禁止引入 cgo 依賴。SQLite 使用純 Go 的 `modernc.org/sqlite`，不使用 `mattn/go-sqlite3`。
- **1.3 工程結構：** 單一 Go module + `internal/` 分包（8 個 package + `cmd/oryxos`），不做多 module。
- **1.4 標準庫優先：** Web 服務用 `net/http`（可搭配 `chi`）、LLM 呼叫用 `go-openai` 接 OpenAI 兼容協議、命令行用 `cobra`，絕不引入非必需的重框架。

---

## 第二條：Agent 核心 (Agent Core)
- **2.1 自實現 ReAct：** ReAct loop 自己實現、完全可控，不採用任何框架的自動執行 Agent 抽象。
- **2.2 LLM 邊界：** `go-openai` 只做協議轉換與 tool schema 生成；tool 調度與循環由 `ReActLoop` + `ToolExecutor` 自行控制。
- **2.3 顯式優於魔法：** Provider、Tool 一律顯式註冊，絕不靠反射或型別掃描自動裝配。

---

## 第三條：簡單性原則 (Simplicity First)
遵循 Go 語言「少即是多」哲學，絕不進行不必要的抽象。
- **3.1 (YAGNI)：** 只實作 `spec.md` 中明確要求的功能。
- **3.2 (反過度工程)：** 簡單的函式和資料結構優於複雜的介面和繼承體系。
- **3.3 (交付節奏)：** 核心階段交付執行時內核（五大核心能力優先）；企業級治理層（多租戶、SSO、完整審計、Tool Policy）放擴展階段。

---

## 第四條：測試先行鐵律 (Test-First Imperative) — 不可協商
所有新功能或 Bug 修復，都必須從編寫一個（或多個）失敗的測試開始。
- **4.1 (TDD 循環)：** 嚴格遵循「Red-Green-Refactor」循環。
- **4.2 (表格驅動)：** 單元測試必須優先採用表格驅動測試（Table-Driven Tests）。
- **4.3 (拒絕 Mocks)：** 優先編寫整合測試，使用真實的依賴。
- **4.4 (可演示)：** 每個 user story 完成後有可演示 demo，跑通優先於完美。

---

## 第五條：明確性原則 (Clarity and Explicitness)
程式碼的首要目的是讓人類易於理解。
- **5.1 (錯誤處理)：** **不可協商**：所有錯誤都必須被顯式處理，錯誤傳遞時必須使用 `fmt.Errorf("...: %w", err)` 進行包裝。
- **5.2 (無全域變數)：** 絕不允許使用全域變數傳遞狀態，所有依賴必須透過函式參數或結構體成員顯式注入。
- **5.3 (併發紀律)：** 所有阻塞路徑走 `context.Context`（取消、超時、追蹤傳遞），避免 goroutine 洩漏。

---

## 第六條：資料與審計 (Data & Audit)
- **6.1 持久化：** 核心階段用 SQLite（`modernc` 純 Go 驅動）+ `MEMORY.md` 檔案；向量檢索放擴展階段。
- **6.2 審計 day-one：** `tool_invocations` 與 `llm_calls` 於核心階段即落庫（不是只放日誌），讓可審計的資料地基從一開始就立起來。

---

## 治理 (Governance)
本憲法具有最高優先級，其效力高於任何 `CLAUDE.md` 或單次會話中的指令。修訂需明確版本號與批准日期。
