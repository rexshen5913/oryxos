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
	// wantSoul 為 false 時**完全不碰 SOUL.md**（回傳的 Soul 必為空）。呼叫端在
	// Profile 已有 identity.prompt 時傳 false：那份檔案被互斥排除、根本不會進
	// prompt，一個用不到的檔案壞掉不該讓每個 turn 都失敗。互斥的**判斷**仍在
	// core（見 composeSystemPrompt），這個參數只是讓載入端別去讀不需要的東西。
	Bootstrap(ctx context.Context, wantSoul bool) (BootstrapContext, error)
}
