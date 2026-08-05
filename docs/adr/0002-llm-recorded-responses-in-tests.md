# 自動化測試中 LLM 走錄製回應，其餘依賴用真的

憲法第四條 4.3 規定「拒絕 Mocks、使用真實的依賴」，而 `prompt.md` 的 `/implement` 規定「AI 行為優先使用固定 fixtures、recorded responses」「不要讓一般 CI 測試依賴即時且非確定性的模型輸出」。兩者直接衝突，且會擋住第一個測試——ReActLoop 的唯一依賴就是 LLM。我們界定 4.3 的適用邊界而不選邊站：**所有可確定化的依賴照 4.3 用真的**（SQLite 用 `modernc` ＋ `t.TempDir()`、檔案系統與 `MEMORY.md` 用真實檔案、MCP Client 起真實的本地 stdio server），**LLM 是唯一例外**，在自動化測試中以 `httptest.Server` 回放錄製好的 OpenAI 兼容回應。憲法已據此修訂至 1.2（2026-08-05）。

## Considered Options

考慮過兩個選邊站的做法，都拒絕了：

- **憲法字面優先，LLM 也打真實 API。** 後果是同一個 prompt 可能回傳不同的 `tool_calls`，紅綠燈隨機閃，4.1 的 Red-Green-Refactor 循環直接失效——等於用 4.3 破壞 4.1。CI 還要帶 API key、計費、承受 rate limit。
- **`prompt.md` 優先，全面走 fixtures 並大幅放寬 4.3。** 不必要地犧牲了 4.3 真正站得住的部分：SQLite 是純 Go 驅動、檔案系統用 `t.TempDir()`，用真的零成本，沒有理由 mock 掉。

LLM 與其他依賴是**質上不同**的東西：非確定性、計費、需網路。4.3 的意圖是「不要為了測試方便 mock 掉真正的行為」，這個意圖在可確定化的依賴上完全成立，在 LLM 上則自我矛盾。

## Consequences

「使用真實 LLM」的要求並未消失，而是**轉移到 demo 驗收層**：需求文檔第 13 章五個 demo 是核心功能發布的硬條件，於該層以真實 API 手動跑通，並在那裡驗證「至少跑通 DeepSeek 與 Kimi 兩個 Provider」。可重現性由 CI 承擔，真實鏈路由 demo 承擔。

實作上需要一份錄製回應的 fixture 集，涵蓋 ReAct 循環的關鍵分支：無 tool 呼叫直接回應、單輪 tool 呼叫、多輪連續 tool 呼叫、達到 `MAX_ITERATIONS` 強制終止、tool 執行失敗後重試。

未來讀者若看到 LLM 測試用 `httptest.Server` 回放而非真實呼叫，這是**刻意的**，不是對憲法 4.3 的違反或疏漏，請勿「修正」。
