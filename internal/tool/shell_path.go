package tool

import (
	"os"
	"path/filepath"
	"strings"
)

// EffectivePathDirs 把父進程的 PATH 切段，**丟掉所有相對段與空段**，回傳剩下的絕對段
// （已標準化，順序保持不變）。
//
// 這一份清單有兩個消費者，而且必須是**同一份**：解析執行檔時只在這些目錄裡找，
// 子進程 Env 的 PATH 也只放這些目錄。兩邊同源，「環境已收窄但實際執行的檔案仍由繼承
// 的 PATH 決定」那個落差因此在結構上不存在（ADR-0005，US 55）。
//
// **為什麼丟掉相對段與空段**：它們的語義依賴當下的工作目錄，而 oryxos 進程與 shell
// 子進程的工作目錄**必然不同**——後者固定在 Workspace 根（`Cmd.Dir`）。同一段字串
// （`./bin`，或 POSIX 上等於當前目錄的空段）在兩邊指向不同的目錄，留著它就是留一個
// 無法自圓其說的分支。
//
// **這是拒絕，不是忽略。** 若某個白名單命令只在被丟掉的段裡找得到，結果是「找不到
// 該程式」的明確錯誤，而**不是**悄悄從別處執行。
//
// 順帶消掉的還有 Go 1.19 起 `LookPath` 對相對路徑回的 `exec.ErrDot` 分支——因為我們
// 根本不呼叫 `LookPath`，一律以絕對路徑建構 `Cmd`（見 shell.go）。
func EffectivePathDirs(rawPath string) []string {
	segments := filepath.SplitList(rawPath)
	dirs := make([]string, 0, len(segments))
	for _, seg := range segments {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		if !filepath.IsAbs(seg) {
			continue
		}
		dirs = append(dirs, filepath.Clean(seg))
	}
	return dirs
}

// PathDirsOverlappingAllowedPaths 回傳 pathDirs 之中**落在 file.allowed_paths 任一
// 子樹內**的目錄，供組裝點印一行啟動警告。
//
// 這偵測的是一條**內部提權路徑**（ADR-0005 第一條，US 59／62）：父進程的 PATH 若含有
// 一個落在 file.allowed_paths 之內的目錄，Agent 光靠**已被授權的 write_file** 就能在
// 那裡放一個與白名單命令同名的檔案、或覆寫該目錄下既有的可執行檔——**把「檔案寫入
// 權限」升級成「shell 白名單內的程式執行權限」**。這不需要任何外部攻擊者，兩個能力
// 都是使用者自己開的，所以「攻擊者已經能寫檔了」那套論證在此不成立。
//
// 比對規則有兩條，兩條都不能省：
//
//  1. **兩邊都先走 EvalSymlinks 再比。** 純字串比對會漏掉「PATH 寫 /opt/tools、而它
//     是一個指向 Workspace 內某目錄的符號連結」——那是同一條提權路徑，只是繞了一層。
//  2. **判準是子樹包含，不是字串前綴**（與 CheckFilePath 第 4 條同一個判準）：
//     PATH 有 /tmp/foobar、白名單有 /tmp/foo 時**不得**警告。
//
// **只算絕對目錄**：相對段與空段在 EffectivePathDirs 那一步就被丟掉了，不構成這條
// 路徑；而 direnv／mise／nvm 這類工具展開後放進 PATH 的**是絕對路徑**，那才是真正
// 要防的形態。
//
// 回傳的是**原始的 PATH 條目**（不是化開後的路徑），因為警告要讓使用者認得出自己
// 設定裡的那一行。
func PathDirsOverlappingAllowedPaths(pathDirs, allowedPaths []string, workspaceDir string) []string {
	effective := EffectiveAllowedPaths(allowedPaths)
	if len(effective) == 0 {
		return nil
	}
	wsReal := resolveReal(workspaceDir)
	bases := make([]string, 0, len(effective))
	for _, rel := range effective {
		bases = append(bases, resolveReal(filepath.Join(wsReal, rel)))
	}

	var overlapping []string
	for _, dir := range pathDirs {
		real := resolveReal(dir)
		for _, base := range bases {
			if withinSubtree(base, real) {
				overlapping = append(overlapping, dir)
				break
			}
		}
	}
	return overlapping
}

// resolveReal 把路徑化到真實路徑；化不開（不存在、沒權限）時退回標準化後的絕對路徑。
//
// **化不開不當成錯誤**是刻意的：這個檢查只是一行啟動警告，不該因為 PATH 上有一個
// 不存在的目錄就整個失效。退回字面比對仍然抓得到字面上的重疊，只是抓不到經連結的
// 那種——而一個還不存在的目錄本來就不構成提權路徑（write_file 不會建目錄）。
func resolveReal(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// ParentPathDirs 是組裝點取用的入口：讀父進程的 PATH 並收斂成絕對段清單。
//
// 獨立成一個函式而不是讓組裝點自己寫 os.Getenv("PATH")，是為了讓「PATH 從哪裡來」
// 只有一個答案——兩個 buildToolRegistry 呼叫點若各自取值，日後有人改了其中一個，
// 兩個命令看到的可執行範圍就會不一致。
func ParentPathDirs() []string {
	return EffectivePathDirs(os.Getenv("PATH"))
}
