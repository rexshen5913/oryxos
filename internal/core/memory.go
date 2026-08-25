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
// 第 5 層是 Skill 段（漸進揭露的第一層，**只有** name ＋ description，正文不在其中）。
// 它排在最後：「這次要做什麼」比「這個人偏好什麼」更當下。段落本身由
// ComposeSkillSection 組好傳進來——它有自己的長度上限與截斷政策，且截斷份數要在
// 啟動時另發警示，那些不屬於「把各層接起來」這件事。
//
// 任一層為空（含只有空白字元）就整段略過，不留一個空標題給 LLM 猜。
//
// **位元組級確定是兩條不變式，不是一條。** 兩者範圍不同，混談會讓其中一條看起來被
// 另一條保證了，實際上沒有：
//
//  1. **本函式（局部）**：這是個純函式，輸出只由四個參數決定。同一組參數值連續組
//     兩次，結果必須位元組完全相同——這裡不得混入時間戳、隨機值或任何不確定的排序。
//  2. **整條鏈路（真正要守的那條）**：來源沒變時（範圍見下一段，比四個參數寬），
//     一個 turn 走完 Profile.BootstrapSelection、ContextLoader.Bootstrap、
//     MemoryService.LongTermMemory、Profile.SkillRefs、ContextLoader.Skills、
//     ComposeSkillSection 之後餵進來的那四個參數必須相同，因而 prompt 也相同。
//
// **第 1 條推不出第 2 條。** 上游若把 Skill 段改成 `map` 迭代，skillSection 這個
// 參數本身就變了——本函式收到不同的輸入、忠實地產出不同的輸出，第 1 條完全沒有被
// 違反，而前綴已經每個 turn 都不一樣了。所以守在這裡的斷言擋不住上游；真正釘住第 2
// 條的是 agent_prompt_determinism_test.go（ticket #45），它從 AgentService.Process
// 驅動、斷言送往 Provider 的 system 訊息位元組相等，涵蓋整條鏈路。
//
// **「來源沒變」指的是四個參數的「有效輸入」沒變，不是磁碟上的一切沒變。** 有效輸入
// 由兩類東西決定，兩類都得不變：
//
//   - **Profile 的相關設定** — identity.prompt（直接成為第一個參數）、bootstrap 欄位
//     （經 Profile.BootstrapSelection 決定讀哪幾份）、skills 欄位（經 Profile.SkillRefs
//     決定載入哪幾份、依什麼順序）
//   - **有效輸入本身** — 被選取的那幾份 Bootstrap 的**載入結果**、MemoryService
//     .LongTermMemory 的**輸出**、被引用的那幾份 Skill 的 name 與 description 及其順序
//
// **這些明確不算，prompt 也不該因它們而變**（算進來會讓下面反向那半條契約過度主張）：
// 沒被 bootstrap 欄位選取的檔案（沒被選中的完全不碰）、沒被 skills 欄位引用的
// SKILL.md、任何 SKILL.md 的**正文**（漸進揭露第一層只取 name 與 description，正文
// 走 load_skill 回填成 tool 訊息、不進 system prompt），以及被既有注入截斷裁掉的部分
// （internal/config 的 maxBootstrapRunes、internal/memory 的 maxInjectRunes）——上面
// 寫「載入結果」與「輸出」而不是「檔案內容」，就是為了這一條。
//
// 四項裡只有長期記憶與 Profile 無關（由 MemoryService 從 Workspace 讀）。**別把
// 「只有 identityPrompt 綁 Profile」當成來源層的事實**——那句話只在**參數層**成立：
// Profile 不直接提供 boot 與 skillSection 的**內容**，卻決定了它們**由哪些檔案組成**。
// 一個字都沒動的 Bootstrap 檔案，只要 Profile 的 bootstrap 欄位改了，前綴一樣會變。
//
// **契約是單向的，反過來不成立——確定不等於單射。** 從有效來源到 prompt 是個確定的
// 函式，但**不是一對一**：有效來源形成之後，組裝路上還有幾道正規化與截斷，不同的來源
// 可以正確地映到同一串位元組——
//
//   - 本函式對每一層做 strings.TrimSpace，整層為空（含只有空白字元）就整段略過：在
//     AGENTS.md 末尾多打幾個空白，來源變了，prompt 正確地不變
//   - ComposeSkillSection 把 description 裡的換行折成空白，並在超過上限時**整份**丟掉
//     尾端的 Skill：改一份已經被丟掉的 description，來源變了，Skill 段正確地不變
//
// （上一段那四類是「根本不是有效來源」，這一段是「是有效來源、但被正規化吸收掉」，
// 兩者不同、不要混為一談。）
//
// 所以要守的只有單向那條：**有效來源相同 ⇒ prompt 相同。** 反向只能主張一句較弱的
// 話：**會改變正規化與截斷後組裝結果的內容變更，必須反映到 prompt**——也就是每個 turn
// 重讀、不緩存（技術方案 §5.3），那是 save_memory 與 Bootstrap 熱更新的價值所在。
// 讀成「輸出永遠相同」而去把某一層釘死或緩存起來，會弄壞既有行為。下列測試守的是這
// 一句，**示範的是具體幾個變更確實傳到了 prompt，不是一條普遍的單射律**：
// Bootstrap 見 TestSystemPromptPrefixChangesWhenBootstrapChanges 與
// TestBootstrapRereadEveryTurn，長期記憶見 TestLongTermMemoryRereadEachTurn，
// Skill 見 TestSkillRereadEveryTurn。
//
// 守的是 Provider 端的**前綴快取**——**前提是那家 Provider 有這個能力**。前綴快取
// 是 Provider 的特性，不是 OpenAI 兼容協議的保證：協議只規定請求與回應的形狀，沒有
// 規定快取存不存在。技術方案 §5.3 的措辭也是限定的（「對**有前綴快取的** Provider
// 較友善——這是效能補充理由，不作為架構依據」）。在有支援的 Provider 上，這條協議
// 下不需要顯式的快取標記，能做的就只有讓前綴保持穩定；沒支援的 Provider 上這條不變
// 式不會有壞處，只是收不到那份收益。
//
// 之所以仍值得寫成契約並用測試守：失效是**靜默**的——沒有錯誤、沒有日誌，只有帳單
// 變貴，靠自覺守不住。「現在時間」這類每個 turn 都會變的東西尤其不該放進來，它會讓
// 每一次呼叫都完全落空。
func composeSystemPrompt(identityPrompt string, boot BootstrapContext, longTerm, skillSection string) string {
	persona := identityPrompt
	if strings.TrimSpace(persona) == "" {
		persona = boot.Soul // 互斥：只有 identity.prompt 缺席時 SOUL.md 才生效
	}

	layers := []struct{ intro, body string }{
		{"", persona},
		{agentsIntro, boot.Agents},
		{userIntro, boot.User},
		{longTermMemoryIntro, longTerm},
		// Skill 段自帶引言（ComposeSkillSection 已經拼好），這裡不再加一層。
		{"", skillSection},
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
