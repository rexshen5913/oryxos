// 本檔驗 MCP 的**時間上限與進程生命週期**（ticket #22）：連線期限、單次工具呼叫的
// 自有期限、關閉的整體期限與逾期強制終止，以及 server 中途死掉之後的行為。
//
// **失敗形態一律由真實的本地 stdio server 子進程製造**（見 mcp_server_test.go），
// 不 mock 協議、不注入假 transport（憲法 4.3、ADR-0002）。
//
// 為什麼是 `package tool` 的白盒測試：這三個期限在 production 是秒級的常數，測試必須
// 把它們設成毫秒級才跑得動——那要直接組 mcpConn 與 McpToolAdapter。期限是**顯式參數**
// 而不是為測試開的注入點：production 傳常數、測試傳短值，兩邊走的是同一條路。
package tool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// discardLogger 供不斷言日誌內容的測試使用。
//
// 一般的資訊型日誌不進自動化測試斷言（spec #3 Testing Decisions）：措辭要能改而不
// 讓測試轉紅。本檔斷言的是可觀察的行為——回傳值、耗時、子進程死了沒有。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// dialTestServer 起一個真實的 stdio MCP server 並完成交握，測試結束時收掉子進程。
func dialTestServer(t *testing.T, mode string, tools ...string) *mcpConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialMcpStdio(ctx, toolMcpSpec(t, "probe", mode, tools...), discardLogger())
	if err != nil {
		t.Fatalf("連上測試 MCP server: %v", err)
	}
	t.Cleanup(func() {
		// 一律收掉，否則失敗的測試會留下孤兒進程給後面的測試。期限給得寬鬆——
		// 這裡是清理，不是被測的行為。
		_ = conn.close(5 * time.Second)
	})
	return conn
}

// firstToolDecl 走真實的 tools/list 取回第一個工具宣告。
func firstToolDecl(t *testing.T, conn *mcpConn) mcpToolDecl {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	decls, err := conn.listTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(decls) == 0 {
		t.Fatalf("tools/list 回了 0 個工具")
	}
	return decls[0]
}

// TestMcpConnectHasItsOwnDeadline 驗**連線這一段自己有期限**。
//
// 這一格防的是「server 起得來但半死不活」：進程 spawn 成功、stdin/stdout 都通，就是
// 不回 initialize（依賴沒裝好、卡在自己的初始化上都會長這樣）。
//
// **ctx 刻意用 context.Background()——沒有任何來自呼叫端的期限。** 這是關鍵：`oryxos
// chat` 傳進來的就是一個沒有 deadline 的 ctx，能救場的只有連線這一層自己那個。用一個
// 帶 deadline 的 ctx 來測會讓「期限來自呼叫端」與「期限來自這一層」分不出來，那條測試
// 在期限被拿掉之後照樣會綠。
//
// 順帶驗的第二件事同樣重要：連不上時**必須把已經 spawn 的子進程收掉**。收不掉的話
// 這條測試會卡在收尾上，而不是乾淨地返回錯誤。
func TestMcpConnectHasItsOwnDeadline(t *testing.T) {
	const timeout = 300 * time.Millisecond

	start := time.Now()
	conn, _, err := connectMcpServer(context.Background(),
		toolMcpSpec(t, "silent", modeHangInitialize, "echo"), timeout, discardLogger())
	elapsed := time.Since(start)

	if err == nil {
		_ = conn.close(5 * time.Second)
		t.Fatal("期望交握逾時報錯，實際成功連上")
	}
	// 上限給得寬鬆（期限的 20 倍）：這條測試要證明的是「有期限」而不是期限有多準，
	// 在忙碌的 CI 上多等幾百毫秒不該讓它閃紅。
	if elapsed > 20*timeout {
		t.Errorf("連線等了 %v 才放棄，期限是 %v——期限沒有生效", elapsed, timeout)
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("錯誤 = %q, 期望指出是 initialize 交握失敗", err.Error())
	}
}

// TestMcpConnectStopsWhenCallerCancels 釘住**取消不是降級**。
//
// 降級的判準是「這個 server 掛了」，而呼叫端取消（使用者在啟動途中按 Ctrl-C）是
// 「不要再啟動了」——兩者在程式裡都表現成一個錯誤，混為一談的後果很難看：剩下的每個
// server 各自以 context.Canceled 失敗、各自被記成連不上、各自再花一次清理時間，最後
// ConnectMcpServers 還回 nil，於是 Agent 帶著一組空的 MCP 工具照常起來——取消等於沒按。
//
// 三條斷言分別對應那三個後果：回錯誤（而不是 nil）、不記成連線失敗、**後續 server
// 不再 spawn**。最後一條用 marker 檔驗，那是從外部唯一看得到「子進程有沒有真的被
// 啟動過」的方式。
func TestMcpConnectStopsWhenCallerCancels(t *testing.T) {
	dir := t.TempDir()
	markers := []string{filepath.Join(dir, "first.marker"), filepath.Join(dir, "second.marker")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	registry := NewRegistry()
	svc, err := ConnectMcpServers(ctx, registry, []core.McpServerSpec{
		toolMcpSpecWithEnv(t, "first", "", map[string]string{toolMcpSpawnMarkerEnv: markers[0]}, "echo"),
		toolMcpSpecWithEnv(t, "second", "", map[string]string{toolMcpSpawnMarkerEnv: markers[1]}, "echo"),
	}, nil, discardLogger())
	t.Cleanup(func() { _ = svc.Close() })

	if err == nil {
		t.Fatal("呼叫端取消時應該中止並回錯誤，而不是把取消當成 server 掛掉、照常回 nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("錯誤 = %v, 期望包住 context.Canceled（呼叫端才分得出是取消還是 server 壞了）", err)
	}
	if got := svc.Failures(); len(got) != 0 {
		t.Errorf("取消被記成了 %d 筆連線失敗: %v", len(got), got)
	}
	for _, marker := range markers {
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Errorf("取消之後仍然 spawn 了子進程（%s 存在）", filepath.Base(marker))
		}
	}
}

// TestMcpToolCallHasItsOwnDeadline 驗**單次工具呼叫有自己的時間上限**。
//
// 這是本票最核心的一格。呼叫端的 ctx 已經能貫穿（#21 做的），但互動模式的 ctx 通常
// **沒有 deadline**——一個收下請求卻不回應的 server 會讓那個 turn 永遠掛住，使用者只能
// 按 Ctrl-C 殺掉整個 oryxos chat。所以呼叫這一層必須自己有上限，而不是指望呼叫端給。
//
// 因此這裡刻意傳 context.Background()：**沒有任何來自呼叫端的期限**，能救場的只有
// adapter 自己那一個。
func TestMcpToolCallHasItsOwnDeadline(t *testing.T) {
	const callTimeout = 300 * time.Millisecond
	conn := dialTestServer(t, modeHangCall, "echo")
	adapter := newMcpToolAdapter(conn, "probe", firstToolDecl(t, conn), callTimeout)

	done := make(chan core.ToolResult, 1)
	start := time.Now()
	go func() { done <- adapter.Execute(context.Background(), `{"text":"hi"}`) }()

	select {
	case result := <-done:
		if result.OK {
			t.Fatalf("期望呼叫逾時失敗，實際成功: %+v", result)
		}
		if result.Error == "" {
			t.Error("失敗必須有 Error 內容回填給 LLM，實際是空字串")
		}
		if elapsed := time.Since(start); elapsed > 20*callTimeout {
			t.Errorf("呼叫等了 %v 才放棄，上限是 %v——上限沒有生效", elapsed, callTimeout)
		}
	case <-time.After(10 * time.Second):
		// 拿掉 Execute 裡的 WithTimeout 就會走到這裡：沒有人給期限，呼叫永遠不返回。
		t.Fatal("呼叫在 10 秒內沒有返回：單次呼叫沒有自己的時間上限")
	}
}

// TestMcpToolCallFailuresAreFilledBackNotFatal 是**工具呼叫的失敗矩陣**。
//
// 共通的斷言是同一條，也是這一票要守住的語義：不論 server 怎麼壞，Execute 都以
// ToolResult.Error 回填、**不 panic 也不中斷 turn**（沿 spec #1 既有的 Tool 失敗語義），
// 讓 LLM 據此換一條路。
func TestMcpToolCallFailuresAreFilledBackNotFatal(t *testing.T) {
	const callTimeout = 300 * time.Millisecond
	tests := []struct {
		name string
		mode string
		// input 為空時送一份合法的參數。
		input string
	}{
		{
			name: "server 收下呼叫卻不回應：逾時回填",
			mode: modeHangCall,
		},
		{
			name: "server 中途死掉：連線結束回填",
			mode: modeDieOnCall,
		},
		{
			// 參數不是合法 JSON 是 LLM 那一側的問題，同樣不該炸掉 turn——錯誤訊息本身
			// 就是給模型的修改指示。
			name:  "參數不是合法 JSON：回填錯誤而不是送出去",
			mode:  "",
			input: "{not json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := dialTestServer(t, tt.mode, "echo")
			adapter := newMcpToolAdapter(conn, "probe", firstToolDecl(t, conn), callTimeout)

			input := tt.input
			if input == "" {
				input = `{"text":"hi"}`
			}
			result := adapter.Execute(context.Background(), input)

			if result.OK {
				t.Fatalf("期望失敗，實際成功: %+v", result)
			}
			if result.Error == "" {
				t.Error("失敗必須有 Error 內容回填給 LLM，實際是空字串")
			}
			// 註冊名要出現在回填裡：一個 turn 可能呼叫多個工具，LLM 得知道是哪一個壞了。
			if !strings.Contains(result.Error, adapter.Name()) {
				t.Errorf("回填 = %q, 期望含工具名 %q", result.Error, adapter.Name())
			}
		})
	}
}

// TestMcpSubsequentCallsFailAfterServerDeath 驗 server 死掉之後**後續呼叫同樣失敗**。
//
// 核心階段不自動重連（spec #3 Further Notes 已決），所以這裡要釘住的是「不重連」這個
// 決定的可觀察後果：第二次呼叫不會莫名其妙地又活過來，也不會掛住等一個永遠不來的回應
// ——它立刻拿到明確的失敗。
func TestMcpSubsequentCallsFailAfterServerDeath(t *testing.T) {
	const callTimeout = 300 * time.Millisecond
	conn := dialTestServer(t, modeDieOnCall, "echo")
	adapter := newMcpToolAdapter(conn, "probe", firstToolDecl(t, conn), callTimeout)

	first := adapter.Execute(context.Background(), `{"text":"one"}`)
	if first.OK {
		t.Fatalf("第一次呼叫期望失敗（server 收到呼叫就死），實際成功: %+v", first)
	}

	done := make(chan core.ToolResult, 1)
	go func() { done <- adapter.Execute(context.Background(), `{"text":"two"}`) }()
	select {
	case second := <-done:
		if second.OK {
			t.Fatalf("server 已經死了，第二次呼叫卻成功: %+v", second)
		}
		if second.Error == "" {
			t.Error("第二次失敗同樣要有 Error 回填")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server 死後的第二次呼叫掛住了：連線失效之後應該立刻失敗")
	}
}

// TestMcpCloseForceKillsUnresponsiveServer 驗**關閉有整體期限、逾期強制終止**。
//
// 關 stdin 是 MCP stdio 規範裡「請你收工」的標準訊號，但那只是請求——一個不理會它的
// server（或卡在自己的清理程式裡的 server）會讓 close 永遠等下去，於是 oryxos chat
// 打完招呼就再也退不出來。使用者對這種情況唯一的手段是 Ctrl-C，而那會留下孤兒進程，
// 正好是關閉這件事本來要避免的。
func TestMcpCloseForceKillsUnresponsiveServer(t *testing.T) {
	const closeTimeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialMcpStdio(ctx, toolMcpSpec(t, "stubborn", modeIgnoreStdinClose, "echo"), discardLogger())
	if err != nil {
		t.Fatalf("連上測試 MCP server: %v", err)
	}
	// 觸發那個模式：server 回完 tools/list 之後就不再讀 stdin 了。
	if _, err := conn.listTools(ctx); err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	start := time.Now()
	if err := conn.close(closeTimeout); err != nil {
		t.Errorf("close 回了錯誤: %v（強制終止是預期路徑，不該當成失敗上拋）", err)
	}
	elapsed := time.Since(start)

	if elapsed > 20*closeTimeout {
		t.Errorf("close 等了 %v 才返回，期限是 %v——期限沒有生效，CLI 會卡在退出上",
			elapsed, closeTimeout)
	}
	// 子進程必須真的被收掉：ProcessState 有值代表 Wait 已經回收過它，沒有孤兒留下。
	if conn.cmd.ProcessState == nil {
		t.Error("close 返回時子進程還沒有被回收，會留下孤兒進程")
	}
}

// TestMcpCloseIsQuickWhenServerCooperates 是上一條的對照組。
//
// 沒有它的話，一個「一律先睡滿期限再 kill」的實作也會讓上一條通過——那會讓每次退出
// 都平白多等一個期限。這一條釘住正常路徑仍然走「關 stdin → server 自己退出」。
func TestMcpCloseIsQuickWhenServerCooperates(t *testing.T) {
	const closeTimeout = 10 * time.Second
	conn := dialTestServer(t, "", "echo")

	start := time.Now()
	if err := conn.close(closeTimeout); err != nil {
		t.Errorf("close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("守規矩的 server 也等了 %v 才關掉：關閉不該無條件等滿期限", elapsed)
	}
}

// TestMcpToolResultShapes 驗 adapter 對 server 回應的處理，含 tools/list 回空之後的
// 未知工具呼叫。
//
// 放在這裡而不是 happy path 的測試裡，是因為它們都屬「壞掉但不該中斷 turn」那一類。
func TestMcpToolCallOnUnknownToolIsFilledBack(t *testing.T) {
	conn := dialTestServer(t, "", "echo")
	decl := firstToolDecl(t, conn)
	// 把宣告改成一個 server 沒有的工具名：server 照規範回 result 帶 isError，
	// 那是工具執行層的失敗，不是協議層的。
	decl.Name = "ghost"
	adapter := newMcpToolAdapter(conn, "probe", decl, 5*time.Second)

	result := adapter.Execute(context.Background(), `{"text":"hi"}`)
	if result.OK {
		t.Fatalf("期望失敗，實際成功: %+v", result)
	}
	if !strings.Contains(result.Error, "ghost") {
		t.Errorf("回填 = %q, 期望含工具名", result.Error)
	}
}

// TestMcpEmptyToolListIsNotAnError 驗 **tools/list 回空不算錯誤**。
//
// 一個沒有工具的 server 是合法的（它可能只提供 resources，或它的工具還沒設定好）。
// 把它當錯誤會讓一個無關緊要的 server 擋下整個啟動；靜默跳過則讓使用者查不出「為什麼
// 這個 server 沒給我工具」——所以是「照常連上、取回 0 個、留下日誌」。
func TestMcpEmptyToolListIsNotAnError(t *testing.T) {
	conn := dialTestServer(t, "") // 不宣告任何工具
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	decls, err := conn.listTools(ctx)
	if err != nil {
		t.Fatalf("tools/list 回空不該是錯誤: %v", err)
	}
	if len(decls) != 0 {
		t.Errorf("工具數 = %d, 期望 0", len(decls))
	}
}
