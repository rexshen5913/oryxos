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
// 比對前把 CRLF 正規化成 LF，**只影響比對、不影響回傳的內容**。理由：Bootstrap
// 檔案明確設計成隨 Workspace 進 git（技術方案 §9.3），而 Git for Windows 安裝時
// 預設 `core.autocrlf=true`——同一份未經編輯的舊模板在 Windows checkout 出來就是
// CRLF，純位元比對會失手，那些說明文字又會被注入。
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
