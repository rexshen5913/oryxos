// Bootstrap 長度上限的整合測試（ticket #18）：沿用既有兩個 seam——從
// AgentService.Process 驅動、斷言送往 LLM 邊界請求的 system prompt——Bootstrap 檔案
// 在 seam 之下用真實檔案（t.TempDir()，憲法 4.3）。
//
// 這裡驗的是**使用者付錢的那一端**：一份幾萬字的 AGENTS.md 不會每個 turn 都整份
// 送出去。上限本身的邊界矩陣由 internal/config 的單元測試覆蓋，不在這裡重複。
package core_test

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rexshen5913/oryxos/internal/core"
)

// bigBootstrapFile 產生一份 n 個 rune 的 Bootstrap 檔案內容，開頭與結尾各埋一個
// 可辨識的哨兵——斷言「開頭進了 prompt、結尾沒進」用它們，不必數長度。
func bigBootstrapFile(headSentinel, tailSentinel string, n int) string {
	filler := strings.Repeat("這是一行不重要的填充內容，用來把檔案撐長。\n", n)
	return headSentinel + "\n" + filler + tailSentinel + "\n"
}

// TestOversizedBootstrapIsTruncatedInPrompt 釘住本票的使用者故事：使用者不小心把
// 一份幾萬字的文件放進 AGENTS.md 時，不會每個 turn 都把它整份送給 LLM 燒錢。
//
// 三檔各驗一輪：漏掉任何一份的截斷都會被抓到。
func TestOversizedBootstrapIsTruncatedInPrompt(t *testing.T) {
	const (
		head = "HEAD_SENTINEL_開頭必須進 prompt"
		tail = "TAIL_SENTINEL_結尾必須被截掉"
	)

	tests := []struct {
		name string
		file string
		// identityPrompt 為空時 SOUL.md 才有機會生效（ADR-0003 互斥）。
		identityPrompt string
	}{
		{name: "AGENTS.md", file: "AGENTS.md", identityPrompt: bootIdentity},
		{name: "USER.md", file: "USER.md", identityPrompt: bootIdentity},
		{name: "SOUL.md", file: "SOUL.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			content := bigBootstrapFile(head, tail, 2000)
			seedBootstrap(t, dir, tt.file, content)

			agent := newBootstrapAgentWithProfile(t, srv.URL, root,
				profileWithBootstrap(nil, tt.identityPrompt))
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			if !strings.Contains(got, head) {
				t.Errorf("system prompt 應保留 %s 的開頭: %q", tt.file, got[:min(len(got), 200)])
			}
			if strings.Contains(got, tail) {
				t.Errorf("system prompt 不該含 %s 的結尾——超過上限就該截斷", tt.file)
			}
			// 整個 system prompt 都該遠小於原檔：截斷若沒生效，這個數字會爆掉。
			if n := utf8.RuneCountInString(got); n >= utf8.RuneCountInString(content) {
				t.Errorf("system prompt %d rune，未小於原檔 %d rune——截斷沒生效",
					n, utf8.RuneCountInString(content))
			}
		})
	}
}

// TestOversizedBootstrapKeepsOtherLayersIntact 釘住截斷不會波及其他層：一份超長的
// AGENTS.md 被截斷後，USER.md、長期記憶與人格層都要完整進 prompt，順序也不變
// （ADR-0003）。截斷是單一來源的事，不是整個 prompt 的事。
func TestOversizedBootstrapKeepsOtherLayersIntact(t *testing.T) {
	const (
		head     = "AGENTS_HEAD_專案慣例開頭"
		tail     = "AGENTS_TAIL_應被截掉"
		user     = "使用者偏好繁體中文回覆"
		longTerm = "使用者的後端統一用 Go"
	)

	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", bigBootstrapFile(head, tail, 2000))
	seedBootstrap(t, dir, "USER.md", user)
	seedMemory(t, filepath.Join(dir, memoryRelPath), "## 2026-08-01\n\n- "+longTerm+"\n")

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap(nil, bootIdentity))
	got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

	if strings.Contains(got, tail) {
		t.Error("超長的 AGENTS.md 未被截斷")
	}
	// 其餘各層完整且按 ADR-0003 的順序。
	var last int
	for _, want := range []string{bootIdentity, head, user, longTerm} {
		at := strings.Index(got, want)
		if at < 0 {
			t.Fatalf("system prompt 遺失 %q——截斷波及了其他層: %q", want, got)
		}
		if at < last {
			t.Errorf("順序錯誤：%q 出現在前一層之前", want)
		}
		last = at
	}
}
