package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rexshen5913/oryxos/internal/core"
)

// progressPrefix 讓進度行一眼與 Agent 的回應分得開。
//
// 前綴而不是清行重寫：後者要 ANSI 控制碼、要處理視窗寬度與換行，而它換來的只是
// 「畫面比較乾淨」。進度的用途是讓等待中的使用者知道它沒卡死，多留幾行不是問題
// （憲法 3.2）。
const progressPrefix = "  · "

// progressSink 是 CLI 的 EventSink 實作：把執行過程印成進度行。
//
// **不阻塞**（EventSink 的契約）：只做一次同步的 Fprintf，沒有網路、沒有磁碟等待。
// 寫入失敗一律忽略——播報是旁路，一個寫不出去的終端機不該讓使用者的對話失敗。
//
// **不加鎖是刻意的，而且有前提。** EventSink 不承諾播報來自單一 goroutine，但這個
// 實作只服務 CLI：一個進程一個 Session，`RunInteractive` 一次處理一條訊息，播報因此
// 是序列的。若哪天有第二個呼叫端（Web Service 會是），它要的是自己的實作而不是這個
// ——那邊的輸出目標也不是終端機。
type progressSink struct {
	out io.Writer
}

// newProgressSink 建立寫向 out 的進度 sink。
func newProgressSink(out io.Writer) *progressSink {
	return &progressSink{out: out}
}

// ProgressSinkFor 依 out 是不是終端機，決定要顯示進度還是完全不做事。
//
// 非 TTY 一律退回 NopEventSink：`--message` 接進管線、輸出重導向到檔案、以及所有把
// 輸出接進 buffer 的既有測試，都不該因為多了進度顯示而看到多出來的字。判斷在這裡
// 做一次，組裝點就不必自己推導（也就不會兩個組裝點推出不同答案）。
//
// **判準是「字元裝置」，不是真的去問終端機。** 純標準庫做得到的就到這裡：`/dev/null`
// 也是字元裝置，因此重導向到 `/dev/null` 會被當成 TTY 而印出進度。代價是幾行沒人看到
// 的字，換掉的是一個 x/term 依賴（憲法 1.4 標準庫優先），這個交換划算。
func ProgressSinkFor(out io.Writer) core.EventSink {
	f, ok := out.(*os.File)
	if !ok {
		return core.NopEventSink{}
	}
	info, err := f.Stat()
	if err != nil {
		return core.NopEventSink{} // 問不出來就當它不是終端機：寧可不印，不要印錯地方
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return core.NopEventSink{}
	}
	return newProgressSink(f)
}

// Emit 把一則事件印成一行進度。
//
// **七種只印四種。** 另外三種留白的理由：turn_started 說的是使用者自己剛做的事；
// assistant_text 與 turn_finished 都會被 Channel 印出的回應本身取代，重複印會讓
// 使用者分不出哪一句才是答案。
func (s *progressSink) Emit(_ context.Context, e core.Event) {
	var line string
	switch e.Kind {
	case core.EventIteration:
		line = fmt.Sprintf("第 %d 輪思考中…", e.Iteration)
	case core.EventToolStarted:
		line = fmt.Sprintf("正在執行 %s", e.ToolName)
	case core.EventToolFinished:
		status := "失敗"
		if e.OK {
			status = "完成"
		}
		line = fmt.Sprintf("%s %s", e.ToolName, status)
	case core.EventToolRetrying:
		line = fmt.Sprintf("%s 失敗，第 %d 次重試", e.ToolName, e.Iteration)
	default:
		return
	}
	_, _ = fmt.Fprint(s.out, progressPrefix+line+"\n")
}
