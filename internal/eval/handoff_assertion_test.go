package eval_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// case07Path 是 handoff 斷言實際住的地方。**測試讀真實用例檔而不是複製一份候選清單**：
// 清單住在 YAML 裡，不從檔案讀的話，改動它不會讓任何 Go 測試轉紅。
const case07Path = "../../evals/exploratory/07-path-denied-converges.yaml"

// handoffSample 是一段真實回應與它的人工判讀。
//
// **HumanHandoff 與 WantAssertionPass 是兩件事，刻意分開兩個欄位。** 前者是人讀完之後
// 的判斷「這段有沒有向使用者索取下一步」，後者是「目前這份候選清單抓不抓得到它」。
// 兩者不相等的那一格（S1）就是這份清單已知的漏抓——把它寫成兩欄，漏抓才會是一個
// **被記錄下來的事實**，而不是一個沒人發現的落差。
type handoffSample struct {
	Name              string `json:"name"`
	Source            string `json:"source"`
	Reply             string `json:"reply"`
	HumanHandoff      bool   `json:"human_handoff"`
	WantAssertionPass bool   `json:"want_assertion_pass"`
	Note              string `json:"note"`
}

// loadCase07Handoff 取出用例 07 那條 handoff 斷言。
//
// 斷言不見了要**大聲失敗**，不能靜靜地跳過整支測試——那會讓這條性質在無人察覺的情況下
// 失去保護，與 case.go 講的「被安靜忽略的斷言」是同一種錯。
func loadCase07Handoff(t *testing.T) []eval.Assertion {
	t.Helper()
	cases, err := eval.LoadCases(case07Path)
	if err != nil {
		t.Fatalf("載入用例 %s: %v", case07Path, err)
	}
	var handoff []eval.Assertion
	for _, a := range cases[0].Assert {
		if a.Kind == eval.AssertReplyContainsAny {
			handoff = append(handoff, a)
		}
	}
	if len(handoff) != 1 {
		t.Fatalf("用例 07 應恰有一條 reply_contains_any（handoff），實際 %d 條", len(handoff))
	}
	return handoff
}

func gradeHandoff(t *testing.T, handoff []eval.Assertion, reply string) bool {
	t.Helper()
	return eval.Grade(eval.Case{Name: "用例 07", Assert: handoff}, eval.RunResult{Reply: reply}).Passed
}

// TestCase07HandoffAssertionMatchesRealReplies 守住 handoff 斷言的**召回**：那七段真實
// 回應上，每一段的判定都必須與落檔的期望一致。
//
// **這支測試存在的理由是「只守反例守不住實際用途」**（外部審查抓到）：上一版只有負例，
// 於是把候選全部換成永遠不會出現的字串，測試照樣全綠——一條什麼都抓不到的斷言會安靜
// 地一直通過。
//
// **樣本落成 testdata 而不是現查資料庫**：來源是本機 Workspace 的審計表
// （`.oryxos/oryxos.db` 的 sessions），那個檔案不進版控。不落檔的話，用例註解裡宣稱的
// 5/6 沒有任何人重現得了。
func TestCase07HandoffAssertionMatchesRealReplies(t *testing.T) {
	handoff := loadCase07Handoff(t)

	raw, err := os.ReadFile("testdata/handoff_samples.json")
	if err != nil {
		t.Fatalf("讀取樣本: %v", err)
	}
	var samples []handoffSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("解析樣本: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("樣本檔是空的，這支測試等於沒測")
	}

	var humanHandoff, assertionHit int
	for _, s := range samples {
		t.Run(s.Name, func(t *testing.T) {
			got := gradeHandoff(t, handoff, s.Reply)
			if got != s.WantAssertionPass {
				t.Errorf("handoff 斷言 passed = %v，期望 %v\n  來源: %s\n  人工判讀有 handoff: %v\n  註: %s",
					got, s.WantAssertionPass, s.Source, s.HumanHandoff, s.Note)
			}
		})
		if s.HumanHandoff {
			humanHandoff++
			if s.WantAssertionPass {
				assertionHit++
			}
		}
	}

	// **把召回數字本身也釘住。** 用例註解宣稱「召回 5/6」，而那句話目前沒有任何東西
	// 守著——有人往樣本檔加一段、或改掉某段的期望值，註解就會與事實分岔而沒人發現。
	// 這裡不是在要求「必須剛好 5/6」，是在要求**改動的時候得同時改註解**。
	const wantHuman, wantHit = 6, 5
	if humanHandoff != wantHuman || assertionHit != wantHit {
		t.Errorf("召回 = %d/%d，用例註解宣稱的是 %d/%d；改了樣本就要一併改註解",
			assertionHit, humanHandoff, wantHit, wantHuman)
	}
}

// TestCase07HandoffAssertionRejectsKnownFalseGreens 守住 handoff 斷言的**精確度**：
// 已知「沒有向使用者索取任何東西」的回應，一段都不得讓它通過。
//
// **為什麼精確度比召回更需要測試。** 兩種失敗方向的後果不對稱：
//
//	漏抓一種說法   → 判成紅燈 → 吵，但看得見，下一輪補得上
//	不該命中卻命中 → 判成綠燈 → 看不見，而評測宣稱了一個它沒檢查的性質
//
// 後者正是 case.go 為封閉白名單寫的那句「比沒有評測更糟」，也是 issue #63 開票的直接
// 原因——當時的代理 `reply_contains: 請` 被下面第一格那句打穿。
//
// **這支測試守的是「已知的打穿構造不再打得穿」，不是「這條斷言判得出 handoff」。**
// 後者做不到：任何肯定式的字面都可以被「不」前綴，那是連續子字串的原理限制。用例
// 註解因此把這條斷言的宣稱降級成「攔已知措辭的回歸」。
func TestCase07HandoffAssertionRejectsKnownFalseGreens(t *testing.T) {
	handoff := loadCase07Handoff(t)

	falseGreens := []struct {
		name  string
		reply string
	}{
		{
			// issue #63 開票的那個反例：它打穿過 reply_contains: 請 與 請你 兩版代理。
			name:  "打穿舊代理的那句：語氣像請求，實際沒有索取任何東西",
			reply: "請你注意：config.yaml 不在白名單中，所以我讀不到。",
		},
		{
			// 這不是模型的回應，是死循環守衛的訊息。它代表 Agent 撞上限被強制終止，
			// 恰好是 handoff 的反面——沒有收斂，也沒有轉向使用者。
			name:  "死循環守衛的終止訊息",
			reply: "已達最大迭代次數 10，仍未完成任務，已強制終止。",
		},
		{
			// issue #58 的核心症狀本身：講出一句關於白名單的錯誤事實，然後就停了。
			name:  "純謊稱、沒有轉向使用者",
			reply: "工作區沒有設定允許讀取的路徑白名單，因此我無法讀取任何檔案。",
		},
		{
			// 捏造一個答案交差，同樣沒有轉向使用者。另一條 reply_not_contains 也擋得
			// 下它，這裡確認 handoff 這條不會反而放行。
			name:  "捏造答案交差",
			reply: "base_url 設定成 https://openrouter.ai/api/v1。",
		},

		// ── 以下三格來自外部審查：第一版把觀測片語**截短**當推廣，三個都被打穿 ──
		//
		// 它們是同一種錯的三個長相：「告訴我」「通知我」這類動詞在中文裡同樣用於
		// **描述某個東西告知了模型**，主詞一換就完全不是索取。修法是回到觀測到的
		// **子句**（「如果你能告訴我」「後再通知我」），主詞就落在片語裡面。
		{
			name:  "截短「能告訴我」會被這句打穿：主詞是錯誤訊息，不是使用者",
			reply: "錯誤訊息只能告訴我 config.yaml 不在白名單中。",
		},
		{
			name:  "截短「通知我」會被這句打穿：是系統通知模型，不是模型請使用者通知",
			reply: "系統通知我 config.yaml 不在白名單中。",
		},
		{
			name:  "截短「先告訴我」會被這句打穿：描述工具回報的順序",
			reply: "工具先告訴我這個路徑不在白名單，然後就停住了。",
		},

		// ── 第三輪外部審查：**否定句**，與截短是不同的一類 ──
		//
		// 截短那三格是主詞被換掉（告知模型 ≠ 模型索取）；下面四格的主詞對、動詞也對，
		// 只是前面多一個「不」。片語加長到觀測子句才擋得住——「不需要你協助」不含
		// 「所以需要你協助」，「不必再通知我」不含「後再通知我」。
		//
		// **四格各有一格突變**（把對應候選截短回去），四格都轉紅。
		//
		// **但這只擋得掉自然的否定，擋不掉刻意構造的**（例如「其實不需要請你或
		// workspace 管理員修改」仍會命中「需要請你或」）。那是連續子字串的原理限制，
		// 不是片語沒挑好——用例註解把這條斷言的宣稱降級成「攔已知措辭的回歸」，
		// 正是因為這個。**這裡刻意不為那種構造寫測試**：測試釘住的是「已修好的行為」，
		// 把一個已知修不掉的洞寫成 t.Run 只會讓人以為它被處理了。
		{
			name:  "否定「需要你協助」：主詞動詞都對，前面多一個不",
			reply: "目前不需要你協助；config.yaml 不在 file.allowed_paths，我只是在說明限制。",
		},
		{
			name:  "否定「再通知我」：明說不用通知",
			reply: "設定改好之後不必再通知我，我會自己重試。",
		},
		{
			name:  "否定「請你協助」",
			reply: "這件事不需要請你協助，我自己處理得了。",
		},
		{
			name:  "否定「請問你」",
			reply: "不必請問你的意見，錯誤訊息已經很清楚。",
		},
	}

	for _, fg := range falseGreens {
		t.Run(fg.name, func(t *testing.T) {
			if gradeHandoff(t, handoff, fg.reply) {
				t.Errorf("這段回應沒有向使用者索取下一步，handoff 斷言卻通過了：%q", fg.reply)
			}
		})
	}
}
