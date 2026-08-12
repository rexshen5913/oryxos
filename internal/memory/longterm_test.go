// 長期記憶檔案層的單元測試。整份鏈路的行為由 internal/core 的整合測試從
// AgentService.Process seam 驅動；這裡補的是**單一 turn 的 seam 觀察不到**的
// 性質：條目邊界的組裝細節、並行追加、以及檔案操作不得越出 Workspace。
package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// memoryRelPath 是長期記憶檔在 Workspace 內的相對路徑。
const memoryRelPath = "memory/MEMORY.md"

// newTestMemory 在 t.TempDir() 開一個 Workspace 根，回傳長期記憶讀寫與該檔的
// 絕對路徑（供測試直接讀寫檔案做斷言）。
func newTestMemory(t *testing.T) (*LongTermMemory, string) {
	t.Helper()
	dir := t.TempDir()
	return openMemoryAt(t, dir), filepath.Join(dir, memoryRelPath)
}

// openMemoryAt 在 dir 上開一個**獨立**的 Workspace root 與長期記憶讀寫。對同一個
// dir 呼叫多次會得到彼此無關的實例（各自的 os.Root 與 mutex），用來模擬同一個
// Workspace 上並行的兩個 oryxos 進程。
func openMemoryAt(t *testing.T, dir string) *LongTermMemory {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("關閉 Workspace root: %v", err)
		}
	})
	return NewLongTermMemory(root, memoryRelPath)
}

// entries 組出一份 n 條、每條 runesPerEntry 個字的長期記憶（同一天、共用 header）。
func entries(n, runesPerEntry int) string {
	var b strings.Builder
	b.WriteString("## 2026-08-01\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- 第%02d條-%s\n", i, strings.Repeat("記", runesPerEntry))
	}
	return b.String()
}

// TestTruncateForInjection 是 prompt 注入路徑的截斷矩陣：超過 maxInjectRunes 時
// 保留**最近**內容、截斷點落條目邊界（日期 header 或列表項），不落 rune 或條目
// 中間。這是常態主線——即使單條寫入上限 1000 rune，累積數十條仍會超標。
func TestTruncateForInjection(t *testing.T) {
	tests := []struct {
		name    string
		content string
		// wantKept 是截斷後必須留下的子串；wantDropped 是必須被丟掉的。
		wantKept    []string
		wantDropped []string
		// wantMarker 為 true 時，結果須含自述省略標記（單條超標的 fallback）。
		wantMarker bool
	}{
		{
			name:     "未超閾值：原樣回傳",
			content:  "## 2026-08-01\n\n- 使用者的專案用 Go 開發\n",
			wantKept: []string{"## 2026-08-01", "使用者的專案用 Go 開發"},
		},
		{
			name:     "恰好 maxInjectRunes：原樣回傳",
			content:  strings.Repeat("記", maxInjectRunes),
			wantKept: []string{strings.Repeat("記", maxInjectRunes)},
		},
		{
			name:        "跨多條超標：丟最舊的、保留最近的",
			content:     entries(10, 500),
			wantKept:    []string{"第09條", "第08條"},
			wantDropped: []string{"第00條", "第01條"},
		},
		{
			name:        "單一條目自身超標：保留開頭硬切並附自述省略標記",
			content:     "## 2026-08-01\n\n- 開頭標記" + strings.Repeat("記", maxInjectRunes+500) + "\n",
			wantKept:    []string{"開頭標記"},
			wantDropped: []string{"## 2026-08-01"},
			wantMarker:  true,
		},
		{
			// MEMORY.md 是使用者可直接手改的檔案，寫成沒有列表項也沒有日期
			// header 的一段散文完全合法。它不是「一條記憶」，套用一般政策保留
			// **最近**的內容——若沿用單條超標的「保留開頭」，注入的會是最舊的
			// 內容、使用者最近寫的全部消失。
			name:        "整份沒有條目邊界（手寫散文）：保留結尾、省略開頭",
			content:     "最舊的開頭" + strings.Repeat("記", maxInjectRunes+500) + "最近的結尾",
			wantKept:    []string{"最近的結尾"},
			wantDropped: []string{"最舊的開頭"},
			wantMarker:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateForInjection(tt.content)

			if n := utf8.RuneCountInString(got); n > maxInjectRunes {
				t.Errorf("截斷後 %d 字, 超過上限 %d", n, maxInjectRunes)
			}
			for _, want := range tt.wantKept {
				if !strings.Contains(got, want) {
					t.Errorf("結果遺失應保留的內容 %q（結果 %d 字）", want, utf8.RuneCountInString(got))
				}
			}
			for _, drop := range tt.wantDropped {
				if strings.Contains(got, drop) {
					t.Errorf("結果仍含應被丟棄的內容 %q", drop)
				}
			}
			if tt.wantMarker {
				if !strings.Contains(got, "已省略") || !strings.Contains(got, "recall_memory") {
					t.Errorf("單條超標的結果未附自述省略標記（須含省略量與 recall_memory 提示）: %q", tail(got, 80))
				}
			} else if strings.Contains(got, "已省略") {
				t.Errorf("不該出現省略標記: %q", tail(got, 80))
			}
			// 截斷點必須落在條目邊界：結果的第一行是日期 header 或列表項。
			if !tt.wantMarker && utf8.RuneCountInString(tt.content) > maxInjectRunes {
				if first, _, _ := strings.Cut(got, "\n"); !isEntryBoundary(first) {
					t.Errorf("截斷點未落在條目邊界，首行 = %q", first)
				}
			}
		})
	}
}

// tail 回傳字串末尾的 n 個字，用來讓錯誤訊息不至於印出整份記憶。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// TestRecallMatches 是 recallByKeyword 的匹配矩陣：關鍵詞檢索回匹配行，回傳
// 總量同樣以 maxInjectRunes 為上限、截斷點落匹配行邊界並附明確標記。
func TestRecallMatches(t *testing.T) {
	const content = "## 2026-08-01\n\n" +
		"- 使用者的專案用 Go 開發\n" +
		"- 部署在 K8s\n" +
		"- 使用者偏好繁體中文回覆\n" +
		"## 2026-08-02\n\n" +
		"- Go 的測試一律表格驅動\n"

	tests := []struct {
		name        string
		content     string
		query       string
		wantLines   []string
		wantMissing []string
		wantMarker  bool
		// wantAlsoIn 是結果必須額外含有的子串（標記的細節）。
		wantAlsoIn []string
	}{
		{
			name:        "中文關鍵詞：多行匹配",
			content:     content,
			query:       "使用者",
			wantLines:   []string{"專案用 Go 開發", "偏好繁體中文回覆"},
			wantMissing: []string{"部署在 K8s"},
		},
		{
			name:        "英文關鍵詞大小寫不敏感",
			content:     content,
			query:       "go",
			wantLines:   []string{"專案用 Go 開發", "測試一律表格驅動"},
			wantMissing: []string{"部署在 K8s"},
		},
		{
			name:      "無匹配：回空",
			content:   content,
			query:     "Rust",
			wantLines: nil,
		},
		{
			// LLM 送來的關鍵詞幾乎都是多個詞，而多個詞連起來不會是任何一行的
			// 連續子字串——整串當子字串比對等於讓這個 Tool 永遠查不到東西（#14）。
			name:        "多關鍵詞：全部命中的行才算匹配",
			content:     content,
			query:       "使用者 Go",
			wantLines:   []string{"專案用 Go 開發"},
			wantMissing: []string{"偏好繁體中文回覆", "表格驅動", "部署在 K8s"},
		},
		{
			// 順序顛倒仍要匹配，這條直接證明比對的不是子字串。
			name:        "多關鍵詞：順序與行內順序相反仍匹配",
			content:     content,
			query:       "Go 使用者",
			wantLines:   []string{"專案用 Go 開發"},
			wantMissing: []string{"表格驅動"},
		},
		{
			// AND 而非 OR：關鍵詞越多結果應該越窄。OR 會讓「Go Kubernetes 訂單
			// 部署」把整份記憶倒回去，那不是檢索。
			name:      "多關鍵詞：只中一部分不算匹配",
			content:   content,
			query:     "使用者 Rust",
			wantLines: nil,
		},
		{
			name:        "多關鍵詞：分隔用的空白多寡不影響",
			content:     content,
			query:       "  使用者 \t  Go  ",
			wantLines:   []string{"專案用 Go 開發"},
			wantMissing: []string{"表格驅動"},
		},
		{
			// AND 的作用域是「同一行」：關鍵詞分散在不同行不算命中，否則檢索
			// 回來的行各自只沾到一個詞，合起來答非所問。
			name:      "多關鍵詞：分屬不同行不算匹配",
			content:   content,
			query:     "K8s 表格驅動",
			wantLines: nil,
		},
		{
			// 中文不做斷詞（要引依賴、且離線不可確定化），所以沒有空白的中文串
			// 仍走整串比對——這條釘住「只切空白」這個界線。
			name:      "未以空白分隔的中文串仍整串比對",
			content:   content,
			query:     "偏好繁體",
			wantLines: []string{"偏好繁體中文回覆"},
		},
		{
			// #14 的原始重現：Demo 二驗收時 Agent 就是這樣查不到自己剛存的記憶。
			name: "#14 重現：Demo 二驗收當下的查詢與記憶",
			content: "## 2026-08-12\n\n" +
				"- 使用者團隊後端統一使用 Go 語言，所有服務都部署在 Kubernetes (K8s) 上。\n",
			query:     "Go 語言 Kubernetes",
			wantLines: []string{"Kubernetes"},
		},
		{
			// 切完一個詞都不剩時絕不能「全部匹配」——strings.Contains(x, "") 恆真，
			// 少了這道防線會把整份記憶倒回給 LLM。
			name:      "只有空白的關鍵詞：不匹配任何行",
			content:   content,
			query:     " \t ",
			wantLines: nil,
		},
		{
			// 只切空白會在標點上破功，而繁中 LLM 送來的關鍵詞幾乎必然帶頓號或
			// 逗號——「語言、Kubernetes」整團不是任何一行的子字串，等於 #14 換個
			// 形式復發。標點也算分隔。
			name:      "標點黏著的關鍵詞：頓號也算分隔",
			content:   content,
			query:     "Go、使用者",
			wantLines: []string{"專案用 Go 開發"},
		},
		{
			name:      "標點黏著的關鍵詞：逗號與空白混用",
			content:   content,
			query:     "使用者, Go",
			wantLines: []string{"專案用 Go 開發"},
		},
		{
			// 記憶行裡的標點不該擋住命中：括號、句號都只是分隔，不進關鍵詞。
			name: "關鍵詞帶括號時仍命中行內的同一段文字",
			content: "## 2026-08-12\n\n" +
				"- 使用者團隊後端統一使用 Go 語言，所有服務都部署在 Kubernetes (K8s) 上。\n",
			query:     "Kubernetes (K8s)、Go",
			wantLines: []string{"Kubernetes"},
		},
		{
			// 英數字不該被拆開，否則 K8s 會變成 K 與 8s 兩個詞、命中一堆無關的行。
			name:        "英數混合詞不被拆開",
			content:     content,
			query:       "K8s",
			wantLines:   []string{"部署在 K8s"},
			wantMissing: []string{"專案用 Go 開發"},
		},
		{
			// 重複的關鍵詞在 AND 下是多餘的，去重不改變結果——同時讓掃描成本
			// 由**相異**詞數決定，而不是 LLM 送來幾個詞就掃幾遍。
			name:        "重複的關鍵詞不改變結果",
			content:     content,
			query:       "Go go GO 使用者 使用者",
			wantLines:   []string{"專案用 Go 開發"},
			wantMissing: []string{"表格驅動"},
		},
		{
			name:       "匹配總量超上限：保留最近的匹配並附截斷標記",
			content:    entries(20, 500),
			query:      "記",
			wantLines:  []string{"第19條"},
			wantMarker: true,
		},
		{
			name:       "手改造成的單行超長：硬切並附省略標記",
			content:    "- 超長行" + strings.Repeat("記", maxInjectRunes+500) + "\n",
			query:      "超長行",
			wantLines:  []string{"超長行"},
			wantMarker: true,
		},
		{
			// 最新一行自己就放不下時，仍要讓 LLM 知道「還有別的匹配」——否則
			// 它只看到一行被切掉的內容，不會想到換更精確的關鍵詞再查。
			name:       "最新一行超長且另有匹配被丟掉：標記須同時交代兩件事",
			content:    entries(3, 500) + "- 超長行" + strings.Repeat("記", maxInjectRunes+500) + "\n",
			query:      "記",
			wantLines:  []string{"超長行"},
			wantMarker: true,
			wantAlsoIn: []string{"未顯示"},
		},
		{
			// 命中位置決定要摘哪一段。固定保留行首的話，關鍵詞落在行尾就會回一段
			// 完全不含它的內容——看起來像答案、其實答非所問，而且重查也一樣。
			name:       "關鍵詞落在超長行的尾端：摘錄須涵蓋命中位置",
			content:    "- " + strings.Repeat("記", maxInjectRunes+1000) + "尾端關鍵詞\n",
			query:      "尾端關鍵詞",
			wantLines:  []string{"尾端關鍵詞"},
			wantMarker: true,
		},
		{
			// 詞距放得進窗口時就該全部保留。以最早命中為中心開窗會讓窗口有一半
			// 浪費在命中之前的內容上，把本來放得下的後詞裁掉——開窗要看的是
			// **所有命中的涵蓋區間**，不是單一命中點。
			name: "多關鍵詞詞距小於窗寬：全部命中都要保留",
			content: "- " + strings.Repeat("記", 2500) + "前詞" +
				strings.Repeat("憶", 2500) + "後詞" + strings.Repeat("念", 1000) + "\n",
			query:      "前詞 後詞",
			wantLines:  []string{"前詞", "後詞"},
			wantMarker: true,
		},
		{
			// 詞距超過窗寬時涵蓋不了全部，退回**最早命中**並從它往後展開——
			// 這是有界限的取捨。這一列有鑑別力：改以最後命中開窗，前詞會整個消失。
			name:        "多關鍵詞詞距超過窗寬：退回最早命中並往後展開",
			content:     "- 前詞" + strings.Repeat("記", maxInjectRunes+1000) + "後詞\n",
			query:       "前詞 後詞",
			wantLines:   []string{"前詞"},
			wantMissing: []string{"後詞"},
			wantMarker:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recallMatches(tt.content, tt.query)

			if n := utf8.RuneCountInString(got); n > maxInjectRunes {
				t.Errorf("回傳 %d 字, 超過上限 %d", n, maxInjectRunes)
			}
			if len(tt.wantLines) == 0 && got != "" {
				t.Errorf("無匹配應回空字串，實際 %q", tail(got, 80))
			}
			for _, want := range tt.wantLines {
				if !strings.Contains(got, want) {
					t.Errorf("回傳遺失匹配行 %q", want)
				}
			}
			for _, missing := range tt.wantMissing {
				if strings.Contains(got, missing) {
					t.Errorf("回傳含未匹配的行 %q", missing)
				}
			}
			if tt.wantMarker && !strings.Contains(got, "省略") && !strings.Contains(got, "未顯示") {
				t.Errorf("截斷後未附標記: %q", tail(got, 80))
			}
			for _, want := range tt.wantAlsoIn {
				if !strings.Contains(got, want) {
					t.Errorf("標記未交代 %q: %q", want, tail(got, 120))
				}
			}
		})
	}
}

// TestAbbreviate 釘死引述上限的邊界：省略號本身要算進上限，否則回傳長度是
// max+1，宣稱的上限就守不住。
func TestAbbreviate(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		max      int
		wantLen  int
		wantSame bool
	}{
		{name: "恰好等於上限：原樣回傳", in: strings.Repeat("查", 100), max: 100, wantLen: 100, wantSame: true},
		{name: "剛好超過上限：縮到上限（含省略號）", in: strings.Repeat("查", 101), max: 100, wantLen: 100},
		{name: "遠超上限", in: strings.Repeat("查", 5000), max: 100, wantLen: 100},
		{name: "短於上限：原樣回傳", in: "查", max: 100, wantLen: 1, wantSame: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := abbreviate(tt.in, tt.max)
			if n := utf8.RuneCountInString(got); n != tt.wantLen {
				t.Errorf("長度 = %d, 期望 %d", n, tt.wantLen)
			}
			if tt.wantSame && got != tt.in {
				t.Errorf("未超上限不該改動內容: %q", tail(got, 20))
			}
			if !tt.wantSame && !strings.HasSuffix(got, "…") {
				t.Errorf("縮短後未以省略號收尾: %q", tail(got, 20))
			}
		})
	}
}

// TestRecallByKeywordReadsFile 驗證檢索走真實檔案，且空白關鍵詞被擋下——
// 空字串的 strings.Contains 恆真，會把整份記憶當成「全部匹配」倒回去。
func TestRecallByKeywordReadsFile(t *testing.T) {
	mem, path := newTestMemory(t)
	mkdirTest(t, filepath.Dir(path))
	// 「後端語言是 Go」刻意讓關鍵詞落在行尾：前後帶空白的 query 若不 trim，
	// 這一行就匹配不到——用前後都有空格的句子當種子是測不出這件事的。
	writeTestFile(t, path, "## 2026-08-01\n\n- 使用者的專案用 Go 開發\n- 後端語言是 Go\n- 部署在 K8s\n")

	// 關鍵詞的前後空白在檢索裡不該有意義：LLM 產生的 JSON 常帶上它們，
	// 不 trim 就會在記憶明明存在時回報「沒有符合的內容」，而模型無從診斷。
	for _, query := range []string{"Go", " Go ", "\nGo"} {
		got, err := mem.RecallByKeyword(context.Background(), query)
		if err != nil {
			t.Fatalf("RecallByKeyword(%q): %v", query, err)
		}
		for _, want := range []string{"專案用 Go 開發", "後端語言是 Go"} {
			if !strings.Contains(got, want) {
				t.Errorf("RecallByKeyword(%q) 遺失 %q: %q", query, want, got)
			}
		}
		if strings.Contains(got, "K8s") {
			t.Errorf("RecallByKeyword(%q) 含未匹配的行: %q", query, got)
		}
	}

	// 切不出關鍵詞的 query 一律拒絕：空白、以及只有標點（切詞把標點當分隔，
	// 「、、、」切完同樣一個詞都不剩）。
	for _, query := range []string{"   ", "、，。"} {
		if _, err := mem.RecallByKeyword(context.Background(), query); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("切不出關鍵詞的 query %q 應以 ErrInvalidEntry 拒絕，實際 %v", query, err)
		}
	}

	// 關鍵詞過多一律拒絕、不截掉多餘的詞：AND 之下少幾個詞會讓結果變寬，回傳的
	// 就不是模型要求的那個查詢。錯誤訊息要能讓模型自己修（帶上實際數量與上限）。
	distinct := make([]string, 0, maxRecallTerms+1)
	for i := range maxRecallTerms + 1 {
		distinct = append(distinct, fmt.Sprintf("詞%d", i))
	}
	_, err := mem.RecallByKeyword(context.Background(), strings.Join(distinct, " "))
	if !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("相異關鍵詞超過 %d 個應以 ErrInvalidEntry 拒絕，實際 %v", maxRecallTerms, err)
	}
	if err != nil && !strings.Contains(err.Error(), "上限") {
		t.Errorf("拒絕訊息未告知上限，模型無從修正: %v", err)
	}

	// 判定看的是相異詞數：重複的詞不該把查詢擋下來。
	repeated := strings.TrimSpace(strings.Repeat("Go 專案 Go 專案 ", maxRecallTerms))
	if _, err := mem.RecallByKeyword(context.Background(), repeated); err != nil {
		t.Errorf("重複的關鍵詞不該觸發詞數上限，實際 %v", err)
	}
}

// TestLoadDoesNotModifyFile 守住「截斷只發生在讀取側」這個前提：Load 會把超標的
// 內容裁掉，但磁碟上的 MEMORY.md 一個 byte 都不能變。這不是潔癖——fallback 標記
// 敢寫「完整內容仍在 MEMORY.md」正是因為它確實還在；哪天有人把
// 截斷改成回寫檔案，那句提示就變成騙人的，而且使用者的長期記憶會被真的刪掉。
func TestLoadDoesNotModifyFile(t *testing.T) {
	mem, path := newTestMemory(t)
	mkdirTest(t, filepath.Dir(path))
	writeTestFile(t, path, entries(20, 500)) // 遠超注入上限
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}

	injected, err := mem.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(injected, "第00條") {
		t.Fatal("最舊的條目未被截掉——這個測試沒有測到截斷路徑")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("重讀 MEMORY.md: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("Load 改動了 MEMORY.md：%d bytes → %d bytes", len(before), len(after))
	}

	// 被注入截掉的舊條目仍檢索得回來——這正是截斷標記承諾使用者的事。
	got, err := mem.RecallByKeyword(context.Background(), "第00條")
	if err != nil {
		t.Fatalf("RecallByKeyword: %v", err)
	}
	if !strings.Contains(got, "第00條") {
		t.Error("被注入截掉的舊條目無法用 recall_memory 取回")
	}
}

// TestEntryBlock 是條目邊界的組裝矩陣：同一天共用日期 header、換日另起一段、
// 使用者手改後沒有換行收尾也接得上，且記憶內容裡自帶的 markdown 標題不會被
// 誤認成日期 header。
func TestEntryBlock(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		existing string
		entry    string
		want     string
	}{
		{
			name:     "首次寫入：起日期 header",
			existing: "",
			entry:    "使用者的專案用 Go 開發",
			want:     "## 2026-08-11\n\n- 使用者的專案用 Go 開發\n",
		},
		{
			name:     "同一天：共用既有 header，只補列表項",
			existing: "## 2026-08-11\n\n- 使用者的專案用 Go 開發\n",
			entry:    "部署在 K8s",
			want:     "- 部署在 K8s\n",
		},
		{
			name:     "換日：另起一段新的 header",
			existing: "## 2026-08-10\n\n- 使用者的專案用 Go 開發\n",
			entry:    "部署在 K8s",
			want:     "\n## 2026-08-11\n\n- 部署在 K8s\n",
		},
		{
			name:     "手改後沒有換行收尾：先補換行再接",
			existing: "## 2026-08-11\n\n- 使用者的專案用 Go 開發",
			entry:    "部署在 K8s",
			want:     "\n- 部署在 K8s\n",
		},
		{
			name:     "記憶內容自帶 markdown 標題：不當成日期 header",
			existing: "## 2026-08-11\n\n- 專案筆記\n## Go 服務\n",
			entry:    "部署在 K8s",
			want:     "- 部署在 K8s\n",
		},
		{
			name:     "只有非日期標題：視同沒有 header，另起一段",
			existing: "# 我的長期記憶\n",
			entry:    "部署在 K8s",
			want:     "\n## 2026-08-11\n\n- 部署在 K8s\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entryBlock(tt.existing, tt.entry, now); got != tt.want {
				t.Errorf("entryBlock = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestAppendPreservesExistingEntries 驗證追加不會動到既有內容——長期記憶的全部
// 價值就在於留得住，這條是 read-modify-write 覆寫式實作最容易毀掉的性質。
func TestAppendPreservesExistingEntries(t *testing.T) {
	mem, path := newTestMemory(t)
	entries := []string{"使用者的專案用 Go 開發", "部署在 K8s", "偏好繁體中文回覆"}
	for _, entry := range entries {
		if err := mem.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append(%q): %v", entry, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}
	for _, entry := range entries {
		if !strings.Contains(string(data), entry) {
			t.Errorf("MEMORY.md 遺失條目 %q: %s", entry, data)
		}
	}
	// 同一天寫的三條共用一個日期 header。
	if got := strings.Count(string(data), "## "); got != 1 {
		t.Errorf("日期 header 數 = %d, 期望 1（同一天共用）: %s", got, data)
	}
}

// TestAppendConcurrentAcrossInstances 驗證並行追加不遺失條目——這正是本張改用
// O_APPEND（而非「讀整檔、組新內容、覆寫」）所要換來的性質。
//
// **每個寫入者持有自己的 LongTermMemory**（自己的 os.Root 與 mutex），模擬同一個
// Workspace 上並行的兩個 oryxos 進程。共用一個實例是測不到東西的：實例內的 mutex
// 會把測試完全串行化，即使退回覆寫式寫入也照樣全綠。單一 turn 的 seam 同樣觀察
// 不到這件事——它要兩個以上的寫入者才成立。
func TestAppendConcurrentAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, memoryRelPath)
	const writers = 16

	mems := make([]*LongTermMemory, writers)
	for i := range writers {
		mems[i] = openMemoryAt(t, dir)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			errs[i] = mems[i].Append(context.Background(), fmt.Sprintf("並行條目-%02d", i))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("並行 Append(%d): %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 MEMORY.md: %v", err)
	}
	for i := range writers {
		if entry := fmt.Sprintf("並行條目-%02d", i); !strings.Contains(string(data), entry) {
			t.Errorf("並行追加遺失 %q: %s", entry, data)
		}
	}
}

// secretContent 是「不該被長期記憶讀走、也不該被它改寫」的目標檔內容。
const secretContent = "SUPER-SECRET-TOKEN"

// TestRejectsSymlinkedPath 驗證長期記憶只讀寫它自己那個固定目標：路徑上任何一段
// 是符號連結就拒絕跟隨。MEMORY.md 隨 Workspace 進 git，一個惡意 repo 或使用者的
// 一次手滑都可能讓它指向別的檔案——讀取端會把該檔內容注入 prompt 送往 Provider、
// 寫入端會把 markdown 追加進去把它弄壞。
//
// 越界的連結由 os.Root 擋，但 Workspace **內**的連結（如指向 profiles/default.yaml）
// os.Root 是照樣跟隨的，兩種都要測。
func TestRejectsSymlinkedPath(t *testing.T) {
	tests := []struct {
		name string
		// link 建好符號連結，回傳被指到、必須保持原樣的目標檔路徑。
		link func(t *testing.T, ws, memPath string) string
	}{
		{
			name: "最終檔連到 Workspace 外",
			link: func(t *testing.T, _, memPath string) string {
				target := filepath.Join(t.TempDir(), "secret.txt")
				writeTestFile(t, target, secretContent)
				mkdirTest(t, filepath.Dir(memPath))
				symlinkTest(t, target, memPath)
				return target
			},
		},
		{
			// 相對連結是關鍵：os.Root 本來就拒絕絕對的符號連結，但相對且
			// 指向 Workspace 內的連結它會照樣跟隨——這條只有 ensureNoSymlink 擋得住。
			name: "最終檔以相對路徑連到 Workspace 內的其他檔案",
			link: func(t *testing.T, ws, memPath string) string {
				target := filepath.Join(ws, "profiles", "default.yaml")
				mkdirTest(t, filepath.Dir(target))
				writeTestFile(t, target, secretContent)
				mkdirTest(t, filepath.Dir(memPath))
				symlinkTest(t, filepath.Join("..", "profiles", "default.yaml"), memPath)
				return target
			},
		},
		{
			name: "父目錄以相對路徑連到 Workspace 內的其他目錄",
			link: func(t *testing.T, ws, memPath string) string {
				target := filepath.Join(ws, "profiles", "default.yaml")
				mkdirTest(t, filepath.Dir(target))
				writeTestFile(t, target, secretContent)
				symlinkTest(t, "profiles", filepath.Dir(memPath))
				return target
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, memPath := newTestMemory(t)
			target := tt.link(t, mem.root.Name(), memPath)

			got, err := mem.Load(context.Background())
			if err == nil {
				t.Errorf("符號連結應讀取失敗，實際讀到 %q", got)
			}
			if strings.Contains(got, secretContent) {
				t.Errorf("目標檔內容被讀進長期記憶: %q", got)
			}

			if err := mem.Append(context.Background(), "覆寫測試"); err == nil {
				t.Error("符號連結應寫入失敗，實際成功")
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("讀取目標檔 %s: %v", target, err)
			}
			if string(data) != secretContent {
				t.Errorf("目標檔被改寫: %q", data)
			}
			// 連結指向的目錄裡也不該多出一個新檔。
			if _, err := os.Lstat(filepath.Join(filepath.Dir(target), "MEMORY.md")); err == nil {
				t.Errorf("經符號連結在 %s 建出了 MEMORY.md", filepath.Dir(target))
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("建立檔案 %s: %v", path, err)
	}
}

func mkdirTest(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建立目錄 %s: %v", dir, err)
	}
}

func symlinkTest(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("此環境不支援建立符號連結: %v", err)
	}
}
