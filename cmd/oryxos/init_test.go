package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
		// 只斷言外部可觀察的欄位存在（identity、provider name: openai、tools、settings），
		// 不綁死模板全文。
		for _, want := range []string{
			"identity:",
			"provider:",
			"name: openai",
			"model:",
			"tools:",
			// 預設 Profile 就帶 save_memory，快速開始能直接走 Demo 二的記事場景。
			"- save_memory",
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
			"${OPENAI_API_KEY}", // API key 以環境變數佔位，敏感值不明文落檔
			"base_url",          // 可選 base_url（註解形式亦可）
			"allowed_domains",   // http.allowed_domains 白名單段
		} {
			if !strings.Contains(cfg, want) {
				t.Errorf("config.yaml 缺少 %q，實際內容：\n%s", want, cfg)
			}
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
