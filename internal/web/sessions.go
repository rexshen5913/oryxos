// sessions.go 收 Session 的三個端點：建立、查詢、歸檔（ticket #78）。
package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/storage"
)

const (
	// sessionChannel 是 Web Service 建立的 Session 在 sessions.channel 欄位的值（spec #73 第五節）。
	//
	// 這個欄位記的是**接入來源**：寫 web 不代表 Web Service 是 Channel（CONTEXT.md：HTTP 接入歸 Web
	// Service，不算 Channel）。它讓 Web Service 建立的 Session 與 CLI 的落在不同的聯合標識上，兩邊的
	// 對話因此互不干擾。
	sessionChannel = "web"

	// maxUserIDBytes 是 user_id 的長度上限。字元集只有 ASCII，所以位元組數就是字元數。
	maxUserIDBytes = 128

	// historyWindow 是 GET /sessions/{id} 最多回傳的訊息條數（spec #73 第八節）。
	historyWindow = 100
)

// userIDPattern 是 user_id 允許的字元集：英文字母、數字與 - _ . @（spec #73 第五節）。
//
// **限定字元集是為了 Session ID**：ID 由聯合標識拼成，會出現在 URL 路徑、日誌與啟動輸出裡。擋掉
// 換行一類的控制字元，也讓 ID 不必跳脫就能直接放進路徑。
var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// createSessionRequest 是 POST /api/v1/sessions 的 body。
type createSessionRequest struct {
	Profile string `json:"profile"`
	UserID  string `json:"user_id"`
}

// createSession 替一位使用者與一份 Profile 建立 Session（spec #73 使用者故事 13～15）。
//
// **先驗形狀、再驗語意、最後才落庫**：body 與欄位不對回 400；形狀對了才找 Profile（404／503）；
// 都過了才寫 sessions 表。被拒絕的請求因此不會留下任何資料列。
//
// user_id 由呼叫端必填，不給預設值：給了預設值，所有沒帶 user_id 的呼叫端都會落在同一個聯合標識
// 上，第二個建立請求就撞上「同一聯合標識只能有一個 active Session」。
func (h *handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !h.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Profile == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "缺少必填欄位 profile")
		return
	}
	if problem := userIDProblem(req.UserID); problem != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request", problem)
		return
	}
	entry, ok := h.lookupAvailableProfile(w, req.Profile)
	if !ok {
		return
	}
	record, err := h.opts.Sessions.Create(r.Context(), sessionChannel, req.UserID, entry.Name)
	var exists *storage.ActiveSessionExistsError
	if errors.As(err, &exists) {
		noteSessionID(w, exists.SessionID)
		h.writeJSON(w, http.StatusConflict, sessionExistsResponse{
			errorResponse: errorResponse{
				ErrorCode: "session_exists",
				Message:   fmt.Sprintf("使用者 %s 對 Profile %s 已有 active Session；可以接著用它，或先歸檔再建立", req.UserID, entry.Name),
				Timestamp: time.Now(),
			},
			SessionID: exists.SessionID,
		})
		return
	}
	if err != nil {
		h.writeInternalError(w, r, "session_create_failed", "建立 Session 失敗", err)
		return
	}
	noteSessionID(w, record.Session.ID)
	h.writeJSON(w, http.StatusCreated, sessionViewOf(record))
}

// sessionExistsResponse 是 409 session_exists 的回應：統一的錯誤形狀，加上既有那一場的 session_id，
// 呼叫端可以接著用它（spec #73 第八節）。只有這一個錯誤帶 session_id，所以它有自己的型別，不把
// 這個欄位加進所有錯誤共用的 errorResponse。
type sessionExistsResponse struct {
	errorResponse
	SessionID string `json:"session_id"`
}

// getSession 回傳一個 Session 與它最近的對話歷史（spec #73 使用者故事 18～20）。
func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	record, ok := h.lookupWebSession(w, r)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, sessionDetailOf(record))
}

// deleteSession 把一個 Session 歸檔（spec #73 使用者故事 21、22）。已經歸檔的再歸檔一次也回 200：
// 網路重試不該把一次成功的歸檔變成錯誤。對話歷史不動，之後仍查得到。
//
// **turn 進行中不能歸檔**：先佔用「進行中」標記，佔不到就立即回 409 session_busy，不排隊（spec #73
// 第四節）。否則 turn 跑到一半 Session 被歸檔，它的存檔會因為「只寫得進 active 的列」而失敗，整輪
// rollback——呼叫端的訊息白送，而他看到的原因是一個他沒做的歸檔。
//
// **先讀一次、確認是 Web Service 建立的，才歸檔**：CLI 的 Session 不能被 Web Service 動到（見 lookupWebSession）。
// channel 欄位建立之後就不會再變，所以「先檢查、再動作」之間沒有空隙讓它變掉。
func (h *handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	id, ok := h.markSessionBusy(w, r)
	if !ok {
		return
	}
	defer h.busy.release(id)
	record, ok := h.lookupWebSession(w, r)
	if !ok {
		return
	}
	archived, err := h.opts.Sessions.ArchiveByID(r.Context(), record.Session.ID)
	if err != nil {
		h.writeInternalError(w, r, "session_archive_failed", "歸檔 Session 失敗", err)
		return
	}
	h.writeJSON(w, http.StatusOK, sessionViewOf(archived))
}

// lookupWebSession 依路徑上的 {id} 找出一個**由 Web Service 建立的** Session。找不到、或它不是
// Web 建立的，回 404 session_not_found；這些情形都已經寫好錯誤回應，呼叫端看到 false 直接返回。
//
// **CLI 的 Session 在 Web Service 一律當成不存在**。spec #73 第四節的並行設計建立在「chat 與 server
// 永遠不會碰到同一個 Session，因為兩者的接入來源不同」這個前提上：server 的「進行中」標記只在
// 單一進程內有效。若 Web Service 能依 ID 讀取或歸檔 CLI 的 Session，這個前提就破了——到了 #79，
// server 還會對它跑 turn，與 chat 同時寫同一段歷史。回 404 而不是 403：對 Web Service 來說，它就是
// 不存在。
func (h *handler) lookupWebSession(w http.ResponseWriter, r *http.Request) (*storage.SessionRecord, bool) {
	id := r.PathValue("id")
	noteSessionID(w, id)
	record, err := h.opts.Sessions.SessionByID(r.Context(), id)
	if errors.Is(err, storage.ErrSessionNotFound) || (err == nil && record.Session.Channel != sessionChannel) {
		h.writeError(w, http.StatusNotFound, "session_not_found", fmt.Sprintf("沒有 ID 為 %q 的 Session", id))
		return nil, false
	}
	if err != nil {
		h.writeInternalError(w, r, "session_read_failed", "讀取 Session 失敗", err)
		return nil, false
	}
	return record, true
}

// userIDProblem 回傳 user_id 不合格的原因；合格時回空字串。原因一律指名 user_id。
func userIDProblem(userID string) string {
	switch {
	case userID == "":
		return "缺少必填欄位 user_id"
	case len(userID) > maxUserIDBytes:
		return fmt.Sprintf("user_id 超過 %d 個字元的上限", maxUserIDBytes)
	case !userIDPattern.MatchString(userID):
		return "user_id 只能包含英文字母、數字與 - _ . @"
	}
	return ""
}

// sessionView 是 Session 物件（spec #73 第八節）。archived_at 在 active 時是 null。
type sessionView struct {
	SessionID    string     `json:"session_id"`
	Profile      string     `json:"profile"`
	UserID       string     `json:"user_id"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	LastActiveAt time.Time  `json:"last_active_at"`
	ArchivedAt   *time.Time `json:"archived_at"`
}

// sessionDetailView 是 GET /sessions/{id} 的回應：Session 物件的欄位攤平在外層，再加上 messages
// 與 total_messages。所以它是 POST 與 DELETE 回應的超集，呼叫端用同一套欄位名讀三個端點。
type sessionDetailView struct {
	sessionView
	Messages []messageView `json:"messages"`
	// TotalMessages 是完整歷史的條數：messages 只有最近 historyWindow 條，呼叫端靠它知道回傳的
	// 是不是全部（spec #73 使用者故事 19）。
	TotalMessages int `json:"total_messages"`
}

// messageView 是一條歷史訊息。tool_calls 一律是陣列（沒有時是 []），tool_call_id 不是 tool 訊息時
// 是 null。
type messageView struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Timestamp  time.Time      `json:"timestamp"`
	ToolCalls  []toolCallView `json:"tool_calls"`
	ToolCallID *string        `json:"tool_call_id"`
}

// toolCallView 是一筆 Tool 呼叫。Arguments 已套用與審計相同的去敏規則。
type toolCallView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// sessionViewOf 把儲存層的紀錄轉成 Session 物件。
func sessionViewOf(record *storage.SessionRecord) sessionView {
	return sessionView{
		SessionID:    record.Session.ID,
		Profile:      record.Session.ProfileName,
		UserID:       record.Session.UserID,
		Status:       record.Status,
		CreatedAt:    record.CreatedAt,
		LastActiveAt: record.LastActiveAt,
		ArchivedAt:   record.ArchivedAt,
	}
}

// sessionDetailOf 把儲存層的紀錄轉成 GET 的回應：最近 historyWindow 條、由舊到新。
//
// **Tool 呼叫的參數在這裡去敏，sessions 表裡的原文不動**（spec #73 第八節）：對話歷史必須能原樣
// 重放給 Provider，去敏只能發生在離開這個進程的那一刻。規則與審計相同（core.RedactArgs），密鑰
// 不會因為多了一條輸出路徑而外洩（使用者故事 20）。
func sessionDetailOf(record *storage.SessionRecord) sessionDetailView {
	all := record.Session.Messages
	recent := all[max(0, len(all)-historyWindow):]
	messages := make([]messageView, 0, len(recent))
	for _, msg := range recent {
		calls := make([]toolCallView, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			calls = append(calls, toolCallView{ID: call.ID, Name: call.Name, Arguments: core.RedactArgs(call.Arguments)})
		}
		view := messageView{Role: string(msg.Role), Content: msg.Content, Timestamp: msg.Timestamp, ToolCalls: calls}
		if msg.ToolCallID != "" {
			toolCallID := msg.ToolCallID
			view.ToolCallID = &toolCallID
		}
		messages = append(messages, view)
	}
	return sessionDetailView{sessionView: sessionViewOf(record), Messages: messages, TotalMessages: len(all)}
}
