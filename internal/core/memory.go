package core

import "context"

// MemoryService 是 Memory 的統一門面，也是 core 取得與寫回記憶的出向介面，由
// internal/memory 實作、在組裝點顯式注入（憲法 5.2）。介面定義在 core 是為了守住
// 依賴方向（memory 依賴 core，core 不反向依賴 memory）——ReAct 鏈路因此不必知道
// `.oryxos/memory/MEMORY.md` 這條路徑，也不必分別去問 Session 儲存與 MEMORY.md
// 兩個地方（技術方案 §5.1）。
//
// 門面內部只有兩個委託對象（會話記憶、長期記憶）。**不叫「三層門面」**——情景
// 記憶屬擴展階段刻意未實作，別去補它（CONTEXT.md）。
type MemoryService interface {
	// SaveSession 持久化 session 當前的對話歷史（會話記憶）。
	SaveSession(ctx context.Context, session *Session) error
	// LongTermMemory 回傳一份長期記憶快照，供 turn 開始時注入 system prompt。
	// 記憶為空時回空字串、不算錯誤；讀取故障回錯誤，由呼叫端 fail 該 turn。
	LongTermMemory(ctx context.Context) (string, error)
}

// longTermMemoryIntro 是注入 system prompt 時標明來源的引言。注入內容要讓 LLM
// 知道這是它自己記下的成長記錄（與使用者手寫的 Bootstrap 初始設定角色不同），
// 措辭不進測試斷言。
const longTermMemoryIntro = "以下是你的長期記憶——先前經 save_memory 記下、跨對話保留的使用者偏好與關鍵事實："

// composeSystemPrompt 把 system prompt 拼起來：本切片為 Profile 的
// identity.prompt ＋ 長期記憶段。
//
// 長期記憶在完整序列中的位置是 **USER.md 之後、SKILL.md 之前**，由 spec #2
// 維護者定案（2026-08-07），依據是 ADR-0003 自身的排序原理「最穩定普遍 →
// 最具體當下」：長期記憶是持續更新的成長記錄，比 USER.md 這種一次性初始設定
// 更當下、比 SKILL.md「這次要做什麼」更穩定。已知後果是衝突時長期記憶（近因）
// 蓋過使用者手寫的 USER.md——這是刻意的，反過來排會讓使用者口頭更新的偏好被
// 初始設定蓋回去，save_memory 就廢了一半。
//
// **ADR-0003 本文尚未涵蓋這一層**（它只定義 SOUL／identity → AGENTS → USER →
// SKILL 四層）。修訂 ADR 已由 spec #2 指派給 spec #3 一併辦理（Bootstrap 落地
// 的那張），此處不代為改 ADR。本切片產出的是該最終順序的**前綴**，Bootstrap
// 落地時把 AGENTS.md／USER.md 插進中間即可，長期記憶的相對位置不動。
//
// 兩段皆可為空：長期記憶為空時完全不加段落，不留一個空標題給 LLM 猜。
func composeSystemPrompt(identityPrompt, longTerm string) string {
	switch {
	case longTerm == "":
		return identityPrompt
	case identityPrompt == "":
		return longTermMemoryIntro + "\n\n" + longTerm
	default:
		return identityPrompt + "\n\n" + longTermMemoryIntro + "\n\n" + longTerm
	}
}
