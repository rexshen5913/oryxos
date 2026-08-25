---
name: implement-ticket
description: 在 OryxOS 專案實作一張 ticket／issue，從讀齊上下文到產出開發解釋文檔的完整流程——依 CLAUDE.md §3 的順序讀憲法與 ADR、整理成約束表、TDD 紅綠燈、兩階段落地保住遷移安全性、突變測試自我審查、對照九點清單、更新工作記錄，最後發布一份記錄推導過程的開發解釋 Artifact。觸發時機：使用者要求實作或落地某張 ticket／issue（「實作 ticket #54」「把 #48 做掉」「做這兩張票」）、要求繼續一張做到一半的票、或要求依審查意見修正剛完成的實作。不用於純探索問答、純文檔編輯、或只是跑一次測試。
---

# 實作一張 ticket

七個階段，順序不可調換。每個階段有明確產出，做完才進下一個。

```
1 讀上下文 → 2 提計畫 → 3 TDD 紅燈 → 4 第一階段落地
                                          ↓
      7 收尾 ← 6 自我審查 ← 5 第二階段落地（綠燈）
```

詳細規格在兩份 reference，**在對應階段才讀**：

- `references/testing.md` — 階段 3 開始前讀。seam 規則、斷言對象、真實依賴邊界、突變測試設計
- `references/explainer.md` — 階段 7 產出 Artifact 前讀。結構、命名、設計 token

工具：`scripts/mutate.py` — 階段 6 的突變測試執行器。

---

## 硬規則

這幾條最容易踩，先看：

**不 commit、不 `git add`。** 預設停在未提交狀態，讓使用者自己在 Source Control 看 diff 再決定。只約束提交，`git status`／`git diff`／`git log` 照常用。若本機 `CLAUDE.local.md` 另有指定，以它為準。

**不擴大 scope。** 只做 ticket 明確要求的。看到相鄰的問題就記進工作記錄的「刻意不做的事」，不要順手修。

**proposal 不等於 confirmed decision。** issue 本文與 comment 裡的定案照做、不重議。標為「建議」「待評估」的不要當定案實作。

**不為測試新開 seam。** 透過既有 seam 驅動；新增觀測面不算新 seam。

**術語用 `CONTEXT.md` 的正式詞。** 不漂移到 `_Avoid_` 那欄。特別是不要用「Tool Policy」指稱 Profile 的 `tools` 欄位過濾——那是兩件事。

**不支援 Windows（ADR-0006）。** 不新增任何 Windows 相容路徑、build tag 或 `runtime.GOOS` 判斷。但既有處理「來自 Windows 的檔案」的程式碼（`internal/config` 的 CRLF 正規化）與這條無關，不要動。

**評測 harness 絕不進 `go test`。** 它跑真實 Provider，也不得在 `make test` 或 CI 裡被觸發。

---

## 階段 1：讀上下文，整理成約束表

依 `CLAUDE.md` §3 的順序讀，衝突時上位優先：

1. `constitution.md` — 不可協商原則
2. `CONTEXT.md` — 正式術語表
3. `docs/adr/` — 與工作範圍相關的架構決策
4. originating spec ＋ ticket 全文（含 **comments**，定案常在留言裡）
5. `docs/` 規劃文檔

```bash
gh issue view <spec-number>
gh issue view <spec-number> --comments --json comments -q '.comments[] | "=== \(.author.login) ===\n\(.body)"'
gh issue view <ticket-number>
```

同時讀既有程式碼：這次要動的檔案、要抄形狀的既有介面、既有測試的 helper。

**產出：一張約束表。** 不是背景知識，是後面每一步都要對照的東西：

| 來源 | 約束 | 對這次的實際影響 |
|---|---|---|
| 憲法 4.1 | TDD 紅綠燈 | 每個行為先寫失敗測試 |
| 憲法 5.2 | 依賴顯式注入 | 新介面從組裝點傳進來 |
| ADR-000X | … | … |
| `CONTEXT.md` | 某術語的定義 | 決定某個東西該放哪一層 |

後面每個「為什麼這樣做」都要指得回這張表的某一列。

若產出與既有 ADR 抵觸，**明白指出並說明為何值得重新討論**，不要默默覆寫。

## 階段 2：提計畫

動手前提出計畫，讓使用者有機會攔。計畫要包含：

- 新增哪些型別／檔案，改哪些既有檔案
- 測試怎麼安排（哪個 seam、哪些格）
- 是否需要兩階段落地
- 這次**刻意不做**的相鄰事項

ticket 有多張且耦合時（`Blocked by` 或 spec 說「應相鄰落地」），一起做；說明耦合點具體在哪。

## 階段 3：TDD 紅燈

**先讀 `references/testing.md`。**

寫測試的當下就在決定 API——先寫出「怎麼用它」才寫得出斷言。

跑一次，確認紅燈是**預期的那種**（型別未定義、斷言不符），不是環境壞掉：

```bash
go test ./internal/<pkg>/ -count=1 -run '<新測試>'
```

## 階段 4：第一階段落地（行為零變更）

**適用時機**：新增型別、新增介面參數、新增欄位——任何會動到既有呼叫點的改動。ticket 或 spec 明文要求「兩階段落地」時一定要做。

只做兩件事：

1. 加型別／參數
2. 既有呼叫點傳零值或空實作（`NopXxx{}`、變參留空）

**不加任何行為。** 然後停下來驗：

```bash
go build ./... && go vet ./...
go test ./... -skip '<這次新增的測試名，用 | 串起來>'
```

要求：**既有測試全綠，且斷言一行未改。** 新測試此時仍紅。

**為什麼值得多這一步**：一口氣做完的話，某個既有測試轉紅時分不出是「加參數弄壞的」還是「加行為弄壞的」。中間這次全綠把責任切乾淨——之後任何紅燈都只可能來自第二階段。

把這次的輸出留著，階段 7 要寫進工作記錄當驗收證據。

## 階段 5：第二階段落地（綠燈）

加行為，讓新測試轉綠。

實作時每個**位置選擇**都要能說出理由，並寫進註解：

- 播報／記錄放在呼叫之前還是之後
- 這個東西屬於哪一層（追控制流找答案，不要憑印象）
- 為什麼是這個順序

註解寫「為什麼」，不寫「這行在做什麼」。既有程式碼的註解風格就是這樣，跟著走。

跑該 package 的測試確認轉綠，再跑全量：

```bash
go test ./internal/<pkg>/ -count=1
go test ./... -count=1
```

## 階段 6：自我審查

三件事，都要做。

### 6a 突變測試

**測試全綠不代表測試有用。** 設計突變定義（格數依改動大小，通常 8–16 格），寫成 JSON 放 scratchpad：

```bash
python3 .claude/skills/implement-ticket/scripts/mutate.py <scratchpad>/mutations.json
```

哪些性質值得一格、對照組怎麼用、五種輸出各代表什麼，見 `references/testing.md`。

**只有「轉紅 ✓」算通過**，其餘五種（仍綠、對照組也轉紅、無效證據、baseline 不可信、跳過）都計入失敗、離開碼非零；空的定義也回非零。**不要跳過任何一格**：補測試，或承認那條理由沒有證據。

執行器解析 `go test -json` 而不是只看離開碼——編譯失敗、測試名拼錯、以及「測試過了但 package 另外失敗」都會產生假證據，三種都在 `references/testing.md` 有實測對照表。

改動 `mutate.py` 之後跑它自己的測試：`python3 .claude/skills/implement-ticket/scripts/test_mutate.py`。

### 6b 九點清單

對照 `docs/AIProgrammingGuide.md` §3 逐條自檢：

1. 框架自動執行抽象 — 有沒有引入
2. Provider 顯式註冊 — 有沒有改成反射／掃描
3. Tool package 拆分 — 有沒有動（`internal/tool` 不拆，issue #44 的評估另案）
4. `SKILL.md` 被當可執行 Tool — 有沒有
5. 審計落庫 — 有沒有變成只寫日誌
6. cgo 依賴 — 有沒有引入
7. 阻塞路徑走 `context` — goroutine 有沒有洩漏
8. MemoryService「三層門面」— 有沒有去補第三層
9. `identity.prompt` 與 `SOUL.md` 疊加 — 有沒有破壞互斥

逐條寫出「未動」或實際情況，不要只寫「全部通過」。

外加 ADR-0006：確認沒有新增 Windows 路徑或 build tag。

### 6c 完整驗證

```bash
go build ./... && go vet ./... && gofmt -l .
make test
make build && rm -f oryxos
go test -race ./internal/<動到的 pkg>/ -count=1
git status --short          # 確認變更範圍就是預期的那些檔案
```

`gofmt -l .` 要無輸出。`make build` 產出的二進制記得刪掉（`.gitignore` 有它，但別留在工作區）。

## 階段 7：收尾

### 7a 更新工作記錄

`implement.md`（已 gitignore，逐次工作整檔替換）。它同時是本機 Codex 審查 gate 的審查基準——**內容必須與實際變更一致**，寫得比實際多或少都會讓 gate 誤判。

必要章節：

- **交付物** — 新檔（檔案／內容）、改檔（各自改了什麼）、既有測試檔的改動行數
- **設計決策** — 每個編號一條，寫**為什麼**不是寫做了什麼；被否決的路徑也寫
- **遷移安全性** — 第一階段的驗收輸出（階段 4 留的那份）
- **測試策略** — seam、斷言對象、AC 對照表（每條 AC 由哪支測試守）
- **突變測試** — 表格：突變／該轉紅的測試／結果
- **已知限制** — 沒測到的地方要主動寫，不要藏
- **刻意不做的事** — 相鄰但不在 scope 內的
- **九點清單自檢** — 逐條
- **驗證** — 實際跑過的指令與結果

### 7b 產出開發解釋 Artifact

**先讀 `references/explainer.md`**，再讀 `artifact-design` skill（發布任何 Artifact 前的必要步驟）。

檔名帶 ticket 編號寫在 scratchpad，票號寫進 `description`——同一路徑重發會覆蓋同一個 URL。

十段結構、設計 token 都在 `references/explainer.md`。核心要求：**貼真的程式碼，寫推導過程不是結論清單**。

### 7c 回報

終端輸出要包含：

- 做了什麼（分票）
- 幾個值得看的決策，各一句理由
- 驗證結果（照實寫，測試沒過就說沒過）
- 已知缺口
- Artifact 連結
- 變更停在未提交狀態，範圍是哪些檔案
- 下一步建議（同一 spec 還剩哪些票，哪張接著做最省事）

---

## 若本機有 Codex 審查 gate

以下**僅在 `.claude/hooks/codex-stop-review.sh` 存在時**適用，不成立就整段跳過：

Stop 時會對整個 repo 的未提交變更跑唯讀審查，比對 `implement.md`。未通過會 block 並回饋意見。

**收到意見時**：逐點判斷，不要全盤照收也不要全盤反駁。

- 同意的：修，並補上對應的測試（**修了但沒有測試守著，等於下一輪還會被抓**）
- 不同意的：逐點說服，把理由寫清楚——下一次 Stop 會把論證送回同一個 session
- 意見指出「論證與證據對不上」時特別要當真：那通常代表**有一條理由沒有測試守著**，用突變測試驗證缺口是否真的存在，再補測試

修完要更新 `implement.md`，加一節記錄那一輪的意見與處置。
