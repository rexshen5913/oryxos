// 並行 turn 的資料競爭檢查（ticket #81，spec #73 第四節）。CLI 從來只跑一個 Session，server 卻讓同一份
// Profile 的 AgentService、Executor、Provider 服務與 MCP 連線被多個 goroutine 同時使用：這是引擎第一次被
// 並行使用。這裡的測試要在 go test -race 下跑才有意義（make test 沒有開 race detector）。
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// tagPlaceholder 是並行測試的錄製回應裡，要換成這個 turn 標籤的佔位符。標籤會原樣放進 JSON 字串（其中
// 一處還在 Tool 參數那個巢狀的 JSON 字串裡），所以只用不需跳脫的字元（測試用 tag-alice、tag-bob）。
const tagPlaceholder = "{{TAG}}"

// newTaggedReplayServer 起一個回放伺服器，依請求內容挑選錄製回應、替換標籤，給並行測試用。
//
// **不照到達順序發回應**：兩個 turn 同時在跑，誰先到是不確定的，照順序發會讓測試時紅時綠。它看請求的
// 最後一則訊息：使用者訊息 → toolCalls，Tool 結果 → final。錄製回應裡的 {{TAG}} 換成這個 turn 的使用者
// 訊息（測試讓它就是一個標籤），並行的兩個 turn 才拿得到**可以分辨**的內容，串到對方身上時看得出來。
// 回應仍是 testdata 裡錄好的內容，只替換標籤，結果完全確定——ADR-0002 要的是確定性，不是每個請求都
// 回逐位元組相同的東西。
//
// **回 Tool 呼叫的那一個 iteration 設柵欄**：要等 barrier 個請求都到齊才一起回覆。它證明兩個 turn 真的
// 同時在跑：任何把 turn 串行化的東西——例如 Web Service 層的一把全域鎖（ticket #81 明文禁止）——會讓先
// 到的那個卡在這裡、後到的那個卡在鎖外，10 秒後報錯並放行。沒有柵欄的話，那把鎖會安靜地通過。race
// detector 抓不抓得到競爭則不靠它：它看的是兩次存取之間有沒有先後關係，不是時間上是否重疊（實測：柵欄
// 改成 1，Executor 上的競爭照樣 10 次抓到 10 次）。
func newTaggedReplayServer(t *testing.T, toolCalls, final string, barrier int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var arrived int
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("讀取 LLM 請求 body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("解析 LLM 請求: %v\nbody: %s", err, body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Messages) == 0 {
			t.Errorf("LLM 請求沒有任何訊息\nbody: %s", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var tag string
		for _, m := range req.Messages {
			if m.Role == "user" {
				tag = m.Content
				break
			}
		}
		fixture := final
		if req.Messages[len(req.Messages)-1].Role == "user" {
			fixture = toolCalls
			mu.Lock()
			arrived++
			if arrived == barrier {
				close(ready)
			}
			mu.Unlock()
			select {
			case <-ready:
			case <-time.After(10 * time.Second):
				t.Errorf("10 秒內只有部分 turn 走到呼叫 LLM：兩個 turn 沒有同時在跑")
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.ReplaceAll(fixture, tagPlaceholder, tag))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// participant 是並行測試裡的一方：哪位使用者、用哪份 Profile、送出哪個標籤，以及建立出來的 Session。
type participant struct {
	user, profile, tag string
	sessionID          string
}

// TestServerConcurrentTurns 讓兩個 Session 並行跑 turn（spec #73 第四節，ticket #81 AC）：兩個 turn 都呼叫
// 一個 MCP Tool（demo__echo）與一個內建 Tool（save_memory），而且真的同時在跑（見
// newTaggedReplayServer 的柵欄）。在 go test -race 下，同一份 Profile 與兩份 Profile 兩種情形都要通過：
//
//   - 兩邊的回應與歷史各自正確，不含對方的內容；
//   - MEMORY.md 裡兩筆都在，日期標題只有一個（兩個 turn 同時 save_memory）；
//   - 審計記在各自的 Session 名下；
//   - server 關閉之後，沒有殘留的 goroutine。
//
// **MCP 排在前面**：save_memory 會拿長期記憶的鎖，那把鎖會在兩個 turn 之間排出先後。柵欄一放行就先打
// MCP，讓兩個 turn 在碰到那把鎖之前就用上共用的 MCP 連線。
//
// **跑一次不保證抓到競爭**：Go 的 race runtime 把 I/O 當成同步——每次 syscall.Write 都對一個全域的
// ioSync 做 ReleaseMerge、每次成功的 syscall.Read 都做 Acquire——所以進程裡任何一次「寫→讀」都會在
// 兩個 turn 之間建立先後關係，不論是日誌、SQLite、MCP 的 pipe 還是 HTTP。兩個 turn 都大量做 I/O，夾在
// I/O 之間的競爭就不容易被看見（實測：MCP 連線上製造的競爭，10 個獨立進程抓到 9 次）。這支測試是一道
// 網，不是證明。
//
// goroutine 的檢查沿用 #75 的手法（assertGoroutinesBackToBaseline）。數之前先關掉回放伺服器：實測不關的
// 話，關閉後只多出它的 Accept 迴圈（httptest.Server.goServe），server 那一側沒有留下任何 goroutine，
// 關掉它不會藏起 OryxOS 的殘留。
func TestServerConcurrentTurns(t *testing.T) {
	// mcpProfile 的 tools 是 demo__echo，再加上 save_memory：MCP 在前、內建在後。
	const profileBody = mcpProfile + "  - save_memory\n"
	tests := []struct {
		name string
		// profiles 是兩方各自所屬的 Profile。
		profiles [2]string
	}{
		{name: "同一份 Profile 的兩個 Session", profiles: [2]string{"default", "default"}},
		{name: "兩份 Profile 各一個 Session", profiles: [2]string{"default", "second"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parties := [2]participant{
				{user: "alice", profile: tt.profiles[0], tag: "tag-alice"},
				{user: "bob", profile: tt.profiles[1], tag: "tag-bob"},
			}
			baseline := runtime.NumGoroutine()
			provider := newTaggedReplayServer(t,
				readFixture(t, "web_reply_concurrent_tool_calls.json"), readFixture(t, "web_reply_concurrent_final.json"), 2)
			dir := setupChatWorkspace(t, provider.URL)
			writeMcpServers(t, dir, "mcp_servers:\n"+testMcpServerEntry(t, "demo", "echo"))
			writeProfile(t, dir, profileBody)
			writeNamedProfile(t, dir, "second", profileBody)
			s := startServer(t, dir)

			for i := range parties {
				parties[i].sessionID = createSession(t, s, parties[i].profile, parties[i].user).SessionID
			}
			var results [2]<-chan asyncResult
			for i, p := range parties {
				results[i] = sendAsync(s.client, newMessageRequest(t, context.Background(), s, p.sessionID, p.tag))
			}
			for i, p := range parties {
				res := waitResult(t, results[i])
				if res.err != nil || res.status != http.StatusOK {
					t.Fatalf("%s 的 turn = %d（%v）, 期望 200\nbody: %s", p.user, res.status, res.err, res.body)
				}
				var got messageReply
				if err := json.Unmarshal(res.body, &got); err != nil {
					t.Fatalf("解析 %s 的回應: %v", p.user, err)
				}
				if want := "完成：" + p.tag; got.Reply != want {
					t.Errorf("%s 的 reply = %q, 期望 %q", p.user, got.Reply, want)
				}
			}

			for i, p := range parties {
				other := parties[1-i]
				history := getSession(t, s, p.sessionID)
				var roles []string
				for _, m := range history.Messages {
					roles = append(roles, m.Role)
				}
				if want := []string{"user", "assistant", "tool", "tool", "assistant"}; !slices.Equal(roles, want) {
					t.Fatalf("%s 的歷史角色順序 = %v, 期望 %v", p.user, roles, want)
				}
				var calls []string
				for _, c := range history.Messages[1].ToolCalls {
					calls = append(calls, c.Name)
				}
				if want := []string{"demo__echo", "save_memory"}; !slices.Equal(calls, want) {
					t.Errorf("%s 的 Tool 呼叫 = %v, 期望 %v（一個 MCP、一個內建）", p.user, calls, want)
				}
				if !strings.Contains(history.Messages[2].Content, p.tag) {
					t.Errorf("%s 的 MCP Tool 結果 = %q, 期望含 %q", p.user, history.Messages[2].Content, p.tag)
				}
				if strings.Contains(historyJSON(t, dir, p.sessionID), other.tag) {
					t.Errorf("%s 的歷史裡出現了 %s 的 %q", p.user, other.user, other.tag)
				}
			}

			if err := s.stop(); err != nil {
				t.Fatalf("收掉 server: %v", err)
			}
			provider.Close()

			memory, err := os.ReadFile(filepath.Join(dir, workspaceDir, "memory", memoryFile))
			if err != nil {
				t.Fatalf("讀取 MEMORY.md: %v", err)
			}
			for _, p := range parties {
				if want := p.tag + " 的偏好是綠茶"; !strings.Contains(string(memory), want) {
					t.Errorf("MEMORY.md 沒有 %q（兩個 turn 同時 save_memory，有一筆遺失）:\n%s", want, memory)
				}
			}
			// 檔案原本不存在，兩筆又在同一天寫入：日期標題只該有一個。這不是 race detector 看得到的競爭
			// ——兩個 goroutine 沒有共用記憶體，是各自讀到舊內容、各補一個標題（LongTermMemory.mu 防的就是它）。
			if headers := strings.Count("\n"+string(memory), "\n## "); headers != 1 {
				t.Errorf("MEMORY.md 有 %d 個日期標題, 期望 1（兩個 turn 同時追加，各自補了一個）:\n%s", headers, memory)
			}
			for _, p := range parties {
				if n := countRows(t, dir, `SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`, p.sessionID); n != 2 {
					t.Errorf("%s 的 tool_invocations = %d 筆, 期望 2", p.user, n)
				}
			}

			assertGoroutinesBackToBaseline(t, baseline)
		})
	}
}
