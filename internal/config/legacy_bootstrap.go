package config

import "strings"

// spec #1～#2 時期的 `oryxos init` 會把說明文字寫進三份 Bootstrap 檔案——當時它們
// 從未被載入，所以那些字是無害的。spec #3 讓 Bootstrap 真的生效之後，這些字會被
// **逐字注入每個 turn** 並被 LLM 當成真的專案慣例／使用者偏好來遵循；最糟的是
// Profile 沒設 `identity.prompt` 時，舊 `SOUL.md` 的說明文字會變成 Agent 的整個人格。
//
// 新裝的 Workspace 已改為建空檔（見 cmd/oryxos/init.go），但**既有 Workspace 升級
// 後這些檔案原封不動還在**，而 `oryxos init` 偵測到既有 Workspace 就完全不動它。
// 「既有 Workspace 一律免遷移」是本專案的既定承諾，不能要求使用者手動清檔。
//
// 因此載入端把「內容與舊版出廠模板**完全相同**」視為空——那等於使用者從未編輯過
// 這份檔案，語義上就是空的。比對必須是**完全相同**：使用者只要動過一個字元就不
// 匹配，他寫的內容照常注入，絕不會覆寫或忽略真正的使用者編輯。
//
// 這是一次性的升級相容墊片。日後若再改出廠模板，新模板不需要加進這裡——只有
// 「曾經出貨過、且當時不會被注入」的內容才需要，而那個窗口已經關上了。
var legacyTemplates = map[string]string{
	agentsFile: `# AGENTS.md — 專案級行為說明

由你手寫、OryxOS 只讀不寫。描述這個專案怎麼做事：慣例、流程、禁忌。
內容之後會載入 Agent 的系統提示詞；留空亦可。
`,
	soulFile: `# SOUL.md — 預設 Agent 人格定義

由你手寫、OryxOS 只讀不寫。定義 Agent 的人格與語氣。
注意：若 Profile 已設定 identity.prompt，則以其為準，本檔不載入（兩者互斥）。
`,
	userFile: `# USER.md — 使用者偏好

由你手寫、OryxOS 只讀不寫。記錄你的偏好：語言、輸出風格、常用約定等。
`,
}

// isUneditedLegacyTemplate 判斷 content 是否原封不動就是該檔的舊版出廠模板。
//
// 比對前把 CRLF 正規化成 LF，**只影響比對、不影響回傳的內容**。一份未經編輯的舊模板
// 若帶著 CRLF，純位元比對會失手，那些說明文字又會被注入。
//
// **這與 oryxos 跑在哪個平台無關**——ADR-0006 已明確不支援 Windows，所以理由不能是
// 「在 Windows 上讀到 CRLF」。要防的是 **CRLF 隨檔案抵達一台 Unix 機器**：Bootstrap
// 檔案設計成隨 Workspace 散佈（技術方案 §9.3），而它不一定經過 Git 的文字往返
// （`core.autocrlf=true` 會在 commit 時把 CRLF 收回 LF）。三條真實路徑繞過那道正規化——
// 壓縮檔／`scp`／`COPY` 直送、撰寫端 `core.autocrlf=false` 讓 CRLF 進到 repo、或檔案在
// Windows 上由編輯器以 UTF-8 ＋ CRLF 存檔（VS Code 在 Windows 的預設換行）後被複製進來。
// 三者的失敗地點都在這裡，不在 Windows。
//
// **舉的例子刻意都是 UTF-8**：下面的 `normalizeNewlines` 是位元組層的 `\r\n` → `\n`
// 替換，只對 UTF-8／ASCII 成立。UTF-16 存檔（例如 Windows PowerShell 5.1 的 `Out-File`
// 預設）換行是 `\r\x00\n\x00`，這道正規化比對不到，也不在它要處理的範圍內。
//
// 只正規化換行、不做 TrimSpace 之類的寬鬆比對：那會把「使用者在檔尾多敲一個空行」
// 也當成未編輯，越界到覆寫使用者編輯那一側去。
func isUneditedLegacyTemplate(name, content string) bool {
	legacy, ok := legacyTemplates[name]
	return ok && normalizeNewlines(content) == legacy
}

// normalizeNewlines 把 CRLF 換成 LF。舊模板常數本身一律以 LF 書寫。
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
