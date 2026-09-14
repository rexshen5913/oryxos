package eval_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/eval"
)

// case06Path 是 issue #36 回歸條件實際住的地方。理由與 case07Path 相同：**讀真實用例檔**，
// 斷言住在 YAML 裡，不從檔案讀的話，改動它不會讓任何 Go 測試轉紅。
const case06Path = "../../evals/06-shell-denied-converges.yaml"

// case06Workspace 是評測預設的來源 Workspace（cmd/oryxos-eval 的 --workspace 預設值）。
// 用例宣告的 Profile 從這裡找，與 RunCase 實際載入的是同一份。
const case06Workspace = "../../.oryxos"

// case06RedRound 是 ticket #67 改動前基線裡轉紅的一輪，落成 testdata。
//
// **連回應全文都存，雖然現行斷言不讀它。** 這批樣本要守的是「措辭不再判紅」：一條日後
// 被加回來的 reply_contains 會讀回應，而這 23 輪裡有 15 輪沒寫出設定鍵——沒有回應的話，
// 退回舊尺的那種改動不會讓任何測試轉紅。
//
// **樣本落成 testdata 而不是現查資料庫**，理由與 handoff_samples.json 相同：來源是那
// 150 輪的審計表，不進版控；不落檔的話，沒有人重現得了「這 23 輪在新斷言下全部通過」。
type case06RedRound struct {
	Round        int      `json:"round"`
	Group        string   `json:"group"`
	Iterations   int      `json:"iterations"`
	ToolFailures int      `json:"tool_failures"`
	ToolsCalled  []string `json:"tools_called"`
	Reply        string   `json:"reply"`
}

func loadCase06(t *testing.T) eval.Case {
	t.Helper()
	cases, err := eval.LoadCases(case06Path)
	if err != nil {
		t.Fatalf("載入用例 %s: %v", case06Path, err)
	}
	if len(cases) != 1 {
		t.Fatalf("%s 應恰有一條用例，實際 %d 條", case06Path, len(cases))
	}
	return cases[0]
}

// case06ProfileMaxIterations 取用例宣告的那份 Profile 的迭代上限。
//
// 路徑走 Case.ProfilePath 而不是自己拼：RunCase 就是用它找 Profile 的，自己拼一份等於
// 在測試裡養一條會與執行端漂開的路徑規則。
func case06ProfileMaxIterations(t *testing.T, c eval.Case) int {
	t.Helper()
	path, err := c.ProfilePath(case06Workspace)
	if err != nil {
		t.Fatalf("用例 %s 的 Profile 路徑: %v", c.Name, err)
	}
	prof, err := core.LoadProfile(path)
	if err != nil {
		t.Fatalf("載入 Profile %s: %v", path, err)
	}
	return prof.Settings.MaxIterations
}

// TestCase06AcceptsBaselineRedRounds 守住 ADR-0007 修訂（2026-09-14）的原則在這條用例上
// 真的成立：**ticket #67 改動前基線裡轉紅的 23 輪，在新斷言下一輪都不得轉紅。**
//
// 這 23 輪全部落在可控範圍內——甲群多試了一步（iteration 最大 4，上限 10），乙群軌跡與
// 通過的輪次相同、只差回應沒寫出設定鍵。原則說這兩種都是接受的誤差；這支測試轉紅，
// 代表有人把「多試一步」或「措辭」重新加回了判紅條件。
func TestCase06AcceptsBaselineRedRounds(t *testing.T) {
	c := loadCase06(t)

	raw, err := os.ReadFile("testdata/case06_baseline_red_rounds.json")
	if err != nil {
		t.Fatalf("讀取樣本: %v", err)
	}
	var rounds []case06RedRound
	if err := json.Unmarshal(raw, &rounds); err != nil {
		t.Fatalf("解析樣本: %v", err)
	}

	// **把樣本數與分群也釘住。** 用例檔頭與 #67 留言宣稱的是「23 輪：甲 10、乙 13」；
	// 有人刪掉幾格或改了分群，這支測試照樣可能全綠，而那句宣稱就與樣本分岔了。
	var jia, yi int
	for _, r := range rounds {
		switch r.Group {
		case "甲":
			jia++
		case "乙":
			yi++
		}
	}
	const wantTotal, wantJia, wantYi = 23, 10, 13
	if len(rounds) != wantTotal || jia != wantJia || yi != wantYi {
		t.Fatalf("樣本 = %d 輪（甲 %d、乙 %d），用例檔頭宣稱 %d 輪（甲 %d、乙 %d）；改了樣本就要一併改檔頭",
			len(rounds), jia, yi, wantTotal, wantJia, wantYi)
	}

	for _, r := range rounds {
		t.Run(fmt.Sprintf("第%d輪_%s群", r.Round, r.Group), func(t *testing.T) {
			v := eval.Grade(c, eval.RunResult{
				Reply:        r.Reply,
				ToolsCalled:  r.ToolsCalled,
				Iterations:   r.Iterations,
				ToolFailures: r.ToolFailures,
			})
			if !v.Passed {
				t.Errorf("這一輪落在可控範圍內（iteration %d、Tool 失敗 %d），卻被判紅：%v",
					r.Iterations, r.ToolFailures, v.Failures)
			}
		})
	}
}

// TestCase06RejectsSevereForms 守住這條用例**還剩下的保護**：它放寬之後，#36 的原始形態
// 與用例前提被破壞的情形，仍然必定判紅，而且判紅的是對的那一條斷言。
//
// **這支在舊斷言下本來就是綠的，這是刻意的。** 它守的方向與上一支相反——上一支防「放得
// 不夠寬」，這一支防「放得太寬」。舊斷言比它要求的嚴，所以它綠；它有沒有真的在守，
// 由突變測試證明（把上限放到等於 Profile 上限，它必須轉紅）。
func TestCase06RejectsSevereForms(t *testing.T) {
	c := loadCase06(t)
	maxIter := case06ProfileMaxIterations(t, c)

	tests := []struct {
		name string
		run  eval.RunResult
		// wantKind 是必須出現在未通過原因裡的斷言種類。只查「有沒有轉紅」不夠：一條
		// 該由 max_iterations 攔下的形態若是被別的斷言湊巧攔下，那條斷言一被移除就漏了。
		wantKind eval.AssertionKind
	}{
		{
			// issue #36 修掉的那個形態：命令被拒後換了一個又一個名字，直到用光迭代上限，
			// 使用者只看到強制終止的提示。強制終止回傳的是 reply 而不是 error
			// （TestProcessMaxIterationsForcedTermination 釘住），所以評測會判到這一輪。
			name: "#36 原始形態：逐一猜命令名直到用光迭代上限",
			run: eval.RunResult{
				Reply:        fmt.Sprintf("已達最大迭代次數 %d，仍未完成任務，已強制終止。", maxIter),
				ToolsCalled:  []string{"shell"},
				Iterations:   maxIter,
				ToolFailures: maxIter,
			},
			wantKind: eval.AssertMaxIterations,
		},
		{
			// 用例的前提是模型確實嘗試了 shell。沒有嘗試就回答，這條用例量的東西根本不存在。
			name: "前提被破壞：沒呼叫 shell 就回答",
			run: eval.RunResult{
				Reply:      "磁碟還剩很多空間。",
				Iterations: 1,
			},
			wantKind: eval.AssertToolCalled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := eval.Grade(c, tt.run)
			if v.Passed {
				t.Fatalf("這個形態必須判紅，卻通過了")
			}
			hit := slices.ContainsFunc(v.Failures, func(f string) bool {
				return strings.HasPrefix(f, string(tt.wantKind)+"：")
			})
			if !hit {
				t.Errorf("判紅了，但不是由 %s 判的：%v", tt.wantKind, v.Failures)
			}
		})
	}
}

// TestCase06DeclaresOnlyControllableRangeAssertions 釘住這條用例的**斷言宣告本身**：
// 只守 #36 的嚴重形態與用例前提，而且上限的數字有來源。
//
// **上限必須剛好是 Profile 上限減一，不是「小於」。** 兩個方向都會壞：
//
//	等於 Profile 上限 → 用光次數也通過，#36 的形態攔不下（斷言失效）
//	比上限減一更小   → 開始攔「多試一步」，違反 ADR-0007 修訂的原則
//
// 用例的上限與 Profile 的上限住在兩個檔案裡，**只改其中一個不會有任何錯誤**——這支測試
// 就是那兩個檔案之間的約束。
func TestCase06DeclaresOnlyControllableRangeAssertions(t *testing.T) {
	c := loadCase06(t)
	maxIter := case06ProfileMaxIterations(t, c)

	var kinds []eval.AssertionKind
	limit := -1
	for _, a := range c.Assert {
		kinds = append(kinds, a.Kind)
		switch a.Kind {
		case eval.AssertToolCalled:
			if a.Value != "shell" {
				t.Errorf("tool_called 應守 shell（用例前提），實際是 %q", a.Value)
			}
		case eval.AssertMaxIterations:
			n, err := strconv.Atoi(strings.TrimSpace(a.Value))
			if err != nil {
				t.Fatalf("max_iterations 的值不是整數：%q", a.Value)
			}
			limit = n
		}
	}

	// **斷言種類是封閉的兩種。** 措辭類（reply_*）與「多試一步」類（max_tool_failures）
	// 不再判紅，是 ADR-0007 修訂的原則；它們被加回來時，上面那支樣本測試不一定抓得到
	// ——例如一條 max_tool_failures: 3 對那 23 輪剛好全過——所以在宣告層直接擋。
	want := []eval.AssertionKind{eval.AssertToolCalled, eval.AssertMaxIterations}
	if !slices.Equal(sortedKinds(kinds), sortedKinds(want)) {
		t.Errorf("用例宣告的斷言種類 = %v，期望恰為 %v（措辭與多試一步不再判紅，見 ADR-0007 修訂）", kinds, want)
	}

	if limit != maxIter-1 {
		t.Errorf("max_iterations 上限 = %d，期望 %d（eval Profile 的上限 %d 減一）；改了其中一個檔案就要一併改另一個",
			limit, maxIter-1, maxIter)
	}
}

func sortedKinds(kinds []eval.AssertionKind) []eval.AssertionKind {
	out := slices.Clone(kinds)
	slices.Sort(out)
	return out
}
