package tool

import "sync"

// ShellLimiter 是 shell 的 admission slot：**在啟動 lifecycle worker 之前**取得一張
// 入場券，滿了就在啟動之前拒絕。
//
// **為什麼需要它。** 走到第三道防線時我們會放棄等待並留下一條回收 goroutine（憲法 5.3
// 的一個明列例外）。既有的 MCP 實作把「留下一條 goroutine」當成刻意的取捨，那在 MCP
// **成立**——server 數量由 config.yaml 限定、洩漏有天然上限。shell 沒有這個性質：由 LLM
// 觸發、一個 turn 內可反覆呼叫、也可跨 session 重複。原樣套用等於「每次不可回收的命令
// 都永久留下一個進程 ＋ 一條 goroutine」而觸發次數無上限——那是一條可被反覆踩的資源
// 洩漏路徑。slot 把那個例外關在**有界**的範圍內，不是無限度的豁免。
//
// **時機是啟動 worker 之前，不是走到第三道時**（spec #29 下修表第十四列）。「第三道才佔」
// 擋不住並發：多個呼叫可以**同時**啟動命令並一起走進第三道，那些不可回收的進程與
// goroutine 在 slot 滿之前就已經產生，事後拒絕後續呼叫收不回它們。門必須開在最外面
// ——連 PATH 解析都可能卡住（第零道），所以是「worker 開始之前」而不只是「Start 之前」。
//
// **作用域是單一 OryxOS 進程共用一份。** 若每個 Profile／session／shellTool 實例各建
// 一個，跨 session 的總量又變回無界，這整段的威脅模型自我作廢。因此它由 **composition
// root 建立一次再注入**（憲法 5.2），**不是 package 級全域變數**，**也不在
// buildToolRegistry 內部建立**——那個函式有兩個呼叫點，在內部建立就是每次呼叫一份新的。
//
// 零值不可用，一律經 NewShellLimiter。
type ShellLimiter struct {
	// slots 的長度就是目前未完成的 worker 數。用 buffered channel 而不是 mutex ＋
	// 計數器：取得要的是「有位子就拿、沒有就**立刻**回失敗」，那正是 select ＋ default
	// 的語義，寫成計數器反而要自己處理競態。
	slots chan struct{}
}

// NewShellLimiter 建立一個容量 maxShellLifecycleWorkers 的 limiter。
//
// **這個函式只該在 composition root 被呼叫**（cmd/oryxos 的 runChat 與 runTools 各
// 一次），呼叫結果再注入 buildToolRegistry。理由見型別說明。
func NewShellLimiter() *ShellLimiter {
	return &ShellLimiter{slots: make(chan struct{}, maxShellLifecycleWorkers)}
}

// acquire 取一張入場券，滿了立刻回 false。
//
// **不排隊等待**：排隊會把「拒絕」變成「掛住」，違背 bounded return 的初衷——使用者
// 要的是一個明確、可行動的錯誤，不是一個安靜卡住的 turn。
func (l *ShellLimiter) acquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release 歸還一張入場券。
func (l *ShellLimiter) release() {
	<-l.slots
}

// inFlight 回傳目前未完成的 lifecycle worker 數，供 slot 已滿的錯誤訊息說得出
// 「目前有幾個命令未回收、需人介入」。
func (l *ShellLimiter) inFlight() int {
	return len(l.slots)
}

// shellSlot 是一張已取得的入場券，附帶「只歸還一次」的保證。
//
// **為什麼要 sync.Once 而不是各條路徑自己小心。** 所有權的移交有三條終點——worker
// 自行 kill＋reap、主路徑接管後的 reap goroutine、detached reaper——而它們是由一個
// 狀態機在競態下選出來的。用 Once 讓「重複歸還」在**結構上**不可能，勝過要求每條路徑
// 都記得檢查：重複歸還會讓 slots 少掉一個位子（release 從 channel 取走一個別人的
// 佔位），那是一個會隨時間累積、卻完全沒有錯誤訊息的洩漏。
type shellSlot struct {
	limiter *ShellLimiter
	once    sync.Once
}

// release 歸還這張入場券；重複呼叫是安全的無操作。
func (s *shellSlot) release() {
	if s == nil {
		return
	}
	s.once.Do(s.limiter.release)
}
