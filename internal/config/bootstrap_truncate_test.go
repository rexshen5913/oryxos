// Bootstrap 注入來源的長度上限與截斷（ticket #18）。檔案一律用真的
// （t.TempDir()，憲法 4.3），從 BootstrapLoader.Bootstrap 觀察——截斷是讀取側的
// 行為，這裡是它唯一的出口。
//
// 斷言只看**結構性事實**：長度有沒有守住、行有沒有被切開、省略量有沒有寫出來、
// 磁碟上的檔案有沒有被動到。標記的措辭不進斷言。
package config

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// runesWithLines 產生**恰好** n 個 rune 的內容，每 width 個字元換一行（換行本身
// 算一個 rune）。width 為 0 時產生單獨一行——那是「沒有行邊界可切」的形態。
func runesWithLines(ch rune, n, width int) string {
	if n <= 0 {
		return ""
	}
	out := make([]rune, 0, n)
	for i := range n {
		if width > 0 && i > 0 && i%width == 0 {
			out = append(out, '\n')
			continue
		}
		out = append(out, ch)
	}
	return string(out)
}

// TestBootstrapTruncatesAtLimit 是長度上限的邊界矩陣。上限一律以 **rune** 計：
// 含中文的檔案用 byte 計會讓內容莫名縮水到三分之一。
func TestBootstrapTruncatesAtLimit(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantTruncate bool
	}{
		{
			name:    "遠未達上限",
			content: runesWithLines('a', 100, 20),
		},
		{
			name:    "差一個 rune 到上限",
			content: runesWithLines('a', maxBootstrapRunes-1, 80),
		},
		{
			name:    "恰好等於上限：不截斷、不加標記",
			content: runesWithLines('a', maxBootstrapRunes, 80),
		},
		{
			name:         "超過上限一個 rune",
			content:      runesWithLines('a', maxBootstrapRunes+1, 80),
			wantTruncate: true,
		},
		{
			name:         "遠超上限",
			content:      runesWithLines('a', maxBootstrapRunes*5, 80),
			wantTruncate: true,
		},
		{
			name:         "單行就超過上限：沒有行邊界可切，硬切",
			content:      runesWithLines('a', maxBootstrapRunes+500, 0),
			wantTruncate: true,
		},
		{
			name:    "中文恰好等於上限：以 rune 計不截斷",
			content: runesWithLines('中', maxBootstrapRunes, 80),
		},
		{
			name:         "中文超過上限",
			content:      runesWithLines('中', maxBootstrapRunes+1, 80),
			wantTruncate: true,
		},
		{
			// 軟換行的 Markdown 常見形態：短標題後面接一個沒有硬換行的長段落。
			// 「退回最近的行邊界」在這裡會退到標題那一行，把整個預算丟掉。
			name:         "短標題 ＋ 一個超長段落",
			content:      "# 專案慣例\n" + runesWithLines('x', maxBootstrapRunes*10, 0),
			wantTruncate: true,
		},
		{
			name: "前段正常換行、尾段是超長單行",
			content: runesWithLines('a', 500, 50) + "\n" +
				runesWithLines('x', maxBootstrapRunes*10, 0),
			wantTruncate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader, dir := newLoader(t)
			write(t, dir, "AGENTS.md", tt.content)

			got, err := loader.Bootstrap(context.Background(), allFiles)
			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}

			gotRunes := utf8.RuneCountInString(got.Agents)
			if gotRunes > maxBootstrapRunes {
				t.Errorf("注入內容 %d rune，超過上限 %d（標記本身也要算進預算）", gotRunes, maxBootstrapRunes)
			}
			if !tt.wantTruncate {
				if got.Agents != tt.content {
					t.Errorf("未超過上限不該動內容：長度 %d → %d", utf8.RuneCountInString(tt.content), gotRunes)
				}
				return
			}
			if got.Agents == tt.content {
				t.Fatal("超過上限卻原樣回傳")
			}
			// 超過上限的檔案，注入量應該**逼近**上限而不是趨近零。退回行邊界時
			// 若把預算幾乎全丟掉，這一層就等於沒進 prompt——那比超量更糟，因為
			// 使用者看到的是「檔案有被讀、標記也在」，內容卻沒到 LLM 手上。
			if gotRunes < maxBootstrapRunes/2 {
				t.Errorf("只注入了 %d rune，不到上限 %d 的一半——截斷把預算丟掉了",
					gotRunes, maxBootstrapRunes)
			}
			// 保留下來的必須是原內容的**前綴**：截斷只能從結尾裁，不得改寫或
			// 重排使用者寫的東西。這同時給出「保留了多少」，不必去解析標記。
			kept := commonPrefixRunes(tt.content, got.Agents)
			if kept == 0 {
				t.Fatalf("回傳內容不是原內容的前綴（一個字都對不上）: %q", head(got.Agents, 80))
			}
			// 標記要自述省略量，而且那個數字必須誠實：保留數 ＋ 省略數 ＝ 原長。
			// 措辭不進斷言，只驗那個數字在。
			omitted := utf8.RuneCountInString(tt.content) - kept
			if !strings.Contains(got.Agents, strconv.Itoa(omitted)) {
				t.Errorf("截斷後未自述省略量（保留 %d、應省略 %d，內容裡找不到這個數字）: %q",
					kept, omitted, tail(got.Agents, 120))
			}
		})
	}
}

// TestBootstrapTruncationKeepsWholeLines 釘住截斷點落**行邊界**：切點的前一個
// 字元必須是換行，保留下來的內容才不會有半行。
//
// 直接驗切點的位置，而不是去數每行多長：後者會隨產生器的寬度定義漂移，前者就是
// 「不把一行從中間切開」這句話本身。
func TestBootstrapTruncationKeepsWholeLines(t *testing.T) {
	widths := []int{20, 100, 617} // 617 讓切點不會剛好落在整數倍上
	for _, width := range widths {
		t.Run("每行 "+strconv.Itoa(width)+" 字", func(t *testing.T) {
			loader, dir := newLoader(t)
			content := runesWithLines('x', maxBootstrapRunes*3, width)
			write(t, dir, "USER.md", content)

			got, err := loader.Bootstrap(context.Background(), allFiles)
			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}

			kept := commonPrefixRunes(content, got.User)
			if kept == 0 {
				t.Fatal("回傳內容不是原內容的前綴")
			}
			if r := []rune(content)[kept-1]; r != '\n' {
				t.Errorf("切點落在第 %d 個 rune，前一個字元是 %q 而不是換行——一行被從中間切開了",
					kept, string(r))
			}
		})
	}
}

// TestBootstrapTruncationHardCutsUnbreakableLine 釘住沒有行邊界可切時的退路：
// 使用者把整份文件寫成不換行的一大段時，硬切仍好過整份丟掉。
func TestBootstrapTruncationHardCutsUnbreakableLine(t *testing.T) {
	loader, dir := newLoader(t)
	content := runesWithLines('x', maxBootstrapRunes*2, 0) // width 0 = 單獨一行
	write(t, dir, "USER.md", content)

	got, err := loader.Bootstrap(context.Background(), allFiles)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if n := utf8.RuneCountInString(got.User); n > maxBootstrapRunes {
		t.Errorf("硬切後 %d rune，仍超過上限 %d", n, maxBootstrapRunes)
	}
	if kept := commonPrefixRunes(content, got.User); kept == 0 {
		t.Error("沒有行邊界時應硬切保留開頭，不該整份丟掉")
	}
}

// TestBootstrapTruncationDoesNotTouchFile 釘住「截斷只發生在讀取側」：磁碟上的
// 檔案一個 byte 都不能變。使用者寫的東西不會因為 OryxOS 讀了它而被裁掉。
func TestBootstrapTruncationDoesNotTouchFile(t *testing.T) {
	loader, dir := newLoader(t)
	content := runesWithLines('a', maxBootstrapRunes*3, 80)
	write(t, dir, "SOUL.md", content)

	before, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Bootstrap(context.Background(), allFiles); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}

	if string(before) != string(after) {
		t.Errorf("截斷動到了磁碟上的檔案：%d byte → %d byte", len(before), len(after))
	}
	if string(after) != content {
		t.Error("檔案內容與寫入時不符")
	}
}

// TestBootstrapLimitsAreIndependentPerFile 釘住三檔**各自獨立**計算上限、不共用
// 預算：三份各自略低於上限時，一份都不該被截斷。共用一個總預算的話，第二、三份
// 會被吃掉。
func TestBootstrapLimitsAreIndependentPerFile(t *testing.T) {
	const each = maxBootstrapRunes - 10
	loader, dir := newLoader(t)
	agents := runesWithLines('a', each, 80)
	user := runesWithLines('u', each, 80)
	soul := runesWithLines('s', each, 80)
	write(t, dir, "AGENTS.md", agents)
	write(t, dir, "USER.md", user)
	write(t, dir, "SOUL.md", soul)

	got, err := loader.Bootstrap(context.Background(), allFiles)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"AGENTS.md", got.Agents, agents},
		{"USER.md", got.User, user},
		{"SOUL.md", got.Soul, soul},
	} {
		if c.got != c.want {
			t.Errorf("%s 被截斷了（%d → %d rune）——三檔應各自獨立計算預算",
				c.name, utf8.RuneCountInString(c.want), utf8.RuneCountInString(c.got))
		}
	}
}

// commonPrefixRunes 回傳 orig 與 got 共同前綴的 rune 數。截斷後的內容必須是原內容
// 的前綴，這個長度就是「保留了多少」——不必去解析標記的措辭就問得出來。
func commonPrefixRunes(orig, got string) int {
	a, b := []rune(orig), []rune(got)
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// head／tail 回傳 s 的首／末 n 個 rune，供錯誤訊息使用（別把四千字倒進測試輸出）。
func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
