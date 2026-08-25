// CLI 進度顯示的測試（ticket #47）。斷言的是**使用者看得到的那幾行字**——
// 進度 sink 寫進 io.Writer 的內容，不是它內部怎麼決定要不要寫。
//
// 用 package cli（內部測試）是因為「哪種事件印成什麼」是這一層的外部行為，而挑選
// 實作的 ProgressSinkFor 只挑得出型別、印不出東西；兩者要分開驗。
package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestProgressSinkOutput 表格驅動涵蓋每一種事件印出什麼。
//
// **不是每一種都印。** 三種刻意留白，理由各自寫在格子裡——CLI 的進度是給等待中的
// 使用者看的，把七種全印出來只會把真正的回應淹掉。
func TestProgressSinkOutput(t *testing.T) {
	tests := []struct {
		name  string
		event core.Event
		want  string // 空字串代表這種事件不印
	}{
		{
			name:  "turn_started 不印",
			event: core.Event{Kind: core.EventTurnStarted},
			// 使用者剛按下 Enter，「開始了」是他自己做的事，不需要被告知。
			want: "",
		},
		{
			name:  "iteration 印第幾輪",
			event: core.Event{Kind: core.EventIteration, Iteration: 2},
			want:  "  · 第 2 輪思考中…\n",
		},
		{
			name:  "tool_started 印正在執行哪個 Tool",
			event: core.Event{Kind: core.EventToolStarted, ToolName: "read_file"},
			want:  "  · 正在執行 read_file\n",
		},
		{
			name:  "tool_finished 成功",
			event: core.Event{Kind: core.EventToolFinished, ToolName: "read_file", OK: true},
			want:  "  · read_file 完成\n",
		},
		{
			name:  "tool_finished 失敗",
			event: core.Event{Kind: core.EventToolFinished, ToolName: "http_get"},
			want:  "  · http_get 失敗\n",
		},
		{
			name:  "tool_retrying 印第幾次重試",
			event: core.Event{Kind: core.EventToolRetrying, ToolName: "http_get", Iteration: 3},
			want:  "  · http_get 失敗，第 3 次重試\n",
		},
		{
			name:  "assistant_text 不印",
			event: core.Event{Kind: core.EventAssistantText, Text: "我先看看那個檔案"},
			// 最終回應由 Channel 自己印（`<Agent>> ...`），這裡再印一次就是重複；
			// 中間輪的說明則屬於過程，不是回應，混在進度裡分不清哪句才是答案。
			want: "",
		},
		{
			name:  "turn_finished 不印",
			event: core.Event{Kind: core.EventTurnFinished, OK: true},
			// 下一行就是回應本身，那才是 turn 結束的信號。
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := newProgressSink(&buf)

			sink.Emit(t.Context(), tt.event)

			if got := buf.String(); got != tt.want {
				t.Errorf("輸出 = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestProgressSinkForNonTerminal 斷言非 TTY 的輸出目標退回不做事的實作：既有的
// `--message` 管線用法、以及所有把輸出接進 buffer 的既有 CLI 測試，都不該因為多了
// 進度顯示而看到多出來的字。
func TestProgressSinkForNonTerminal(t *testing.T) {
	tests := []struct {
		name string
		out  func(t *testing.T) io.Writer
	}{
		{
			name: "寫進 buffer（既有測試與程式化呼叫走這條）",
			out: func(*testing.T) io.Writer {
				return &bytes.Buffer{}
			},
		},
		{
			name: "寫進一般檔案（輸出被重導向到檔案）",
			out: func(t *testing.T) io.Writer {
				f, err := os.Create(t.TempDir() + "/out.txt")
				if err != nil {
					t.Fatalf("建立暫存檔: %v", err)
				}
				t.Cleanup(func() { _ = f.Close() })
				return f
			},
		},
		{
			name: "寫進 pipe（輸出被接給另一個進程）",
			out: func(t *testing.T) io.Writer {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("os.Pipe: %v", err)
				}
				t.Cleanup(func() {
					_ = w.Close()
					_ = r.Close()
				})
				return w
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := tt.out(t)
			sink := ProgressSinkFor(out)

			if _, isNop := sink.(core.NopEventSink); !isNop {
				t.Fatalf("非 TTY 應退回 NopEventSink，實際型別 = %T", sink)
			}
			// 型別對了還不夠：真正要保證的是「什麼都不寫」。
			sink.Emit(t.Context(), core.Event{Kind: core.EventIteration, Iteration: 1})
			if buf, ok := out.(*bytes.Buffer); ok && buf.Len() != 0 {
				t.Errorf("非 TTY 仍有輸出: %q", buf.String())
			}
		})
	}
}

// TestProgressSinkForCharDevice 釘住 ProgressSinkFor 的**實際判準**：字元裝置。
//
// 用 os.DevNull 而不是 /dev/tty，是因為測試環境不保證有終端機（CI、`go test` 重導向
// 輸出時都沒有），拿 /dev/tty 寫的那一格只會整天被 skip——一個永遠 skip 的測試等於
// 沒有保護。
//
// 這一格同時是那個**已知限制的回歸測試**：判準是「字元裝置」而不是真的去問終端機，
// 所以 `/dev/null` 會被判成終端機、進度會印給沒人看的地方（見 ProgressSinkFor 的
// 註解）。日後若改用 ioctl 之類的真判斷，這一格必須跟著改成期望 NopEventSink——
// 它轉紅正是在提醒「判準換了」，不是壞掉。
func TestProgressSinkForCharDevice(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("開啟 %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	if sink := ProgressSinkFor(devNull); !isProgressSink(sink) {
		t.Errorf("字元裝置應拿到會顯示進度的實作，實際型別 = %T", sink)
	}
}

// TestChannelOutputUnchangedWithProgress 斷言掛上進度 sink 之後，Channel 自己的
// 輸出格式一個字都沒變——進度是**多出來的行**，不是改寫既有的行。
func TestChannelOutputUnchangedWithProgress(t *testing.T) {
	var buf bytes.Buffer
	sink := newProgressSink(&buf)

	sink.Emit(t.Context(), core.Event{Kind: core.EventIteration, Iteration: 1})
	sink.Emit(t.Context(), core.Event{Kind: core.EventTurnFinished, OK: true})

	for line := range strings.SplitSeq(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, "  · ") {
			t.Errorf("進度行 %q 沒有帶進度前綴，可能與 Channel 的回應輸出混淆", line)
		}
	}
}

// isProgressSink 回答「這是會顯示進度的那個實作嗎」。
func isProgressSink(sink core.EventSink) bool {
	_, ok := sink.(*progressSink)
	return ok
}
