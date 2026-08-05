# Domain docs

本 repo 為 **single-context**：根目錄一份 `CONTEXT.md`（術語表）＋ `docs/adr/`（架構決策紀錄）。

## 消費規則

- 探索前先讀 `CONTEXT.md`，再讀與工作範圍相關的 ADR。完整的上下文讀取順序見 `CLAUDE.md` §3。
- 產出提到領域概念時（issue 標題、ticket、假設、測試名稱），一律使用 `CONTEXT.md` 的正式術語，不要漂移到 `_Avoid_` 列出的同義詞。
- 需要的術語不在 `CONTEXT.md` 裡，是個信號：要嘛你在發明專案不用的語言（重新考慮），要嘛是真的缺口（交給 `/domain-modeling`）。
- 產出與既有 ADR 抵觸時明白指出，不要默默覆寫。

## 維護規則

- `CONTEXT.md` 只收 OryxOS 特有的概念，不收一般程式設計概念，且不含實作細節與模組名——它是術語表，不是規格書。
- ADR 只在三個條件同時成立時建立：難以逆轉、缺少上下文會讓人困惑、是真實取捨的結果。編號依 `docs/adr/` 現有最大值遞增。
