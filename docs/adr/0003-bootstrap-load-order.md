# Bootstrap 上下文的載入順序與覆蓋語義

需求文檔第 12 章把「Bootstrap 檔案載入順序和優先級」列為未決事項並指定技術方案階段決議，但技術方案第 8.3 節只寫 ContextLoader 把三個檔案「拼接」成 system prompt，未定義順序。由於 system prompt 有近因效應（靠後的內容在衝突時通常勝出），而三個檔案的語義層級不同，衝突是必然的。現決定按「**最穩定普遍 → 最具體當下**」排序，後者覆蓋前者：

1. `SOUL.md` 或 Profile 的 `identity.prompt`（我是誰 —— 人格）
2. `AGENTS.md`（這個專案怎麼做事）
3. `USER.md`（這個人偏好什麼）
4. `SKILL.md`（這次要做什麼）

並且 **Profile 的 `identity.prompt` 與 `SOUL.md` 互斥，前者優先**。需求文檔 5.2 寫 identity 段「也可以引用 SOUL.md 檔案」本就是二選一的語氣；若疊加會產生雙重人格。Profile 是比 Workspace 級預設更具體的配置，理應蓋過。

## Consequences

這個語義是**可測**的，因此順序必須以測試固定下來，至少涵蓋：`USER.md` 與 `SOUL.md` 指令衝突時 `USER.md` 勝；`identity.prompt` 存在時 `SOUL.md` 完全不進 prompt；四層皆存在時的拼接順序。若改採「不定順序、讓 LLM 自行權衡」的方案，這些斷言都寫不出來。

未來讀者若看到 `identity.prompt` 存在時 `SOUL.md` 被完全略過，這是**刻意的**互斥設計，不是遺漏拼接，請勿「修正」成疊加。
