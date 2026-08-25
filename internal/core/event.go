package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EventKind 是一則執行事件的種類。
//
// **七種一次定義完整**：循環側播報五種（turn_started、iteration、tool_retrying、
// assistant_text、turn_finished），Tool 中介層播報另外兩種（tool_started、
// tool_finished）。分兩次擴充會讓同一個型別被兩張票各動一次，而中間那個形狀不是定案。
//
// 取值是**線路上的字串**：CLI 現在消費它，日後的 Web Service 會把它送給 API 呼叫端。
// 改一個字就是一次對外破壞，不是重新命名一個常數而已。
type EventKind string

const (
	// EventTurnStarted 是一個 turn 開始——邊界與 AgentService.Process 一致，
	// 不是 ReAct 循環開始。持久化失敗也算 turn 失敗，那一段在循環之外。
	EventTurnStarted EventKind = "turn_started"
	// EventIteration 是一次 Provider 呼叫開始，Iteration 帶序號（自 1 起）。
	EventIteration EventKind = "iteration"
	// EventToolStarted 是一次 Tool 執行開始，由 Tool 中介層播報。
	EventToolStarted EventKind = "tool_started"
	// EventToolFinished 是一次 Tool 執行結束，由 Tool 中介層播報，OK 帶執行結果。
	EventToolFinished EventKind = "tool_finished"
	// EventToolRetrying 是 ReAct 循環決定重試，Iteration 帶第幾次重試。
	//
	// **刻意留在循環而非中介層**：重試是循環的決策、不屬於單次執行，中介層也拿不到
	// 重試次數——它每次只看得到一次執行。
	EventToolRetrying EventKind = "tool_retrying"
	// EventAssistantText 是 LLM 產出的文字內容，Text 帶去敏後的內容。
	EventAssistantText EventKind = "assistant_text"
	// EventTurnFinished 是一個 turn 結束，OK 帶成敗。
	EventTurnFinished EventKind = "turn_finished"
)

// Event 是執行過程中的一則結構化事件。欄位是所有種類共用的並集，用不到的留零值。
type Event struct {
	Kind      EventKind
	SessionID string
	// Iteration 的語義隨 Kind 而定：EventIteration 是第幾次 Provider 呼叫，
	// EventToolRetrying 是第幾次重試。兩者都是「第幾次」，共用一個欄位而不是各開
	// 一個——事件形狀是 spec #5 的定案（issue #38 的欄位表），這裡照它。
	Iteration int
	ToolName  string
	// Text 在播報前套用**與審計相同**的去敏規則（見 EmitEvent），不另立第二套。
	Text string
	OK   bool
	At   time.Time
}

// EventSink 是 core 播報執行事件的出向介面，由 Channel 與日後的 Web Service 實作、
// 在組裝點顯式注入（憲法 5.2）。介面定義在 core 是為了守住依賴方向，形狀與既有的
// AuditStore 同構。
//
// 實作要守兩條規則：
//
//   - **不得阻塞。** 播報發生在對話的關鍵路徑上，一個慢的實作會直接變成使用者等待
//     的時間。需要慢速 I/O（網路、磁碟）的實作必須自行緩衝。
//   - **旁路不得中斷對話。** 播報失敗、panic、逾時都不該讓使用者的 turn 失敗。
//
// 併發上這個介面**不承諾**播報只來自單一 goroutine。目前的唯一呼叫者 ReAct 循環是
// 序列的（Tool 不並行執行，spec #5 的 Out of Scope 明列），但日後的 Web Service 會
// 讓一份 AgentService 同時服務多個請求。會被那樣用的實作要自己處理併發。
//
// **方法刻意不回傳 error**，理由與 AuditStore 相同：把「這是旁路」寫進型別，呼叫端
// 就沒有機會順手把錯誤往上傳。第二條規則型別擋不住（實作可以 panic），所以吸收
// panic 的責任在播報端，見 EmitEvent。
type EventSink interface {
	Emit(ctx context.Context, e Event)
}

// NopEventSink 是不做事的預設實作：不關心執行過程的呼叫端傳入它即維持現有行為，
// 不必為了多一個介面而寫一份空實作。
type NopEventSink struct{}

// Emit 什麼都不做。
func (NopEventSink) Emit(context.Context, Event) {}

// sessionIDKey 是 Session ID 在 context 中的 key。用未導出的空結構體型別當 key 是
// 標準做法：不會與其他 package 放進去的值撞名。
type sessionIDKey struct{}

// WithSessionID 把 Session ID 放進 ctx，讓**深處的播報點**（Tool 中介層）拿得到
// 「這是哪一場對話」。憲法 5.3 明列 context 的用途之一就是追蹤傳遞。
//
// 為什麼不用參數傳：中介層掛在 Profile 過濾後的 Executor 上，簽名是
// (ctx, ToolCall, next)——它看不到 Session，而 Session 是 turn 級的東西，把它塞進
// Executor 會讓一個 Profile 級的物件持有一個 turn 級的狀態。
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// SessionIDFromContext 取出 ctx 中的 Session ID；沒有時回空字串。
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// EmitEvent 是播報事件的**單一收口**：補上共通欄位、套去敏、吸收 sink 的 panic。
//
// 三件事都在這裡做而不是散在各個播報點，理由與 RedactArgs 住在 core 相同——多一個
// 播報點就多一次漏掉其中一件的機會，而漏掉去敏那次不會有任何測試轉紅。
//
// SessionID 一律從 ctx 取（見 WithSessionID）：呼叫端不填，就不會有人填錯。
//
// panic 在這裡吸收並落一筆警告日誌，而不是往上傳：EventSink 的契約寫明「旁路不得
// 中斷對話」，一個會 panic 的實作違反的是契約，代價不該由使用者的 turn 承擔。
// 錯誤仍被顯式處理（憲法 5.1），只是處理的方式是記錄而非傳遞。
//
// panic 值本身也走去敏：它是**任意字串**，一個實作寫 `panic("送不出去: " + url)` 就會
// 讓密鑰進到日誌。日誌是落盤路徑，落盤路徑一律套同一套規則。
func EmitEvent(ctx context.Context, sink EventSink, logger *slog.Logger, e Event) {
	if sink == nil {
		return // 組裝點應傳 NopEventSink；漏傳時安靜跳過，不讓每次播報都走 panic 路徑
	}
	e.SessionID = SessionIDFromContext(ctx)
	e.Text = RedactErrorText(e.Text)
	if e.At.IsZero() {
		e.At = time.Now()
	}
	defer func() {
		if r := recover(); r != nil {
			logger.WarnContext(ctx, "event_sink_panic",
				"kind", string(e.Kind), "panic", RedactErrorText(fmt.Sprint(r)))
		}
	}()
	sink.Emit(ctx, e)
}
