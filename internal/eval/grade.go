package eval

import (
	"fmt"
	"slices"
	"strings"
)

// RunResult 是一次執行的結果摘要，判卷唯一看得到的東西。
//
// **這個型別是判卷與驅動之間的那道牆。** 判卷拿不到 Provider、拿不到 Session、拿不到
// 資料庫控制代碼——它只能對這裡的欄位下判斷，所以它必然是純函式，也就必然能表格驅動
// 測試（ticket #50 的核心結構決策）。
//
// 欄位刻意只有本票兩種斷言用得到的兩項。指標型斷言（iteration 數、Tool 失敗次數）屬
// 下一張票，屆時在這裡加欄位即可，判卷的形狀不必改。
type RunResult struct {
	// Reply 是 Agent 這一個 turn 的最終回應。
	Reply string
	// ToolsCalled 是這一場 Session 呼叫過的 Tool 名，去重、依首次呼叫的先後排序。
	// 來源是**審計表**，不是 Session 上的計數器（ticket #50 定案）。
	ToolsCalled []string
}

// Verdict 是判卷的結論：通過與否，以及未通過的原因。
type Verdict struct {
	Passed bool
	// Failures 依用例宣告的順序列出每一條沒通過的斷言。通過時為空。
	Failures []string
}

// Grade 判一份用例的卷：吃「用例宣告 ＋ 一次執行的結果摘要」，吐「通過與否 ＋ 未通過
// 原因」，完全不碰 Provider。
//
// **每一條斷言都判，不在第一條失敗時就停。** 一次真實執行是要花錢的，把那一次的全部
// 落差一起說出來，才不會讓人修完第一條、再花一次錢、才看到第二條。
func Grade(c Case, result RunResult) Verdict {
	var failures []string
	for _, a := range c.Assert {
		if reason := checkAssertion(a, result); reason != "" {
			failures = append(failures, reason)
		}
	}
	return Verdict{Passed: len(failures) == 0, Failures: failures}
}

// checkAssertion 判一條斷言，通過回空字串，不通過回原因。
func checkAssertion(a Assertion, result RunResult) string {
	switch a.Kind {
	case AssertReplyContains:
		if strings.Contains(result.Reply, a.Value) {
			return ""
		}
		// 原因裡不夾整段回應：回應可以很長，而呼叫端本來就會把它印出來。這裡只說
		// 「少了什麼」，讓失敗清單維持一行一條、掃得動。
		return fmt.Sprintf("%s：最終回應不含 %q", a.Kind, a.Value)

	case AssertToolCalled:
		// 完全相等而非前綴：read 不得因為 read_file 被呼叫過就算通過，否則一個手誤
		// 的斷言會安靜地一直綠燈。
		if slices.Contains(result.ToolsCalled, a.Value) {
			return ""
		}
		// 一併列出實際呼叫過的 Tool：只說「期望 read_file 卻沒有」，看的人還得自己
		// 去翻審計表才知道 Agent 到底選了什麼工具——而那正是這條斷言失敗時要查的事。
		actual := "一個 Tool 都沒呼叫"
		if len(result.ToolsCalled) > 0 {
			actual = strings.Join(result.ToolsCalled, "、")
		}
		return fmt.Sprintf("%s：未呼叫 %q（實際：%s）", a.Kind, a.Value, actual)
	}

	// 走不到的分支，但**必須存在**：不認得的種類在解析層就被擋下了，然而 Grade 是匯出
	// 的純函式，下一張票加斷言種類時會直接呼叫它。那時若只改了解析層的白名單而忘了在
	// 這裡加分支，預設放行會讓評測宣稱一個它根本沒檢查的性質成立——綠燈比紅燈危險。
	return fmt.Sprintf("不認得的斷言種類 %q，判為不通過（判卷未實作這一種）", a.Kind)
}
