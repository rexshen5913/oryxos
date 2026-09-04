package eval_test

import (
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// TestGrade 是本票的主要測試面：判卷吃「用例宣告 ＋ 一次執行的結果摘要」，吐「通過
// 與否 ＋ 未通過原因」，**完全不碰 Provider**。
//
// **三種布林斷言**（reply_contains、reply_not_contains、tool_called）各自涵蓋通過與
// 失敗；兩種指標型斷言在 TestGradeMetricAssertions。斷言對象是判卷的回傳值，不是它
// 內部怎麼比對——把 strings.Contains 換成別的比法而語義不變時，這張表該保持綠色。
//
// **加斷言種類時這段敘述要一起改。** 它宣告了這張表涵蓋多少種，落後時看不出缺口。
//
// reply_not_contains 落地過程中，同類的數量敘述漂移過的位置有：01-reply-only.yaml 的
// schema 列表與分類段、case.go 的白名單註解、case_test.go 的涵蓋表名稱、implement.md
// 的範圍與驗收條件，以及這裡——外部審查逐輪抓出來的。
//
// **這份清單刻意不寫總數。** 上一版寫了「四個地方」，而括號裡實際列了五個——一個
// 統計自己漂移的敘述又漂移了一次。可變的計數只要寫死就會過期，能列舉時就列舉。
func TestGrade(t *testing.T) {
	tests := []struct {
		name        string
		asserts     []eval.Assertion
		result      eval.RunResult
		wantPassed  bool
		wantReasons []string // 依序，每個未通過原因裡都該出現的片段
	}{
		{
			name:       "reply_contains 命中",
			asserts:    []eval.Assertion{{Kind: eval.AssertReplyContains, Value: "牛奶"}},
			result:     eval.RunResult{Reply: "你的待辦是買牛奶。"},
			wantPassed: true,
		},
		{
			name:        "reply_contains 沒命中",
			asserts:     []eval.Assertion{{Kind: eval.AssertReplyContains, Value: "牛奶"}},
			result:      eval.RunResult{Reply: "我不知道。"},
			wantPassed:  false,
			wantReasons: []string{"牛奶"},
		},
		{
			name:       "tool_called 命中",
			asserts:    []eval.Assertion{{Kind: eval.AssertToolCalled, Value: "read_file"}},
			result:     eval.RunResult{ToolsCalled: []string{"list_dir", "read_file"}},
			wantPassed: true,
		},
		{
			name:        "tool_called 沒命中",
			asserts:     []eval.Assertion{{Kind: eval.AssertToolCalled, Value: "read_file"}},
			result:      eval.RunResult{ToolsCalled: []string{"list_dir"}},
			wantPassed:  false,
			wantReasons: []string{"read_file"},
		},
		{
			name:        "tool_called 在一個 Tool 都沒呼叫時失敗",
			asserts:     []eval.Assertion{{Kind: eval.AssertToolCalled, Value: "read_file"}},
			result:      eval.RunResult{},
			wantPassed:  false,
			wantReasons: []string{"read_file"},
		},
		{
			name: "全部斷言都通過才算通過",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyContains, Value: "牛奶"},
				{Kind: eval.AssertToolCalled, Value: "read_file"},
			},
			result:     eval.RunResult{Reply: "買牛奶", ToolsCalled: []string{"read_file"}},
			wantPassed: true,
		},
		{
			name: "一條失敗就不通過，且只列失敗的那條",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyContains, Value: "牛奶"},
				{Kind: eval.AssertToolCalled, Value: "read_file"},
			},
			result:      eval.RunResult{Reply: "買牛奶", ToolsCalled: []string{"list_dir"}},
			wantPassed:  false,
			wantReasons: []string{"read_file"},
		},
		{
			name: "多條失敗要全部列出，不是遇到第一條就停",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyContains, Value: "牛奶"},
				{Kind: eval.AssertToolCalled, Value: "read_file"},
			},
			result:      eval.RunResult{Reply: "我不知道。"},
			wantPassed:  false,
			wantReasons: []string{"牛奶", "read_file"},
		},
		{
			name:       "比對是子字串，不是完全相等",
			asserts:    []eval.Assertion{{Kind: eval.AssertReplyContains, Value: "牛奶"}},
			result:     eval.RunResult{Reply: "牛奶"},
			wantPassed: true,
		},
		{
			name:        "Tool 名比對是完全相等，不是前綴",
			asserts:     []eval.Assertion{{Kind: eval.AssertToolCalled, Value: "read"}},
			result:      eval.RunResult{ToolsCalled: []string{"read_file"}},
			wantPassed:  false,
			wantReasons: []string{"read"},
		},

		// ── reply_not_contains（issue #58 的量測缺口）──
		//
		// 它是 reply_contains 的反面，但兩者的邊界不對稱，所以各自都要有格：同一個
		// 空回應，reply_contains 必定失敗、reply_not_contains 必定通過。
		{
			name:       "reply_not_contains 沒出現禁語時通過",
			asserts:    []eval.Assertion{{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"}},
			result:     eval.RunResult{Reply: "config.yaml 不在 file.allowed_paths 白名單中。"},
			wantPassed: true,
		},
		{
			name:        "reply_not_contains 出現禁語時不通過",
			asserts:     []eval.Assertion{{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"}},
			result:      eval.RunResult{Reply: "白名單沒有允許任何路徑。"},
			wantPassed:  false,
			wantReasons: []string{"沒有允許任何"},
		},
		{
			// **空回應讓這條斷言通過，這是對的，但要寫下來。** 一個什麼都沒說的回應
			// 確實沒說禁語，可是它本身正是 issue #60 那個缺陷的形態。這條斷言不負責
			// 抓空回應，別讓後來的人以為它涵蓋了。
			name:       "reply_not_contains 對空回應通過（它不負責抓空回應）",
			asserts:    []eval.Assertion{{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"}},
			result:     eval.RunResult{Reply: ""},
			wantPassed: true,
		},
		{
			name: "多條 reply_not_contains 各自獨立判，命中幾條就列幾條",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyNotContains, Value: "沒有設定任何"},
				{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"},
			},
			result:      eval.RunResult{Reply: "白名單沒有允許任何路徑。"},
			wantPassed:  false,
			wantReasons: []string{"沒有允許任何"},
		},
		{
			// 與 reply_contains 同一條理由：比的是子字串而非整段相等。禁語是一句話
			// 裡的片語，本來就不會單獨構成整個回應。
			name:        "reply_not_contains 比對是子字串，不是完全相等",
			asserts:     []eval.Assertion{{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"}},
			result:      eval.RunResult{Reply: "很抱歉，白名單沒有允許任何路徑，因此無法讀取。"},
			wantPassed:  false,
			wantReasons: []string{"沒有允許任何"},
		},
		{
			name: "reply_contains 與 reply_not_contains 併用：說了該說的、也沒說禁語",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyContains, Value: "file.allowed_paths"},
				{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"},
			},
			result:     eval.RunResult{Reply: "config.yaml 不在 file.allowed_paths 白名單中。"},
			wantPassed: true,
		},
		{
			// **這一格是 issue #58 的實際形態，也是新增這種斷言的理由。**
			//
			// 謊稱句本身就滿足 reply_contains（它確實提到了 file.allowed_paths），
			// 所以在只有既有四種斷言的世界裡，這段回應是綠的——2026-09-04 的 A／B
			// 取樣量到兩組各 3 輪「判卷通過但同時在謊稱」。擋得下它的只有這一種。
			name: "issue #58 形態：謊稱句滿足 reply_contains，要靠 reply_not_contains 才擋得下",
			asserts: []eval.Assertion{
				{Kind: eval.AssertReplyContains, Value: "file.allowed_paths"},
				{Kind: eval.AssertReplyNotContains, Value: "沒有允許任何"},
			},
			result:      eval.RunResult{Reply: "file.allowed_paths 白名單沒有允許任何路徑。"},
			wantPassed:  false,
			wantReasons: []string{"沒有允許任何"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.Grade(eval.Case{Name: "測試用例", Assert: tt.asserts}, tt.result)
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %v，期望 %v（原因: %v）", got.Passed, tt.wantPassed, got.Failures)
			}
			if len(got.Failures) != len(tt.wantReasons) {
				t.Fatalf("未通過原因數 = %d %v，期望 %d", len(got.Failures), got.Failures, len(tt.wantReasons))
			}
			for i, want := range tt.wantReasons {
				if !strings.Contains(got.Failures[i], want) {
					t.Errorf("Failures[%d] = %q，期望含 %q", i, got.Failures[i], want)
				}
			}
			// 通過時不得帶任何原因：呼叫端會直接把 Failures 印出來，一條殘留的
			// 「原因」會讓一個通過的用例看起來像失敗。
			if got.Passed && len(got.Failures) != 0 {
				t.Errorf("通過卻帶了未通過原因: %v", got.Failures)
			}
		})
	}
}

// TestGradeEmptyValueIsVacuous 從判卷這一側證明「value 是空字串」為什麼非在解析層
// 擋下不可：strings.Contains 對空字串恆為真，於是 reply_contains **恆過**、
// reply_not_contains **恆敗**——兩條都與回應內容完全無關，而那正是「等於沒測」。
//
// **這支的存在理由是診斷訊息不能只講一個方向**（Codex 審查抓到）。原本的訊息寫
// 「純空白的斷言幾乎對任何回應都成立」，那對 reply_contains 為真、對
// reply_not_contains 恰好相反；加第三種斷言時若照著同一個方向再寫一次，訊息會第二
// 次講反。表格裡三段回應差異極大（空的、謊稱的、準確的），判卷結果卻一模一樣，
// 這就是「與內容無關」最直接的證據。
//
// 與 case_test.go 那三格空白案例是一組：那邊釘「被擋掉了」，這邊釘「為什麼非擋不可」。分開
// 寫的理由是問的問題不同——解析層的規則哪天放寬了，這裡量到的語義也不會變。
func TestGradeEmptyValueIsVacuous(t *testing.T) {
	replies := []struct {
		name  string
		reply string
	}{
		{"空回應", ""},
		{"謊稱句", "file.allowed_paths 白名單沒有允許任何路徑。"},
		{"準確句", "config.yaml 不在 file.allowed_paths 白名單中。"},
	}
	for _, r := range replies {
		t.Run(r.name, func(t *testing.T) {
			result := eval.RunResult{Reply: r.reply}

			pos := eval.Grade(eval.Case{
				Name:   "測試用例",
				Assert: []eval.Assertion{{Kind: eval.AssertReplyContains, Value: ""}},
			}, result)
			if !pos.Passed {
				t.Errorf("reply_contains 配空字串應恆過，卻不通過: %v", pos.Failures)
			}

			neg := eval.Grade(eval.Case{
				Name:   "測試用例",
				Assert: []eval.Assertion{{Kind: eval.AssertReplyNotContains, Value: ""}},
			}, result)
			if neg.Passed {
				t.Error("reply_not_contains 配空字串應恆敗，卻通過了")
			}
		})
	}
}

// TestGradeFailureNamesActualTools 釘住 tool_called 失敗訊息的可用性：只說「期望
// read_file 卻沒有」，看的人不知道 Agent 到底呼叫了什麼，得自己去翻審計表。
func TestGradeFailureNamesActualTools(t *testing.T) {
	got := eval.Grade(
		eval.Case{Name: "x", Assert: []eval.Assertion{{Kind: eval.AssertToolCalled, Value: "read_file"}}},
		eval.RunResult{ToolsCalled: []string{"list_dir", "shell"}},
	)
	if got.Passed {
		t.Fatal("期望不通過")
	}
	for _, actual := range []string{"list_dir", "shell"} {
		if !strings.Contains(got.Failures[0], actual) {
			t.Errorf("未通過原因 = %q，期望列出實際呼叫過的 %q", got.Failures[0], actual)
		}
	}
}

// TestGradeUnknownAssertionKindFails 是一道**後衛**。
//
// 未知的斷言種類在解析層就會被擋下（見 TestParseCase），所以正常流程走不到這裡。
// 但判卷是匯出的純函式，日後加斷言種類時會直接被呼叫——那時若有人只改了解析層的
// 白名單而忘了在判卷加分支，一條沒人認得的斷言**安靜地通過**是最糟的結果：評測會
// 宣稱一個它根本沒檢查的性質成立。
//
// 哨兵名從 `max_iterations` 換成一個**永遠不會成真**的字串（ticket #53）：本票把
// 指標型斷言落地了，繼續拿它當「未知種類」的話，這道後衛會在解析層與判卷層都認得
// 它之後安靜地失去意義——測試照樣綠，守的東西卻沒了。
func TestGradeUnknownAssertionKindFails(t *testing.T) {
	got := eval.Grade(
		eval.Case{Name: "x", Assert: []eval.Assertion{{Kind: "no_such_kind", Value: "3"}}},
		eval.RunResult{Reply: "任何回應"},
	)
	if got.Passed {
		t.Fatal("未知的斷言種類不得判成通過")
	}
	if len(got.Failures) != 1 || !strings.Contains(got.Failures[0], "no_such_kind") {
		t.Errorf("未通過原因 = %v，期望指出是哪一種斷言不認得", got.Failures)
	}
}

// TestGradeMetricAssertions 是兩種指標型斷言的表格（ticket #53 驗收條件）。
//
// **每一種都涵蓋通過、剛好等於上限、超出三格。** 中間那格是重點：上限是「不得超過」
// 而不是「必須小於」，把 <= 寫成 < 的話，一條 max_iterations: 3 的用例會在跑滿 3 次
// 時判成退步——而 3 正是它宣告可以接受的數字。邊界寫錯的評測會製造假的回歸警報，
// 那比沒有指標更消耗人。
func TestGradeMetricAssertions(t *testing.T) {
	tests := []struct {
		name       string
		assertion  eval.Assertion
		result     eval.RunResult
		wantPassed bool
		wantReason []string // 未通過時原因裡該出現的片段
	}{
		{
			name:       "max_iterations 低於上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxIterations, Value: "3"},
			result:     eval.RunResult{Iterations: 1},
			wantPassed: true,
		},
		{
			name:       "max_iterations 剛好等於上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxIterations, Value: "3"},
			result:     eval.RunResult{Iterations: 3},
			wantPassed: true,
		},
		{
			name:       "max_iterations 超出上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxIterations, Value: "3"},
			result:     eval.RunResult{Iterations: 4},
			wantPassed: false,
			// 原因要同時說出實際值與上限：只說「超出上限」的話，看的人還得自己去
			// 翻歷史檔案才知道退步了多少。
			wantReason: []string{"4", "3"},
		},
		{
			name:       "max_tool_failures 低於上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxToolFailures, Value: "2"},
			result:     eval.RunResult{ToolFailures: 1},
			wantPassed: true,
		},
		{
			name:       "max_tool_failures 剛好等於上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxToolFailures, Value: "2"},
			result:     eval.RunResult{ToolFailures: 2},
			wantPassed: true,
		},
		{
			name:       "max_tool_failures 超出上限",
			assertion:  eval.Assertion{Kind: eval.AssertMaxToolFailures, Value: "2"},
			result:     eval.RunResult{ToolFailures: 5},
			wantPassed: false,
			wantReason: []string{"5", "2"},
		},
		{
			// 上限 0 是合法且有用的宣告：「這條用例一次 Tool 失敗都不准有」。
			name:       "max_tool_failures 上限 0 且真的沒失敗",
			assertion:  eval.Assertion{Kind: eval.AssertMaxToolFailures, Value: "0"},
			result:     eval.RunResult{ToolFailures: 0},
			wantPassed: true,
		},
		{
			name:       "max_tool_failures 上限 0 但失敗了一次",
			assertion:  eval.Assertion{Kind: eval.AssertMaxToolFailures, Value: "0"},
			result:     eval.RunResult{ToolFailures: 1},
			wantPassed: false,
			wantReason: []string{"1", "0"},
		},
		{
			// **後衛**：值不是數字時判不通過，不是判通過。解析層擋得掉這種宣告，但
			// Grade 是匯出的純函式，一個繞過解析直接建 Case 的呼叫端不該拿到綠燈——
			// 綠燈代表「這個性質成立」，而這裡根本沒有比對過任何東西。
			name:       "值不是數字時判不通過",
			assertion:  eval.Assertion{Kind: eval.AssertMaxIterations, Value: "三次"},
			result:     eval.RunResult{Iterations: 1},
			wantPassed: false,
			wantReason: []string{"三次"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.Grade(eval.Case{Name: "測試用例", Assert: []eval.Assertion{tt.assertion}}, tt.result)
			if got.Passed != tt.wantPassed {
				t.Fatalf("Passed = %v，期望 %v（原因: %v）", got.Passed, tt.wantPassed, got.Failures)
			}
			if tt.wantPassed {
				if len(got.Failures) != 0 {
					t.Errorf("通過卻帶了未通過原因: %v", got.Failures)
				}
				return
			}
			if len(got.Failures) != 1 {
				t.Fatalf("未通過原因數 = %d %v，期望 1", len(got.Failures), got.Failures)
			}
			for _, want := range tt.wantReason {
				if !strings.Contains(got.Failures[0], want) {
					t.Errorf("未通過原因 = %q，期望含 %q", got.Failures[0], want)
				}
			}
		})
	}
}
