package tool

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrSandboxViolation 標識 Sandbox 白名單校驗失敗；屬不可重試錯誤，
// Tool 執行終止、錯誤作為 tool 結果回填給 LLM。
var ErrSandboxViolation = errors.New("SandboxViolation")

// SandboxChecker 做 Tool 執行前的應用層白名單校驗；本切片僅 HTTP 域名白名單
// （檔案路徑與 Shell 命令白名單隨其 Tool 於後續 ticket 加入）。
type SandboxChecker struct {
	allowedDomains []string
}

// NewSandboxChecker 以 http.allowed_domains 白名單建立校驗器；空白名單全部拒絕。
func NewSandboxChecker(allowedDomains []string) *SandboxChecker {
	return &SandboxChecker{allowedDomains: allowedDomains}
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
