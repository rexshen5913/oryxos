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

// allFiles 是「三份都要」的選擇，等同 Profile 省略 bootstrap 欄位且沒有設
// identity.prompt——這些測試關心的是載入本身，不是選擇。
var allFiles = core.BootstrapSelection{Soul: true, Agents: true, User: true}

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

			got, err := loader.Bootstrap(context.Background(), allFiles)
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

	got, err := loader.Bootstrap(ctx, allFiles)
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

	first, err := loader.Bootstrap(context.Background(), allFiles)
	if err != nil {
		t.Fatalf("第一次 Bootstrap: %v", err)
	}
	write(t, dir, "USER.md", "新偏好")
	second, err := loader.Bootstrap(context.Background(), allFiles)
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

// TestBootstrapSkipsUnselectedFiles 釘住 BootstrapSelection 的語義：沒被選中的檔案
// **完全不碰**——連壞掉的都不看。這是「不讀」與「讀了但丟棄」的差別，只驗回傳值
// 的話一個「照讀再清空」的實作也會綠，所以每一列都把未選中的那份做成讀不到的形態。
//
// 三份各自獨立驗一輪：漏掉任何一份的條件判斷都會被抓到。
func TestBootstrapSkipsUnselectedFiles(t *testing.T) {
	tests := []struct {
		name string
		// broken 是被做成目錄（存在但不是普通檔）的那份。
		broken string
		// withBroken 選中了 broken 那份，必錯；withoutBroken 沒選中，必成功。
		withBroken    core.BootstrapSelection
		withoutBroken core.BootstrapSelection
	}{
		{
			name:          "SOUL.md 未選中（identity.prompt 互斥排除的形態）",
			broken:        "SOUL.md",
			withBroken:    allFiles,
			withoutBroken: core.BootstrapSelection{Agents: true, User: true},
		},
		{
			name:          "AGENTS.md 未選中",
			broken:        "AGENTS.md",
			withBroken:    allFiles,
			withoutBroken: core.BootstrapSelection{Soul: true, User: true},
		},
		{
			name:          "USER.md 未選中",
			broken:        "USER.md",
			withBroken:    allFiles,
			withoutBroken: core.BootstrapSelection{Soul: true, Agents: true},
		},
		{
			name:          "一份都不選（bootstrap 空清單的形態）：三份全壞也不失敗",
			broken:        "AGENTS.md",
			withBroken:    allFiles,
			withoutBroken: core.BootstrapSelection{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader, dir := newLoader(t)
			if err := os.Mkdir(filepath.Join(dir, tt.broken), 0o755); err != nil {
				t.Fatalf("建立目錄 %s: %v", tt.broken, err)
			}

			if _, err := loader.Bootstrap(context.Background(), tt.withBroken); err == nil {
				t.Errorf("選中壞掉的 %s 時應回錯誤", tt.broken)
			}

			got, err := loader.Bootstrap(context.Background(), tt.withoutBroken)
			if err != nil {
				t.Fatalf("未選中 %s 時不該去碰它，實際回錯誤: %v", tt.broken, err)
			}
			if got != (core.BootstrapContext{}) {
				t.Errorf("未建立內容的檔案應留空，實際 %+v", got)
			}
		})
	}
}

// TestValidateBootstrapFiles 釘住啟動時的存在性校驗：**明確要求**的檔案必須存在，
// 缺一份就是設定錯誤。省略欄位（Explicit 為假）不校驗——那是「載入預設三檔」，
// 缺檔視為該層為空。
//
// 校驗的對象是 selection 而不是欄位的字面清單：被 ADR-0003 互斥排除的 SOUL.md 不在
// selection 裡，缺了它不該讓程式起不來——否則同一份用不到的檔案會變成「壞掉可以跑、
// 缺檔卻起不來」。
func TestValidateBootstrapFiles(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		sel     core.BootstrapSelection
		wantSub string // 空字串表示期望通過
	}{
		{
			name: "欄位省略：不校驗，缺檔也放行",
			sel:  core.BootstrapSelection{Soul: true, Agents: true, User: true},
		},
		{
			name: "空清單：沒有東西要校驗",
			sel:  core.BootstrapSelection{Explicit: true},
		},
		{
			name:    "列出的都在",
			present: []string{"AGENTS.md", "USER.md"},
			sel:     core.BootstrapSelection{Agents: true, User: true, Explicit: true},
		},
		{
			name:    "列出的檔案不存在",
			sel:     core.BootstrapSelection{Agents: true, Explicit: true},
			wantSub: "AGENTS.md",
		},
		{
			name:    "列了兩份、缺的是其中一份",
			present: []string{"AGENTS.md"},
			sel:     core.BootstrapSelection{Agents: true, Soul: true, Explicit: true},
			wantSub: "SOUL.md",
		},
		{
			// 空檔是使用者刻意留空，與「檔案不存在」不同——照常放行。
			name:    "列出的檔案存在但為空：照常",
			present: []string{"USER.md"},
			sel:     core.BootstrapSelection{User: true, Explicit: true},
		},
		{
			// 使用者寫了 identity.prompt 又列出 SOUL.md：互斥把它排除在 selection
			// 之外，缺檔不該讓程式起不來。
			name:    "列了 SOUL.md 但被互斥排除：缺檔照常放行",
			present: []string{"AGENTS.md"},
			sel:     core.BootstrapSelection{Agents: true, Explicit: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.present {
				write(t, dir, name, "")
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatalf("OpenRoot: %v", err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Errorf("關閉 root: %v", err)
				}
			})

			err = ValidateBootstrapFiles(root, tt.sel)
			if tt.wantSub != "" {
				if err == nil {
					t.Fatalf("期望錯誤含 %q，實際通過", tt.wantSub)
				}
				if !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("錯誤訊息 %q 未含 %q", err.Error(), tt.wantSub)
				}
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("錯誤鏈應 unwrap 得出 os.ErrNotExist: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通過，實際錯誤: %v", err)
			}
		})
	}
}

// TestExplicitMissingFileFailsEveryLoad 釘住「明確要求」在**載入端**每次都判，而不是
// 只在啟動時驗過就算。啟動校驗只是提前回報；真正的把關在這裡，否則啟動後才被刪掉的
// 檔案會安靜地變成空值。
func TestExplicitMissingFileFailsEveryLoad(t *testing.T) {
	explicit := core.BootstrapSelection{User: true, Explicit: true}
	byDefault := core.BootstrapSelection{Soul: true, Agents: true, User: true}

	t.Run("明確要求：缺檔回錯誤且可 unwrap 出 os.ErrNotExist", func(t *testing.T) {
		loader, dir := newLoader(t)
		write(t, dir, "USER.md", "偏好")

		if _, err := loader.Bootstrap(context.Background(), explicit); err != nil {
			t.Fatalf("檔案還在時應成功: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "USER.md")); err != nil {
			t.Fatal(err)
		}
		_, err := loader.Bootstrap(context.Background(), explicit)
		if err == nil {
			t.Fatal("明確列出的檔案被刪掉後應回錯誤，不得靜默視為空")
		}
		if !strings.Contains(err.Error(), "USER.md") {
			t.Errorf("錯誤訊息 %q 未指名是哪一份", err.Error())
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("錯誤鏈應 unwrap 得出 os.ErrNotExist: %v", err)
		}
	})

	t.Run("預設選取：缺檔仍視為該層為空", func(t *testing.T) {
		loader, dir := newLoader(t)
		write(t, dir, "USER.md", "偏好")

		if err := os.Remove(filepath.Join(dir, "USER.md")); err != nil {
			t.Fatal(err)
		}
		got, err := loader.Bootstrap(context.Background(), byDefault)
		if err != nil {
			t.Fatalf("省略欄位時缺檔應照常: %v", err)
		}
		if got != (core.BootstrapContext{}) {
			t.Errorf("三份都不在時應回空快照，實際 %+v", got)
		}
	})
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

			got, err := loader.Bootstrap(context.Background(), allFiles)
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
