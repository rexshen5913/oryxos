# 貢獻指南

感謝你對 OryxOS 的興趣！本文說明如何參與這個專案。

> [!IMPORTANT]
> **先讀 [`constitution.md`](./constitution.md)。**
> 專案憲法定義六條不可協商的核心開發原則，效力高於本文與任何單次討論。
> 任何與憲法牴觸的 PR 都不會被合併——若你認為某條原則應該修訂，請先開 Issue 討論，而不是在 PR 裡繞過它。

---

## 專案現況

OryxOS 目前處於 **pre-alpha 實作初期**：Go module 骨架（`go.mod`、`cmd/oryxos`、8 個 `internal/` package、`Makefile`）已落地，核心功能依 spec（issue #1）的 tickets 逐張實作中。

現階段最有價值的貢獻仍是**文檔與設計討論**：

- 對 [`docs/`](./docs/) 下規劃文檔的修正、補充或質疑
- 對 [`docs/adr/`](./docs/adr/) 的架構決策提出替代方案（附理由與取捨分析）
- 指出文檔之間的不一致，或 [`CONTEXT.md`](./CONTEXT.md) 術語與文檔用詞的落差

建置系統已落地（`make test`／`make build`），下方「開發流程」與「程式碼準則」自此生效。核心功能 tickets 由專案方依 spec 推進，社區程式碼貢獻的主場在核心階段完成後的增量階段（見 `docs/AIProgrammingGuide.md` §4）。

---

## 回報問題

開 Issue 前請先搜尋既有 Issue，避免重複。

- **Bug** — 使用 Bug 回報範本，附上重現步驟、預期行為與實際行為、Go 版本與作業系統。
- **功能建議** — 使用功能建議範本。請說明**你要解決的問題**，而不只是你想要的解法。
- **安全漏洞** — **請勿開公開 Issue**，改依 [`SECURITY.md`](./SECURITY.md) 的流程私下回報。

---

## 開發流程

> 以下指令目前皆可使用；`oryxos` 的功能命令（`init`、`chat` 等）會隨 tickets 逐步補齊。

```bash
# 前置需求：Go >= 1.24
git clone https://github.com/rexshen5913/oryxos.git
cd oryxos

make test                                    # 執行所有測試
CGO_ENABLED=0 go build -o oryxos ./cmd/oryxos  # 建置單一靜態二進制
```

提交 PR 前：

1. 從 `main` 開新分支
2. **先寫失敗的測試**（見下方「測試先行」）
3. 實作至測試通過
4. 確認 `make test` 全綠、`go vet ./...` 無警告
5. 開 PR，填寫 PR 範本

---

## 程式碼準則

**完整規則見 [`constitution.md`](./constitution.md)，本節不重複條文。** 以下是 PR 最常被退回的四個原因：

1. **只有實作、沒有測試。** 憲法第四條要求從失敗的測試開始，不可協商。
2. **LLM 測試打了真實 API。** 可確定化的依賴（SQLite、檔案系統、本地 MCP server）一律用真的；LLM 是唯一例外，測試中以 `httptest.Server` 回放錄製回應（憲法 §4.3–4.4）。
3. **引入 cgo 依賴。** 會破壞 `CGO_ENABLED=0` 單一靜態二進制（憲法 §1.2）。
4. **用了反射或型別掃描自動裝配。** Provider 與 Tool 一律顯式註冊（憲法 §2.3）。

新增第三方依賴需在 PR 描述中說明理由。

領域概念的用詞請對照 [`CONTEXT.md`](./CONTEXT.md)；若你的改動與既有 [ADR](./docs/adr/) 抵觸，請在 PR 中明講。

---

## Commit 規範

嚴格遵循 [Conventional Commits](https://www.conventionalcommits.org/)：

```
<type>(<scope>): <subject>
```

常用 `type`：`feat`、`fix`、`docs`、`refactor`、`test`、`chore`、`perf`、`build`、`ci`

`scope` 使用 package 名稱（如 `core`、`provider`、`memory`、`tool`、`web`、`storage`、`config`）；文檔期則用 `docs`、`website`、`agents`。

範例：

```
feat(provider): 新增 OpenAI 兼容 adapter 的 streaming 支援
fix(memory): 修正長期記憶寫入時未包裝 error
docs(readme): 補上快速開始章節
```

---

## 授權

提交貢獻即表示你同意你的貢獻以 [MIT License](./LICENSE) 授權釋出。
