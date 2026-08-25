# 測試策略

憲法第四條的落地細節。動手寫第一支測試前讀這份。

## 目錄

- [seam：透過既有的，不新開](#seam透過既有的不新開)
- [斷言什麼](#斷言什麼)
- [真實依賴的邊界](#真實依賴的邊界)
- [讓真實依賴真的失敗](#讓真實依賴真的失敗)
- [記錄型觀測面](#記錄型觀測面)
- [負向路徑](#負向路徑)
- [突變測試](#突變測試)

---

## seam：透過既有的，不新開

**規則：透過既有 seam 驅動，不為了測試在生產程式碼上開新入口。**

`internal/core` 的主 seam 是 `AgentService.Process`，外部測試 package（`core_test`）已圍繞它建好完整 helper：Provider 以 `httptest` 回放錄製回應、記錄型伺服器保存每次請求 body、配套解析函式把 body 轉成可斷言的最小形狀。

為測試新開的入口從此要一直維護，而且只有測試在用。**新增觀測面不算新 seam**：記錄型 `EventSink` 掛在同一個 `Process` 上，與既有測試用記錄型伺服器觀測 Provider 請求是同一個手法。

判斷方式：問「這個東西是**驅動**執行的，還是**觀察**執行的？」觀察面可以隨便加，驅動點不行。

## 斷言什麼

斷言**外部可觀察行為**：

- 送給 Provider 的請求內容
- 回填給 LLM 的訊息內容
- 播報出來的事件序列
- 落進審計表的記錄
- 結構化日誌的欄位
- 寫進 `io.Writer` 的那幾行字

不斷言「某個內部函式被呼叫幾次」、「某個結構體欄位的值」。

**判準**：重構實作（換資料結構、拆函式、調整內部流程）而外部行為不變時，這個測試應該保持綠色。斷言內部細節的測試會在重構時無辜轉紅，久了就沒人相信它。

**不拿一個來源驗自己**。事件數要跟**記錄型伺服器實際收到的請求數**比、Tool 事件對數要跟 **`tool_invocation` 日誌筆數**比——兩個獨立來源對得上才算數。

## 真實依賴的邊界

憲法 4.3／4.4 與 ADR-0002 定死了：

| 依賴 | 測試裡怎麼做 |
|---|---|
| SQLite | 真的，`modernc` 配 `t.TempDir()` |
| 檔案系統、`MEMORY.md` | 真的，`os.Root` 配 `t.TempDir()` |
| MCP client | 真的，起本地 stdio server |
| HTTP 目標端點 | 真的，另一個 `httptest.Server` |
| **LLM** | **唯一例外**：`httptest.Server` 回放錄製回應，絕不打真實 API |

新的錄製回應放 `internal/core/testdata/reply_*.json`，命名說明它在演哪個分支。

## 讓真實依賴真的失敗

要測失敗路徑時，**讓真實依賴真的失敗**，不要塞一個會回錯誤的假物件。既有手法：

```go
// 持久化失敗：先組好引擎，再關掉那個真實的 SQLite
st := newStore(t)
agent := newEventAgentOn(t, srv.URL, sink, st, discardLogger())
st.close()                       // 之後的 SaveSession 會回錯誤
```

```go
// 可重試的網路失敗：目標端點斷線不回應
hj, _ := w.(http.Hijacker)
conn, _, _ := hj.Hijack()
_ = conn.Close()
```

```go
// Sandbox 拒絕：白名單填一個對不上的域名，走真實的校驗路徑
```

這與「模擬進程結束」的既有跨重啟測試是同一個手法，不是繞過憲法 4.3。

## 記錄型觀測面

出向介面（`AuditStore`、`EventSink` 這類）的測試實作：

```go
type recordingSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (s *recordingSink) Emit(_ context.Context, e core.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}
```

**帶鎖**，即使目前的呼叫端是序列的——它是出向介面的實作，而介面沒有承諾單一 goroutine，測試的實作不該比契約更寬。

## 負向路徑

**不得只錄 happy path。** 每個功能至少要有一格負向：依賴故障、輸入不合法、資源被拒、上游 panic、逾時／取消。

錄製回應也一樣：LLM 回應沒有 `choices`、Tool 失敗後的下一輪、重試耗盡後的收尾，都要有 fixture。

## 突變測試

自我審查的主要手段。**測試全綠不代表測試有用**——把生產程式碼改成「錯的那個樣子」，對應的測試應該轉紅。仍綠代表那條理由現在只是一段散文。

用 `scripts/mutate.py`，突變定義寫成 JSON 放 scratchpad：

```json
[
  {
    "label": "EmitEvent 拿掉 Text 去敏",
    "package": "./internal/core/",
    "test": "TestProcessEventTextRedacted",
    "edits": [
      { "file": "internal/core/event.go",
        "old": "\te.Text = RedactErrorText(e.Text)\n",
        "new": "" }
    ]
  }
]
```

### 哪些性質值得一格

每一條**寫進註解或工作記錄的理由**都該有一格。具體來說：

- 順序性質（先掛載者在外層、播報在呼叫之前）
- 成對性質（進與出、開始與結束）
- 邊界選擇（這個播報點在 A 層不在 B 層）
- 去敏、截斷、上限這類「漏了不會有人發現」的收口
- 護欄（`recover`、fail-closed 的零值、白名單預設拒絕）
- 遷移安全性（不掛中介層時行為一致）

### 對照組

當一條性質**只有某一支測試守得到**時，用 `control` 欄位把另一支拉進來當對照，期望它**仍綠**：

```json
{
  "label": "turn_finished 挪到 loop.Run 之後",
  "package": "./internal/core/",
  "test": "TestProcessPersistenceFailureStillEmits",
  "control": "TestProcessFailedTurnStillEmits",
  "edits": [ "..." ]
}
```

輸出會寫「對照 X 仍綠（只有前者守得到這條）」。**那個「仍綠」就是缺口的證據**——它證明在補這支測試之前，那條理由沒有人守著。

### 同一檔案多處改動

`edits` 是**累積套用**的。多處改動要各自寫一個 edit，工具會依序疊加。（早期版本各自從原始內容出發，導致只有最後一處生效、測試卻照樣轉紅——那種假通過比沒測還糟。）

### 讀輸出

| 輸出 | 意思 | 該做什麼 |
|---|---|---|
| 轉紅 ✓ | 測試守得到 | 記進工作記錄 |
| 仍綠 ✗ | 測試沒守到這條 | **補測試**，或承認那條理由沒有證據 |
| 對照組也轉紅 ✗ | 「只有前者守得到」不成立 | 換一支真正守不到這條的當對照，或拿掉這個宣稱 |
| 無效證據 ✗ | 這次執行說明不了任何事：編譯不過、沒匹配到、或**測試過了但 package 另外失敗** | 把突變寫得更小、只改行為不改語法或收尾 |
| 拒絕（baseline） | 測試突變前就是紅的，或**名字沒匹配到** | 先讓它綠；名字打錯就改對 |
| 跳過 | `old` 命中不唯一 | 把 `old` 寫得更長、更唯一 |

**只有「轉紅 ✓」算通過**，其餘五種都計入失敗並讓離開碼非零。空的定義也回非零——沒有證據就不是通過。跑完會自動 `go build ./...` 確認無殘留；build 失敗就立刻查 `git diff`。

### 為什麼不只看 `go test` 的離開碼

執行器解析 `go test -json`，要求**至少一支匹配的測試真的執行過**。只看離開碼會產生兩種假證據，兩種都實測確認過：

| 情境 | 離開碼 | Test 事件 | package 結果 | 天真的判定 |
|---|---|---|---|---|
| 突變造成編譯失敗 | 非零 | 無 | build-fail | 轉紅 ✓（**假的**，測試沒跑） |
| 測試名拼錯／零匹配 | **0** | 無 | pass | 仍綠（**假的**，測試沒跑） |
| 測試過了但 package 另外失敗 | 非零 | run→pass | **fail** | 看離開碼→轉紅、看 Test 事件→仍綠（**都假的**） |

第二種對 `control` 特別危險：一個拼錯的對照組名字會安靜地產生「只有前者守得到這條」的假證據。所以 baseline 階段就會檢查名字匹配得到。

第三種最陰險——一個同時弄壞 target 斷言與 `TestMain` 收尾的突變，會被報成「target 轉紅、control 仍綠」，**一份憑空捏造的證據，而且離開碼是 0**。所以綠色結果要求三件事同時成立：有 `Test` 的 run 事件、package 乾淨收尾、離開碼 0。

### 工具自己的測試

`scripts/mutate.py` 是這套自我審查的地基——它把「測試沒守到」判成通過的話，整套機制就成了擺設。改動它之後跑：

```bash
python3 .claude/skills/implement-ticket/scripts/test_mutate.py
```

31 格，注入假的 test runner、不打真的 `go test`。刻意不進 `make test`：那是 Go 專案的測試指令，這是工具腳本自己的測試，兩者不同層。
