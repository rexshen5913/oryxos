package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
	"github.com/rexshen5913/oryxos/internal/tool"
)

// runInit 在 dir 下執行 `oryxos init`，回傳合併的輸出與錯誤。
func runInit(t *testing.T, dir string) (string, error) {
	t.Helper()
	t.Chdir(dir)

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"init"})

	err := cmd.Execute()
	return out.String(), err
}

func TestInitCommand(t *testing.T) {
	// Workspace 全部產物：五個子目錄＋三個 Bootstrap 模板＋預設 Profile＋Workspace 設定檔
	// ＋MCP server 宣告檔。
	wantDirs := []string{"profiles", "sessions", "skills", "memory", "logs"}
	// 三份 Bootstrap 檔案必須**建立且為空**：它們的內容會逐字注入每個 turn 的
	// system prompt，任何說明文字都會被 LLM 當成真的專案慣例／偏好／人格來遵循。
	// 說明屬於給人看的東西，歸 init 的輸出訊息。
	wantEmptyFiles := []string{"AGENTS.md", "SOUL.md", "USER.md"}
	wantFiles := []string{
		filepath.Join("profiles", "default.yaml"),
		"config.yaml",
		// 模板的內容契約（帶註解、且能被載入器讀成 0 個 server）另見
		// TestInitMcpServersTemplate；這裡只驗它存在且非空。
		"mcp_servers.yaml",
	}

	t.Run("首次 init 建立全部產物", func(t *testing.T) {
		dir := t.TempDir()
		out, err := runInit(t, dir)
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}

		ws := filepath.Join(dir, ".oryxos")
		for _, d := range wantDirs {
			info, err := os.Stat(filepath.Join(ws, d))
			if err != nil {
				t.Errorf("子目錄 %s 不存在：%v", d, err)
				continue
			}
			if !info.IsDir() {
				t.Errorf("%s 不是目錄", d)
			}
		}
		for _, f := range wantFiles {
			data, err := os.ReadFile(filepath.Join(ws, f))
			if err != nil {
				t.Errorf("檔案 %s 不存在：%v", f, err)
				continue
			}
			if len(data) == 0 {
				t.Errorf("檔案 %s 為空", f)
			}
		}
		for _, f := range wantEmptyFiles {
			data, err := os.ReadFile(filepath.Join(ws, f))
			if err != nil {
				t.Errorf("檔案 %s 不存在：%v", f, err)
				continue
			}
			if strings.TrimSpace(string(data)) != "" {
				t.Errorf("Bootstrap 檔案 %s 出廠就有內容，會被逐字注入 system prompt 當成真指令：%q", f, data)
			}
		}
		// 說明要在輸出訊息裡給人看，不在會被送往 Provider 的檔案裡。
		for _, want := range []string{"AGENTS.md", "USER.md", "SOUL.md", "系統提示詞"} {
			if !strings.Contains(out, want) {
				t.Errorf("init 輸出未說明 %q，使用者不知道這幾份空檔要寫什麼：\n%s", want, out)
			}
		}
		if !strings.Contains(out, ".oryxos") {
			t.Errorf("輸出未提及 .oryxos，實際輸出：\n%s", out)
		}
	})

	t.Run("default.yaml 欄位齊備", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := runInit(t, dir); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, ".oryxos", "profiles", "default.yaml"))
		if err != nil {
			t.Fatalf("讀取 default.yaml：%v", err)
		}
		profile := string(data)
		// 只斷言外部可觀察的欄位存在（identity、provider name: openrouter、tools、settings），
		// 不綁死模板全文。
		// 模板的 base_url 指向 OpenRouter，而它只認 vendor/model 形式的 ID——裸的
		// `gpt-4o-mini` 會被端點拒絕，快速開始第一步就撞牆。斷言的是**形式**，
		// 換成別的 vendor 的模型不該讓這條無故轉紅。
		if !regexp.MustCompile(`(?m)^\s*model:\s+\S+/\S+`).MatchString(profile) {
			t.Errorf("default.yaml 的 model 不是 vendor/model 形式，實際內容：\n%s", profile)
		}
		for _, want := range []string{
			"identity:",
			"provider:",
			"name: openrouter",
			"tools:",
			// 預設 Profile 就帶兩個 Memory Tool，快速開始能直接走 Demo 二的記事場景。
			"- save_memory",
			"- recall_memory",
			"settings:",
			"max_iterations:",
			"max_history_turns:",
		} {
			if !strings.Contains(profile, want) {
				t.Errorf("default.yaml 缺少 %q，實際內容：\n%s", want, profile)
			}
		}
	})

	t.Run("config.yaml 含環境變數佔位與白名單段", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := runInit(t, dir); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, ".oryxos", "config.yaml"))
		if err != nil {
			t.Fatalf("讀取 config.yaml：%v", err)
		}
		cfg := string(data)
		for _, want := range []string{
			"${OPENROUTER_API_KEY}", // API key 以環境變數佔位，敏感值不明文落檔
			// base_url 必須是**生效的設定**而非註解：模板的 provider 是 OpenRouter，
			// 少了它 go-openai 會打回 OpenAI 的預設端點，憑證與模型 ID 全部對不上。
			"base_url: https://openrouter.ai/api/v1",
			"allowed_domains", // http.allowed_domains 白名單段
		} {
			if !strings.Contains(cfg, want) {
				t.Errorf("config.yaml 缺少 %q，實際內容：\n%s", want, cfg)
			}
		}
	})

	// 兩份模板是一組要能直接對話的組合：Profile 的 provider.name 是 config.yaml
	// providers 段的 key，改一邊沒改另一邊，快速開始就會停在「找不到 provider」。
	t.Run("Profile 的 provider name 對得上 config.yaml 的 providers key", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := runInit(t, dir); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}

		profile, err := os.ReadFile(filepath.Join(dir, ".oryxos", "profiles", "default.yaml"))
		if err != nil {
			t.Fatalf("讀取 default.yaml：%v", err)
		}
		cfg, err := os.ReadFile(filepath.Join(dir, ".oryxos", "config.yaml"))
		if err != nil {
			t.Fatalf("讀取 config.yaml：%v", err)
		}

		name := providerNameOf(t, string(profile))
		if !strings.Contains(string(cfg), "\n  "+name+":\n") {
			t.Errorf("config.yaml 的 providers 段沒有 %q，Profile 卻引用它，實際內容：\n%s", name, cfg)
		}
	})

	// 重複 init 的偵測與不覆蓋，含 .oryxos 為目錄與為一般檔案兩種既有形態。
	// probe 是重複 init 後必須原樣保留的檔案（相對 dir）。
	repeatTests := []struct {
		name  string
		probe string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "重複 init 提示且不覆蓋既有檔案",
			probe: filepath.Join(".oryxos", "AGENTS.md"),
			setup: func(t *testing.T, dir string) {
				if _, err := runInit(t, dir); err != nil {
					t.Fatalf("首次 init：%v", err)
				}
				// 模擬使用者已手寫的配置，重複 init 後必須原樣保留。
				custom := filepath.Join(dir, ".oryxos", "AGENTS.md")
				if err := os.WriteFile(custom, []byte("使用者手寫內容"), 0o644); err != nil {
					t.Fatalf("寫入自訂內容：%v", err)
				}
			},
		},
		{
			name:  ".oryxos 為既有一般檔案時提示且不動它",
			probe: ".oryxos",
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, ".oryxos")
				if err := os.WriteFile(path, []byte("使用者手寫內容"), 0o644); err != nil {
					t.Fatalf("建立既有檔案：%v", err)
				}
			},
		},
	}
	for _, tt := range repeatTests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			out, err := runInit(t, dir)
			if err != nil {
				t.Fatalf("重複 init 不應回傳錯誤，得到：%v", err)
			}
			if !strings.Contains(out, "已存在") {
				t.Errorf("輸出缺少既有 Workspace 提示，實際輸出：\n%s", out)
			}

			// 既有內容原樣保留，一個位元組都不覆蓋。
			data, err := os.ReadFile(filepath.Join(dir, tt.probe))
			if err != nil {
				t.Fatalf("讀取既有檔案：%v", err)
			}
			if string(data) != "使用者手寫內容" {
				t.Errorf("既有檔案被覆蓋，內容變為：%s", data)
			}
		})
	}

	t.Run("目標目錄不可寫時報錯且訊息清晰", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 不受檔案權限限制，無法以唯讀目錄模擬失敗")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("設定唯讀目錄：%v", err)
		}
		t.Cleanup(func() {
			// 還原權限，讓 t.TempDir 能正常清理。
			_ = os.Chmod(dir, 0o755)
		})

		_, err := runInit(t, dir)
		if err == nil {
			t.Fatal("唯讀目錄下 init 應回傳錯誤")
		}
		// %w 包裝的錯誤鏈必須可 unwrap 出底層權限錯誤（憲法 5.1）。
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("錯誤鏈無法 unwrap 出權限錯誤，實際錯誤：%v", err)
		}
		if !strings.Contains(err.Error(), ".oryxos") {
			t.Errorf("錯誤訊息未指出建立目標，實際錯誤：%v", err)
		}
	})
}

// providerNameOf 取出 Profile 模板裡 provider.name 的值。刻意手寫而不引 yaml：
// 這裡要驗的是模板產物本身，用解析器讀等於拿模板的一種詮釋去驗模板。
func providerNameOf(t *testing.T, profile string) string {
	t.Helper()
	inProvider := false
	for line := range strings.SplitSeq(profile, "\n") {
		if strings.HasPrefix(line, "provider:") {
			inProvider = true
			continue
		}
		// 空行與頂格註解不代表離開區塊——把它們當結束會在模板多一個空行時
		// 誤報「找不到 provider.name」。
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inProvider && !strings.HasPrefix(line, "  ") {
			break // 離開 provider 區塊
		}
		if name, ok := strings.CutPrefix(line, "  name:"); inProvider && ok {
			name, _, _ = strings.Cut(name, "#") // 去掉行末註解
			return strings.TrimSpace(name)
		}
	}
	t.Fatalf("Profile 模板找不到 provider.name，實際內容：\n%s", profile)
	return ""
}

// TestInitFileSectionTemplate 釘住 init 產出的 file 段：既有註解引導使用者，又要能
// 被**自己的載入器**讀出來，而且解出來的語義與註解描述的一致。
//
// prior art 是 TestInitMcpServersTemplate。「只斷言檔案裡有那幾行字」不算通過——
// 一份縮排寫錯的模板照樣含有那幾個字串，卻會讓每個新 Workspace 一啟動就報錯，而那
// 是 init 自己造出來的。所以這裡把產物真的餵回 config.Load，再把解出來的白名單餵給
// 真正在用它的 SandboxChecker，驗「預設全部拒絕」那句註解是真的。
func TestInitFileSectionTemplate(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	path := filepath.Join(dir, ".oryxos", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 config.yaml: %v", err)
	}
	// 註解要說清楚兩件使用者一定會踩到的事：基準是 Workspace 根、預設全部拒絕。
	for _, want := range []string{"file:", "allowed_paths: []", "Workspace", "拒絕"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.yaml 的 file 段缺少 %q，實際內容：\n%s", want, data)
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("init 產出的 config.yaml 讀不進來: %v", err)
	}
	if len(cfg.File.AllowedPaths) != 0 {
		t.Fatalf("模板的 allowed_paths = %v, 期望空清單（預設全部拒絕）", cfg.File.AllowedPaths)
	}

	// 「預設全部拒絕」不是模板註解的一句話而已：把解出來的白名單交給真正在用它的
	// 校驗器，任何路徑都必須被擋下。
	checker := tool.NewSandboxChecker(sandboxConfig(cfg))
	for _, p := range []string{"notes/todo.md", "config.yaml", "."} {
		if _, err := checker.CheckFilePath(p); !errors.Is(err, tool.ErrSandboxViolation) {
			t.Errorf("stock Workspace 對 %q 的校驗 = %v, 期望 SandboxViolation（空白名單全拒）", p, err)
		}
	}
}
