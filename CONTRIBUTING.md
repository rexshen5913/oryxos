# 貢獻指南

感謝你對 OryxOS 的興趣！本文說明如何參與這個專案。

> [!IMPORTANT]
> **先讀 [`constitution.md`](./constitution.md)。**
> 專案憲法定義六條不可協商的核心開發原則，效力高於本文與任何單次討論。
> 任何與憲法牴觸的 PR 都不會被合併——若你認為某條原則應該修訂，請先開 Issue 討論，而不是在 PR 裡繞過它。

---

## 專案現況

OryxOS 目前處於**文檔級規劃期**，repo 尚無 Go 程式碼（`go.mod`、`cmd/`、`internal/`、`Makefile` 皆未建立）。

這代表現階段最有價值的貢獻是**文檔與設計討論**：

- 對 [`docs/`](./docs/) 下規劃文檔的修正、補充或質疑
- 對架構決策提出替代方案（附理由與取捨分析）
- 指出文檔之間的不一致

程式碼相關的流程（下方「開發流程」與「程式碼準則」）在建置系統落地後才會生效，先列於此作為預期規則。

---

## 回報問題

開 Issue 前請先搜尋既有 Issue，避免重複。

- **Bug** — 使用 Bug 回報範本，附上重現步驟、預期行為與實際行為、Go 版本與作業系統。
- **功能建議** — 使用功能建議範本。請說明**你要解決的問題**，而不只是你想要的解法。
- **安全漏洞** — **請勿開公開 Issue**，改依 [`SECURITY.md`](./SECURITY.md) 的流程私下回報。

---

## 開發流程

> 以下指令待建置系統落地後生效。

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

以下為憲法的實作要點摘要，完整條文以 [`constitution.md`](./constitution.md) 為準。

### 測試先行（不可協商）

所有新功能與 Bug 修復，都**必須從一個失敗的測試開始**。

- 嚴格遵循 **Red → Green → Refactor** 循環
- 單元測試**優先採用表格驅動測試（Table-Driven Tests）**
- **優先寫使用真實依賴的整合測試**，而非堆疊 mock

只有實作、沒有測試的 PR 不會被合併。

### 明確性

- **錯誤一律顯式處理**，傳遞時必須以 `fmt.Errorf("...: %w", err)` 包裝
- **不使用全域變數傳遞狀態**，所有依賴透過函式參數或結構體成員顯式注入
- **所有阻塞路徑走 `context.Context`**（取消、超時、追蹤傳遞），避免 goroutine 洩漏

### 簡單性

- **YAGNI** — 只實作規格中明確要求的功能
- 簡單的函式與資料結構，優於複雜的介面與繼承體系
- **顯式優於魔法** — Provider 與 Tool 一律顯式註冊，絕不靠反射或型別掃描自動裝配

### 依賴

- **禁止引入 cgo 依賴**，以守住 `CGO_ENABLED=0` 單一靜態二進制
  （SQLite 用純 Go 的 `modernc.org/sqlite`，不用 `mattn/go-sqlite3`）
- **標準庫優先**，不引入非必需的重框架
- 新增第三方依賴需在 PR 描述中說明理由

---

## Commit 規範

嚴格遵循 [Conventional Commits](https://www.conventionalcommits.org/)：

```
<type>(<scope>): <subject>
```

常用 `type`：`feat`、`fix`、`docs`、`refactor`、`test`、`chore`、`perf`、`build`、`ci`

`scope` 使用 package 名稱（如 `core`、`provider`、`memory`、`tool`、`web`、`storage`、`config`）。

範例：

```
feat(provider): 新增 OpenAI 兼容 adapter 的 streaming 支援
fix(memory): 修正長期記憶寫入時未包裝 error
docs(readme): 補上快速開始章節
```

---

## 授權

提交貢獻即表示你同意你的貢獻以 [MIT License](./LICENSE) 授權釋出。
