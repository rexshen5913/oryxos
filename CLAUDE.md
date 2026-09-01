# oryxos 專案上下文總入口

@./constitution.md

你是一名資深的 Go 語言工程師，正在協助開發 OryxOS——一個用 Go 實作的企業級 Agent OS。所有行動必須嚴格遵守上面導入的專案憲法。

---

## 0. 專案現況

**實作啟動。** Go module 骨架已落地（`go.mod`、`cmd/oryxos`、8 個 `internal/` package、`Makefile`；ticket #2）。核心功能依 spec #1（issue #1）的 tickets #3～#6 逐張落地中，§2 的 package scope 自此生效。

---

## 1. 技術棧與環境

- **語言**：Go >= 1.24
- **建置與測試**：
  - 執行所有測試：`make test`
  - 建置二進制：`make build`（`CGO_ENABLED=0 go build -o oryxos ./cmd/oryxos`）
  - Web 服務（`make web`／`oryxos server`）尚未實作，屬後續 ticket，落地時再補對應 target

---

## 2. Git 與版本控制

Commit message 嚴格遵循 Conventional Commits：`<type>(<scope>): <subject>`。

`scope` 用 package 名（`core`、`provider`、`memory`、`tool`、`web`、`storage`、`config`、`cli`、`eval`）；文檔期則用 `docs`、`website`、`agents`。

---

## 3. 上下文讀取順序

動工前依序讀，衝突時上位優先：

1. **`constitution.md`** — 不可協商原則，唯一的原則來源
2. **`CONTEXT.md`** — 正式術語表。輸出提到領域概念時一律用其中的詞，不要漂移到 `_Avoid_` 列出的同義詞
3. **`docs/adr/`** — 與工作範圍相關的架構決策（「為什麼」的唯一來源）
4. **originating spec ＋ ticket 全文**
5. **`docs/` 規劃文檔** — 需求與技術方案的細節依據

若你的產出與既有 ADR 抵觸，明白指出並說明為何值得重新討論，不要默默覆寫。

---

## 4. 協作準則

- **新增功能**：先讀上述上下文並對照憲法，提出計畫後再動手。不擴大 ticket scope。
- **編寫測試**：優先表格驅動測試。真實依賴 vs LLM 錄製回應的邊界見憲法 §4.3–4.4。
- **建置專案**：優先提議使用 `Makefile` 中定義好的指令。
- **開發流程**：`/to-spec` → `/to-tickets` → `/implement`，拆解形狀為 tracer bullet。詳見 `docs/AIProgrammingGuide.md`。

---

## 5. Agent skills

### Issue tracker

GitHub Issues（`rexshen5913/oryxos`），透過 `gh` CLI 操作。詳見 `docs/agents/issue-tracker.md`。

### Triage labels

五個標準角色沿用預設標籤字串：`needs-triage`、`needs-info`、`ready-for-agent`、`ready-for-human`、`wontfix`。詳見 `docs/agents/triage-labels.md`。

### Domain docs

Single-context：根目錄 `CONTEXT.md`（術語）＋ `docs/adr/`（決策）。讀取順序見 §3。
