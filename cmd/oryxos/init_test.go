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
	// Workspace 全部產物：五個子目錄＋三個 Bootstrap 模板＋預設 Profile＋Workspace 設定檔。
	wantDirs := []string{"profiles", "sessions", "skills", "memory", "logs"}
	wantFiles := []string{
		"AGENTS.md", "SOUL.md", "USER.md",
		filepath.Join("profiles", "default.yaml"),
		"config.yaml",
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
