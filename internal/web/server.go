// server.go 收 Web Service 的 HTTP handler：路由、錯誤形狀、CORS 與請求日誌（ticket #75）。
package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/memory"
	"github.com/rexshen5913/oryxos/internal/storage"
)

// productName 是 info 回應裡的 name。
const productName = "OryxOS"

const (
	// corsAllowMethods 是核心 10 個端點用到的全部方法（spec #73 第八節），加上預檢本身的 OPTIONS。
	//
	// **列的是整份端點契約，不是這一刻已經註冊的路由**。隨著每張票補路由時逐一加的話，只要漏
	// 加一次，症狀就是瀏覽器的預檢失敗：前端只看得到一個不透明的網路錯誤，curl 卻完全正常，
	// 是最難查的那一種。
	corsAllowMethods = "GET, POST, DELETE, OPTIONS"
	// corsAllowHeaders 只需要 Content-Type：瀏覽器送 application/json 的 body 時，會為它發預檢。
	corsAllowHeaders = "Content-Type"
)

// Options 是組出 HTTP handler 所需的一切，由命令層（composition root）組好交進來。
type Options struct {
	// Logger 收每個請求一行的結構化日誌，落在 Workspace 的 logs 目錄。
	Logger *slog.Logger
	// Version 是這個二進制的版本，由命令層從 Go 的建置資訊讀出來。
	Version string
	// StartedAt 是 server 啟動的時間。
	StartedAt time.Time
	// Profiles 是 profiles/ 底下每一份 Profile 在這次啟動的載入結果，可用與不可用都在裡面。
	Profiles []ProfileEntry
	// Providers 是 config.yaml 裡已配置的 Provider 名。
	//
	// **只收名字**：憑證與 base_url 從型別上就進不了這個 package，info 端點也就沒有機會把它們
	// 送出去（spec #73 使用者故事 43）。
	Providers []string
	// LongTerm 是 Workspace 的長期記憶，GET /memory 讀它的原文。整個進程只有一份，不分 Profile。
	LongTerm *memory.LongTermMemory
	// Sessions 是 Workspace 的 Session 儲存，Session 端點以它建立、查詢與歸檔。整個進程只有一份。
	Sessions *storage.SessionManager
	// TurnTimeout 是單一 turn 的時間上限（--turn-timeout），必須大於 0。
	TurnTimeout time.Duration
}

// ProfileEntry 是一份 Profile 在這次啟動的載入結果，由命令層組好交進來。
type ProfileEntry struct {
	// Name 是 Profile 對外的名字，也就是檔名去掉 .yaml（spec #73 第三節）。
	Name string
	// Profile 是讀進來的設定。YAML 讀不出來時是 nil：那時連描述、Agent 名與 Provider 都不知道。
	Profile *core.Profile
	// Reason 是不可用的原因，已套用錯誤文字去敏。空字串代表這份 Profile 可用。
	Reason string
	// Agent 是這份 Profile 組出來的 Agent，發訊息時由它跑 turn。不可用時為 nil。
	Agent *core.AgentService
	// StatelessAgent 是同一個 Agent 的第二個 AgentService，無狀態呼叫由它跑 turn（spec #73 第七節）：
	// Session 的持久化不做事，其餘依賴都與 Agent 共用（一份 Profile 仍只對應一個 Agent，見
	// CONTEXT.md）。不可用時為 nil。
	StatelessAgent *core.AgentService
	// Tools 是這份 Profile 這次啟動**實際可用**的 Tool：Profile 過濾後的子集，已套用 MCP 降級、
	// 含自動加入的 load_skill（ticket #77）。不可用時為 nil。
	//
	// 是啟動時的快照，不是每次請求重算：server 執行期間不重新載入 Profile（spec #73 第三節），
	// 子集在這次啟動中不會變。
	Tools []ToolEntry
}

// ToolEntry 是一個實際可用的 Tool。
//
// **web 自己定這個型別，不直接用 tool.ToolInfo**：spec #73 第九節定案 web 依賴 core、storage、
// memory，不含 tool。命令層（composition root）從 Executor 取出清單、逐欄搬過來，同形搬運是這條
// 解耦的成本，形狀與 config／provider 那一對相同。
type ToolEntry struct {
	Name        string
	Description string
	// Server 是這個 Tool 來自哪一台 MCP server；內建 Tool 為空字串，回應裡是 null。
	Server string
}

// Available 回報這份 Profile 在這次啟動是否可用。
func (e ProfileEntry) Available() bool { return e.Reason == "" }

// handler 是 Web Service 各個端點共用的狀態。
type handler struct {
	opts Options
	// busy 是「哪些 Session 正在被一個請求使用」的進程內標記，見 busySessions。
	busy *busySessions
}

// NewHandler 組出 /api/v1 之下的路由，外面由外到內依序包上請求日誌、CORS、JSON 形狀的 404／405。
//
// 這個順序各有理由：
//
//   - **請求日誌在最外層**，CORS 預檢與 404／405 這些根本沒進到端點的請求才記得到。
//   - **CORS 包在 404／405 外面**，錯誤回應才帶得到 CORS 標頭。少了它，瀏覽器裡的前端在出錯時
//     連錯誤 body 都讀不到。
func NewHandler(opts Options) http.Handler {
	h := &handler{opts: opts, busy: newBusySessions()}
	// 路由只用標準庫：Go 1.22 起 ServeMux 支援「方法＋路徑萬用字元」，核心 10 個端點都表達得
	// 出來，所以不引入 chi（spec #73 第一節）。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", h.health)
	mux.HandleFunc("GET /api/v1/info", h.info)
	mux.HandleFunc("GET /api/v1/profiles", h.profiles)
	mux.HandleFunc("GET /api/v1/tools", h.tools)
	mux.HandleFunc("GET /api/v1/memory", h.memory)
	mux.HandleFunc("POST /api/v1/sessions", h.createSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", h.getSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", h.deleteSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/messages", h.postMessage)
	mux.HandleFunc("POST /api/v1/agents/{name}/invoke", h.invoke)
	return h.withRequestLog(withCORS(h.withJSONNotFound(mux)))
}

// health 回報進程還在服務。
//
// **不呼叫 Provider**：負載平衡器每幾秒探測一次，每次都打 Provider 就是每幾秒一筆費用，而且
// 答案轉眼就過期（spec #73 使用者故事 41）。
func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

// infoResponse 是 GET /api/v1/info 的回應形狀。
type infoResponse struct {
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	StartedAt time.Time     `json:"started_at"`
	Profiles  profileCounts `json:"profiles"`
	Providers []string      `json:"providers"`
}

// profileCounts 是 info 回應裡可用與不可用的 Profile 數。
type profileCounts struct {
	Available   int `json:"available"`
	Unavailable int `json:"unavailable"`
}

// info 回報系統資訊。不主動探測 Provider 是否正常，理由同 health。
func (h *handler) info(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, infoResponse{
		Name:      productName,
		Version:   h.opts.Version,
		StartedAt: h.opts.StartedAt.UTC(), // 一律 UTC，見 writeError
		Profiles:  h.countProfiles(),
		Providers: h.opts.Providers,
	})
}

// countProfiles 數出可用與不可用的 Profile 數。
func (h *handler) countProfiles() profileCounts {
	var counts profileCounts
	for _, entry := range h.opts.Profiles {
		if entry.Available() {
			counts.Available++
		} else {
			counts.Unavailable++
		}
	}
	return counts
}

// profileView 是 GET /api/v1/profiles 回應裡的一筆（spec #73 第八節）。
//
// **description、agent_name、provider 是指標**：YAML 讀不出來的那份，這三個值不知道，回 null。
// 回空字串的話，會被讀成「作者沒寫描述」，而事實是「讀不出來」。
type profileView struct {
	Name        string        `json:"name"`
	Description *string       `json:"description"`
	AgentName   *string       `json:"agent_name"`
	Provider    *providerView `json:"provider"`
	Status      string        `json:"status"`
	// Error 只在不可用時出現。
	Error string `json:"error,omitempty"`
}

// providerView 是一份 Profile 引用的 Provider 與模型。只有名字：憑證與 base_url 屬於 config.yaml，
// 不屬於 Profile，本來就不在這裡。
type providerView struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// profiles 列出這次啟動的每一份 Profile，不可用的也列，並附上原因（spec #73 使用者故事 35）：
// 運維人員從 API 就能診斷，不必登入主機翻啟動輸出。
func (h *handler) profiles(w http.ResponseWriter, _ *http.Request) {
	views := make([]profileView, 0, len(h.opts.Profiles))
	for _, entry := range h.opts.Profiles {
		view := profileView{Name: entry.Name, Status: "available", Error: entry.Reason}
		if !entry.Available() {
			view.Status = "unavailable"
		}
		if prof := entry.Profile; prof != nil {
			view.Description = &prof.Description
			view.AgentName = &prof.Identity.AgentName
			view.Provider = &providerView{Name: prof.Provider.Name, Model: prof.Provider.Model}
		}
		views = append(views, view)
	}
	h.writeJSON(w, http.StatusOK, struct {
		Profiles []profileView `json:"profiles"`
	}{Profiles: views})
}

// toolsResponse 是 GET /api/v1/tools 的回應形狀：依 Profile 分組（spec #73 第八節）。
//
// 最外層叫 groups，對應 spec 的「依 Profile 分組」：叫 tools 的話，每一組裡面又有一個 tools，
// 讀起來分不出是哪一層。
type toolsResponse struct {
	Groups []toolGroup `json:"groups"`
}

// toolGroup 是一份 Profile 與它這次啟動實際可用的 Tool。
type toolGroup struct {
	Profile string     `json:"profile"`
	Tools   []toolView `json:"tools"`
}

// toolView 是一個 Tool。
//
// **server 是指標**：內建 Tool 回 null。回空字串的話，呼叫端得自己記住「空字串代表內建」這條
// 約定；null 本身就是「沒有來源 server」。
type toolView struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Server      *string `json:"server"`
}

// tools 列出 Profile 這次啟動實際可用的 Tool（spec #73 使用者故事 36、37）。清單取自 Profile 過濾後
// 的 Executor，已套用 MCP 降級、含自動加入的 load_skill，見 ProfileEntry.Tools。
//
// 沒帶 profile 參數時列出全部**可用**的 Profile：不可用的沒有 Tool 可列，它的原因在 GET /profiles。
//
// **帶了空值（?profile=）算帶了，回 404**：它多半是呼叫端把一個還沒填的變數接進了網址。當成「不
// 篩選」的話，呼叫端以為拿到了某一份的 Tool，其實拿到全部，錯誤被蓋過去。spec 對輸入的原則是讓
// 這類錯誤當場暴露（JSON 欄位拼錯回 400，同一個理由）。所以判斷用 Has，不看值是不是空字串。
//
// **query 用 url.ParseQuery 解析，不用 r.URL.Query()**：後者會靜默丟掉解析不了的參數（無效的編碼
// `%ZZ`、未編碼的分號），`?profile=%ZZ` 於是變成「沒帶參數」、回 200 與全部 Profile，同樣把錯誤
// 蓋過去。解析失敗回 400 invalid_request。
func (h *handler) tools(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("查詢參數無法解析：%v", err))
		return
	}
	if !query.Has("profile") {
		groups := make([]toolGroup, 0, len(h.opts.Profiles))
		for _, entry := range h.opts.Profiles {
			if entry.Available() {
				groups = append(groups, toolGroupOf(entry))
			}
		}
		h.writeJSON(w, http.StatusOK, toolsResponse{Groups: groups})
		return
	}
	entry, ok := h.lookupAvailableProfile(w, query.Get("profile"))
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, toolsResponse{Groups: []toolGroup{toolGroupOf(entry)}})
}

// toolGroupOf 把一份 Profile 的 Tool 轉成回應形狀。tools 一律是非 nil 的切片：一份沒有任何 Tool 的
// Profile，JSON 裡是 [] 而不是 null。
func toolGroupOf(entry ProfileEntry) toolGroup {
	views := make([]toolView, 0, len(entry.Tools))
	for _, info := range entry.Tools {
		view := toolView{Name: info.Name, Description: info.Description}
		if info.Server != "" {
			server := info.Server
			view.Server = &server
		}
		views = append(views, view)
	}
	return toolGroup{Profile: entry.Name, Tools: views}
}

// lookupAvailableProfile 找出名為 name、而且可用的 Profile。
//
// 找不到回 404 profile_not_found；找到但不可用回 503 profile_unavailable，並附上原因（已去敏）。
// 兩個狀態碼分開，是因為呼叫端該做的事不同：前者是名字寫錯，改請求；後者是那份 Profile 的設定
// 壞了，請求本身沒錯，要等運維人員修好重啟（spec #73 使用者故事 26）。這兩種情形都已經寫好錯誤
// 回應，呼叫端看到 false 直接返回。
func (h *handler) lookupAvailableProfile(w http.ResponseWriter, name string) (ProfileEntry, bool) {
	entry, found := h.findProfile(name)
	if !found {
		h.writeError(w, http.StatusNotFound, "profile_not_found", fmt.Sprintf("沒有名為 %q 的 Profile", name))
		return ProfileEntry{}, false
	}
	if !entry.Available() {
		h.writeError(w, http.StatusServiceUnavailable, "profile_unavailable",
			fmt.Sprintf("Profile %s 在這次啟動中不可用：%s", name, entry.Reason))
		return ProfileEntry{}, false
	}
	return entry, true
}

// findProfile 依對外的名字（檔名）找出這次啟動的一份 Profile，可用與不可用的都找得到。
func (h *handler) findProfile(name string) (ProfileEntry, bool) {
	for _, entry := range h.opts.Profiles {
		if entry.Name == name {
			return entry, true
		}
	}
	return ProfileEntry{}, false
}

// memory 回傳 Workspace 長期記憶（MEMORY.md）的原文（spec #73 使用者故事 38、39）。
//
// **原文，不是注入 system prompt 的那一份**：後者截到 4000 rune、去掉頭尾空白（見
// memory.LongTermMemory.Read 與 Load 的差別）。檔案不存在時 content 是空字串，不是錯誤。
//
// 讀取經 Workspace root，越界的符號連結、權限不足等故障回 500，原因同時落錯誤日誌：回給呼叫端的
// 只有狀態碼與訊息，運維人員要在日誌裡查得到發生了什麼。
func (h *handler) memory(w http.ResponseWriter, r *http.Request) {
	content, err := h.opts.LongTerm.Read(r.Context())
	if err != nil {
		h.writeInternalError(w, r, "memory_read_failed", "讀取長期記憶失敗", err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct {
		Content string `json:"content"`
	}{Content: content})
}

// errorResponse 是所有錯誤回應共用的形狀（spec #73 第八節）：呼叫端的程式依 error_code 分支，
// message 給人看。
type errorResponse struct {
	ErrorCode string    `json:"error_code"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// writeError 寫出統一形狀的錯誤回應。
//
// **API 回應的時間戳一律是 UTC**：Session 的三個時間戳來自 storage，本來就是 UTC；錯誤回應、歷史
// 訊息、/info 的 started_at 若照 time.Now() 原樣輸出，會帶著 server 的本地時區，同一個回應裡混著兩種
// 寫法，呼叫端比對時得先知道 server 在哪個時區。轉換只在回應出門時做，資料庫與 chat 的顯示不動。
func (h *handler) writeError(w http.ResponseWriter, status int, code, message string) {
	h.writeJSON(w, status, errorResponse{ErrorCode: code, Message: message, Timestamp: time.Now().UTC()})
}

// writeInternalError 回 500 internal_error，原因（已去敏）同時落錯誤日誌 event：呼叫端看得到訊息，
// 但運維人員查的是日誌，原因要在那裡也查得到。
func (h *handler) writeInternalError(w http.ResponseWriter, r *http.Request, event, what string, err error) {
	reason := core.RedactErrorText(err.Error())
	h.opts.Logger.ErrorContext(r.Context(), event, "error", reason)
	h.writeError(w, http.StatusInternalServerError, "internal_error", what+"："+reason)
}

// writeJSON 寫出 JSON 回應。編碼或寫出失敗時，狀態碼已經送出、改不了，只能落日誌。
func (h *handler) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.opts.Logger.Error("http_response_write_failed", "status", status, "err", err)
	}
}

// withJSONNotFound 把 ServeMux 自己產生的 404 與 405 換成統一的 JSON 錯誤形狀。
//
// ServeMux 對「沒有這條路徑」與「路徑對、方法錯」回的是 http.Error 的純文字，呼叫端的程式沒辦法
// 依錯誤碼分支（spec #73 使用者故事 45）。
//
// **分辨 404 與 405 的工作交還給 ServeMux 自己，不在這裡重算**。沒有路由匹配時，mux.Handler
// 回傳它內建的那個 handler；讓它對一個只記狀態碼與標頭的 probe 跑一次，就知道它原本要回 404
// 還是 405，連 Allow 標頭都是它算好的。這裡若自己比對「別的方法有沒有這條路徑」，等於在
// ServeMux 旁邊再寫一份路由規則，兩份遲早會分岔。
//
// 匹配到路由的請求交給 mux.ServeHTTP，而不是直接呼叫 mux.Handler 回傳的 handler：只有前者會
// 填好路徑萬用字元（r.PathValue），之後的 /sessions/{id} 要用。
func (h *handler) withJSONNotFound(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builtin, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &statusProbe{header: http.Header{}}
		builtin.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s 不支援 %s 方法", r.URL.Path, r.Method))
			return
		}
		h.writeError(w, http.StatusNotFound, "route_not_found", fmt.Sprintf("沒有 %s 這個路徑", r.URL.Path))
	})
}

// statusProbe 只記下 ServeMux 內建 handler 想回的狀態碼與標頭，body 丟掉。
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header { return p.header }

func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }

func (p *statusProbe) WriteHeader(status int) { p.status = status }

// withCORS 讓所有回應都允許所有來源，並直接回應 OPTIONS 預檢（技術方案 §7.4）。
//
// **核心階段刻意全開**，後果寫在 SECURITY.md 的 Web Service 一節：加上沒有認證，使用者瀏覽的任何
// 網頁都能經由瀏覽器驅動 Agent。之後要收緊時，改動集中在這裡、--addr 的預設值與 Host 檢查三處
// （spec #73 Further Notes）。
//
// **不送 Access-Control-Allow-Credentials**：沒有這個標頭，瀏覽器就不會把「帶著使用者 cookie
// 送出」的跨來源回應交給網頁。核心階段沒有認證、也沒有 cookie 要保護，但擴展階段補上認證之後，
// 這一條不該被順手打開。
//
// OPTIONS 一律當成預檢，在這裡回 204，不分路徑：預檢只問「能不能送」，路徑存不存在，由接下來
// 那個真正的請求回答（它會拿到 404）。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRequestLog 讓每個請求落一行結構化日誌：方法、路徑、狀態碼、耗時（spec #73 使用者故事 53）。
//
// **不記請求與回應的 body，路徑也不含 query string**：訊息內容是使用者的對話，query 同樣是
// 呼叫端給的內容，寫進日誌就多了一份不受 Session 生命週期管理的副本。
//
// 日誌在 handler 返回之後才寫，這樣記得到最終的狀態碼。
func (h *handler) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", recorder.status,
			"duration_ms", time.Since(start).Milliseconds()}
		if recorder.sessionID != "" {
			attrs = append(attrs, "session_id", recorder.sessionID)
		}
		h.opts.Logger.InfoContext(r.Context(), "http_request", attrs...)
	})
}

// noteSessionID 讓請求日誌記下這個請求處理的 Session（spec #73 第八節「有 Session ID 時一併記上」）。
//
// 請求日誌在最外層，看不到 handler 做了什麼；建立 Session 時，新的 ID 只出現在回應裡、請求的路徑
// 上沒有。所以由 handler 把 ID 留在 statusRecorder 上，日誌寫出時帶上。handler 拿到的 w 就是
// withRequestLog 包的那一個（中間的 CORS 與 404／405 都原樣往下傳），將來若有一層中介層換掉了 w，
// 這裡會找不到它而記不到，由 TestServerRequestLogRecordsSessionID 守著。
func noteSessionID(w http.ResponseWriter, id string) {
	if recorder, ok := w.(*statusRecorder); ok {
		recorder.sessionID = id
	}
}

// statusClientClosedRequest 是「呼叫端在回應寫出之前就斷線」時，請求日誌記下的狀態碼。HTTP 沒有替
// 這種情況定義狀態碼，499 沿用 nginx 的慣例（Client Closed Request），運維人員在日誌裡認得出來。它只
// 進日誌，不會送上連線，因為連線已經斷了。
const statusClientClosedRequest = 499

// noteClientGone 讓請求日誌記下「呼叫端已斷線、沒有寫回應」。不記的話，statusRecorder 停在初始值
// 200，一個被取消、已經 rollback 的 turn 在日誌裡看起來就像成功了（Spec 審查）。
func noteClientGone(w http.ResponseWriter) {
	if recorder, ok := w.(*statusRecorder); ok {
		recorder.status = statusClientClosedRequest
	}
}

// statusRecorder 記下 handler 送出的狀態碼。handler 沒有明確呼叫 WriteHeader 就寫 body 時，
// net/http 送的是 200，所以初始值是 200。
type statusRecorder struct {
	http.ResponseWriter
	status int
	// sessionID 是這個請求處理的 Session，由 handler 經 noteSessionID 留下；沒有時是空字串。
	sessionID string
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
