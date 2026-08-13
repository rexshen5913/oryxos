package core

import (
	"context"
	"strings"
)

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

// bootstrapIntro 是各層 Bootstrap 注入時標明來源的引言。Bootstrap 是**使用者手寫、
// OryxOS 只讀不寫**的初始設定，與長期記憶那份「Agent 自己記下的成長記錄」角色不同
// （技術方案 §5.4），標明來源讓 LLM 分得出誰說的。措辭不進測試斷言。
const (
	agentsIntro = "以下是這個專案的做事方式（AGENTS.md，由使用者手寫）："
	userIntro   = "以下是使用者的偏好與慣例（USER.md，由使用者手寫）："
)

// composeSystemPrompt 依 ADR-0003 的「最穩定普遍 → 最具體當下」把 system prompt
// 拼起來，衝突時後者勝出（近因效應）：
//
//  1. 人格——Profile 的 identity.prompt **或** SOUL.md
//  2. AGENTS.md（這個專案怎麼做事）
//  3. USER.md（這個人偏好什麼）
//  4. 長期記憶（Agent 經 save_memory 累積的成長記錄）
//
// **第 1 層是互斥、不是疊加**：identity.prompt 存在時 SOUL.md 完全不進 prompt
// （ADR-0003）。Profile 是比 Workspace 級預設更具體的配置，理應蓋過；疊加會產生
// 雙重人格。這是刻意設計，未來讀者不得「修正」成疊加。
//
// 長期記憶排在 USER.md 之後由 spec #2 維護者定案（2026-08-07），依據是同一條排序
// 原理：成長記錄比 USER.md 這種一次性初始設定更當下。已知後果是衝突時長期記憶蓋過
// 使用者手寫的 USER.md——這是刻意的，反過來排會讓使用者口頭更新的偏好被初始設定
// 蓋回去，save_memory 就廢了一半。
//
// Skill 段（漸進揭露的第一層，只有 name ＋ description）排在長期記憶之後，隨
// SKILL.md 載入落地，本切片不涉。
//
// 任一層為空（含只有空白字元）就整段略過，不留一個空標題給 LLM 猜。
func composeSystemPrompt(identityPrompt string, boot BootstrapContext, longTerm string) string {
	persona := identityPrompt
	if strings.TrimSpace(persona) == "" {
		persona = boot.Soul // 互斥：只有 identity.prompt 缺席時 SOUL.md 才生效
	}

	layers := []struct{ intro, body string }{
		{"", persona},
		{agentsIntro, boot.Agents},
		{userIntro, boot.User},
		{longTermMemoryIntro, longTerm},
	}
	sections := make([]string, 0, len(layers))
	for _, l := range layers {
		body := strings.TrimSpace(l.body)
		if body == "" {
			continue
		}
		if l.intro == "" {
			sections = append(sections, body)
			continue
		}
		sections = append(sections, l.intro+"\n\n"+body)
	}
	return strings.Join(sections, "\n\n")
}
