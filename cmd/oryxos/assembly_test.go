// 共用組裝（assembly.go）的測試，ticket #74。
//
// chat 與 tools 的既有測試是這次抽取的遷移安全網，**一行不改**；本檔只補它們守不到的
// 性質：MCP 子進程在每條離開路徑都收得掉、審計先於 SQLite 關閉，以及進程層級的東西在
// 多份 Profile 之間真的只有一份——chat 一次只組一份 Profile，最後這條它從結構上就驗不到。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// writeNamedProfile 在 Workspace 寫一份指定名字的 Profile；body 不含 name 那行。
// writeProfile 只寫得出 default，多份 Profile 並存的案例需要第二個名字。
func writeNamedProfile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, workspaceDir, "profiles", name+".yaml")
	if err := os.WriteFile(path, []byte("name: "+name+"\n"+body), 0o644); err != nil {
		t.Fatalf("寫入 Profile %s: %v", name, err)
	}
}

// writeWorkspaceConfig 覆寫 Workspace 的 config.yaml：providers 段沿用 setupChatWorkspace
// 的形狀，extra 接在後面。
func writeWorkspaceConfig(t *testing.T, dir, baseURL, extra string) {
	t.Helper()
	cfg := "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    base_url: " + baseURL + "\n" + extra
	if err := os.WriteFile(filepath.Join(dir, workspaceDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("覆寫 config.yaml: %v", err)
	}
}

// assembleWorkspace 組一次進程層級，再對每個 Profile 名各組一次 Profile 層級。
//
// 這是「一個進程、多份 Profile」的形狀；chat 只組一份，驗不到多份之間共用什麼。收尾
// 排進 t.Cleanup，失敗時的半成品也一併交給 Close——與組裝函式對呼叫端的要求相同。
func assembleWorkspace(t *testing.T, out io.Writer, dir string, profileNames ...string) []*profileAssembly {
	t.Helper()
	ctx := context.Background()
	proc, err := assembleProcess(ctx, out, dir)
	var profiles []*profileAssembly
	t.Cleanup(func() {
		if cerr := proc.Close(profiles...); cerr != nil {
			t.Errorf("收尾: %v", cerr)
		}
	})
	if err != nil {
		t.Fatalf("assembleProcess: %v", err)
	}
	for _, name := range profileNames {
		prof, err := core.LoadProfile(filepath.Join(dir, workspaceDir, "profiles", name+".yaml"))
		if err != nil {
			t.Fatalf("載入 Profile %s: %v", name, err)
		}
		assembled, err := assembleProfile(ctx, out, proc, prof, core.NopEventSink{})
		profiles = append(profiles, assembled)
		if err != nil {
			t.Fatalf("assembleProfile %s: %v", name, err)
		}
	}
	return profiles
}

// TestChatClosesMcpSubprocessesOnEveryExitPath 釘住「在任何失敗路徑上都會收掉 MCP 子
// 進程」（ticket #74 AC）。
//
// ticket #74 之前，這條性質靠 runChat 裡一個無條件的 defer 成立，卻沒有任何 chat 測試守著——
// TestToolsClosesMcpSubprocessesOnExit 守的是另一個命令。組裝拆成兩層之後，MCP 連線建在
// Profile 層級、收尾在呼叫端，「中途失敗時半成品有沒有交出來」就成了一個真的漏得掉的
// 問題，而漏掉的症狀是每跑一次留一隻孤兒進程，從輸出完全看不出來。
//
// 三格的差別在「離開時 MCP 已經連上了沒、組裝走到哪」：
//
//   - 成功：組裝完成、turn 成功，正常收尾。
//   - tools 校驗失敗：MCP **已經連上**，Profile 層級組裝才在 Subset 擋下——這是組裝函式
//     內部失敗、手上握著子進程的那一條。
//   - turn 失敗：組裝全部完成，Provider 在對話中回錯誤，runChat 帶著錯誤離開。
//
// marker 由 server 在 stdin 被關閉（MCP stdio 的收工訊號）而退出前寫下。子進程根本沒
// 起來的話 marker 也不存在，所以這條斷言不會因為「沒 spawn」而假綠。
func TestChatClosesMcpSubprocessesOnEveryExitPath(t *testing.T) {
	tests := []struct {
		name string
		// providerURL 回傳 Provider 的 base_url。
		providerURL func(t *testing.T) string
		// tools 是 Profile 的 tools 段。
		tools   string
		wantErr bool
	}{
		{
			name: "成功",
			providerURL: func(t *testing.T) string {
				return newReplayServer(t, readFixture(t, "chat_reply_1.json")).URL
			},
			tools: "tools:\n  - alive__echo\n",
		},
		{
			name: "tools 校驗失敗：MCP 已連上之後才被 Subset 擋下",
			providerURL: func(t *testing.T) string {
				return "http://provider.invalid" // 走不到 turn，不會被呼叫
			},
			tools:   "tools:\n  - alive__echo\n  - no_such_tool\n",
			wantErr: true,
		},
		{
			name: "turn 失敗：Provider 回錯誤",
			providerURL: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
				}))
				t.Cleanup(srv.Close)
				return srv.URL
			},
			tools:   "tools:\n  - alive__echo\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "alive.exited")
			dir := setupChatWorkspace(t, tt.providerURL(t))
			writeMcpServers(t, dir, "mcp_servers:\n"+
				testMcpServerEntryWithExitMarker(t, "alive", marker, "echo"))
			writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
				"mcp_servers:\n  - alive\n"+tt.tools)

			var out bytes.Buffer
			err := runChat(context.Background(), strings.NewReader(""), &out, dir,
				chatOptions{profileName: "default", message: "你好"})
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("runChat 錯誤 = %v, 期望有錯誤 = %v\n輸出:\n%s", err, tt.wantErr, out.String())
			}
			if _, statErr := os.Stat(marker); statErr != nil {
				t.Errorf("runChat 返回時 MCP 子進程還沒被收掉（marker %s 不存在）——每跑一次就留一隻孤兒", marker)
			}
		})
	}
}

// TestProcessAssemblyClosesAuditBeforeStore 釘住「審計先於 SQLite 關閉」（ticket #74 AC）。
//
// 審計寫入走背景佇列：turn 結束的當下，記錄可能都還沒落庫。Close 若先關 SQLite，佇列裡
// 剩下的每一筆都寫不進去——審計表出現破洞，而對話本身一切正常，沒有人會發現。
//
// ticket #74 之前，這條靠 runChat 裡兩個 defer 的先後（後進先出）成立，只有一段註解守著；
// 抽取後它集中在 processAssembly.Close 一處，這一格守的就是那一處。
//
// **為什麼灌 200 筆而不是跑一個 turn。** 一個 turn 只產生一兩筆，而 turn 結束到收尾之間
// 還夾著 SaveSession 的同步寫入，背景 worker 多半早就寫完了——關閉順序寫錯也照樣全綠。
// 200 筆在一個緊迴圈裡排入只要幾微秒，遠快於逐筆寫進 SQLite，所以 Close 被呼叫時佇列
// 裡一定還有東西；200 也在佇列容量 256 之內，排入本身不會因為佇列滿而丟。
func TestProcessAssemblyClosesAuditBeforeStore(t *testing.T) {
	const records = 200
	const sessionID = "close-order"

	dir := setupChatWorkspace(t, "http://provider.invalid")
	ctx := context.Background()
	proc, err := assembleProcess(ctx, io.Discard, dir)
	if err != nil {
		t.Fatalf("assembleProcess: %v（收尾：%v）", err, proc.Close())
	}

	now := time.Now()
	for range records {
		proc.audit.RecordLLMCall(ctx, core.LLMCall{
			SessionID: sessionID, Provider: "openrouter", Model: "m",
			Status: core.AuditStatusCompleted, StartedAt: now, CompletedAt: now,
		})
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, workspaceDir, sessionDBFile))
	if err != nil {
		t.Fatalf("開啟資料庫: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉資料庫: %v", err)
		}
	}()
	var got int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_calls WHERE session_id = ?`, sessionID).Scan(&got); err != nil {
		t.Fatalf("查詢 llm_calls: %v", err)
	}
	if got != records {
		t.Errorf("Close 之後 llm_calls 有 %d 筆, 期望 %d 筆——佇列還沒排空 SQLite 就先關了", got, records)
	}
}

// TestShellLimiterIsSharedAcrossProfileAssemblies 釘住「shell admission limiter 由進程層級
// 建立後傳入 Profile 層級，不在 Profile 層級內建立」（ticket #74 AC，ticket #35 定案）。
//
// TestShellLimiterIsSharedAcrossBuildToolRegistryCalls 守的是下一層：limiter 不在
// buildToolRegistry 裡建立。那一格自己手動建一份 limiter 再傳兩次，所以**有人把
// tool.NewShellLimiter() 移進 assembleProfile** 時它照樣全綠——而 server 正是對每份
// Profile 各呼叫一次 assembleProfile，那時每份 Profile 各拿一個 8 格的池子，跨 Session
// 的總量又變回沒有上限。
//
// 這一格照 server 將會走的形狀：一次 assembleProcess、兩次 assembleProfile，N+1 個並發
// shell 呼叫橫跨兩份 Profile 的 Executor。
func TestShellLimiterIsSharedAcrossProfileAssemblies(t *testing.T) {
	const maxWorkers = 8 // 與 internal/tool 的 maxShellLifecycleWorkers 對齊（對外契約）

	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeWorkspaceConfig(t, dir, "http://provider.invalid",
		"http:\n  allowed_domains: []\nshell:\n  allowed_commands: [sleep]\n  timeout_seconds: 2\n")
	for _, name := range []string{"default", "second"} {
		writeNamedProfile(t, dir, name, "provider:\n  name: openrouter\n  model: m\ntools:\n  - shell\n")
	}
	profiles := assembleWorkspace(t, io.Discard, dir, "default", "second")

	// 並發才驗得到：循序呼叫的話，slot 會在每次之間歸還。
	const calls = maxWorkers + 1
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]core.ToolResult, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 一起出發，確保 N+1 個真的同時在場
			results[i] = profiles[i%len(profiles)].executor.Execute(context.Background(),
				core.ToolCall{ID: strconv.Itoa(i), Name: "shell", Arguments: `{"command":"sleep","args":["30"]}`})
		}()
	}
	close(start)
	wg.Wait()

	rejected := 0
	for _, result := range results {
		if !result.OK && strings.Contains(result.Error, "已達上限") {
			rejected++
		}
	}
	if rejected == 0 {
		t.Errorf("%d 個並發呼叫橫跨兩份 Profile，沒有任何一個被 slot 擋下——"+
			"limiter 不是進程層級那一份（是不是在 assembleProfile 裡建立了？）", calls)
	}
	if rejected > calls-maxWorkers {
		t.Errorf("被拒 %d 個，期望至多 %d 個——上限比 %d 更嚴", rejected, calls-maxWorkers, maxWorkers)
	}
}

// TestWorkspaceReminderPrintedOncePerProcess 釘住「只跟 Workspace 有關的啟動提醒屬於進程
// 層級」（ticket #74 的分層表）。
//
// PATH 與 file.allowed_paths 重疊是 config.yaml 與父進程 PATH 的性質，與哪份 Profile 無關。
// 留在 Profile 層級的話，chat 看不出差別（只組一份），server 載入 N 份 Profile 就會把同
// 一句話印 N 次——使用者會以為是 N 個不同的問題。
func TestWorkspaceReminderPrintedOncePerProcess(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	pathDir := filepath.Join(dir, workspaceDir, "scripts", "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatalf("建立 scripts/bin: %v", err)
	}
	// PATH 只放那一個目錄，與 TestChatPathOverlapWarning 同一個佈置。
	t.Setenv("PATH", pathDir)
	writeWorkspaceConfig(t, dir, "http://provider.invalid",
		"http:\n  allowed_domains: []\nfile:\n  allowed_paths: [scripts]\n")
	for _, name := range []string{"default", "second"} {
		writeNamedProfile(t, dir, name, "provider:\n  name: openrouter\n  model: m\n")
	}

	var out bytes.Buffer
	assembleWorkspace(t, &out, dir, "default", "second")

	if n := strings.Count(out.String(), "升級成執行權限"); n != 1 {
		t.Errorf("PATH 重疊提醒出現 %d 次, 期望 1 次（兩份 Profile、一個 Workspace）:\n%s", n, out.String())
	}
}
