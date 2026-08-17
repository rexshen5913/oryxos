// 本檔驗**啟動階段連線多個 MCP server 的整體行為**（issue #26）：連線是並行的、
// 失敗在當下就回饋出去、而 Failures() 仍照使用者宣告的順序。
//
// 與 mcp_failure_test.go 的分工：那一檔是 ticket #22 的**單一** server 期限與生命週期，
// 這一檔是**跨 server** 的總體行為。兩者要守的東西不同，混在一起讀不出各自的主題。
//
// 失敗形態一律由真實的本地 stdio server 子進程製造（見 mcp_server_test.go），
// 不 mock 協議、不注入假 transport（憲法 4.3、ADR-0002）。
package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// unstartableMcpSpec 指向一個不存在的執行檔：spawn 當下就失敗，不佔任何可觀察的時間。
//
// 這是現實中最常見的形態（機器上沒裝 node、路徑打錯、套件沒安裝好），也正好是本檔
// 要的「立刻失敗」對照組。
func unstartableMcpSpec(t *testing.T, name string) core.McpServerSpec {
	t.Helper()
	return core.McpServerSpec{
		Name:      name,
		Transport: core.McpTransportStdio,
		Command:   []string{filepath.Join(t.TempDir(), "nonexistent-oryxos-mcp-server")},
	}
}

// slowMcpSpecs 產出 n 份「交握會成功、但各拖 slowInitializeDelay」的 server 宣告。
func slowMcpSpecs(t *testing.T, n int) []core.McpServerSpec {
	t.Helper()
	specs := make([]core.McpServerSpec, 0, n)
	for i := range n {
		specs = append(specs, toolMcpSpec(t, fmt.Sprintf("slow%d", i), modeSlowInitialize, "echo"))
	}
	return specs
}

// TestMcpServersConnectInParallel 釘住**連線是並行的**（issue #26 方向一）。
//
// 序列連線之下每個 server 各吃一份自己的連線期限，總等待是全部加起來：三個「起得來
// 但交握不完成」的 server 會讓 `oryxos chat` 靜默等上一分半。使用者看不出它在等什麼，
// 也不知道是不是當掉了。
//
// **用「慢但會成功」而不是「逾時」來量**：逾時場景要把期限壓到毫秒級才跑得動，那需要
// 把 mcpConnectTimeout 開成參數；而並行與否這件事，一個會如期完成的延遲量得更準，也不
// 必動生產程式碼的簽名。
//
// 門檻取 n×delay 的一半：序列至少是 n×delay，並行是 1×delay 加上 spawn 的成本，
// 兩者之間有兩倍以上的餘裕——足以證明是並行，又不會在忙碌的 CI 上閃紅。
func TestMcpServersConnectInParallel(t *testing.T) {
	const servers = 5
	specs := slowMcpSpecs(t, servers)

	registry := NewRegistry()
	start := time.Now()
	svc, err := ConnectMcpServers(context.Background(), registry, specs, nil, discardLogger())
	elapsed := time.Since(start)
	t.Cleanup(func() {
		if cerr := svc.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})

	if err != nil {
		t.Fatalf("這些 server 都連得上，不該回錯誤: %v", err)
	}
	if got := svc.Failures(); len(got) != 0 {
		t.Fatalf("Failures() = %v, 期望 0 筆（慢不等於失敗）", got)
	}
	// 並行不得偷工：每一個 server 的工具都要真的註冊進 Registry。
	for _, spec := range specs {
		name := McpToolName(spec.Name, "echo")
		if _, subsetErr := registry.Subset([]string{name}, nil, discardLogger()); subsetErr != nil {
			t.Errorf("%s 的工具沒有註冊: %v", spec.Name, subsetErr)
		}
	}

	if limit := servers * slowInitializeDelay / 2; elapsed > limit {
		t.Errorf("連 %d 個各慢 %v 的 server 花了 %v（上限 %v）——連線仍是序列的，"+
			"總等待是每個 server 加起來而不是最慢的那一個",
			servers, slowInitializeDelay, elapsed, limit)
	}
}

// TestMcpConnectFailureIsReportedWhileOtherServersStillConnecting 釘住**失敗在當下就
// 回饋，不等整批跑完**（issue #26 方向三）。
//
// 光是並行還不夠：並行之後總等待仍然是最慢的那一個（冷啟動的連線期限給到 30 秒），
// 這段時間裡使用者依然什麼都看不到。已經確定連不上的 server，沒有理由陪著還在連的
// 那些一起等——那個等待正是「不知道它在幹嘛」的來源。
//
// 兩條斷言缺一不可：
//
//   - 對照組「慢的那一個真的慢」防的是假綠——兩個 server 都瞬間結束的話，「回饋很早」
//     會自動成立，這條測試就再也抓不到回歸。
//   - 本體斷言回饋發生在慢 server 完成之前。
//
// 回饋管道是 callback 而不是 io.Writer：internal/tool 不該知道要印去哪、也不該決定
// 措辭（那是組裝點的事，見 cmd/oryxos 的 warnMcpServerUnavailable）。它只負責把
// 「這個 server 這次沒連上」在發生的當下交出去。
func TestMcpConnectFailureIsReportedWhileOtherServersStillConnecting(t *testing.T) {
	specs := []core.McpServerSpec{
		unstartableMcpSpec(t, "broken"),
		toolMcpSpec(t, "slow", modeSlowInitialize, "echo"),
	}

	var reported []McpConnectFailure
	var reportedAt []time.Duration
	start := time.Now()
	svc, err := ConnectMcpServers(context.Background(), NewRegistry(), specs,
		func(f McpConnectFailure) {
			reported = append(reported, f)
			reportedAt = append(reportedAt, time.Since(start))
		}, discardLogger())
	elapsed := time.Since(start)
	t.Cleanup(func() {
		if cerr := svc.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})

	if err != nil {
		t.Fatalf("一個 server 連不上不該中斷啟動: %v", err)
	}
	if len(reported) != 1 {
		t.Fatalf("回饋了 %d 筆失敗，期望 1 筆: %v", len(reported), reported)
	}
	if reported[0].Server != "broken" {
		t.Errorf("回饋的 server = %q, 期望 %q", reported[0].Server, "broken")
	}
	// 原始錯誤要原文帶出去：「命令找不到」與「交握逾時」要修的東西完全不同，摘要成
	// 一句「連線失敗」等於把使用者唯一的線索丟掉。
	if reported[0].Err == nil {
		t.Error("回饋沒有帶上原始錯誤")
	}
	// 即時回饋**不取代** Failures()：組裝點在連線結束後仍要據它把工具從 Profile 拿掉。
	if got := svc.Failures(); len(got) != 1 || got[0].Server != "broken" {
		t.Errorf("Failures() = %v, 期望只有 broken 一筆", got)
	}

	if elapsed < slowInitializeDelay {
		t.Fatalf("整批只花了 %v（慢 server 應該至少拖 %v）——對照組沒有生效，"+
			"下面那條斷言會恆真", elapsed, slowInitializeDelay)
	}
	if limit := slowInitializeDelay / 2; reportedAt[0] > limit {
		t.Errorf("broken 連不上這件事等到第 %v 才回饋（上限 %v，整批共 %v）——"+
			"回饋仍在等整批跑完，使用者在這段時間裡看不到任何東西",
			reportedAt[0], limit, elapsed)
	}
}

// TestMcpConnectFailuresKeepDeclaredOrder 釘住 **Failures() 照使用者宣告的順序**，
// 不隨連線快慢浮動。
//
// 並行連線唯一該影響的是「花多久」，不該影響「看到什麼」。若就地按到達順序 append，
// Failures() 會變成一次擲骰子：同一份設定跑兩次，回傳的順序可能不同。那種不確定性
// 對一個公開回傳值來說是退化，而它是併發帶進來的、不是使用者要的。
//
// 這一條**不管即時警示的順序**——那個順序本來就該是完成順序（誰先確定連不上誰先喊，
// 見上一條測試）。兩者刻意不同：警示是事件，Failures() 是結果。
//
// spec 順序刻意讓慢的排前面：實作若不做任何事，快的那個會先到、先被記下，
// 兩個名字就會反過來。
func TestMcpConnectFailuresKeepDeclaredOrder(t *testing.T) {
	specs := []core.McpServerSpec{
		toolMcpSpec(t, "slow-broken", modeSlowInitializeThenDie, "echo"),
		unstartableMcpSpec(t, "fast-broken"),
	}

	svc, err := ConnectMcpServers(context.Background(), NewRegistry(), specs, nil, discardLogger())
	t.Cleanup(func() {
		if cerr := svc.Close(); cerr != nil {
			t.Errorf("關閉 MCP server: %v", cerr)
		}
	})
	if err != nil {
		t.Fatalf("server 連不上不該中斷啟動: %v", err)
	}

	failures := svc.Failures()
	if len(failures) != 2 {
		t.Fatalf("Failures() = %v, 期望兩個 server 各記一筆", failures)
	}
	got := []string{failures[0].Server, failures[1].Server}
	want := []string{"slow-broken", "fast-broken"}
	if !slices.Equal(got, want) {
		t.Errorf("Failures() 的順序 = %v, 期望 %v（宣告順序）——順序跟著連線快慢跑了", got, want)
	}
	// 每個失敗仍各自帶著自己的原因：並行不得把兩個錯誤併成一句。
	for _, f := range failures {
		if f.Err == nil {
			t.Errorf("%s 的失敗沒有帶上原始錯誤", f.Server)
		}
	}
}
