package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 長度上限一律以 **rune** 計而非 byte——中文一字一 rune，用 byte 會讓中文體感
// 縮水，也可能切壞 UTF-8。兩者都取常數、不開配置欄位（YAGNI）：文檔只給預設值，
// 未定義配置面。
const (
	// maxInjectRunes 是長期記憶進 LLM 的總量上限。進 LLM 的輸入有兩條路徑——
	// prompt 注入走 Load、tool 結果回填走 RecallByKeyword——兩條都套這個上限。
	maxInjectRunes = 4000
	// maxEntryRunes 是 save_memory 單條記憶的長度上限。它是總注入上限的 1/4，
	// 確保注入預算至少容得下三四條近期記憶與日期 header 的開銷——這個關係是
	// 它的判準，不是孤立魔數。
	maxEntryRunes = maxInjectRunes / 4
)

// dateHeaderLayout 是每條記憶的日期 header 格式。header 讓使用者翻閱 MEMORY.md
// 時知道每條是何時記下的，也是日後截斷時的條目邊界之一。
const dateHeaderLayout = "2006-01-02"

// ErrInvalidEntry 標示 save_memory 的 content 未通過校驗——參數問題，不是瞬時
// 故障，因此回填時標 Retryable: false，不進 spec #1 的指數退避路徑。
var ErrInvalidEntry = errors.New("記憶條目不合法")

// LongTermMemory 是長期記憶的讀寫，底層是 Workspace 內的一個 Markdown 檔案
// （`.oryxos/memory/MEMORY.md`，技術方案 §5.2）。檔案是使用者可直接閱讀、手動
// 編輯、git 追蹤的純文字，格式刻意寬鬆——Agent 寫什麼由 LLM 自己理解。
//
// 所有檔案操作都經 root 進行，越界（含經符號連結指到 Workspace 之外）由
// os.Root 擋下：MEMORY.md 隨 Workspace 進 git，一個惡意的 repo 若把它做成指向
// 使用者敏感檔案的符號連結，讀取端會把該檔內容注入 prompt 送往 Provider、寫入
// 端則會覆寫它。root 的生命週期由組裝點持有，本型別不負責關閉。
//
// 本切片提供 Append 與 Load 兩個方法；recallByKeyword 與 truncateIfNeeded 隨
// recall_memory 落地（ticket #11）。
type LongTermMemory struct {
	root *os.Root
	name string // root 內的相對路徑
	// mu 串行化同一進程內的追加。跨進程的安全靠 O_APPEND（見 Append），
	// 但「建目錄→開檔→寫入」這串動作在進程內同時跑會互相絆倒，且沒有鎖時
	// 兩個 goroutine 會各自讀到舊內容、各補一個重複的日期 header。
	mu sync.Mutex
}

// NewLongTermMemory 以 Workspace 根與其內的相對路徑建立長期記憶讀寫；檔案與其
// 目錄都不預先建立，首次 Append 時才 lazy 建立——spec #1 init 出來的既有
// Workspace 免遷移直接可用。
func NewLongTermMemory(root *os.Root, name string) *LongTermMemory {
	return &LongTermMemory{root: root, name: name}
}

// Append 追加一條記憶並自動補上日期 header。content 先 TrimSpace 再校驗：空字串
// 與超過 maxEntryRunes 的內容一律拒絕且**不寫入檔案**，回傳包裝 ErrInvalidEntry
// 的錯誤（錯誤訊息寫成模型可遵循的規格句）。
//
// 拒絕而非自動截斷是刻意的：用 LLM 重新精簡的品質勝過任何機械式省略，且不默默
// 竄改使用者可直接閱讀的檔案（憲法 2.3）。
//
// 寫入是**真正的追加**（O_APPEND 只補新區塊），不是「讀整檔、組新內容、覆寫」
// ——後者會先截斷檔案，中途失敗（磁碟滿、進程被砍）就把使用者累積的長期記憶
// 整份毀掉，而長期記憶的全部價值就在於它留得住。既有內容只被讀來決定要不要
// 另起日期 header；這份讀取即使過時，最壞只是多出一個重複的 header（純觀感），
// 不會遺失任何條目。
func (m *LongTermMemory) Append(ctx context.Context, content string) error {
	// 檔案 I/O 沒有 ctx-aware 的標準庫 API；在阻塞動作前先看一眼取消訊號，
	// 讓已取消的 turn 不再往下寫（憲法 5.3）。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("追加長期記憶: %w", err)
	}

	entry := strings.TrimSpace(content)
	if entry == "" {
		return fmt.Errorf("%w：內容為空；請提供一條穩定的偏好或事實後重新呼叫 save_memory", ErrInvalidEntry)
	}
	if n := utf8.RuneCountInString(entry); n > maxEntryRunes {
		return fmt.Errorf("%w：內容 %d 字，超過單條上限 %d 字；請濃縮為一條穩定的偏好或事實後重新呼叫 save_memory",
			ErrInvalidEntry, n, maxEntryRunes)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.read()
	if err != nil {
		return err
	}
	f, err := m.openForAppend()
	if err != nil {
		return err
	}
	if _, err := f.WriteString(entryBlock(existing, entry, time.Now())); err != nil {
		return errors.Join(fmt.Errorf("寫入長期記憶 %s: %w", m.name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("關閉長期記憶 %s: %w", m.name, err)
	}
	return nil
}

// Load 回傳供注入 system prompt 的長期記憶，超過 maxInjectRunes 時截斷、保留
// 最近內容（見 truncateForInjection）。檔案不存在或內容為空白視為空記憶（回空
// 字串、不算錯誤），對話照常；權限不足、I/O 錯誤、越界的符號連結等真實故障以
// %w 包裝上拋，由呼叫端 fail 該 turn——把故障吞成空值會讓 Agent 在使用者不知情
// 下失憶。
//
// 截斷只發生在**讀取側**：檔案本身一個字都不動，被截掉的內容仍可用
// recall_memory 檢索回來。
func (m *LongTermMemory) Load(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("載入長期記憶: %w", err)
	}
	content, err := m.read()
	if err != nil {
		return "", err
	}
	return truncateForInjection(strings.TrimSpace(content)), nil
}

// RecallByKeyword 以關鍵詞檢索長期記憶，回傳匹配的行（總量同樣受 maxInjectRunes
// 限制，見 recallMatches）；沒有匹配時回空字串、不算錯誤。
//
// 方法名保留升級空間：擴展階段要加語義檢索時，這裡演進成帶 mode 參數的 recall
// （keyword ＋ semantic），上層的 recall_memory Tool 不必跟著改（技術方案 §5.1）。
//
// 關鍵詞先 TrimSpace 再用於匹配：LLM 送來的關鍵詞可能帶前後空白，而空白在
// 關鍵詞檢索裡不該有意義——`" Go "` 應該和 `"Go"` 查到同一批行。空白關鍵詞則
// 一律拒絕：`strings.Contains(x, "")` 恆真，會把整份記憶當成「全部匹配」倒回給
// LLM，那不是檢索。
func (m *LongTermMemory) RecallByKeyword(ctx context.Context, query string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("檢索長期記憶: %w", err)
	}
	needle := strings.TrimSpace(query)
	if needle == "" {
		return "", fmt.Errorf("%w：關鍵詞為空；請帶上要檢索的關鍵詞後重新呼叫 recall_memory", ErrInvalidEntry)
	}
	content, err := m.read()
	if err != nil {
		return "", err
	}
	return recallMatches(content, needle), nil
}

// openAttempts 是「開檔／補建目錄」的嘗試次數上限，見 openForAppend。
const openAttempts = 3

// openForAppend 以 O_APPEND 開啟長期記憶檔。先樂觀直接開，只有父目錄不存在時
// 才補建目錄再開——`oryxos init` 已建好 memory/，MkdirAll 只是給被刪掉的
// Workspace 用的後備，常態下一次就開成。
//
// 為什麼要重試而不是「補建一次就好」：實測 Go 1.25 的 os.Root.MkdirAll 在多個
// Root 並行建同一個目錄時，會回報成功但緊接著的 openat 仍拿到 ENOENT。同一個
// Workspace 上兩個 oryxos 進程同時第一次記事就會踩到，重試幾次即收斂。
func (m *LongTermMemory) openForAppend() (*os.File, error) {
	const flags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
	if err := m.ensureNoSymlink(); err != nil {
		return nil, err
	}

	dir := filepath.Dir(m.name)
	var err error
	for range openAttempts {
		var f *os.File
		if f, err = m.root.OpenFile(m.name, flags, 0o644); err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) || dir == "." {
			break
		}
		if mkErr := m.root.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("建立長期記憶目錄 %s: %w", dir, mkErr)
		}
	}
	return nil, fmt.Errorf("開啟長期記憶 %s: %w", m.name, err)
}

// ensureNoSymlink 檢查 name 的每一段路徑元件（含最終檔案）都不是符號連結。
//
// os.Root 只擋「指到 Workspace **之外**」，Workspace 內的符號連結它照樣跟隨。
// 但 `memory/MEMORY.md -> ../profiles/default.yaml` 一樣有害：讀取端會把 Profile
// 內容當記憶注入 prompt，寫入端會把 markdown 追加進 YAML 把它弄壞。長期記憶只
// 該讀寫它自己那個固定目標，路徑上出現符號連結一律拒絕、不猜使用者的意圖。
//
// 這是 TOCTOU-racy 的檢查，但要防的是**靜態植入**的符號連結（隨 Workspace 進
// git 的那種），不是即時競爭的攻擊者；沒有可攜的 O_NOFOLLOW 能用（Windows 無此
// 旗標，而單一靜態二進制要跨平台）。
func (m *LongTermMemory) ensureNoSymlink() error {
	parts := strings.Split(filepath.ToSlash(m.name), "/")
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		info, err := m.root.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			return nil // 尚未建立的路徑不可能是符號連結
		}
		if err != nil {
			return fmt.Errorf("檢查長期記憶路徑 %s: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("長期記憶路徑 %s 是符號連結，拒絕跟隨（它只能是 Workspace 內的實體檔案）", component)
		}
	}
	return nil
}

// read 讀回長期記憶檔的原始內容；檔案不存在回空字串。
func (m *LongTermMemory) read() (string, error) {
	if err := m.ensureNoSymlink(); err != nil {
		return "", err
	}
	data, err := m.root.ReadFile(m.name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("讀取長期記憶 %s: %w", m.name, err)
	}
	return string(data), nil
}

// entryBlock 回傳要追加到 existing 之後的**新區塊**（不含既有內容）：同一天的
// 多條記憶共用一個日期 header（檔案是給人讀的，重複 header 只會礙眼），換日或
// 首次寫入才起新的一段。
func entryBlock(existing, entry string, now time.Time) string {
	header := "## " + now.Format(dateHeaderLayout)
	item := "- " + entry + "\n"
	switch {
	case existing == "":
		return header + "\n\n" + item
	case lastDateHeader(existing) == header:
		return lineBreak(existing) + item
	default:
		return lineBreak(existing) + "\n" + header + "\n\n" + item
	}
}

// lineBreak 回傳把游標帶到新一行所需的換行——使用者手改後可能沒有以換行收尾。
func lineBreak(existing string) string {
	if existing == "" || strings.HasSuffix(existing, "\n") {
		return ""
	}
	return "\n"
}

// lastDateHeader 回傳內容中最後一個日期 header（形如 `## 2026-08-11`）；沒有則
// 回空字串。
func lastDateHeader(content string) string {
	for _, line := range slices.Backward(strings.Split(content, "\n")) {
		if isDateHeader(line) {
			return strings.TrimRight(line, " \t")
		}
	}
	return ""
}

// isDateHeader 判斷一行是不是日期 header。只認日期解析得出來的行：記憶內容本身
// 是自由文字，裡頭出現 `## 某某標題` 是常態，把它當條目邊界會誤判。
func isDateHeader(line string) bool {
	date, ok := strings.CutPrefix(strings.TrimRight(line, " \t"), "## ")
	if !ok {
		return false
	}
	_, err := time.Parse(dateHeaderLayout, date)
	return err == nil
}
