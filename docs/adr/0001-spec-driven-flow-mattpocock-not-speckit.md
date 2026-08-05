# 用 Matt Pocock skills 流程，不用 Spec-Kit

主體開發原本規劃用 GitHub Spec-Kit（`constitution` → `specify` → `plan` → `tasks` → `implement`，每個 user story 後跑 `analyze`），並按五大核心能力拆成 US-1～US-5。改用 Matt Pocock 的 engineering skills 流程：`/grill-with-docs` → `/to-spec` → `/to-tickets` → `/implement`，拆解形狀為 tracer bullet。

## Considered Options

Spec-Kit 的工具鏈當時已就緒（`specify` 與 `uv` 都已安裝），完整的 US-1～US-5 task 拆解也已寫好，切換等於作廢那份拆解。仍然改用 Matt Pocock 流程，理由是：

1. **雙憲法風險。** repo 根目錄已有 `constitution.md`，並由 `CLAUDE.md` 以最高優先級導入。`/speckit.constitution` 會另外產出 `.specify/memory/constitution.md`，兩份憲法並存正是最容易讓 AI agent 走偏的結構。
2. **流程開銷。** Spec-Kit 把 `/speckit.analyze` 定為每個 user story 結束的硬性環節，5 次 analyze 的開銷與核心階段的規模不成比例。
3. **拆解形狀衝突。** Spec-Kit 拆法明說「US-1 實施完成後不立刻有 demo，因為它沒有使用者可見的入口」；tracer-bullet 拆法要求每張 ticket 都產生 end-to-end 可展示行為。兩者互斥，而 tracer-bullet 更貼合「每個階段有可演示成果」的既有目標。

## Consequences

不再產生也不再維護 `.specify/memory/constitution.md`——根目錄的 `constitution.md` 是唯一憲法。

原 AI 編程指南中仍然有效的兩份資產已就位：constitution 原則收進 `constitution.md`，「AI agent 最容易寫錯的地方」清單收進 `docs/AIProgrammingGuide.md` §3 的實作審查清單。其餘 Spec-Kit 相關內容已刪除，需要時查 git 歷史。
