package core

import "context"

// BootstrapContext 是一個 turn 的 Bootstrap 快照——使用者手寫、OryxOS 只讀不寫的
// 三份上下文檔案內容（技術方案 §5.4）。與長期記憶的角色不同：Bootstrap 是「初始
// 設定」，長期記憶是 Agent 經 save_memory 累積的「成長記錄」。
//
// 三個欄位都可能是空字串——檔案不存在或內容為空都視為該層為空，對話照常。
type BootstrapContext struct {
	// Soul 是 SOUL.md 的內容（Workspace 級的預設人格）。它與 Profile 的
	// identity.prompt **互斥**、後者優先（ADR-0003）——互斥的判斷在 core 的
	// prompt 組裝處，不在載入端：載入端只負責把磁碟上的東西讀回來，覆蓋語義
	// 是架構決策，要留在能被測試釘住的那一層。
	Soul string
	// Agents 是 AGENTS.md 的內容（這個專案怎麼做事）。
	Agents string
	// User 是 USER.md 的內容（這個人偏好什麼）。
	User string
}

// ContextLoader 是 Bootstrap 上下文的出向介面，由 internal/config 實作、在組裝點
// 顯式注入（憲法 5.2）。介面定義在 core 是為了守住依賴方向（config 依賴 core，
// core 不反向依賴 config）——ReAct 鏈路因此不必知道 `.oryxos/AGENTS.md` 這條路徑
// （同 MemoryService 的先例，技術方案 §8.3）。
//
// 載入時機是**每個 turn 一次**、在 ReAct 迭代迴圈之外（見 ReActLoop.Run）：
// 使用者手改任何一檔，下一個 turn 立刻生效，不必重啟；同一個 turn 內則保持固定，
// LLM 第二次迭代看到的前提與它第一次決策時一致。不做 in-memory cache 與檔案
// watch（技術方案 §5.3，屬擴展階段）。
type ContextLoader interface {
	// Bootstrap 回傳一份 Bootstrap 快照。檔案不存在或為空視為該層為空、不算
	// 錯誤；權限不足、I/O 故障、路徑不是普通檔等**真實故障**回錯誤，由呼叫端
	// fail 該 turn——把故障吞成空值會讓 Agent 在使用者不知情下失去上下文。
	//
	// sel 指出這次要讀哪幾份；沒被選中的那些**完全不碰**（對應欄位必為空）。
	// 由 core 依 Profile 的 bootstrap 欄位與 ADR-0003 的互斥算出（見
	// Profile.bootstrapSelection），載入端只照著讀——「載入哪些」是配置語義、
	// 「誰蓋過誰」是架構決策，兩者都留在能被測試釘住的那一層。
	Bootstrap(ctx context.Context, sel BootstrapSelection) (BootstrapContext, error)

	// Skills 回傳 names 引用的每份 Skill 的 name 與 description（漸進揭露第一層，
	// **不含正文**），順序等於宣告順序。names 為空時回 nil、不算錯誤。
	//
	// 任何一份出問題都回錯誤（引用不存在、frontmatter 不合法、name 與引用名不一致
	// 都是設定錯誤），由呼叫端 fail 該 turn——靜默降級成「這個 Agent 沒有技能」會讓
	// 使用者以為 Skill 只是沒被觸發，而不是根本沒載入。
	//
	// 與 Bootstrap 同在這條介面上（技術方案 §8.3 的 ContextLoader 模組）：兩者本質
	// 相同，都是注入 system prompt 的 markdown 上下文，只是來源不同。載入時機也
	// 相同——每個 turn 一次、在 ReAct 迭代迴圈之外取快照。
	Skills(ctx context.Context, names []string) ([]SkillMeta, error)
}

// BootstrapSelection 指出一次載入要讀哪幾份 Bootstrap 檔案，以及缺檔算不算錯。
//
// 「不讀」與「讀了但丟棄」不是同一件事：一份被排除的檔案若壞掉（權限不足、被做成
// 目錄），照讀會讓每個 turn 都失敗——而那份檔案根本不會進 prompt。所以這個選擇必須
// 傳到載入端，不能在組裝 prompt 時才過濾。
type BootstrapSelection struct {
	Soul   bool
	Agents bool
	User   bool
	// Explicit 為真代表這組選擇來自 Profile **明確列出**的 bootstrap 欄位，而不是
	// 欄位省略時的預設三檔。兩者的缺檔語義相反：
	//
	//   省略（false）  缺檔視為該層為空、對話照常——使用者只寫其中一兩份是常態
	//   列出（true）   缺檔就是設定錯誤，**每個 turn 都報錯**
	//
	// 這個旗標描述整組選擇而不是逐檔，因為兩者不會混用：bootstrap 欄位要嘛省略
	// （全部是預設）、要嘛列出（列到的全部是明確要求），沒有中間態。
	//
	// 之所以每個 turn 都要判、而不是啟動時驗過就算：Bootstrap 是**每個 turn 重讀**
	// 的（技術方案 §5.3），啟動後被刪掉的檔案若在讀取端被當成「該層為空」，Agent
	// 就會在使用者不知情下少掉一段明確要求的上下文——那正是 fail fast 要避免的
	// 「半殘運作、對話中途才發現」。組裝點的啟動校驗是同一條規則的**提前**回報，
	// 不是它的替代。
	Explicit bool
}
