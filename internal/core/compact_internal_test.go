// 上下文壓縮純函式的表格驅動測試（ticket #48）。
//
// 這一層與整合層分工明確：**這裡驗政策**（哪一條該被犧牲、犧牲成什麼形狀、預算怎麼
// 算），整合層驗「政策真的接上了 ReAct 循環、外部看得見」。票面最後一條 AC 要的
// 「壓縮邏輯可獨立表格驅動測試，與 turn 級截斷分屬兩層」指的就是本檔的存在。
//
// **斷言不比對標記常數本身**：那會變成拿實作驗自己——標記寫錯字測試照樣綠。這裡改
// 驗結構性質（開頭與結尾還在、長度收斂、原內容不再出現），措辭本身由整合層釘住，
// 理由與死循環守衛那票相同：那句話是給 LLM 讀的交付物。
package core

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// 測試用的預算。取 400 而不是生產預設的 100000：預設值下要造出溢出得寫十萬字的
// 測試資料，而政策與數字大小無關。400 除以 contextPerMessageDivisor 得到單條上限
// 100，容得下省略標記本身，邊界因此驗的是政策不是標記長度的巧合。
const testContextBudget = 400

// 一條 Tool 結果內容的可辨識頭尾。「保留開頭與結尾」這條性質要驗得到，內容就不能
// 是同一個字重複——那樣的話頭尾被掉包也看不出來。
const (
	contentHead = "開頭"
	contentTail = "結尾"
)

// markedContent 產生剛好 total 個 rune、帶可辨識頭尾的內容。
func markedContent(t *testing.T, total int) string {
	t.Helper()
	pad := total - utf8.RuneCountInString(contentHead) - utf8.RuneCountInString(contentTail)
	if pad < 0 {
		t.Fatalf("total = %d 太小，容不下頭尾標記", total)
	}
	return contentHead + strings.Repeat("·", pad) + contentTail
}

// sentRunes 是整個訊息序列送出去時佔的 rune 數。
//
// **刻意在測試裡自己算一遍**，不呼叫生產程式碼的 messageRunes：那樣會變成拿實作
// 驗自己，漏算某個欄位時兩邊一起錯、斷言照樣成立。
func sentRunes(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += utf8.RuneCountInString(m.Content) + utf8.RuneCountInString(m.ToolCallID)
		for _, c := range m.ToolCalls {
			n += utf8.RuneCountInString(c.ID) + utf8.RuneCountInString(c.Name) +
				utf8.RuneCountInString(c.Arguments)
		}
	}
	return n
}

// longCallID 產生第 n 次呼叫的 tool_call_id，長 60 rune。
//
// 60 不是誇張的假設：ID 由 Provider 產生，OryxOS 對它的長度沒有任何限制。這一格
// 要證的其實是**累積**——六次呼叫的 ID 加起來就有 360 rune，比內容本身還多。
func longCallID(t *testing.T, n int) string {
	t.Helper()
	id := fmt.Sprintf("call_%02d_%s", n, strings.Repeat("x", 52))
	if got := utf8.RuneCountInString(id); got != 60 {
		t.Fatalf("longCallID 長 %d rune, 期望 60", got)
	}
	return id
}

// bigWriteFileArgs 產生一份長 total rune 的 write_file 參數。
//
// 壓縮只數它的長度、不解析它，但寫成真實的 JSON 形狀讓讀的人看得出這個長度是怎麼
// 來的——write_file 要寫的檔案內容就住在 arguments 裡，它沒有上界。
func bigWriteFileArgs(t *testing.T, total int) string {
	t.Helper()
	const head, tail = `{"path":"notes/a.md","content":"`, `"}`
	pad := total - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)
	if pad < 0 {
		t.Fatalf("total = %d 太小，容不下 JSON 外框", total)
	}
	return head + strings.Repeat("內", pad) + tail
}

// toolMsg 產生一條帶 ToolCallID 的 tool 訊息。
func toolMsg(id, content string) Message {
	return Message{Role: RoleTool, Content: content, ToolCallID: id}
}

// wantShape 是一條訊息壓縮後**該長成的樣子**，不是它的字面內容。
type wantShape int

const (
	// shapeUnchanged：一個 byte 都不動。
	shapeUnchanged wantShape = iota
	// shapeElided：保護區內、單條過長——頭尾保留，中間標註省略量。
	shapeElided
	// shapeDropped：超出保護區——整條換成佔位說明，原內容不再出現。
	shapeDropped
)

func (s wantShape) String() string {
	switch s {
	case shapeUnchanged:
		return "原樣不動"
	case shapeElided:
		return "頭尾保留、中間省略"
	default:
		return "整條替換為佔位"
	}
}

// assertShape 驗一條訊息壓縮後符合預期形狀。
func assertShape(t *testing.T, idx int, want wantShape, before, after string) {
	t.Helper()
	switch want {
	case shapeUnchanged:
		if after != before {
			t.Errorf("messages[%d] 期望原樣不動，卻被改成 %q", idx, after)
		}
	case shapeElided:
		if after == before {
			t.Errorf("messages[%d] 期望被截斷，卻原樣不動（長 %d rune）", idx, utf8.RuneCountInString(before))
			return
		}
		// 開頭與結尾都還在，才叫「保留開頭與結尾」。少任何一邊都是另一種截斷。
		if !strings.HasPrefix(after, contentHead) {
			t.Errorf("messages[%d] 截斷後掉了開頭: %q", idx, after)
		}
		if !strings.HasSuffix(after, contentTail) {
			t.Errorf("messages[%d] 截斷後掉了結尾: %q", idx, after)
		}
		if n := utf8.RuneCountInString(after); n >= utf8.RuneCountInString(before) {
			t.Errorf("messages[%d] 截斷後 %d rune，沒有比原本的 %d rune 短",
				idx, n, utf8.RuneCountInString(before))
		}
	case shapeDropped:
		// 整條替換的判準是**原內容不再出現**。只驗「變短了」的話，一個把內容截掉
		// 一半的實作也會過——那是另一種降級，不是這一種。
		if strings.Contains(after, contentHead) || strings.Contains(after, contentTail) {
			t.Errorf("messages[%d] 期望整條被佔位說明取代，卻仍帶著原內容: %q", idx, after)
		}
		if after == "" {
			t.Errorf("messages[%d] 被換成空字串——佔位說明要能讓 LLM 知道這裡曾經有東西", idx)
		}
	}
}

// TestCompactToolResults 是壓縮政策的主表：同一支函式在五種輸入形狀下的決策。
func TestCompactToolResults(t *testing.T) {
	// 單條上限＝預算的四分之一。超過它的 Tool 結果即使在保護區內也要截斷。
	const perMessage = testContextBudget / contextPerMessageDivisor

	tests := []struct {
		name          string
		msgs          []Message
		want          []wantShape
		wantCompacted int
	}{
		{
			// 未超預算就一個 byte 都不該動——這是「既有 Profile 免遷移、行為與現在
			// 完全一致」在純函式這一層的樣子。
			name: "總量未超預算則完全不動",
			msgs: []Message{
				{Role: RoleSystem, Content: markedContent(t, 50)},
				{Role: RoleUser, Content: "請讀檔"},
				toolMsg("call_1", markedContent(t, 80)),
				toolMsg("call_2", markedContent(t, 80)),
			},
			want:          []wantShape{shapeUnchanged, shapeUnchanged, shapeUnchanged, shapeUnchanged},
			wantCompacted: 0,
		},
		{
			// 票面「久遠的 Tool 結果先被犧牲、最近的保留」的直接驗收。六條各 200 rune
			// 的結果配 400 的預算：由新到舊走，前四條各截到 100 就把預算用完，
			// 剩下兩條落在保護區外、整條換掉。
			name: "超出預算時久遠的先被犧牲",
			msgs: []Message{
				{Role: RoleUser, Content: "讀六個檔"},
				toolMsg("call_1", markedContent(t, 200)),
				toolMsg("call_2", markedContent(t, 200)),
				toolMsg("call_3", markedContent(t, 200)),
				toolMsg("call_4", markedContent(t, 200)),
				toolMsg("call_5", markedContent(t, 200)),
				toolMsg("call_6", markedContent(t, 200)),
			},
			want: []wantShape{
				shapeUnchanged,             // user 訊息不是壓縮對象
				shapeDropped, shapeDropped, // 最久遠的兩條
				shapeElided, shapeElided, shapeElided, shapeElided, // 保護區內的四條
			},
			wantCompacted: 6,
		},
		{
			// 總量沒超出預算時，**單條再長也不切**——整份請求都裝得下，切它換不到任何
			// 空間。這一格是「提前返回」那三行的實際行為內容：少了它，把提前返回拿掉
			// 的實作在其餘各格都還是綠的（其餘各格的單條長度本來就在上限之內）。
			name: "單條過長但總量未超預算則不動",
			msgs: []Message{
				{Role: RoleUser, Content: "讀一個大檔"},
				toolMsg("call_1", markedContent(t, perMessage*3)),
			},
			want:          []wantShape{shapeUnchanged, shapeUnchanged},
			wantCompacted: 0,
		},
		{
			// 邊界：總量**剛好等於**預算。預算是「不得超過」不是「不得達到」，這一格
			// 把那個等號釘住——把條件寫成嚴格小於的實作會在這裡轉紅。
			//
			// 194 這個數字是算出來的，不是挑的：400（預算）− 200（user）− 6（"call_1"
			// 這個 tool_call_id 的長度）。訊息佔多少要連 ID 一起算，見 messageRunes。
			name: "總量剛好等於預算則不動",
			msgs: []Message{
				{Role: RoleUser, Content: markedContent(t, 200)},
				toolMsg("call_1", markedContent(t, 194)),
			},
			want:          []wantShape{shapeUnchanged, shapeUnchanged},
			wantCompacted: 0,
		},
		{
			// 非 Tool 訊息**自己不動，但佔掉的空間要算數**。夾在兩條 Tool 結果中間的
			// 一段長使用者訊息，會讓更久遠的那一條落到保護區外。
			//
			// 少了這一格的話，一個「非 Tool 訊息直接跳過、連帳都不記」的實作也會全綠
			// ——上面幾格的 user 訊息都排在最舊端，由新到舊走時它最後才被算到，記不記
			// 帳都不影響任何一條 Tool 結果的去留。
			name: "非 Tool 訊息佔掉的空間算進預算",
			msgs: []Message{
				toolMsg("call_1", markedContent(t, 100)),
				{Role: RoleUser, Content: markedContent(t, testContextBudget)},
				toolMsg("call_2", markedContent(t, 100)),
			},
			want:          []wantShape{shapeDropped, shapeUnchanged, shapeUnchanged},
			wantCompacted: 1,
		},
		{
			// tool 訊息的 **tool_call_id** 一樣改不得、一樣被送出去（見 internal/provider
			// 的 toOpenAIMessages），所以一樣要算進預算。ID 由 Provider 產生，OryxOS
			// 不限制它的長度，六次呼叫累積起來就是一筆可觀的量。
			//
			// 只算 Content 的話這個序列帳面上是 360 rune、低於預算 400，壓縮完全不啟動
			// ——而實際送出去的是 720 rune。這一格守的就是那個差額。
			name: "累積的 tool_call_id 佔掉的空間算進預算",
			msgs: []Message{
				toolMsg(longCallID(t, 1), markedContent(t, 60)),
				toolMsg(longCallID(t, 2), markedContent(t, 60)),
				toolMsg(longCallID(t, 3), markedContent(t, 60)),
				toolMsg(longCallID(t, 4), markedContent(t, 60)),
				toolMsg(longCallID(t, 5), markedContent(t, 60)),
				toolMsg(longCallID(t, 6), markedContent(t, 60)),
			},
			want: []wantShape{
				shapeDropped, shapeDropped, // 最久遠的兩條落在保護區外
				shapeUnchanged, shapeUnchanged, shapeUnchanged, shapeUnchanged,
			},
			wantCompacted: 2,
		},
		{
			// Tool 呼叫的 arguments **改不得，但照樣被送出去**，所以要算進預算。
			// 這裡的 assistant 帶著一份 400 rune 的 write_file 參數：光是它就把預算
			// 吃掉大半，最久遠的那條 Tool 結果因此落到保護區外。
			//
			// 只算 Content 的話這個序列帳面上只有 200 rune、遠低於預算 400，壓縮完全
			// 不啟動——而實際送出去的是 616 rune。這一格就是為了守住那個差額。
			name: "大型 Tool 呼叫參數佔掉的空間算進預算",
			msgs: []Message{
				toolMsg("call_1", markedContent(t, 100)),
				{Role: RoleAssistant, ToolCalls: []ToolCall{
					{ID: "call_2", Name: "write_file", Arguments: bigWriteFileArgs(t, 400)},
				}},
				toolMsg("call_2", markedContent(t, 100)),
			},
			want:          []wantShape{shapeDropped, shapeUnchanged, shapeUnchanged},
			wantCompacted: 1,
		},
		{
			// 系統提示詞不動、user 與 assistant 也不動——票面只授權壓 Tool 結果。
			// 這三條加起來遠超預算，壓縮仍然只能眼睜睜看著。
			name: "非 Tool 角色一律不動",
			msgs: []Message{
				{Role: RoleSystem, Content: markedContent(t, 300)},
				{Role: RoleUser, Content: markedContent(t, 300)},
				{Role: RoleAssistant, Content: markedContent(t, 300)},
			},
			want:          []wantShape{shapeUnchanged, shapeUnchanged, shapeUnchanged},
			wantCompacted: 0,
		},
		{
			// 保護區內、但單條就超過單條上限：頭尾保留。
			//
			// **序列總量必須先超出預算**，單條上限才輪得到出場——整份請求都裝得下時
			// 切它換不到任何空間，那是 AC「未超出預算時完全不改動」管的範圍。所以
			// 這裡用一條長 user 訊息把總量頂上去：它自己不是壓縮對象，卻讓壓縮啟動。
			name: "保護區內單條過長者頭尾保留",
			msgs: []Message{
				{Role: RoleUser, Content: markedContent(t, testContextBudget)},
				toolMsg("call_1", markedContent(t, perMessage*3)),
			},
			want:          []wantShape{shapeUnchanged, shapeElided},
			wantCompacted: 1,
		},
		{
			// 總量溢出、但這一條 Tool 結果自己沒超過單條上限：它在保護區內，原樣留著。
			// 少了這格的話，一個「溢出就把所有 Tool 結果都截一遍」的實作也會全綠。
			name: "保護區內未過長者原樣保留",
			msgs: []Message{
				{Role: RoleUser, Content: markedContent(t, testContextBudget)},
				toolMsg("call_1", markedContent(t, perMessage/2)),
			},
			want:          []wantShape{shapeUnchanged, shapeUnchanged},
			wantCompacted: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := make([]string, len(tt.msgs))
			for i, m := range tt.msgs {
				before[i] = m.Content
			}

			got, compacted := compactToolResults(tt.msgs, testContextBudget)

			if compacted != tt.wantCompacted {
				t.Errorf("compacted = %d, 期望 %d", compacted, tt.wantCompacted)
			}
			if len(got) != len(tt.msgs) {
				t.Fatalf("訊息數 = %d, 期望 %d——壓縮不得改變訊息的數量", len(got), len(tt.msgs))
			}
			// 每一格都要成立的不變式：**壓縮不得把請求撐大**。這是這支函式存在的
			// 全部理由，違反它的話再漂亮的降級策略都是負收益。
			if after, before := sentRunes(got), sentRunes(tt.msgs); after > before {
				t.Errorf("壓縮後送出 %d rune，比壓縮前的 %d 還多——壓縮把請求撐大了", after, before)
			}
			for i, want := range tt.want {
				assertShape(t, i, want, before[i], got[i].Content)
			}
		})
	}
}

// TestCompactToolResultsNeverGrowsOnTinyBudget 守住極小預算下的收口。
//
// 佔位說明有四十幾個 rune、省略標記有十幾個。預算小到讓它們比原內容還長時，
// 「換上去」就變成把請求撐大——與壓縮的目的正好相反。
//
// **這些預算都是合法設定**：Profile 寫 `max_context_runes: 20` 不會被任何校驗擋下，
// 零值才走預設。所以這不是防禦性的假設，是真的到得了的狀態。
func TestCompactToolResultsNeverGrowsOnTinyBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		msgs   []Message
	}{
		{
			// 佔位說明有四十幾個 rune。最久遠那條只有一個字、又落在保護區外，
			// 無條件替換會讓請求變長。
			name:   "佔位說明比原內容長時不替換",
			budget: 20,
			msgs: []Message{
				toolMsg("call_1", "短"),
				{Role: RoleUser, Content: markedContent(t, 30)},
				toolMsg("call_2", "尾"),
			},
		},
		{
			// 省略標記有十幾個 rune。單條上限是預算的四分之一＝5，標記自己就超過它，
			// 硬換上去會比原本的 15 rune 還長。
			name:   "省略標記比原內容長時不截斷",
			budget: 20,
			msgs: []Message{
				{Role: RoleUser, Content: "問"},
				toolMsg("call_1", markedContent(t, 15)),
				toolMsg("call_2", markedContent(t, 15)),
			},
		},
		{
			name:   "預算為零",
			budget: 0,
			msgs: []Message{
				{Role: RoleUser, Content: "問"},
				toolMsg("call_1", "短"),
				toolMsg("call_2", "尾"),
			},
		},
		{
			// 反向的對照：預算一樣極小，但內容夠長，替換確實划算——這一格該真的壓。
			// 少了它，一個「預算太小就整個放棄」的實作也會過上面三格。
			name:   "預算極小但內容夠長時照壓",
			budget: 1,
			msgs: []Message{
				{Role: RoleUser, Content: "問"},
				toolMsg("call_1", markedContent(t, 200)),
				toolMsg("call_2", markedContent(t, 200)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := sentRunes(tt.msgs)
			if before <= tt.budget {
				t.Fatalf("測試資料沒有超出預算（%d ≤ %d），這一格根本走不進壓縮", before, tt.budget)
			}

			got, compacted := compactToolResults(tt.msgs, tt.budget)

			if after := sentRunes(got); after > before {
				t.Errorf("壓縮後送出 %d rune，比壓縮前的 %d 還多——壓縮把請求撐大了", after, before)
			}
			// 回報的降級量要與事實相符：沒有真的變短就不算壓縮過。它會落進警告
			// 日誌，報一個沒發生的降級比不報更糟。
			changed := 0
			for i, m := range got {
				if m.Content != tt.msgs[i].Content {
					changed++
				}
			}
			if compacted != changed {
				t.Errorf("compacted = %d，實際被改動的是 %d 條", compacted, changed)
			}
		})
	}
}

// TestCompactToolResultsUnderBudgetReturnsSameSlice 釘住「未超出預算時完全不改動
// 訊息，逐條位元組相等」。
//
// 與主表那一格分開是因為斷言對象不同：那格驗每條內容不變，這裡驗**整個序列連同
// 每個欄位**都不變——一個「順手把 ToolCallID 正規化一下」的實作只有這裡抓得到。
func TestCompactToolResultsUnderBudgetReturnsSameSlice(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "你是 Oryx。"},
		{Role: RoleUser, Content: "讀檔"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{"path":"a.md"}`}}},
		toolMsg("call_1", markedContent(t, 100)),
	}
	want := make([]Message, len(msgs))
	copy(want, msgs)

	got, compacted := compactToolResults(msgs, testContextBudget)

	if compacted != 0 {
		t.Errorf("compacted = %d, 期望 0", compacted)
	}
	for i := range want {
		if got[i].Content != want[i].Content || got[i].Role != want[i].Role ||
			got[i].ToolCallID != want[i].ToolCallID || len(got[i].ToolCalls) != len(want[i].ToolCalls) {
			t.Errorf("messages[%d] = %+v, 期望逐欄位相等於 %+v", i, got[i], want[i])
		}
	}
}

// TestCompactToolResultsKeepsToolCallFields 釘住票面三條不變式的第二條：被壓的只有
// 內容欄位，Tool 呼叫欄位是模型行動的證據，動了就是竄改歷史。
//
// 順序與角色一起在這裡驗：三條不變式共用同一次執行才對得起來，分三支測試會讓
// 「數量對、順序卻錯」這種組合從縫隙漏掉。
func TestCompactToolResultsKeepsToolCallFields(t *testing.T) {
	calls := []ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{"path":"big.md"}`}}
	msgs := []Message{
		{Role: RoleSystem, Content: markedContent(t, 50)},
		{Role: RoleUser, Content: "讀檔"},
		{Role: RoleAssistant, ToolCalls: calls},
		toolMsg("call_1", markedContent(t, testContextBudget*3)),
	}

	got, compacted := compactToolResults(msgs, testContextBudget)

	if compacted == 0 {
		t.Fatalf("這組輸入本該觸發壓縮，compacted = 0——後面的不變式因此驗不到東西")
	}
	if len(got) != len(msgs) {
		t.Fatalf("訊息數 = %d, 期望 %d", len(got), len(msgs))
	}
	wantRoles := []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Errorf("messages[%d].Role = %s, 期望 %s——壓縮不得改變訊息的順序", i, got[i].Role, want)
		}
	}
	if got[0].Content != msgs[0].Content {
		t.Errorf("系統提示詞被改動了: %q", got[0].Content)
	}
	if len(got[2].ToolCalls) != 1 {
		t.Fatalf("assistant 的 tool_calls 被動了: %+v", got[2].ToolCalls)
	}
	if c := got[2].ToolCalls[0]; c.ID != "call_1" || c.Name != "read_file" || c.Arguments != `{"path":"big.md"}` {
		t.Errorf("tool_calls[0] = %+v，期望逐欄位不變——那是模型行動的證據", c)
	}
	if got[3].ToolCallID != "call_1" {
		t.Errorf("tool 訊息的 ToolCallID = %q, 期望 call_1——它斷了就配不成對", got[3].ToolCallID)
	}
}

// TestCompactToolResultsDoesNotMutateInput 釘住壓縮是純函式：呼叫端傳進來的那份
// 不受影響。
//
// 這不是潔癖。`buildMessages` 組出來的序列**共用 Session 對話歷史的字串**，就地改
// 會讓壓縮結果寫回持久化的歷史——恢復的 Session 就再也重放不出原樣了（spec #5
// 使用者故事 20 對死循環守衛的同一條要求）。
func TestCompactToolResultsDoesNotMutateInput(t *testing.T) {
	original := markedContent(t, testContextBudget*3)
	msgs := []Message{
		{Role: RoleUser, Content: "讀檔"},
		toolMsg("call_1", original),
	}

	if _, compacted := compactToolResults(msgs, testContextBudget); compacted == 0 {
		t.Fatalf("這組輸入本該觸發壓縮，compacted = 0")
	}

	if msgs[1].Content != original {
		t.Errorf("輸入被就地改動了: 長度 %d → %d",
			utf8.RuneCountInString(original), utf8.RuneCountInString(msgs[1].Content))
	}
}

// TestCompactToolResultsCountsRunesNotBytes 是票面「中英文對稱」那條 AC。
//
// 兩組內容 rune 數完全相同、只差在語言，壓縮的決策必須一致。改回按 byte 計量的話
// 中文那組會是英文的三倍長而提早觸發，這支測試立刻轉紅——它就是為了守住這件事。
func TestCompactToolResultsCountsRunesNotBytes(t *testing.T) {
	// 每條 150 rune：**高於**單條上限 100，但兩條合計 300 仍低於預算 400，兩組都不該
	// 壓縮。單條刻意做得比上限長是關鍵——這樣一旦總量被誤判成溢出，這兩條就會真的
	// 被截斷、決策差異才浮得出來；每條都短於上限的話，誤判之後照樣什麼都沒發生。
	const perLang = 150
	chinese := strings.Repeat("中", perLang)
	english := strings.Repeat("a", perLang)

	if utf8.RuneCountInString(chinese) != utf8.RuneCountInString(english) {
		t.Fatalf("測試資料本身不對稱: %d vs %d",
			utf8.RuneCountInString(chinese), utf8.RuneCountInString(english))
	}
	if len(chinese) == len(english) {
		t.Fatalf("測試資料的 byte 數相同，這支測試就分不出兩種計量方式了")
	}

	build := func(content string) []Message {
		return []Message{
			{Role: RoleUser, Content: "讀兩個檔"},
			toolMsg("call_1", content),
			toolMsg("call_2", content),
		}
	}

	_, zhCompacted := compactToolResults(build(chinese), testContextBudget)
	_, enCompacted := compactToolResults(build(english), testContextBudget)

	if zhCompacted != enCompacted {
		t.Errorf("中文壓了 %d 條、英文壓了 %d 條——預算若按 rune 計，等量語意的兩組決策必須一致",
			zhCompacted, enCompacted)
	}
	if zhCompacted != 0 {
		t.Errorf("中文組被壓了 %d 條，但它按 rune 計並未超出預算 %d", zhCompacted, testContextBudget)
	}
}

// TestElideMiddle 驗單條截斷本身：頭尾保留、中間標註省略量、結果不超過上限。
func TestElideMiddle(t *testing.T) {
	const limit = 100

	tests := []struct {
		name    string
		total   int
		wantCut bool
	}{
		{name: "短於上限則原樣回傳", total: limit / 2, wantCut: false},
		{name: "剛好等於上限則原樣回傳", total: limit, wantCut: false},
		{name: "超過上限則頭尾保留", total: limit * 5, wantCut: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := markedContent(t, tt.total)

			got := elideMiddle(in, limit)

			if !tt.wantCut {
				if got != in {
					t.Errorf("期望原樣回傳，卻被改成 %q", got)
				}
				return
			}
			if !strings.HasPrefix(got, contentHead) {
				t.Errorf("掉了開頭: %q", got)
			}
			if !strings.HasSuffix(got, contentTail) {
				t.Errorf("掉了結尾: %q", got)
			}
			// 標記本身算進預算（沿 internal/config 的 truncateHead 判準）：不這樣算
			// 的話回傳長度會是「上限＋標記」，宣稱的上限守不住。
			if n := utf8.RuneCountInString(got); n > limit {
				t.Errorf("截斷後 %d rune，超過上限 %d——標記沒有算進預算", n, limit)
			}
			// 省略量要說出來。LLM 得知道自己看到的是殘缺內容，才不會據此下結論。
			if !strings.ContainsAny(got, "0123456789") {
				t.Errorf("截斷標記沒有標出省略量: %q", got)
			}
		})
	}
}
