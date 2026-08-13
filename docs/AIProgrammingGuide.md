# 專案篇 OryxOS：AI 編程指南

> 本文檔定義 OryxOS 的開發流程與 AI 協作方式。原則以 `constitution.md` 為準，術語以 `CONTEXT.md` 為準，決策理由見 `docs/adr/`，本文不重複三者的內容。

**目錄**

1. 開發流程
2. 上下文與測試紀律
3. 實作審查清單
4. 增量階段

---

## 1. 開發流程

主體開發使用 Matt Pocock engineering skills：

```
規劃文檔 ＋ CONTEXT.md ＋ docs/adr/
        ↓
/grill-with-docs   審查矛盾與缺口、統一術語、確認決策；產出 CONTEXT.md 與 ADR
        ↓
選定一條最小 end-to-end MVP path
        ↓
/to-spec           產出 MVP Spec
        ↓
/to-tickets        拆成 tracer-bullet tickets
        ↓
/implement         每張 ticket 開新 session
```

### 1.1 拆解形狀：tracer bullet

每張 ticket 必須滿足兩個條件：

- **能在一個全新 context window 內完成**
- **產生可展示或可驗證的 end-to-end 行為**

不要水平分層（先做完所有 database、再做所有 API、最後做 UI）。垂直切片穿透接入層、引擎層、能力層、基礎層，每一刀都留下能跑的東西。observability、fallback 與錯誤處理不得全部留到最後。

AI ticket 必須包含 evaluation 或固定測試案例。明確標記 blocking relationships。

### 1.2 spec 的權威性

`/to-spec` 產出的 spec 是實作契約。實作時：

- 不得擴大 ticket scope
- **proposal 不等於 confirmed decision**——規劃文檔裡的提案不能當成承諾
- 未決事項放進 Further Notes 或 Out of Scope，不要自行補完重要產品決策

---

## 2. 上下文與測試紀律

動工前的上下文讀取順序見 `CLAUDE.md` §3。

測試紀律以憲法第四條為準，其中最容易寫錯的是 4.3 與 4.4 的邊界：

| 依賴 | 測試中怎麼處理 |
| --- | --- |
| SQLite | 用真的（`modernc` ＋ `t.TempDir()`） |
| 檔案系統、`MEMORY.md` | 用真的 |
| MCP server | 用真的（本地 stdio server） |
| **LLM** | **唯一例外**：`httptest.Server` 回放錄製回應，絕不打真實 API |

真實 LLM 呼叫的驗證由需求文檔第 13 章的五個驗收 demo 承擔，不由 CI 承擔。理由見 ADR-0002。

錄製回應需涵蓋 ReAct 循環的關鍵分支：無 tool 呼叫直接回應、單輪 tool 呼叫、多輪連續 tool 呼叫、達 `MAX_ITERATIONS` 強制終止、tool 失敗後重試。

---

## 3. 實作審查清單

每次 implement 後人工檢查這九處。這是 OryxOS 上 AI agent 最常寫錯的地方，規則本文見右欄依據。

| # | 要檢查的錯誤 | 依據 |
| --- | --- | --- |
| 1 | 採用框架的自動執行 Agent 抽象幫忙跑循環 | 憲法 2.1、2.2 |
| 2 | Provider 靠反射或型別掃描區分，而非顯式 map 註冊表 | 憲法 2.3、技術方案 §3.2 |
| 3 | Tool 被拆成 builtin／skill／mcp 多個 package | 技術方案 §6 |
| 4 | `SKILL.md` 被當成可執行 Tool，而非提供給 Agent 閱讀的指令模板（漸進揭露之下正文以 tool 訊息回填，但那個 Tool 遞的是指令文字，Skill 本身沒有 `execute`） | `CONTEXT.md`、技術方案 §6 |
| 5 | 審計表沒落庫，只寫日誌 | 憲法 6.2 |
| 6 | 引入 cgo 依賴（典型如 `mattn/go-sqlite3`） | 憲法 1.2 |
| 7 | 阻塞路徑沒走 `context`，goroutine 洩漏 | 憲法 5.3 |
| 8 | 把 MemoryService 叫「三層門面」並去補第三層（情景記憶屬擴展階段） | `CONTEXT.md` |
| 9 | Bootstrap 疊加 `identity.prompt` 與 `SOUL.md`（兩者互斥） | ADR-0003 |

跨 ticket 上下文斷裂時，回頭讀 spec、`CONTEXT.md` 與最近的程式碼。每張 ticket 完成後 commit，方便回退到穩定狀態。

---

## 4. 增量階段

主體開發完成後進入增量階段（擴展功能、修 bug、加 Plugin Tool）：單次顆粒度小、涉及 1 到 3 個檔案、不跨 package、上下文是已存在的程式碼。

這個階段不強制走完整 spec 流程。社區貢獻者的工作流見 `CONTRIBUTING.md`。

`constitution.md` 與 `CONTEXT.md` 在增量階段仍然有效：前者是社區貢獻程式碼必須遵守的非協商原則，後者是術語的單一事實來源。大 feature（新增 package、改憲法、跨多個核心能力）由專案方決定是否走一次完整 spec 流程。
