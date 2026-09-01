package eval_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 評測 harness 會呼叫**真實 Provider**：憲法 4.4 明訂自動化測試中一律回放錄製回應、
// 絕不打真實 API，而評測的價值恰恰在於用真實模型。兩件事都成立的唯一方式，是讓評測
// 永遠不被 `make test` 或 CI 觸發。
//
// 誤觸的代價是一張真實帳單，而且**不會有任何東西報錯**——測試照樣綠燈，帳單下個月才
// 出現。所以這條約束不能只寫在文檔裡，要有測試守著（ticket #50 驗收條件）。
//
// evalMarkers 是「這裡碰到了評測」的字面證據：Makefile target 名與二進制路徑。
var evalMarkers = []string{"eval", "oryxos-eval"}

// repoRoot 從 internal/eval/ 往上兩層。測試的工作目錄一律是該測試檔所在的 package
// 目錄，所以這個相對路徑是穩定的。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析 repo 根目錄: %v", err)
	}
	return root
}

// makeTarget 是 Makefile 裡一個 target 的前置條件與 recipe。
type makeTarget struct {
	prerequisites []string
	recipe        []string
}

// targetLine 匹配 `name: prereq1 prereq2` 這種 target 宣告行。開頭不能是 tab
// （那是 recipe）；`[^=]` 排除 `name := value` 這種變數賦值。
var targetLine = regexp.MustCompile(`^([A-Za-z0-9_.\-/$()]+):([^=].*)?$`)

// parseMakefile 把 Makefile 拆成 target → 前置條件與 recipe。
//
// 刻意只認 GNU make 語法裡本專案真的用到的那一小塊（簡單 target、tab 起頭的 recipe）：
// 這裡要回答的問題是「test 這條路徑會不會走到評測」，不是實作一個 make 直譯器。
// 解析器與 Makefile 對不上時由下方的前置檢查 fail，不會安靜放行。
func parseMakefile(content string) map[string]makeTarget {
	targets := make(map[string]makeTarget)
	var current string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "\t") {
			if current != "" {
				tgt := targets[current]
				tgt.recipe = append(tgt.recipe, strings.TrimPrefix(line, "\t"))
				targets[current] = tgt
			}
			continue
		}
		// 空行與註解不終結 recipe（GNU make 允許 recipe 之間夾這兩者）。
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := targetLine.FindStringSubmatch(line)
		if m == nil {
			current = "" // 其他非 tab 開頭的內容代表這個 target 結束了
			continue
		}
		current = m[1]
		targets[current] = makeTarget{prerequisites: strings.Fields(m[2])}
	}
	return targets
}

// reachableFrom 回傳從 start 出發、經前置條件可達的全部 target 名（含 start）。
//
// **要看傳遞閉包，不能只看 test 那一行**：`test: check` 加上 `check: eval` 同樣會讓
// 一次 make test 送出真實請求，而只檢查 test 自己那兩行完全看不出來。
func reachableFrom(targets map[string]makeTarget, start string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, prereq := range targets[name].prerequisites {
			walk(prereq)
		}
	}
	walk(start)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

// TestMakeTestDoesNotTriggerEval 斷言 `make test` 這條路徑碰不到評測。
func TestMakeTestDoesNotTriggerEval(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("讀取 Makefile: %v", err)
	}
	targets := parseMakefile(string(content))

	// 前置檢查：解析器要真的認得出這份 Makefile，否則下面的斷言是在對一張空表放行
	// ——那是最糟的一種綠燈，它會在 Makefile 改版後安靜地失去保護力。
	if _, ok := targets["test"]; !ok {
		t.Fatalf("Makefile 裡找不到 test target，解析器與 Makefile 已經對不上:\n%s", content)
	}
	if _, ok := targets["eval"]; !ok {
		t.Fatalf("Makefile 裡找不到 eval target；本測試要守的東西不存在，斷言等於沒有")
	}

	for _, name := range reachableFrom(targets, "test") {
		if name == "eval" {
			t.Fatalf("make test 經前置條件可達 eval target：評測會呼叫真實 Provider 並產生費用")
		}
		for _, line := range targets[name].recipe {
			for _, marker := range evalMarkers {
				if strings.Contains(line, marker) {
					t.Errorf("make test 可達的 target %q 的 recipe 提到 %q：%s", name, marker, line)
				}
			}
		}
	}
}

// TestCIDoesNotTriggerEval 斷言沒有任何 CI workflow 觸發評測。
//
// 與上一格是兩條不同的觸發路徑：Makefile 那條防的是本機與 CI 共用的入口，這一條防的
// 是 workflow 直接下指令繞過 make。
func TestCIDoesNotTriggerEval(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("沒有 .github/workflows/，無 CI 可檢查")
		}
		t.Fatalf("讀取 workflows 目錄: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("沒有任何 workflow 檔案")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("讀取 workflow: %v", err)
			}
			for _, marker := range evalMarkers {
				if strings.Contains(string(content), marker) {
					t.Errorf("workflow 提到 %q：評測會呼叫真實 Provider 並產生費用，不得進 CI", marker)
				}
			}
		})
	}
}
