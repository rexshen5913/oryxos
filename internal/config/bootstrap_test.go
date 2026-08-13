// Bootstrap 載入器的單元測試。整條鏈路的行為由 internal/core 的整合測試從
// AgentService.Process seam 驅動；這裡補的是**seam 觀察不到**的性質：ctx 取消時
// 不去讀檔，以及三份檔案各自獨立的缺檔／空檔語義。檔案一律用真的（憲法 4.3）。
package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// newLoader 在 t.TempDir() 開一個 Workspace 根與其上的 Bootstrap 載入器。
func newLoader(t *testing.T) (*BootstrapLoader, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 root: %v", err)
		}
	})
	return NewBootstrapLoader(root), dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %s: %v", name, err)
	}
}

// TestBootstrapReadsEachFileIndependently 是三份檔案的缺檔／有內容矩陣：每一份
// 各自獨立，缺一份不影響其他兩份。
func TestBootstrapReadsEachFileIndependently(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  func(core.BootstrapContext) bool
	}{
		{name: "三份都缺", files: nil, want: func(b core.BootstrapContext) bool {
			return b.Soul == "" && b.Agents == "" && b.User == ""
		}},
		{name: "只有 AGENTS.md", files: map[string]string{"AGENTS.md": "專案慣例"}, want: func(b core.BootstrapContext) bool {
			return b.Agents == "專案慣例" && b.Soul == "" && b.User == ""
		}},
		{name: "只有 SOUL.md", files: map[string]string{"SOUL.md": "人格"}, want: func(b core.BootstrapContext) bool {
			return b.Soul == "人格" && b.Agents == "" && b.User == ""
		}},
		{name: "三份都有", files: map[string]string{"SOUL.md": "人格", "AGENTS.md": "專案慣例", "USER.md": "偏好"}, want: func(b core.BootstrapContext) bool {
			return b.Soul == "人格" && b.Agents == "專案慣例" && b.User == "偏好"
		}},
		{name: "空檔與有內容並存", files: map[string]string{"AGENTS.md": "", "USER.md": "偏好"}, want: func(b core.BootstrapContext) bool {
			return b.Agents == "" && b.User == "偏好"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader, dir := newLoader(t)
			for name, content := range tt.files {
				write(t, dir, name, content)
			}

			got, err := loader.Bootstrap(context.Background(), true)
			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if !tt.want(got) {
				t.Errorf("快照不符預期: %+v", got)
			}
		})
	}
}

// TestBootstrapHonoursCancelledContext 釘住阻塞路徑吃 ctx（憲法 5.3）：呼叫端已
// 取消時直接回錯誤，不去讀檔。錯誤鏈必須 unwrap 得出 context.Canceled，呼叫端
// 才分得出「使用者取消」與「檔案壞了」。
func TestBootstrapHonoursCancelledContext(t *testing.T) {
	loader, dir := newLoader(t)
	write(t, dir, "AGENTS.md", "專案慣例")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := loader.Bootstrap(ctx, true)
	if err == nil {
		t.Fatal("已取消的 ctx 應回錯誤")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("錯誤鏈無法 unwrap 出 context.Canceled: %v", err)
	}
	if got != (core.BootstrapContext{}) {
		t.Errorf("失敗時應回零值快照，實際 %+v", got)
	}
}

// TestBootstrapRereadsEveryCall 釘住「不緩存」：同一個載入器連續呼叫兩次，中間
// 改檔，第二次要讀到新內容。緩存是擴展階段的事（技術方案 §5.3）。
func TestBootstrapRereadsEveryCall(t *testing.T) {
	loader, dir := newLoader(t)
	write(t, dir, "USER.md", "舊偏好")

	first, err := loader.Bootstrap(context.Background(), true)
	if err != nil {
		t.Fatalf("第一次 Bootstrap: %v", err)
	}
	write(t, dir, "USER.md", "新偏好")
	second, err := loader.Bootstrap(context.Background(), true)
	if err != nil {
		t.Fatalf("第二次 Bootstrap: %v", err)
	}

	if first.User != "舊偏好" {
		t.Errorf("第一次讀到 %q, 期望舊偏好", first.User)
	}
	if second.User != "新偏好" {
		t.Errorf("第二次讀到 %q, 期望新偏好——載入器不該緩存", second.User)
	}
}

// TestBootstrapSkipsSoulWhenNotWanted 釘住 wantSoul 的語義：為 false 時完全不碰
// SOUL.md——連壞掉的都不看。呼叫端在 Profile 已有 identity.prompt 時傳 false，
// 一個被互斥排除的檔案不該讓對話失敗。
func TestBootstrapSkipsSoulWhenNotWanted(t *testing.T) {
	loader, dir := newLoader(t)
	write(t, dir, "AGENTS.md", "專案慣例")
	// 讓 SOUL.md 變成讀不到的形態（目錄），wantSoul=true 時必錯、false 時必成功。
	if err := os.Mkdir(filepath.Join(dir, "SOUL.md"), 0o755); err != nil {
		t.Fatalf("建立目錄: %v", err)
	}

	if _, err := loader.Bootstrap(context.Background(), true); err == nil {
		t.Error("wantSoul=true 時壞掉的 SOUL.md 應回錯誤")
	}

	got, err := loader.Bootstrap(context.Background(), false)
	if err != nil {
		t.Fatalf("wantSoul=false 時不該碰 SOUL.md，實際回錯誤: %v", err)
	}
	if got.Soul != "" {
		t.Errorf("wantSoul=false 時 Soul 應為空，實際 %q", got.Soul)
	}
	if got.Agents != "專案慣例" {
		t.Errorf("其餘層應照常載入，實際 %q", got.Agents)
	}
}

// TestLegacyTemplatesTreatedAsEmpty 釘住升級相容墊片：spec #1～#2 時期的 `oryxos
// init` 把說明文字寫進三份 Bootstrap 檔案（當時它們從未被載入，所以無害）。
// spec #3 讓 Bootstrap 生效後，**既有 Workspace 升級就會把那些字當成真指令注入**
// ——最糟的是舊 SOUL.md 的說明文字變成 Agent 的整個人格。
//
// 未經編輯的舊模板視為空；使用者只要動過一個字元就照常注入，絕不覆寫使用者編輯。
func TestLegacyTemplatesTreatedAsEmpty(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		want    string
	}{
		{
			name:    "未經編輯的舊 AGENTS.md 視為空",
			file:    "AGENTS.md",
			content: legacyTemplates[agentsFile],
			want:    "",
		},
		{
			name:    "未經編輯的舊 SOUL.md 視為空",
			file:    "SOUL.md",
			content: legacyTemplates[soulFile],
			want:    "",
		},
		{
			name:    "未經編輯的舊 USER.md 視為空",
			file:    "USER.md",
			content: legacyTemplates[userFile],
			want:    "",
		},
		{
			// Git for Windows 的 core.autocrlf=true 會把 checkout 出來的檔案轉成
			// CRLF；同一份未編輯的舊模板換個換行形態，仍不該被注入。
			name:    "CRLF 換行的舊 AGENTS.md 仍視為空",
			file:    "AGENTS.md",
			content: strings.ReplaceAll(legacyTemplates[agentsFile], "\n", "\r\n"),
			want:    "",
		},
		{
			name:    "使用者在舊模板後面補了自己的內容：照常注入",
			file:    "AGENTS.md",
			content: legacyTemplates[agentsFile] + "\n本專案測試先行。\n",
			want:    legacyTemplates[agentsFile] + "\n本專案測試先行。\n",
		},
		{
			name:    "使用者只改了一個字元：照常注入",
			file:    "AGENTS.md",
			content: legacyTemplates[agentsFile] + " ",
			want:    legacyTemplates[agentsFile] + " ",
		},
		{
			name:    "使用者完全改寫：照常注入",
			file:    "USER.md",
			content: "偏好繁體中文回覆",
			want:    "偏好繁體中文回覆",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader, dir := newLoader(t)
			write(t, dir, tt.file, tt.content)

			got, err := loader.Bootstrap(context.Background(), true)
			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			var actual string
			switch tt.file {
			case "AGENTS.md":
				actual = got.Agents
			case "USER.md":
				actual = got.User
			case "SOUL.md":
				actual = got.Soul
			}
			if actual != tt.want {
				t.Errorf("%s 載入結果 = %q, 期望 %q", tt.file, actual, tt.want)
			}
		})
	}
}
