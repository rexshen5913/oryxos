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

// SandboxChecker 做 Tool 執行前的應用層白名單校驗：HTTP 域名、檔案路徑，
// 以及 Shell 命令（隨 Shell Tool 於後續 ticket 加入）。
type SandboxChecker struct {
	allowedDomains []string
	allowedPaths   []string
}

// NewSandboxChecker 以 config.yaml 的三段設定建立校驗器；空白名單全部拒絕。
//
// 路徑白名單在這裡就收斂成 EffectiveAllowedPaths 的產物：**校驗器持有的那一份，
// 就是它實際會拿來比對的那一份**。這讓「白名單是不是空的」只有一個答案，組裝點
// 的啟動提醒與校驗結果不可能對不上（見 EffectiveAllowedPaths 的說明）。
func NewSandboxChecker(cfg SandboxConfig) *SandboxChecker {
	return &SandboxChecker{
		allowedDomains: cfg.AllowedDomains,
		allowedPaths:   EffectiveAllowedPaths(cfg.AllowedPaths),
	}
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
func EffectiveAllowedPaths(entries []string) []string {
	effective := make([]string, 0, len(entries))
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
		effective = append(effective, base)
	}
	return effective
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

// CheckHTTPURL 解析 rawURL 的 host 後做通配符匹配；解析不了、非 http/https、
// 或 host 不在白名單一律回 ErrSandboxViolation（deny by default）。
// 錯誤訊息不內嵌原始 URL——它會落日誌與回填 LLM，query 常帶密鑰。
func (c *SandboxChecker) CheckHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: 無法解析 URL（%d bytes）", ErrSandboxViolation, len(rawURL))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q 不被允許（僅 http/https）", ErrSandboxViolation, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: URL 缺 host", ErrSandboxViolation)
	}
	for _, pattern := range c.allowedDomains {
		if matchDomain(strings.ToLower(pattern), host) {
			return nil
		}
	}
	return fmt.Errorf("%w: host %q 不在 http.allowed_domains 白名單", ErrSandboxViolation, host)
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
func (c *SandboxChecker) CheckFilePath(rawPath string) (string, error) {
	if rawPath == "" {
		return "", fmt.Errorf("%w: 路徑不得為空（請給相對 Workspace 根的路徑）", ErrSandboxViolation)
	}
	if isAbsolutePath(rawPath) {
		return "", fmt.Errorf("%w: 路徑 %q 是絕對路徑；file.allowed_paths 的基準是 Workspace 根，請改用相對路徑",
			ErrSandboxViolation, rawPath)
	}

	target := filepath.Clean(filepath.FromSlash(rawPath))
	if escapesWorkspace(target) {
		return "", fmt.Errorf("%w: 路徑 %q 標準化後穿越出 Workspace 根，一律拒絕", ErrSandboxViolation, rawPath)
	}

	// c.allowedPaths 已在建構時經 EffectiveAllowedPaths 收斂：這裡拿到的每一條都
	// 標準化過、也確定有比對的意義，所以迴圈裡只剩單純的子樹包含判斷。
	for _, base := range c.allowedPaths {
		if withinSubtree(base, target) {
			return target, nil
		}
	}
	// 訊息只提被拒的那條路徑與該改哪一段設定：它會落日誌、也會回填給 LLM，把白名單
	// 其餘條目一起倒出來等於交出這個 Workspace 還允許哪些路徑。
	return "", fmt.Errorf("%w: 路徑 %q 不在 file.allowed_paths 白名單（請把它所在的目錄加進 Workspace config.yaml 的 file.allowed_paths）",
		ErrSandboxViolation, rawPath)
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
