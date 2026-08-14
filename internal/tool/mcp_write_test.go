// 本檔是 mcpConn.write 的**內部**單元測試：驗它對 stdin 寫入期限的記帳，以及取消語義。
//
// **為什麼這幾條不走真實的 stdio server**（其餘 MCP 測試一律走真的，見
// internal/core/mcp_server_test.go）：要驗的東西是「ctx 在寫入進行中被取消」這個時序，
// 而真實子進程的 pipe 什麼時候塞滿、Write 什麼時候返回都不是測試控制得了的——用真的
// server 只能靠機率去撞那個窗口，撞不到就是一條假綠的測試。
//
// 這裡替換掉的是 stdin（一個 io.WriteCloser）與「設定寫入期限」這個能力，**不是 MCP
// 協議**：這幾條測試裡沒有任何 JSON-RPC 往返，也沒有任何一句協議語義被模擬。憲法 4.3
// 與本 spec 禁止的是「mock MCP 協議、注入假的 transport 來假裝跑過協議」，不是禁止對
// 一個函式的 I/O 記帳做單元測試。
//
// 因此本檔是 `package tool`（而不是 tool_test）：要直接組出 mcpConn。
package tool

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingWriter 是可以精確控制「什麼時候寫完」的 stdin 替身，並且數得出被呼叫幾次。
type blockingWriter struct {
	// entered 在 Write 被呼叫時收到一則訊息（緩衝 1，滿了就丟）。
	entered chan struct{}
	// calls 是 Write 被呼叫的次數——「一個位元組都沒碰過」這種斷言只能靠它。
	calls atomic.Int64
	// release 讓卡住的 Write 返回。
	release chan struct{}
	// wroteBytes 是 Write 要回報寫出的位元組數；err 非 nil 時連它一起回。
	wroteBytes int
	err        error
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	if w.err != nil {
		return w.wroteBytes, w.err
	}
	return len(p), nil
}

func (w *blockingWriter) Close() error { return nil }

// deadlineRecorder 記下每一次 SetWriteDeadline 的值。
type deadlineRecorder struct {
	mu    sync.Mutex
	calls []time.Time
	// set 在每次被呼叫時通知（緩衝 1，滿了就丟——測試只需要知道「發生過」）。
	set chan struct{}
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{set: make(chan struct{}, 1)}
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.calls = append(d.calls, t)
	d.mu.Unlock()
	select {
	case d.set <- struct{}{}:
	default:
	}
	return nil
}

// last 回傳最後一次設定的期限，以及有沒有被設定過。
func (d *deadlineRecorder) last() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return time.Time{}, false
	}
	return d.calls[len(d.calls)-1], true
}

// newTestConn 組出一條只接得上「寫入」那一半的假連線。
func newTestConn(stdin io.WriteCloser, deadline writeDeadlineSetter) *mcpConn {
	return &mcpConn{
		name:          "fake",
		stdin:         stdin,
		logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		pending:       make(map[int]chan rpcMessage),
		done:          make(chan struct{}),
		writeSlot:     make(chan struct{}, 1),
		stdinDeadline: deadline,
	}
}

func testRequest() rpcRequest {
	return rpcRequest{JSONRPC: "2.0", Method: "ping"}
}

// TestWriteClearsDeadlineAfterCancelRacesWithSuccess 是這條記帳規則的核心：
// **write 返回時，stdin 上不得留下任何過期的寫入期限。**
//
// 讓卡住的寫入能被 ctx 解開的手段是把期限設成「現在」，而那個動作跑在 context.AfterFunc
// 啟動的 goroutine 上。`stop()` **不等回呼跑完**（文件明寫 "The stop function does not
// wait for f to complete"），所以「ctx 取消」與「寫入成功」競態時，回呼可能在寫入者已經
// 放開寫入權之後才把期限設成過去——於是**下一則訊息**的正常寫入立刻以 i/o timeout 失敗。
// 壞的是後面那次無關的呼叫，是最難查的一種形態。
//
// 這裡把那個競態做成**確定性**的：先讓 Write 進入並停住 → 取消 ctx → 等到回呼真的設過
// 期限 → 才讓 Write 成功返回。三個步驟都由測試自己排序，不靠運氣。
func TestWriteClearsDeadlineAfterCancelRacesWithSuccess(t *testing.T) {
	writer := newBlockingWriter()
	recorder := newDeadlineRecorder()
	conn := newTestConn(writer, recorder)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- conn.write(ctx, testRequest(), "測試訊息") }()

	<-writer.entered // Write 已進入，這時 ctx 還沒取消
	cancel()
	<-recorder.set // 回呼真的把期限設成過去了——競態已經確定地製造出來
	close(writer.release)

	if err := <-errCh; err != nil {
		t.Fatalf("寫入其實成功了，write 不該回錯誤: %v", err)
	}

	last, ok := recorder.last()
	if !ok {
		t.Fatal("完全沒有設定過期限——這條測試沒有走到它要測的路徑")
	}
	if !last.IsZero() {
		t.Errorf("write 返回時期限是 %v（非零）——過期的期限外洩給下一則訊息了", last)
	}
	// 連線必須還是健康的：寫入成功了，沒有理由判它死。
	if err := conn.failureErr(); err == nil {
		t.Fatal("failureErr 應該只在連線真的壞掉時有值——這裡不該被判死")
	} else if conn.failure != nil {
		t.Errorf("寫入成功卻把連線判死了: %v", conn.failure)
	}
}

// TestWriteRejectsAlreadyCancelledContext 釘住「已取消的呼叫不產生副作用」：一個位元組
// 都不該碰到 stdin，否則外部工具會照樣執行一次——那正是使用者按下取消要避免的事。
//
// 擋這件事需要**兩道**：取得寫入權的 select 會挑到 `ctx.Done()`，以及**拿到寫入權之後
// 再確認一次 ctx**。第二道不可省，因為兩個 case 同時就緒時 Go 的 select 是隨機挑的。
//
// **這條測試會自我驗證有沒有真的走到第二道**：兩條路的錯誤訊息不同，所以測試看得出這一輪
// 走的是哪一條，並且在沒走到目標分支時直接失敗——它不會因為每次都撞到第一道而悄悄變成
// 一條什麼都沒驗的測試。輪數上限 40 是為了那個隨機性（連續 40 次都撞到第一道的機率是
// 2 的 -40 次方），不是為了碰運氣抓 bug：**每一輪**都斷言 writer 沒被碰過。
func TestWriteRejectsAlreadyCancelledContext(t *testing.T) {
	const maxRounds = 40

	writer := newBlockingWriter()
	// 先放行，這樣萬一真的寫進去了，測試是乾淨地失敗而不是卡死。
	close(writer.release)
	conn := newTestConn(writer, newDeadlineRecorder())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawPostSlotCheck bool
	for round := 1; round <= maxRounds && !sawPostSlotCheck; round++ {
		err := conn.write(ctx, testRequest(), "測試訊息")
		if err == nil {
			t.Fatalf("第 %d 輪：ctx 已取消，write 應該回錯誤", round)
		}
		// 「呼叫已取消」＝這一輪拿到了寫入權、被第二道擋下；另一條是還沒拿到寫入權就
		// 因為 ctx 結束而返回。
		if strings.Contains(err.Error(), "呼叫已取消") {
			sawPostSlotCheck = true
		}
		if n := writer.calls.Load(); n != 0 {
			t.Fatalf("第 %d 輪：已取消的呼叫仍然寫進了 stdin（%d 次）——外部工具會照樣執行", round, n)
		}
		if conn.failure != nil {
			t.Fatalf("第 %d 輪：什麼都沒送出，連線不該被判死: %v", round, conn.failure)
		}
		// 寫入權要還回去，否則下一輪會卡住。
		select {
		case conn.writeSlot <- struct{}{}:
			<-conn.writeSlot
		default:
			t.Fatalf("第 %d 輪：write 沒有釋放寫入權", round)
		}
	}
	if !sawPostSlotCheck {
		t.Fatalf("%d 輪都沒走到「拿到寫入權之後才確認 ctx」那條路——這條測試沒有驗到它要驗的東西",
			maxRounds)
	}
}

// TestWriteFailsConnectionOnlyOnPartialWrite 釘住「判死連線」的判準。
//
// pipe 的寫入超過 PIPE_BUF 就不是原子的：送出半截 JSON 之後，server 讀到的每一則訊息都
// 會錯位，那條連線救不回來、必須判死。但**一個字都沒出去**是另一回事——連線還是乾淨的，
// 判死等於誤殺（最常見的來源正是「呼叫在寫入前一刻被取消」）。
func TestWriteFailsConnectionOnlyOnPartialWrite(t *testing.T) {
	tests := []struct {
		name string
		// wrote 是 Write 回報寫出的位元組數。
		wrote int
		// wantFailed 為真表示這一格應該把連線判死。
		wantFailed bool
	}{
		{name: "一個字都沒出去：連線仍然健康", wrote: 0},
		{name: "寫出半截訊息：連線判死", wrote: 7, wantFailed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := newBlockingWriter()
			writer.wroteBytes = tt.wrote
			writer.err = io.ErrShortWrite
			close(writer.release)
			conn := newTestConn(writer, newDeadlineRecorder())

			err := conn.write(context.Background(), testRequest(), "測試訊息")
			if err == nil {
				t.Fatal("Write 失敗時 write 應該回錯誤")
			}
			if got := conn.failure != nil; got != tt.wantFailed {
				t.Errorf("連線被判死 = %v, 期望 %v（failure=%v）", got, tt.wantFailed, conn.failure)
			}
		})
	}
}

// TestWriteEmitsOneLinePerMessage 釘住 stdio transport 的分幀：一則訊息一行。
//
// 這是「一行一則」這個前提唯一的守門人：json.Marshal 會把字串裡的換行轉義，所以只要
// 我們自己在尾端補一個 \n，就不會有第二個裸換行跑進去。
func TestWriteEmitsOneLinePerMessage(t *testing.T) {
	var captured []byte
	conn := newTestConn(writerFunc(func(p []byte) (int, error) {
		captured = append(captured, p...)
		return len(p), nil
	}), newDeadlineRecorder())

	// 參數裡刻意帶裸換行，看它有沒有被轉義。
	msg := rpcRequest{JSONRPC: "2.0", Method: "tools/call", Params: map[string]any{
		"arguments": json.RawMessage(`{"text":"第一行\n第二行"}`),
	}}
	if err := conn.write(context.Background(), msg, "測試訊息"); err != nil {
		t.Fatalf("write: %v", err)
	}

	if n := countBytes(captured, '\n'); n != 1 {
		t.Errorf("送出的訊息含 %d 個換行，期望剛好 1（結尾那個）: %q", n, captured)
	}
	if captured[len(captured)-1] != '\n' {
		t.Errorf("訊息沒有以換行結尾: %q", captured)
	}
}

// writerFunc 讓一個函式當 io.WriteCloser 用。
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
func (f writerFunc) Close() error                { return nil }

func countBytes(b []byte, target byte) int {
	var n int
	for _, c := range b {
		if c == target {
			n++
		}
	}
	return n
}
