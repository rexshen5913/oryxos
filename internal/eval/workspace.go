package eval

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rexshen5913/oryxos/internal/core"
)

// workspaceDir 是評測用 Workspace 在用例根目錄下的名字。
//
// 沿用 `oryxos init` 那個名字不是必要的——驅動層是把 Workspace 路徑顯式傳進去的，
// 叫什麼都跑得動。用同一個名字是為了**人**：某個用例掛掉時，那份殘留的目錄看起來要
// 就是一個真的 Workspace，才好直接進去翻 logs/ 與 oryxos.db。
const workspaceDir = ".oryxos"

// copiedRootFiles 是要從來源 Workspace 帶過來的根層檔案。
//
// 檔名一律引用定義它們的地方（core），不自己寫字串：這幾個名字同時是 Profile
// bootstrap 欄位的合法值，兩邊各寫一份的話，哪天改名會讓評測安靜地少帶一份上下文
// ——而 Agent 少了 AGENTS.md 仍然跑得動，只是行為變了，從輸出完全看不出來。
var copiedRootFiles = []string{
	"config.yaml",
	core.McpServersFile,
	core.BootstrapAgentsFile,
	core.BootstrapSoulFile,
	core.BootstrapUserFile,
}

// copiedSubdirs 是要帶過來的子目錄，以及各自要收哪種副檔名。
//
// skills/ 是**扁平單檔**（`skills/<name>.md`，見 internal/config 的 skillsDir），
// 所以不遞迴。
var copiedSubdirs = []struct {
	dir string
	ext string
}{
	{"profiles", ".yaml"},
	{"skills", ".md"},
}

// createdSubdirs 是要建成空目錄的那幾個。
//
// **它們必須存在但必須是空的**，這兩件事都是必要的：SQLite 與日誌開檔時不會自動建
// 父目錄（缺目錄 → 啟動失敗），而 sessions/ 或 memory/ 裡若殘留前幾次的東西，Agent
// 會讀到上一場對話寫下的長期記憶——那時判卷可能在**沒有真的做對**的情況下通過。
var createdSubdirs = []string{"profiles", "sessions", "skills", "memory", "logs"}

// PrepareWorkspace 在 caseRoot 下建一個乾淨的 Workspace：帶上來源 Workspace 的配置，
// 不帶任何前幾次執行累積的狀態，最後寫入用例宣告的初始檔案。回傳 Workspace 路徑。
//
// **複製用允許清單，不用排除清單。** 兩者的失敗方向不對稱：允許清單漏一項，症狀是
// 啟動就報錯（少了 config.yaml）或行為明顯不同，看得見；排除清單漏一項，症狀是某次
// 執行安靜地讀到了上一次的 Session 或長期記憶，而評測照樣印出「通過」。日後
// Workspace 多一種狀態檔時，允許清單的預設行為是「不帶」，那是安全的一邊。
//
// **順序是先複製、後寫入佈置檔**：用例因此能覆寫來源的某份檔案（例如給這個用例一份
// 專屬的 AGENTS.md）。反過來的話，複製會把用例宣告的內容蓋掉，而且一句話都不會說。
func PrepareWorkspace(sourceWS, caseRoot string, files map[string]string) (string, error) {
	info, err := os.Stat(sourceWS)
	if err != nil {
		return "", fmt.Errorf("讀取來源 Workspace %s（請確認路徑，或先執行 oryxos init）: %w", sourceWS, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("來源 Workspace %s 不是目錄", sourceWS)
	}

	ws := filepath.Join(caseRoot, workspaceDir)
	if err := os.MkdirAll(caseRoot, 0o755); err != nil {
		return "", fmt.Errorf("建立用例根目錄 %s: %w", caseRoot, err)
	}
	// **os.Mkdir 而不是 os.MkdirAll**，這一行是「乾淨」這條保證的地基（Codex 審查抓到）。
	//
	// MkdirAll 對已經存在的目錄什麼都不做也不報錯，於是同一個 caseRoot 重跑時 Workspace
	// 會被**沿用**。複製清單裡的檔案雖然會被覆寫，但上一次留下的 oryxos.db（含 active
	// Session 與審計記錄）、memory/MEMORY.md 與 logs/ **一個都不在複製清單裡**——它們原封
	// 不動地活下來，Agent 於是帶著上一場對話的記憶開始這一輪，而判卷可能因此在沒有真的
	// 做對的情況下通過。
	//
	// 選擇失敗而不是「先刪掉再重建」：caseRoot 可能是使用者用 --out-dir 指定的路徑，對它
	// 遞迴刪除不該由這支工具自己決定。呼叫端（cmd/oryxos-eval）則從一開始就給每次執行一
	// 個全新的目錄，所以正常使用碰不到這個錯誤。
	if err := os.Mkdir(ws, 0o755); err != nil {
		return "", fmt.Errorf("建立評測 Workspace %s（已存在代表上一次執行的 Session、長期記憶與"+
			"審計資料庫還留著，那會讓這個用例不乾淨；請換一個輸出目錄或自行移除它）: %w", ws, err)
	}
	for _, sub := range createdSubdirs {
		if err := os.MkdirAll(filepath.Join(ws, sub), 0o755); err != nil {
			return "", fmt.Errorf("建立 Workspace 目錄 %s: %w", sub, err)
		}
	}

	for _, name := range copiedRootFiles {
		// 缺檔不是錯誤：Bootstrap 三檔與 mcp_servers.yaml 都可以不存在（缺檔視為
		// 該層為空是既定行為），既有 Workspace 免遷移。真正必要的 config.yaml 缺了
		// 也不在這裡報——由驅動層載入時報，那個錯誤訊息說得比這裡精確。
		if err := copyFileIfExists(filepath.Join(sourceWS, name), filepath.Join(ws, name)); err != nil {
			return "", err
		}
	}
	for _, sub := range copiedSubdirs {
		if err := copyFlatDir(filepath.Join(sourceWS, sub.dir), filepath.Join(ws, sub.dir), sub.ext); err != nil {
			return "", err
		}
	}

	if err := writeSetupFiles(ws, files); err != nil {
		return "", err
	}
	return ws, nil
}

// writeSetupFiles 把用例宣告的初始檔案寫進 Workspace。
//
// 路徑在這裡**再校驗一次**，儘管 ParseCase 已經驗過。兩層各自獨立：那一層擋的是使用者
// 寫在 YAML 裡的東西，這一層擋的是「不管誰呼叫、傳進來什麼」。PrepareWorkspace 是匯出
// 的函式，少了這一層，一個繞過解析直接呼叫它的呼叫端就能往 Workspace 外面寫檔。
func writeSetupFiles(ws string, files map[string]string) error {
	for raw, content := range files {
		clean, err := validateSetupPath(raw)
		if err != nil {
			return err
		}
		target := filepath.Join(ws, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("建立 setup.files %q 的父目錄: %w", raw, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("寫入 setup.files %q: %w", raw, err)
		}
	}
	return nil
}

// copyFileIfExists 複製一個檔案；來源不存在時視為沒有這一項，不報錯。
func copyFileIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("讀取來源 Workspace 的 %s: %w", filepath.Base(src), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("複製 %s 到評測 Workspace: %w", filepath.Base(src), err)
	}
	return nil
}

// copyFlatDir 複製一個目錄下副檔名為 ext 的檔案，不遞迴。
//
// 不遞迴是對的，不是偷懶：profiles/ 與 skills/ 在這個專案裡都是扁平單檔佈局。日後真的
// 長出子目錄時，這裡會安靜地少帶東西——所以那一天要連同這個函式一起改，而不是指望它
// 自己適應。
func copyFlatDir(srcDir, dstDir, ext string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("讀取來源 Workspace 的 %s/: %w", filepath.Base(srcDir), err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ext {
			continue
		}
		if err := copyFileIfExists(filepath.Join(srcDir, entry.Name()), filepath.Join(dstDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
