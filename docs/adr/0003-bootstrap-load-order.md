# Bootstrap 上下文的載入順序與覆蓋語義

需求文檔第 12 章把「Bootstrap 檔案載入順序和優先級」列為未決事項並指定技術方案階段決議，但技術方案第 8.3 節只寫 ContextLoader 把三個檔案「拼接」成 system prompt，未定義順序。由於 system prompt 有近因效應（靠後的內容在衝突時通常勝出），而三個檔案的語義層級不同，衝突是必然的。現決定按「**最穩定普遍 → 最具體當下**」排序，後者覆蓋前者：

1. `SOUL.md` 或 Profile 的 `identity.prompt`（我是誰 —— 人格）
2. `AGENTS.md`（這個專案怎麼做事）
3. `USER.md`（這個人偏好什麼）
4. **長期記憶**（`MEMORY.md` —— Agent 自己記下的成長記錄）
5. `SKILL.md`（這次要做什麼）

**第 4 層由 spec #2 補入**（維護者定案 2026-08-07）：長期記憶排在 `USER.md` 之後、`SKILL.md` 之前，依據是本 ADR 自身的排序原理——長期記憶是持續更新的成長記錄，比 `USER.md` 這種一次性初始設定更當下、比 `SKILL.md`「這次要做什麼」更穩定。已知後果是衝突時長期記憶（近因）蓋過使用者手寫的 `USER.md`，這是刻意的：反過來排會讓使用者口頭更新的偏好被初始設定蓋回去，`save_memory` 就廢了一半。Bootstrap 與長期記憶的角色也不同——前者是使用者手寫、OryxOS 只讀不寫的「初始設定」，後者是 Agent 經 `save_memory` 寫入的「成長記錄」。

**第 5 層採漸進揭露**（維護者定案 2026-08-12，spec #3）：常駐在 system prompt 的只有每份 Skill 的 `name` 與 `description`，**正文不在這一層**——Agent 判斷相關時才經內建 Tool `load_skill` 取回，以 tool 訊息回填進對話。因此本層的體積與 Skill 數量成正比、與 Skill 內容長度無關。

並且 **Profile 的 `identity.prompt` 與 `SOUL.md` 互斥，前者優先**。需求文檔 5.2 寫 identity 段「也可以引用 SOUL.md 檔案」本就是二選一的語氣；若疊加會產生雙重人格。Profile 是比 Workspace 級預設更具體的配置，理應蓋過。

## Consequences

這個語義是**可測**的，因此順序必須以測試固定下來，至少涵蓋：`USER.md` 與 `SOUL.md` 指令衝突時 `USER.md` 勝；`identity.prompt` 存在時 `SOUL.md` 完全不進 prompt；各層皆存在時的拼接順序；長期記憶落在 `USER.md` 之後、Skill 段之前；Skill 段只有 `name` 與 `description`、正文不在 system prompt 裡。若改採「不定順序、讓 LLM 自行權衡」的方案，這些斷言都寫不出來。

未來讀者若看到 `identity.prompt` 存在時 `SOUL.md` 被完全略過，這是**刻意的**互斥設計，不是遺漏拼接，請勿「修正」成疊加。
