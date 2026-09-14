package tool

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// ErrSandboxViolation 標識 Sandbox 白名單校驗失敗；屬不可重試錯誤，
// Tool 執行終止、錯誤作為 tool 結果回填給 LLM。
var ErrSandboxViolation = errors.New("SandboxViolation")

// SandboxDecision 是一次 Sandbox 校驗的**結論**，與拒絕理由（error）成對回傳。
//
// **為什麼結論要獨立成型別，而不是繼續只看 error。** 「拒絕」與「需人工審批」在 error
// 這個型別上長得一模一樣（都是非 nil），呼叫端分辨不了；而擴展階段的 Tool Policy
// （issue #39）與人工審批（issue #40）正需要這個分辨。現在把結論拆出來，第三態落地時
// 只是多產生一個既有的值，三個校驗方法與每一個呼叫點的簽名都不必再動第二次。
//
// **零值是 SandboxDeny，這是刻意的（fail closed）。** 沒被賦值的決策——結構體零值、
// 日後多一條 return 分支忘了填——一律落在「擋下來」。放行必須是有人明確寫出
// SandboxAllow 才會發生，漏填不會變成一個安靜的繞過。
//
// **非放行的決策一律附一個非 nil 的錯誤**，內容是可以直接回填給 LLM 的拒絕理由。
// 這是這個型別對呼叫端的契約，由 TestSandboxCheckDecisions 釘住。
type SandboxDecision int

const (
	// SandboxDeny 拒絕這次呼叫。**它是零值**，理由見型別說明。
	SandboxDeny SandboxDecision = iota

	// SandboxAllow 放行這次呼叫。
	SandboxAllow

	// SandboxAsk 表示這次呼叫要經人工審批才能繼續。
	//
	// **核心階段不產生它。** 本階段的白名單校驗只有放行與拒絕兩態，行為與這個型別
	// 出現之前完全等價（TestSandboxCheckNeverAsks 釘住這一點）。它先佔住型別上的位置，
	// 是為了讓擴展階段的 Tool Policy（issue #39）與人工審批（issue #40）落地時不必回頭
	// 改所有呼叫點——那正是這整個型別存在的理由。
	//
	// 呼叫端因此一律以「決策是不是 SandboxAllow」判斷能不能做，**不是以「有沒有錯誤」**：
	// 兩者今天等價，第三態落地後就不是了，而屆時把「要問人」讀成「放行」是最糟的方向。
	SandboxAsk
)

// sandboxRefusal 取一次**非放行**決策要回填給 LLM 的文字。
//
// 存在的理由只有一個：「非放行必附非 nil 錯誤」是一句文件契約，不是型別保證。四個
// 回填點各寫一次 err.Error() 的話，契約哪天被打破就是四個地方在對話中途對 nil 解參考
// ——repo 裡沒有任何 recover()，那會直接殺掉 CLI。收在這裡，最壞情況只退化成一句
// 沒那麼有幫助的話。
func sandboxRefusal(err error) string {
	if err != nil {
		return err.Error()
	}
	return "Sandbox 拒絕了這次呼叫，但沒有附上理由（這是 SandboxChecker 的 bug，請回報）"
}

// SandboxConfig 是 config.yaml 三段 Sandbox 設定的執行期形狀，由組裝點填好後
// 顯式注入（憲法 5.2）。
//
// **參數形式是結構體而不是一串位置參數**：三組白名單都是 []string，排成三個位置
// 參數的話呼叫端寫錯順序不會編譯失敗，只會安靜地拿路徑白名單去比對域名。
//
// ShellTimeout 不是白名單，SandboxChecker 也不看它——它與另外三個欄位同源（都來自
// config.yaml 的這三段）、在同一個組裝點被消費（Shell Tool 建構時取用），放同一個
// 結構體讓組裝點只需要搬一次。
type SandboxConfig struct {
	AllowedDomains  []string
	AllowedPaths    []string
	AllowedCommands []string
	ShellTimeout    time.Duration
}

// SandboxChecker 做 Tool 執行前的應用層白名單校驗：HTTP 域名、檔案路徑、Shell 命令。
// 三種白名單同一個落點是技術方案 §6.7 的設計。
type SandboxChecker struct {
	allowedDomains  []string
	allowedPaths    []string
	allowedCommands []string
}

// NewSandboxChecker 以 config.yaml 的三段設定建立校驗器；空白名單全部拒絕。
//
// 三段白名單在這裡就收斂成各自 Effective* 的產物：**校驗器持有的那一份，就是它實際
// 會拿來比對的那一份**。這讓「白名單是不是空的」只有一個答案，組裝點的啟動提醒與
// 校驗結果不可能對不上（見 EffectiveAllowedPaths 的說明）。
func NewSandboxChecker(cfg SandboxConfig) *SandboxChecker {
	return &SandboxChecker{
		allowedDomains:  EffectiveAllowedDomains(cfg.AllowedDomains),
		allowedPaths:    EffectiveAllowedPaths(cfg.AllowedPaths),
		allowedCommands: EffectiveAllowedCommands(cfg.AllowedCommands),
	}
}

// EffectiveAllowedDomains 回傳一組 http.allowed_domains 之中**校驗器實際會拿來比對**的
// 條目，已轉小寫並去重。存在的理由與另外兩段相同：讓「白名單是不是空的」只有一個定義
// 點——啟動提醒、白名單比對、拒絕訊息三者共用這一份。
//
// **只剔除一種條目：真正的空字串**（不是 trim 後為空）。這條規則刻意比另外兩段寬鬆，
// 因為它的兩種錯法嚴重程度不對等（ADR-0007）：
//
//   - 剔得太少：啟動提醒少印一行，輕。
//   - 剔得太多：一條使用者授權過、而且比得中的網域被靜默剔除，請求被拒而系統一句話都
//     不說，重。
//
// 空字串是唯一**能證明**比不中的：CheckHTTPURL 在進入比對迴圈之前就擋下空 host。其餘
// 看起來比不中的形狀一律保留——底線、尾點、Unicode、`*` 字面、zoned IPv6，甚至 NBSP 與
// 全形空白（TrimSpace 會吃掉它們，但 url.Parse 接受它們當 host，比得中）。逐一實測見
// TestCheckHTTPURLStillMatchesRetainedDomainShapes。「這條大概寫錯了」的疑慮由組裝點的
// 啟動提醒承擔，不由剔除承擔。
//
// **轉小寫在這裡做一次**：matchDomain 本來就兩側小寫比對，預先轉換與比對時轉換行為
// 相同；而去重也需要它——`Example.com` 與 `example.COM` 是同一個網域，不該佔兩個名額。
func EffectiveAllowedDomains(entries []string) []string {
	effective := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		domain := strings.ToLower(entry)
		if seen[domain] {
			continue
		}
		seen[domain] = true
		effective = append(effective, domain)
	}
	return effective
}

// EffectiveAllowedPaths 回傳一組 file.allowed_paths 之中**校驗器實際會拿來比對**的
// 條目，已標準化。三種條目回不來，因為它們永遠比不中任何請求路徑：
//
//   - **空白條目**（`""`、`"   "`）：使用者寫了一條等於沒寫的設定。把它讀成「全部
//     放行」是最不該猜錯的一種猜測。
//   - **絕對路徑**：白名單的基準是 Workspace 根，而請求路徑一律是相對的（絕對的
//     請求在 CheckFilePath 第一關就被擋），兩邊永遠對不上。
//   - **標準化後穿越出 Workspace**（`../shared`）：同上，能力界定在 Workspace 之內。
//
// **這個函式存在的理由是讓「白名單是不是空的」只有一個定義點。** 組裝點的啟動提醒
// 若自己去數 slice 長度，`allowed_paths: [""]` 或 `[/Users/me/notes]` 會被當成「已
// 配置」而不提醒，實際上卻每次呼叫都被攔——使用者照著錯誤訊息「把目錄加進去了」，
// 然後繼續失敗，而系統一句話都沒說。那正是那行提醒要防的失敗形態。
//
// 條目本身**不做 trim 後再比對**：只有「trim 後為空」才算沒寫。`"  notes  "` 這種
// 前後帶空白的條目原樣保留——檔名前後真的可以有空白，替使用者猜會讓那種路徑永遠
// 碰不到。YAML 的未加引號純量本來就會自動去掉前後空白，會走到這裡的是刻意加了
// 引號的寫法。
//
// **依標準化後的值去重、保留首次出現**（ADR-0007）。`notes`、`notes/`、`./notes` 在
// 校驗器眼中是同一棵子樹，字面比對卻看不出來。去重不改變比對結果——重複條目放行的集合
// 與單一條目完全相同——它只讓拒絕訊息的顯示名額不被同一條佔掉。
func EffectiveAllowedPaths(entries []string) []string {
	effective := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		if isAbsolutePath(entry) {
			continue
		}
		base := filepath.Clean(filepath.FromSlash(entry))
		if escapesWorkspace(base) {
			continue
		}
		if seen[base] {
			continue
		}
		seen[base] = true
		effective = append(effective, base)
	}
	return effective
}

// EffectiveAllowedCommands 回傳一組 shell.allowed_commands 之中**校驗器實際會拿來
// 比對**的條目。兩種條目回不來，因為它們永遠比不中任何請求：
//
//   - **空白條目**（`""`、`"   "`）：使用者寫了一條等於沒寫的設定。
//   - **含路徑分隔符的條目**（`/usr/bin/git`、`./bin/tool`）：合法的 `command` 不含
//     分隔符（見 CheckShellCommand 第二條），所以這種寫法永遠對不上任何請求。
//
// **這個函式存在的理由與 EffectiveAllowedPaths 完全相同**：讓「白名單是不是空的」
// 只有一個定義點。組裝點的啟動提醒若自己去數 slice 長度，`allowed_commands:
// [/usr/bin/git]` 會被當成「已配置」而不提醒，實際上卻每次呼叫都被攔——使用者照著
// 錯誤訊息「把命令加進去了」，然後繼續失敗，而系統一句話都沒說。
//
// 條目本身**不做 trim 後再比對**，理由同 EffectiveAllowedPaths：只有「trim 後為空」
// 才算沒寫。
//
// **字面去重、保留首次出現**（ADR-0007）。判重用字面而不做大小寫或 basename 正規化，
// 因為 CheckShellCommand 的比對就是字面完全相等（spec #4 定案）——`git` 與 `Git` 在
// 校驗器眼中是兩個程式名。去重不改變比對結果。
func EffectiveAllowedCommands(entries []string) []string {
	effective := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		if hasPathSeparator(entry) {
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		effective = append(effective, entry)
	}
	return effective
}

// hasPathSeparator 判斷一個名字含不含路徑分隔符。Windows 的 `\` 與磁碟機代號都要
// 認得——只認 `/` 會讓 `C:\Windows\system32\cmd.exe` 在 Windows 上漏過去。
func hasPathSeparator(name string) bool {
	return strings.ContainsRune(name, '/') ||
		strings.ContainsRune(name, filepath.Separator) ||
		filepath.VolumeName(name) != ""
}

// isAbsolutePath 判斷一個路徑是不是絕對的。兩種寫法都要認得：作業系統自己的絕對
// 形式，以及 LLM 與使用者幾乎一定會用的 POSIX 斜線寫法——後者在 Windows 上
// filepath.IsAbs 判為 false，只靠它會漏。
func isAbsolutePath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != ""
}

// escapesWorkspace 判斷一個**已標準化**的相對路徑是否穿越出 Workspace 根。
func escapesWorkspace(clean string) bool {
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// 拒絕訊息列出白名單時的兩個顯示上限（ADR-0007，人類批准 2026-09-09）。
//
// **兩個數字都是成本取捨，沒有資料支持。** 清單會進 Provider context（計費）、落日誌、落審計，
// 而白名單的條數與單條長度都由使用者配置、沒有上界——不設上限是把成本外包給運氣。它們是
// 可被第一份反例推翻的預設值，不是推導結果。
const (
	// maxListedWhitelistEntries 是一則拒絕訊息最多列出的條數。
	maxListedWhitelistEntries = 32
	// maxListedWhitelistEntryBytes 是單一條目 **%q 渲染後**的 byte 數上限。量渲染後而不是
	// 原字串：%q 會把引號、反斜線與控制字元擴張成逸出序列（一個 \x01 變成 4 bytes），量原
	// 字串的話「32 條 × 256 bytes」這個上界就不成立。
	maxListedWhitelistEntryBytes = 256
)

// describeWhitelist 產生三則拒絕訊息共用的那一段：列出某一段白名單**實際生效**的內容。
//
// effective 必須是校驗器持有的那一份（已經過 Effective* 收斂與去重）：列出來的就是拿去比對
// 的，模型看到的清單與校驗結果不可能對不上。noun 是條目的稱呼（路徑、命令、網域）。
//
// **為什麼要列出來**（ADR-0007）：三則訊息原本刻意不提白名單裡有什麼，模型把這個沉默讀成
// 「白名單是空的」，對使用者說出一句聽起來合理的假話（issue #58）。四次措辭介入都沒有在
// 謊稱率上建立效益——是沒量出效益，不是證明沒有效果——而它們的共同盲點是**沒有一次改變
// 模型手上的事實**：禁令叫它別下結論、指令叫它改做別的事，「白名單是空的」那個空格始終填得
// 進去。列出清單是**提供可核對的事實**，那句話從此與訊息裡的清單直接矛盾；**是否因此減少
// 謊稱尚未驗證**——ADR-0007 修訂後不再以真實模型量測這個效益，保留它的理由是作用點與前四次
// 不同、成本低、要回退只需改回訊息字串。代價是白名單內容會送往 Provider、落進審計資料，
// 取捨照實記在 ADR-0007 的威脅模型一節。
//
// 規則（每一條由 TestWhitelistDenialMessagesListEffectiveEntries 逐格守）：
//
//   - **每個條目以 %q 序列化。** 條目是使用者手寫的 YAML 字串，一個含換行的條目原樣輸出，
//     會讓訊息長出一行看起來像系統輸出的文字——既污染日誌，也在模型的 context 裡製造一句
//     它會當真的假話。
//   - **超出兩個上限的不列出，但計入總數；不做字串截斷。** 截斷會給模型一條殘缺的路徑，它
//     拿去呼叫、再被拒一次、燒掉 iteration——那正是 #58 這條線在治的毛病。不列出是誠實的：
//     模型知道有這條，只是看不到。
//   - **套用順序：去重（收斂時已完成）→ 濾掉過長 → 取前 32。** 反過來先取 32 的話，前 32 條
//     裡每有一條過長就少列一條可用的；被上限擋掉的每一條都是模型看不到的可用項目，能少擋一條
//     就少一條。
//   - **兩類未列出數分開說**，各自為零時省略。兩者的處置不同：過長要使用者縮短那條設定，
//     超過上限要他精簡白名單，合成一個數字他就不知道該做哪一件。
//   - **三個分支**：空白名單明說是空的；有條目但全部過長時說出總數與全部未列出，**不輸出清單
//     的冒號**——冒號後面接空白，正是空白名單那一格在防的形狀，不能從第二條路抵達它；其餘
//     列出清單。零筆可顯示分支一條都列不出來，但仍提供「共 N 條」這個可核對的事實，與「白名單
//     是空的」直接矛盾。
//
// 回傳的句子以句號收尾，呼叫端把它接在「該改哪一段設定」之後、出口之前。**順序有意義**：
// 路徑與網域那兩則的出口寫「已經確認可用的⋯⋯就直接改走那一條」，那個「已經確認可用」指的
// 就是這份清單，排到出口後面條件就沒有來源（TestWhitelistDenialMessagesShareTheSameContract
// 第 4、6 項錨在清單之後）。
func describeWhitelist(noun string, effective []string) string {
	if len(effective) == 0 {
		return fmt.Sprintf("這份白名單目前是空的，沒有任何允許的%s。", noun)
	}

	var listed []string
	tooLong := 0
	for _, entry := range effective {
		rendered := fmt.Sprintf("%q", entry)
		if len(rendered) > maxListedWhitelistEntryBytes {
			tooLong++
			continue
		}
		listed = append(listed, rendered)
	}
	if len(listed) == 0 {
		return fmt.Sprintf("目前允許的%s共 %d 條，但全部因過長未列出。", noun, len(effective))
	}
	overCap := 0
	if len(listed) > maxListedWhitelistEntries {
		overCap = len(listed) - maxListedWhitelistEntries
		listed = listed[:maxListedWhitelistEntries]
	}

	count := fmt.Sprintf("共 %d 條", len(effective))
	var unlisted []string
	if tooLong > 0 {
		unlisted = append(unlisted, fmt.Sprintf("%d 條因過長未列出", tooLong))
	}
	if overCap > 0 {
		unlisted = append(unlisted, fmt.Sprintf("%d 條因超過 %d 條上限未列出", overCap, maxListedWhitelistEntries))
	}
	if len(unlisted) > 0 {
		count += "，其中 " + strings.Join(unlisted, "、")
	}
	return fmt.Sprintf("目前允許的%s（%s）：%s。", noun, count, strings.Join(listed, "、"))
}

// CheckHTTPURL 解析 rawURL 的 host 後做通配符匹配；解析不了、非 http/https、
// 或 host 不在白名單一律回 SandboxDeny ＋ ErrSandboxViolation（deny by default）。
// 錯誤訊息不內嵌原始 URL——它會落日誌與回填 LLM，query 常帶密鑰。
//
// 回傳形狀與另外兩個校驗方法一致：**決策在最前、error 在最後**，中間留給那個檢查
// 特有的產物（見 CheckFilePath）。決策的語義見 SandboxDecision。
func (c *SandboxChecker) CheckHTTPURL(rawURL string) (SandboxDecision, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return SandboxDeny, fmt.Errorf("%w: 無法解析 URL（%d bytes）", ErrSandboxViolation, len(rawURL))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return SandboxDeny, fmt.Errorf("%w: scheme %q 不被允許（僅 http/https）", ErrSandboxViolation, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return SandboxDeny, fmt.Errorf("%w: URL 缺 host", ErrSandboxViolation)
	}
	// c.allowedDomains 已在建構時經 EffectiveAllowedDomains 轉小寫：這裡不再轉一次，讓
	// 「轉小寫」只發生在一個地方（形狀同 CheckFilePath 的迴圈）。
	for _, pattern := range c.allowedDomains {
		if matchDomain(pattern, host) {
			return SandboxAllow, nil
		}
	}
	// 措辭與路徑、命令兩則同一份契約（見 TestWhitelistDenialMessagesShareTheSameContract）：
	// 指名被拒的那一個、說出要往 config.yaml 的哪一段加、列出白名單實際生效的內容
	// （describeWhitelist），再給出口。
	//
	// **ticket #68 之前，清單的位置上是一句「白名單的內容不會在這裡列出，所以看不到允許的
	// 網域不代表白名單是空的——你無從得知它的狀態」**，那是「不列出白名單」這條規則的產物。
	// 規則的代價與推翻理由見 describeWhitelist 與 ADR-0007；清單出現之後那兩句成了與清單
	// 矛盾的假話，所以拿掉。
	//
	// **這一則原本三項只有一項**（issue #58 落地前）：只說「host X 不在
	// http.allowed_domains 白名單」——段名出現了，卻沒有一個字叫人去改設定檔，也沒有
	// 擋住「白名單是空的」那個推論。它比路徑那則更缺，只是驗收沒涵蓋 HTTP Tool
	// （那個 Workspace 的 allowed_domains 是空的）所以沒被真實模型量到。三則各寫各的、
	// 沒有東西比對它們，正是這種漂移的成因。
	//
	// **末段的出口與路徑那則同一次補上**（issue #58 第二輪）：這一則的下半句本來是路徑
	// 那則的逐字複製，而那個形狀剛被受控 A／B 證明對謊稱率無效（p = 1.000）。這裡沒有
	// 真實模型的量測——eval Profile 的 tools 不含 HTTP Tool——但把一個已知惰性的形狀
	// 留在這裡，等於明知故犯地再放一次相同的漂移。理由與量測見 sandbox_test.go 的
	// whitelistHandoffMark。
	//
	// **出口是條件式的，換路那半不可省**（外部審查抓到）：一次拒絕只證明「這一個」不被
	// 允許，不代表模型手上沒有已經確認可用的網域。無條件叫它轉向使用者，會與
	// TestProcessReadFileSandboxRejectionRecovers 那一類「被拒後改走已知可用的那條」的
	// 恢復契約衝突——而那些測試走固定回放，看不出提示詞已經與它們矛盾。形狀沿用
	// core.ToolErrorNotFound 的 Guidance()：先給這次該做什麼，再給什麼時候停下來問人。
	return SandboxDeny, fmt.Errorf("%w: host %q 不在 http.allowed_domains 白名單（要允許它請把這個網域加進 Workspace config.yaml 的 http.allowed_domains）。%s"+
		"已經確認可用的網域就直接改走那一條；沒有的話請直接告訴使用者你需要哪一個網域，由他決定要不要加進白名單",
		ErrSandboxViolation, host, describeWhitelist("網域", c.allowedDomains))
}

// matchDomain 比對單條白名單：`*.example.com` 匹配任意層級子域名（不含裸域名
// example.com，裸域名要另列）；其餘為完全匹配。兩側皆已轉小寫。
func matchDomain(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
}

// CheckFilePath 校驗 LLM 給的路徑落在 file.allowed_paths 的某條子樹內，回傳標準化後
// 相對 Workspace 根的路徑（供 os.Root 開檔）。任何拒絕都是 ErrSandboxViolation。
//
// 技術方案 §6.7 稱它 checkFilePath；這裡與既有的 CheckHTTPURL 並列匯出，讓白名單矩陣
// 能在 package 邊界上被測到——校驗規則是這個型別對外的行為，不是私有細節。
//
// 四條規則，順序有意義：
//
//  1. **拒絕絕對路徑。** 白名單的基準是 Workspace 根，絕對路徑沒有可比對的基準。
//  2. **解析基準一律是 Workspace 根，不是進程當下的工作目錄。** 基準必須固定——否則
//     同一份 config.yaml 在不同目錄下跑會有不同的允許範圍，那是白名單最不該有的性質。
//  3. **先標準化再比對。** `../` 在比對**之前**解掉：notes/../secrets 標準化為
//     secrets，不在 notes 子樹內，拒絕。這是這個檢查存在的理由，不是邊角案例。
//  4. **比對是子樹包含，不是字串前綴。** 白名單 work 不得放行 workspace-secrets/x。
//
// 它是**應用層**的那一道防線，純字串判斷、不碰檔案系統：訊息要告訴使用者去改哪一段
// 設定。開檔層的把關（os.Root ＋ 拒絕符號連結 ＋ Lstat 型別檢查）在 file.go，兩者
// 分工明確、不互相取代。
//
// 回傳形狀與另外兩個校驗方法一致：**決策在最前、error 在最後**。標準化後的路徑夾在
// 中間，因為它是這個檢查特有的產物——三者共有的那兩樣東西因此在三個方法裡都在同一個
// 位置，呼叫端不會有哪一個要記成例外。決策不是 SandboxAllow 時路徑是空字串。
func (c *SandboxChecker) CheckFilePath(rawPath string) (SandboxDecision, string, error) {
	if rawPath == "" {
		return SandboxDeny, "", fmt.Errorf("%w: 路徑不得為空（請給相對 Workspace 根的路徑）", ErrSandboxViolation)
	}
	if isAbsolutePath(rawPath) {
		return SandboxDeny, "", fmt.Errorf("%w: 路徑 %q 是絕對路徑；file.allowed_paths 的基準是 Workspace 根，請改用相對路徑",
			ErrSandboxViolation, rawPath)
	}

	target := filepath.Clean(filepath.FromSlash(rawPath))
	if escapesWorkspace(target) {
		return SandboxDeny, "", fmt.Errorf("%w: 路徑 %q 標準化後穿越出 Workspace 根，一律拒絕", ErrSandboxViolation, rawPath)
	}

	// c.allowedPaths 已在建構時經 EffectiveAllowedPaths 收斂：這裡拿到的每一條都
	// 標準化過、也確定有比對的意義，所以迴圈裡只剩單純的子樹包含判斷。
	for _, base := range c.allowedPaths {
		if withinSubtree(base, target) {
			return SandboxAllow, target, nil
		}
	}
	// **訊息列出白名單實際生效的內容**（describeWhitelist，ticket #68）。
	//
	// 這裡原本寫著相反的規則：「訊息只提被拒的那條路徑與該改哪一段設定——它會落日誌、也會
	// 回填給 LLM，把白名單其餘條目一起倒出來等於交出這個 Workspace 還允許哪些路徑（issue #33
	// 定案）」。但 #33 與其上游 spec 的全文都找不到這條規則的論證，只寫了訊息**要**包含什麼。
	// ADR-0007 是它的第一次論證，結論是反轉——保密性損失是真的，照實記在那份 ADR 的威脅模型
	// 一節，接受它的理由是換到的東西值得，不是損失不存在。
	//
	// **那條規則的代價是「資訊的缺席被讀成資訊」**（issue #58）。訊息完全不提白名單裡有什麼，
	// 模型就把這個沉默讀成「白名單是空的」：真實驗收裡 file.allowed_paths 明明是 [notes]，
	// 模型卻告訴使用者「未設定任何允許路徑」。那句話不會讓任何指標轉紅（收斂正常、iteration
	// 與失敗數都在上限內），它只是**一個聽起來合理的錯誤事實**——使用者可能因此把一個過寬的
	// 路徑加進設定，而其實只要加對一個目錄。以下幾段是 #58 在「不列出」的前提下補措辭的紀錄，
	// 出口那一半沿用至今。
	//
	// **不搬 shell 那則的「不要逐一嘗試其他命令名」**（issue #58 明訂）：那句話治的是
	// 候選近乎無限時逐一猜名字的形態，而路徑被拒時模型本來就會 2 次後轉向使用者
	// （#34 與 ticket #55 驗收都是），加防猜是修沒壞的東西。
	//
	// **但末段那個出口要搬**（issue #58 第二輪，2026-09-04 的受控 A／B）：當時訊息裡那句
	// 反推論句（「看不到允許的路徑不代表白名單是空的」，ticket #68 隨清單出現而拿掉）單獨
	// 落地之後量到謊稱率 4/12 對 4/12、Fisher exact 雙尾 **p = 1.000**——兩組相同。原因是
	// 它只是一句**禁令**：模型走到最後一個 iteration 時必須生出一段話交代為什麼辦不到，
	// 而禁令沒有給它一句可以說的話，於是它照樣自己編一個解釋。
	//
	// #36 在 shell 那則量到 10 次 iteration 變 1 次，靠的是一道**指令**（轉向使用者、
	// 指名你需要哪一個）。搬過來的就是那個形狀，不是那句措辭。這同時履行
	// core.ToolErrorSandbox 的維護契約——那一類的 Guidance() 刻意回空字串，下一步由
	// 每一則訊息自己帶，而這一則在此之前一步都沒帶。
	//
	// **但出口必須是條件式的**（外部審查抓到）。core.ToolErrorKind.Guidance() 的措辭
	// 規則明訂每一段要回答**兩件事**——「這次該做什麼」與「什麼時候該停下來問人」——
	// 而既有的每一段都是那個形狀（not_found：「用確認過的確切名字呼叫一次；沒有辦法
	// 確認⋯⋯就直接告訴使用者」）。第一版寫成無條件轉向，只回答了後者，並且與
	// TestProcessReadFileSandboxRejectionRecovers／TestProcessListDirSandboxRejectionRecovers
	// 釘住的「被拒後改走已知可用的那條」相矛盾。那兩支走固定回放，措辭再怎麼改它們都
	// 綠——所以這個矛盾不會有任何測試轉紅，只會在真實模型上發生。
	//
	// **條件寫「已經確認可用」而不是「可能可用」**，這一個字的差別是量出來的：第一輪那句
	// 反推論句被模型讀成「白名單可能有東西，再試試」，A／B 量到走到第 3 次 Tool 呼叫
	// 從 3/12 升到 7/12。措辭因此沿用 not_found 那段的「用**確認過的**確切名字」，讓
	// 「還沒確認過的」一律落在轉向使用者那一邊，不必再寫一句防猜。ticket #68 之後，「已經
	// 確認可用」有了來源：排在它前面的那份清單。
	return SandboxDeny, "", fmt.Errorf("%w: 路徑 %q 不在 file.allowed_paths 白名單（請把它所在的目錄加進 Workspace config.yaml 的 file.allowed_paths）。%s"+
		"已經確認可用的路徑就直接改走那一條；沒有的話請直接告訴使用者你需要哪一個路徑，由他決定要不要加進白名單",
		ErrSandboxViolation, rawPath, describeWhitelist("路徑", c.allowedPaths))
}

// CheckShellCommand 校驗 command 是 shell.allowed_commands 裡的一個程式名。
// 任何拒絕都是 ErrSandboxViolation。
//
// 技術方案 §6.7 稱它 checkShellCommand（「拆出命令**首個** token 比對白名單」）——
// 結構化 exec 之下 `argv[0]` **就是**首個 token，那句字面因此原樣成立。
//
// **這裡沒有切分器，也不該有**（ADR-0005）。`bash -c` 之下交出去的是一段文字，由
// bash 決定怎麼切、怎麼展開，Go 這邊的任何檢查都是在猜 bash 會怎麼解讀那串字；
// 結構化 exec 交出去的是一個陣列，直接進 execve，中間沒有第二個解析器。白名單檢查
// 因此退化成兩條規則加一次字串比對：
//
//  1. **不得為空。** `command` 是必填的程式名。
//  2. **不得含路徑分隔符。** exec.Command 對含分隔符的名字當路徑用、不查 PATH；
//     放行則 `./x` 與 `/tmp/x` 會繞過「白名單是一份程式名清單」的語義。而「`git`
//     在白名單時 `/usr/bin/git` 算不算」這個問題兩種答案都說得通，選最保守的一種
//     最好解釋。
//  3. **字面完全匹配。** 不做萬用字元、不做 basename 正規化（spec #4 定案）。
//
// **白名單是允許清單，不會長出黑名單**：這裡不硬性擋下任何命令名，`bash`、`sh`、
// `python`、`find`、`git` 都不例外。窮舉不完的黑名單只會製造「我擋住了」的錯覺，而
// 「哪些看似無害的工具能拿來執行別的程式」在定義上窮舉不完（`find -exec`、`git -c`
// 都不是直譯器卻都做得到）。使用者把直譯器列進白名單是他自己的授權決定。
//
// **保證的範圍只到 OryxOS 直接啟動的那個子進程的 `argv[0]`**，不延伸到那個程式接下來
// 啟動什麼。這條界線在 shell.go 的型別說明與 config.yaml 的模板註解裡都要寫出來。
//
// 回傳形狀與另外兩個校驗方法一致：**決策在最前、error 在最後**（見 CheckFilePath）。
func (c *SandboxChecker) CheckShellCommand(command string) (SandboxDecision, error) {
	if command == "" {
		return SandboxDeny, fmt.Errorf("%w: command 不得為空（請給一個程式名，例如 git）", ErrSandboxViolation)
	}
	if hasPathSeparator(command) {
		return SandboxDeny, fmt.Errorf("%w: command %q 含路徑分隔符；shell.allowed_commands 是一份程式名清單，只接受不含路徑的名字（例如 git，不是 /usr/bin/git）",
			ErrSandboxViolation, command)
	}
	for _, allowed := range c.allowedCommands {
		if command == allowed {
			return SandboxAllow, nil
		}
	}
	// **訊息列出白名單實際生效的內容與總數**（describeWhitelist，ticket #68）。這裡原本寫著
	// 相反的規則——「只提被拒的那個名字與該改哪一段設定，把其餘條目倒出來等於交出這個
	// Workspace 還允許跑哪些程式，**連基數都不提**」——推翻的理由與保密性損失見
	// CheckFilePath 對應那段與 ADR-0007。
	//
	// **最後那句是對 LLM 說的，不是對使用者說的**（issue #36）。#34 的真實 API 驗收量到
	// 一組對比：同一個模型、同樣形狀的 SandboxViolation，**路徑**被拒 2 次就停下來告知
	// 使用者，**命令**被拒卻換了 10 個名字（df、diskutil、stat、du⋯⋯）用光 max_iterations，
	// 全程沒告訴使用者辦不到——一個沒有產出的 turn 燒掉 10 次 LLM 呼叫，而使用者只看到
	// 「已達最大迭代次數」，真正的原因完全沒出現在回應裡。
	//
	// 差別不在模型，在**可猜的候選數**：路徑被拒時它推得出沒有別的路徑可試；命令被拒時
	// 候選名近乎無限，而當時那條不列出白名單的規則讓它無從得知什麼是被允許的，於是它只能
	// 一個一個猜。兩條規則在此互相拉扯，而**當時能不動不列出那條的解法就是把行為指示寫進
	// 訊息**：告訴它別猜，轉向使用者。
	//
	// **改完之後在同一個模型、同一句 prompt、同一份白名單上重驗過：10 次變 1 次**，而且
	// 模型把「要加進 config.yaml 的哪一段」原樣轉述給了使用者——那句話舊訊息裡本來就有，
	// 只是模型從沒說出來過，因為它忙著猜下一個名字。（當時白名單內容仍未列出。）
	//
	// **ticket #68 列出清單之後，防猜句一個字都不動**（ADR-0007）。有人會推論：清單都列出來
	// 了，上面那個「候選近乎無限、無從得知什麼被允許」的前提已經不在，防猜句可以拿掉——那是
	// **推論**，10 次變 1 次是**量測**。用推論換掉量測，換來的只是訊息短一點。拿掉的只有接在
	// 它前面的「白名單的內容不會在這裡列出，所以」，清單出現之後那半句是假話。
	return SandboxDeny, fmt.Errorf("%w: 命令 %q 不在 shell.allowed_commands 白名單（要允許它請把這個程式名加進 Workspace config.yaml 的 shell.allowed_commands）。%s"+
		"**不要逐一嘗試其他命令名**——請直接告訴使用者你需要哪一個命令，由他決定要不要加進白名單",
		ErrSandboxViolation, command, describeWhitelist("命令", c.allowedCommands))
}

// withinSubtree 判斷 target 是否落在 base 這棵子樹內。兩者都已標準化過。
//
// **判準是子樹包含，不是字串前綴**：多比一個路徑分隔符，work 才不會放行
// workspace-secrets/x、tmp/foo 才不會放行 tmp/foobar。
func withinSubtree(base, target string) bool {
	if base == "." {
		return true // 白名單條目是 Workspace 根本身：任何沒穿越出去的路徑都在其中
	}
	if base == target {
		return true
	}
	return strings.HasPrefix(target, base+string(filepath.Separator))
}
