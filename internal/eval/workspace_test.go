package eval_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// newSourceWorkspace 造一份「已經用過一陣子」的來源 Workspace：既有配置，加上前幾次
// 對話留下的 Session 資料庫、長期記憶與日誌。乾淨與否要在這種 Workspace 上才驗得出來
// ——對著一份剛 init 出來的空 Workspace 測「沒有被汙染」是同義反覆。
func newSourceWorkspace(t *testing.T) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), ".oryxos")
	for _, sub := range []string{"profiles", "sessions", "skills", "memory", "logs"} {
		if err := os.MkdirAll(filepath.Join(ws, sub), 0o755); err != nil {
			t.Fatalf("建立來源 Workspace 目錄: %v", err)
		}
	}
	files := map[string]string{
		"config.yaml":            "providers:\n  openrouter:\n    api_key: k\n",
		"mcp_servers.yaml":       "mcp_servers: {}\n",
		"AGENTS.md":              "專案慣例\n",
		"SOUL.md":                "",
		"USER.md":                "偏好\n",
		"profiles/default.yaml":  "name: default\n",
		"profiles/reviewer.yaml": "name: reviewer\n",
		"skills/refactor.md":     "---\nname: refactor\n---\n正文\n",

		// 以下四項是**前幾次執行留下的狀態**，一項都不該進乾淨的 Workspace。
		"oryxos.db":         "假裝這是 SQLite 檔",
		"sessions/old.json": "上一場對話",
		"memory/MEMORY.md":  "使用者叫小明\n",
		"logs/oryxos.log":   "{}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatalf("寫入來源 Workspace %s: %v", rel, err)
		}
	}
	return ws
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 %s: %v", path, err)
	}
	return string(data)
}

// TestPrepareWorkspaceCopiesConfiguration 釘住「配置要帶過去」：評測跑的必須是使用者
// 真正在用的那份 Profile 與設定，否則量到的東西與產品無關。
func TestPrepareWorkspaceCopiesConfiguration(t *testing.T) {
	source := newSourceWorkspace(t)
	ws, err := eval.PrepareWorkspace(source, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("準備 Workspace: %v", err)
	}

	tests := []struct {
		rel  string
		want string
	}{
		{"config.yaml", "providers:\n  openrouter:\n    api_key: k\n"},
		{"mcp_servers.yaml", "mcp_servers: {}\n"},
		{"AGENTS.md", "專案慣例\n"},
		{"SOUL.md", ""},
		{"USER.md", "偏好\n"},
		{"profiles/default.yaml", "name: default\n"},
		{"profiles/reviewer.yaml", "name: reviewer\n"},
		{"skills/refactor.md", "---\nname: refactor\n---\n正文\n"},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if got := readFile(t, filepath.Join(ws, filepath.FromSlash(tt.rel))); got != tt.want {
				t.Errorf("內容 = %q，期望 %q", got, tt.want)
			}
		})
	}
}

// TestPrepareWorkspaceLeavesPreviousStateBehind 是「乾淨」的定義：前幾次執行累積的
// Session、長期記憶、審計資料庫與日誌，一項都不得帶進來。
//
// 帶進來的後果不只是髒——Agent 會讀到上一場對話寫下的長期記憶，判卷因此可能在**沒有
// 真的做對**的情況下通過。
func TestPrepareWorkspaceLeavesPreviousStateBehind(t *testing.T) {
	source := newSourceWorkspace(t)
	ws, err := eval.PrepareWorkspace(source, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("準備 Workspace: %v", err)
	}

	for _, rel := range []string{"oryxos.db", "sessions/old.json", "memory/MEMORY.md", "logs/oryxos.log"} {
		t.Run(rel, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Errorf("%s 不該存在於乾淨的 Workspace（err = %v）", rel, err)
			}
		})
	}
	// 目錄本身要在：資料庫與日誌開檔時不會自動建父目錄。
	for _, dir := range []string{"sessions", "memory", "logs"} {
		t.Run(dir+"/", func(t *testing.T) {
			info, err := os.Stat(filepath.Join(ws, dir))
			if err != nil {
				t.Fatalf("%s 目錄不存在: %v", dir, err)
			}
			if !info.IsDir() {
				t.Errorf("%s 不是目錄", dir)
			}
		})
	}
}

// TestPrepareWorkspaceWritesSetupFiles 驗證宣告式佈置：路徑相對 Workspace 根，
// 父目錄不存在時自動建立（用例不必為了放一個檔案先宣告目錄）。
func TestPrepareWorkspaceWritesSetupFiles(t *testing.T) {
	source := newSourceWorkspace(t)
	ws, err := eval.PrepareWorkspace(source, t.TempDir(), map[string]string{
		"notes/todo.md": "買牛奶\n",
		"README.md":     "頂層檔案\n",
	})
	if err != nil {
		t.Fatalf("準備 Workspace: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "notes", "todo.md")); got != "買牛奶\n" {
		t.Errorf("notes/todo.md = %q", got)
	}
	if got := readFile(t, filepath.Join(ws, "README.md")); got != "頂層檔案\n" {
		t.Errorf("README.md = %q", got)
	}
}

// TestPrepareWorkspaceSetupFileOverwritesCopiedFile 釘住覆寫語義：用例要能把來源
// Workspace 的某份檔案換成自己要的內容（例如給這個用例一份專屬的 AGENTS.md）。
//
// 兩者的先後順序因此不能反過來——先寫佈置檔再複製配置的話，複製會把用例宣告的內容
// 蓋掉，而且**一句話都不會說**。
func TestPrepareWorkspaceSetupFileOverwritesCopiedFile(t *testing.T) {
	source := newSourceWorkspace(t)
	ws, err := eval.PrepareWorkspace(source, t.TempDir(), map[string]string{
		"AGENTS.md": "這個用例專屬的慣例\n",
	})
	if err != nil {
		t.Fatalf("準備 Workspace: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "AGENTS.md")); got != "這個用例專屬的慣例\n" {
		t.Errorf("AGENTS.md = %q，期望被用例宣告的內容覆寫", got)
	}
}

// TestPrepareWorkspaceCasesDoNotContaminate 是 ticket 的驗收條件之一：每個用例前建立
// 乾淨的 Workspace，用例之間不互相汙染。
//
// 驗的是**兩個方向**：A 佈置的檔案不得出現在 B，B 也不得看到 A 執行後留下的東西。
func TestPrepareWorkspaceCasesDoNotContaminate(t *testing.T) {
	source := newSourceWorkspace(t)

	wsA, err := eval.PrepareWorkspace(source, t.TempDir(), map[string]string{"a.md": "屬於 A\n"})
	if err != nil {
		t.Fatalf("準備 A 的 Workspace: %v", err)
	}
	// 模擬 A 跑完之後留下的東西。
	if err := os.WriteFile(filepath.Join(wsA, "oryxos.db"), []byte("A 的審計"), 0o644); err != nil {
		t.Fatalf("寫入 A 的資料庫: %v", err)
	}

	wsB, err := eval.PrepareWorkspace(source, t.TempDir(), map[string]string{"b.md": "屬於 B\n"})
	if err != nil {
		t.Fatalf("準備 B 的 Workspace: %v", err)
	}

	if wsA == wsB {
		t.Fatalf("兩個用例共用同一個 Workspace %s", wsA)
	}
	for _, tt := range []struct {
		name string
		path string
	}{
		{"A 的佈置檔案不得出現在 B", filepath.Join(wsB, "a.md")},
		{"A 執行後的資料庫不得出現在 B", filepath.Join(wsB, "oryxos.db")},
		{"B 的佈置檔案不得出現在 A", filepath.Join(wsA, "b.md")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := os.Stat(tt.path); !os.IsNotExist(err) {
				t.Errorf("%s 存在（err = %v）", tt.path, err)
			}
		})
	}
}

// TestPrepareWorkspaceRejectsMissingSource 讓「指錯 Workspace」在第一步就報錯，
// 而不是組到一半才因為找不到 config.yaml 而失敗。
func TestPrepareWorkspaceRejectsMissingSource(t *testing.T) {
	_, err := eval.PrepareWorkspace(filepath.Join(t.TempDir(), "不存在"), t.TempDir(), nil)
	if err == nil {
		t.Fatal("期望對不存在的來源 Workspace 報錯")
	}
}

// TestPrepareWorkspaceRejectsEscapingSetupPath 是 ParseCase 那道校驗的**第二道防線**。
//
// 兩層各自獨立：解析層擋的是使用者寫在 YAML 裡的路徑，這一層擋的是「不管誰呼叫、
// 傳進來什麼」。PrepareWorkspace 是匯出的函式，少了這一層，一個繞過解析直接呼叫它的
// 呼叫端（下一張票、或日後的 Web 觸發）就能往 Workspace 外面寫檔。
func TestPrepareWorkspaceRejectsEscapingSetupPath(t *testing.T) {
	source := newSourceWorkspace(t)
	caseRoot := t.TempDir()
	for _, bad := range []string{"../outside.md", "notes/../../outside.md", "/etc/passwd", ""} {
		t.Run(bad, func(t *testing.T) {
			if _, err := eval.PrepareWorkspace(source, caseRoot, map[string]string{bad: "x"}); err == nil {
				t.Errorf("期望拒絕路徑 %q", bad)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(caseRoot, "outside.md")); !os.IsNotExist(err) {
		t.Errorf("Workspace 外面被寫出了檔案（err = %v）", err)
	}
}

// TestPrepareWorkspaceRefusesExistingWorkspace 是 Codex 審查抓到的缺口：`--out-dir`
// 指同一個目錄重跑時，用例的 Workspace 會被**沿用**而不是重建。
//
// 沿用的後果不是髒而已。複製清單裡的檔案會被覆寫，但上一次執行留下的 `oryxos.db`
// （含 active Session 與審計記錄）、`memory/MEMORY.md`、`logs/` **一個都不在複製清單裡**
// ——它們原封不動地活下來，Agent 於是帶著上一場對話的記憶開始這一輪。
//
// 既有的 TestPrepareWorkspaceCasesDoNotContaminate 守不到這條，因為它給每個用例一份
// 全新的 t.TempDir()，**從來沒有重用過同一個 caseRoot**。
//
// 選擇「拒絕」而不是「先刪掉再重建」：caseRoot 是使用者用 --out-dir 指定的路徑，
// 對它遞迴刪除是一個不該由這支工具自己決定的動作。失敗並說清楚原因，讓人自己判斷。
func TestPrepareWorkspaceRefusesExistingWorkspace(t *testing.T) {
	source := newSourceWorkspace(t)
	caseRoot := t.TempDir()

	ws, err := eval.PrepareWorkspace(source, caseRoot, map[string]string{"a.md": "第一次\n"})
	if err != nil {
		t.Fatalf("第一次準備 Workspace: %v", err)
	}
	// 模擬第一次執行留下的狀態。
	if err := os.WriteFile(filepath.Join(ws, "oryxos.db"), []byte("上一次的審計"), 0o644); err != nil {
		t.Fatalf("寫入資料庫: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "memory", "MEMORY.md"), []byte("上一次記住的事\n"), 0o644); err != nil {
		t.Fatalf("寫入長期記憶: %v", err)
	}

	if _, err := eval.PrepareWorkspace(source, caseRoot, map[string]string{"b.md": "第二次\n"}); err == nil {
		t.Fatal("期望拒絕重用已經存在的 Workspace")
	}

	// 拒絕之後**不得動到既有內容**：使用者可能正想進去看上一次失敗的現場。
	if got := readFile(t, filepath.Join(ws, "oryxos.db")); got != "上一次的審計" {
		t.Errorf("既有的資料庫被動過了: %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "b.md")); !os.IsNotExist(err) {
		t.Errorf("被拒絕的那次仍然寫出了佈置檔（err = %v）", err)
	}
}
