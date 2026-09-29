// messages.go 收對 Session 發訊息的端點（ticket #79）：輸入限制、「進行中」標記與 turn 錯誤的分類。
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/rexshen5913/oryxos/internal/core"
)

// maxMessageBytes 是 message 的上限，以**位元組**計（spec #73 第八節）。這與 prompt 預算以 rune 計並不
// 矛盾：prompt 預算量的是模型要處理的內容，這裡量的是網路傳輸的大小。整個 body 的上限另見
// maxRequestBodyBytes，它已經替 message 的 JSON 跳脫留了空間。
const maxMessageBytes = 32 << 10

// busySessions 記下哪些 Session 正被一個請求使用：正在跑 turn，或正在歸檔（spec #73 第四節）。
//
// **同一個 Session 的第二個請求不排隊，立即回 409 session_busy**：排隊的請求會悄悄吃掉自己的逾時
// 預算，而呼叫端看不出它在等什麼；立即失敗則是可以預期的行為。
//
// **只在單一進程內有效**：Web Service 只看得到自己建立的 Session（見 lookupWebSession），chat 不會碰
// 到它們；兩個 server 進程共用同一個 Workspace 不在範圍內。請求一結束就移除，所以大小不會超過同時
// 進行中的請求數。
type busySessions struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func newBusySessions() *busySessions {
	return &busySessions{ids: make(map[string]struct{})}
}

// tryAcquire 在 id 沒有被佔用時佔用它並回 true；已被佔用時立即回 false，不等待。
func (b *busySessions) tryAcquire(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, busy := b.ids[id]; busy {
		return false
	}
	b.ids[id] = struct{}{}
	return true
}

// release 移除 id 的佔用。
func (b *busySessions) release(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.ids, id)
}

// markSessionBusy 佔用路徑上的 {id}；已被佔用時寫好 409 session_busy 並回 false。佔用成功的呼叫端要
// 自己 release。
func (h *handler) markSessionBusy(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !h.busy.tryAcquire(id) {
		noteSessionID(w, id)
		h.writeError(w, http.StatusConflict, "session_busy",
			fmt.Sprintf("Session %s 正在處理另一個請求，請等它完成再送", id))
		return "", false
	}
	return id, true
}

// checkMessage 檢查 message 本身（spec #73 第八節）：空的或只有空白回 400，超過 maxMessageBytes 回 413
// message_too_large。不合格時寫好錯誤回應並回 false。發訊息與無狀態呼叫共用這一份規則。
func (h *handler) checkMessage(w http.ResponseWriter, message string) bool {
	if strings.TrimSpace(message) == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "message 不可為空，也不可只有空白")
		return false
	}
	if len(message) > maxMessageBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge, "message_too_large",
			fmt.Sprintf("message 有 %d bytes，超過 %d bytes 的上限", len(message), maxMessageBytes))
		return false
	}
	return true
}

// postMessageRequest 是 POST /api/v1/sessions/{id}/messages 的 body。
type postMessageRequest struct {
	Message string `json:"message"`
}

// messageResponse 是發訊息成功時的回應。
type messageResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

// postMessage 對一個 Session 發一則訊息，同步回傳 Agent 的最終回應（spec #73 使用者故事 16）。內部走
// AgentService.Process：失敗時以 turn 為單位 rollback，Session 回到送出前的狀態，重送同一則訊息是
// 安全的（已執行過 Tool 的外部效果除外，錯誤訊息會註記）。
//
// **「進行中」標記在讀 Session 之前取得**：讀了才佔用的話，兩個幾乎同時到的請求會讀到同一份歷史；
// 先佔到的那個跑完存檔之後，後佔到的那個手上是存檔**之前**的舊歷史，它一存，前一輪就被整段蓋掉。
// 佔用之後才讀，讀到的一定是上一個 turn 存完的那一份。
func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	// 一進來就把路徑上的 ID 交給請求日誌：下面的輸入檢查在碰 Session 之前就可能回 400／413，那些
	// 請求也得依 session_id 查得到（Codex gate 第 1 輪）。與 lookupWebSession 同一個口徑：路徑上帶著
	// ID 就記，不論它存不存在。
	noteSessionID(w, r.PathValue("id"))
	var req postMessageRequest
	if !h.decodeJSONBody(w, r, &req) || !h.checkMessage(w, req.Message) {
		return
	}

	id, ok := h.markSessionBusy(w, r)
	if !ok {
		return
	}
	defer h.busy.release(id)
	record, ok := h.lookupWebSession(w, r)
	if !ok {
		return
	}
	if record.ArchivedAt != nil {
		h.writeError(w, http.StatusConflict, "session_archived",
			fmt.Sprintf("Session %s 已歸檔，不再接受新訊息；歷史仍查得到", id))
		return
	}
	// Session 所屬的 Profile 在這次啟動中不可用或根本沒載入，都回 503：Session 本身沒問題，是這次
	// 啟動的那份設定壞了或不見了，要等運維人員修好重啟（spec #73 使用者故事 26）。回 404 會被讀成
	// 「Session 不存在」。
	entry, found := h.findProfile(record.Session.ProfileName)
	if !found || !entry.Available() {
		reason := "這次啟動沒有載入它"
		if found {
			reason = entry.Reason
		}
		h.writeError(w, http.StatusServiceUnavailable, "profile_unavailable",
			fmt.Sprintf("Session %s 所屬的 Profile %s 在這次啟動中不可用：%s", id, record.Session.ProfileName, reason))
		return
	}

	h.runTurn(w, r, entry.Agent, record.Session, req.Message)
}

// runTurn 以 agent 跑一個 turn 並寫出回應：turn 的 context 從請求衍生並帶上 --turn-timeout 的期限，
// 成功回 200 與 session_id、reply，失敗交給 writeTurnFailure 分類。發訊息與無狀態呼叫共用這一份，
// 兩者只差在用哪個 AgentService、哪個 Session。
func (h *handler) runTurn(w http.ResponseWriter, r *http.Request, agent *core.AgentService, session *core.Session, message string) {
	turnCtx, cancel := context.WithTimeout(r.Context(), h.opts.TurnTimeout)
	defer cancel()
	reply, err := agent.Process(turnCtx, session, message)
	if err != nil {
		h.writeTurnFailure(turnCtx, w, r, session.ID, err)
		return
	}
	h.writeJSON(w, http.StatusOK, messageResponse{SessionID: session.ID, Reply: reply})
}

// writeTurnFailure 把 turn 的失敗分類成回應（spec #73 第八節），順序不能換：
//
//  1. **turn 的 context 過了期限 → 504 turn_timeout**。判斷依據是 context 的狀態，不是解析錯誤鏈：
//     逾時打斷的若是 LLM 呼叫，錯誤鏈裡也帶著 ErrProviderFailed，先看錯誤鏈就會誤報成上游故障。
//  2. **呼叫端已斷線 → 不寫回應，只落日誌**（請求日誌記 499，見 noteClientGone）：已經沒有人在收。
//     turn 隨請求的 context 被取消，rollback 照常發生，沒人收的回應不會繼續消耗 Provider 額度
//     （使用者故事 51）。
//  3. **Provider 呼叫失敗 → 503 provider_error**：上游暫時不可用，可以稍後重試。
//  4. **其他 → 500 internal_error**。
//
// 訊息一律套用錯誤文字去敏，並保留 Process 加上的「本輪已執行過 Tool」註記：重試是否安全要讓呼叫端
// 自己判斷。
func (h *handler) writeTurnFailure(turnCtx context.Context, w http.ResponseWriter, r *http.Request, sessionID string, err error) {
	reason := core.RedactErrorText(err.Error())
	var status int
	var code, message string
	switch {
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "turn_timeout"
		message = fmt.Sprintf("turn 超過 %v 的時間上限，已取消，Session 回到送出前的狀態：%s", h.opts.TurnTimeout, reason)
	case r.Context().Err() != nil:
		noteClientGone(w)
		h.opts.Logger.WarnContext(r.Context(), "turn_client_gone", "session_id", sessionID, "error", reason)
		return
	case errors.Is(err, core.ErrProviderFailed):
		status, code = http.StatusServiceUnavailable, "provider_error"
		message = "Provider 暫時無法使用，可以稍後重試：" + reason
	default:
		status, code = http.StatusInternalServerError, "internal_error"
		message = "turn 失敗：" + reason
	}
	h.opts.Logger.ErrorContext(r.Context(), "turn_failed", "session_id", sessionID, "error_code", code, "error", reason)
	h.writeError(w, status, code, message)
}
