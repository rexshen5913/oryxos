// invoke.go 收無狀態呼叫的端點（ticket #80）：不必先建立 Session，發一個請求就呼叫一次 Agent。
package web

import (
	"net/http"

	"github.com/rexshen5913/oryxos/internal/core"
)

// invokeRequest 是 POST /api/v1/agents/{name}/invoke 的 body。
type invokeRequest struct {
	Message string `json:"message"`
	UserID  string `json:"user_id"`
}

// invoke 呼叫一次 Agent（spec #73 第七節，使用者故事 28～33）：每次都建立一個全新的 Session（接入來源
// web、使用者取自請求、Profile 取自路徑），跑一個 turn，回傳最終回應與這個 Session 的 ID。
//
// **由 StatelessAgent 跑，所以不寫入 sessions 表**：它的 Session 持久化不做事，其餘依賴（長期記憶、
// Provider、Executor、審計、Bootstrap 與 Skill 的載入）都與發訊息那一個共用。照常寫審計，回應的
// session_id 對得上審計記錄，但查不到 Session。不採「先落庫再歸檔」：唯一索引會讓同一位使用者的並行
// 呼叫互相衝突，sessions 表也會堆滿只跑過一次的資料列。
//
// **不佔「進行中」標記**：每次都是全新的 Session，不會有另一個請求碰到它。
//
// 輸入順序與建立 Session 相同：先驗形狀（user_id、message），再找 Profile（#78 定案）。跑 turn、錯誤
// 分類與寫回應都與發訊息共用（runTurn）。
func (h *handler) invoke(w http.ResponseWriter, r *http.Request) {
	var req invokeRequest
	if !h.decodeJSONBody(w, r, &req) || !h.checkUserID(w, req.UserID) || !h.checkMessage(w, req.Message) {
		return
	}
	entry, ok := h.lookupAvailableProfile(w, r.PathValue("name"))
	if !ok {
		return
	}

	session := core.NewSession(sessionChannel, req.UserID, entry.Name)
	noteSessionID(w, session.ID)
	h.runTurn(w, r, entry.StatelessAgent, session, req.Message)
}
