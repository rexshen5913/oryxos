package config

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Bootstrap 三檔在 Workspace 根的檔名（技術方案 §8.3）。`oryxos init` 自 spec #1
// 起就把這三份模板建出來，檔案裡也寫著「內容之後會載入 Agent 的系統提示詞」。
//
// 檔名是常數而非配置：本切片一律載入這三份。由 Profile 的 bootstrap 欄位挑選要
// 載入哪些屬另一張票，那時這組常數會變成「省略時的預設集合」。
const (
	agentsFile = "AGENTS.md"
	userFile   = "USER.md"
	soulFile   = "SOUL.md"
)

// BootstrapLoader 從 Workspace 讀取 Bootstrap 上下文，實作 core.ContextLoader。
//
// 所有檔案操作都經 root 進行，越界（含經符號連結指到 Workspace 之外）由 os.Root
// 擋下——理由與長期記憶那份相同、而且更強：這三份檔案隨 Workspace 進 git，一個
// 惡意的 repo 若把 AGENTS.md 做成指向使用者敏感檔案的符號連結，讀取端會把該檔
// 內容注入 system prompt 送往 Provider。root 的生命週期由組裝點持有，本型別不
// 負責關閉。
type BootstrapLoader struct {
	root *os.Root
}

// NewBootstrapLoader 以 Workspace 根建立 Bootstrap 載入器。三份檔案都不預先建立
// 也不檢查存在——缺檔視為該層為空是既定行為，spec #1 init 出來的既有 Workspace
// 免遷移直接可用。
func NewBootstrapLoader(root *os.Root) *BootstrapLoader {
	return &BootstrapLoader{root: root}
}

// Bootstrap 讀回一份 Bootstrap 快照。每次呼叫都真的讀檔——不緩存，使用者手改
// 下一個 turn 就生效（載入頻率由呼叫端決定，見 core.ContextLoader）。
func (l *BootstrapLoader) Bootstrap(ctx context.Context, wantSoul bool) (core.BootstrapContext, error) {
	var boot core.BootstrapContext
	files := []struct {
		name string
		into *string
	}{
		{agentsFile, &boot.Agents},
		{userFile, &boot.User},
	}
	// SOUL.md 只在呼叫端真的需要時才讀（見 core.ContextLoader）。
	if wantSoul {
		files = append(files, struct {
			name string
			into *string
		}{soulFile, &boot.Soul})
	}

	for _, f := range files {
		// 每讀一份前檢查一次：取消要能在中途生效，不是只在進門時看一眼
		// （憲法 5.3）。
		if err := ctx.Err(); err != nil {
			return core.BootstrapContext{}, fmt.Errorf("載入 Bootstrap 上下文: %w", err)
		}
		content, err := l.read(f.name)
		if err != nil {
			return core.BootstrapContext{}, err
		}
		*f.into = content
	}
	return boot, nil
}

// read 讀回一份 Bootstrap 檔案的內容；檔案不存在回空字串（視為該層為空）。
//
// 「不存在」與「讀不到」是**不同**的事：前者是常態（使用者只寫其中一兩份），
// 後者是真實故障（權限不足、路徑不是普通檔、I/O 錯誤），必須以錯誤上拋讓呼叫端
// fail 該 turn——把故障吞成空值會讓 Agent 在使用者不知情下失去上下文。
func (l *BootstrapLoader) read(name string) (string, error) {
	info, err := l.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("檢查 Bootstrap 檔案 %s: %w", name, err)
	}
	// 符號連結一律拒絕、不跟隨。os.Root 已經擋掉指向 Workspace 之外的連結，這裡
	// 連指向 Workspace 之內的也拒絕：Bootstrap 是「使用者手寫的實體檔案」，一個
	// 需要解析連結才讀得到的檔案不符合那個語義，而它的內容會被送往 Provider。
	//
	// 已知殘留：Lstat 與 ReadFile 是兩個 syscall，兩者之間檔案被換成連結的話這道
	// 檢查會被繞過（TOCTOU）。要真正關上得用 O_NOFOLLOW 開檔，但那個 flag 在
	// Windows 不存在、會破壞跨平台建置（spec #2 才為 Windows 路徑處理付過代價）。
	// 殘留風險由 os.Root 界定在 Workspace 之內，且攻擊者得在使用者自己的
	// Workspace 裡贏得競態——不值得為此引入平台分歧。
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Bootstrap 檔案 %s 是符號連結，拒絕跟隨（它只能是 Workspace 內的實體檔案）", name)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Bootstrap 檔案 %s 不是普通檔（實際為 %s）", name, info.Mode().Type())
	}

	data, err := l.root.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("讀取 Bootstrap 檔案 %s: %w", name, err)
	}
	// 未經編輯的舊版出廠模板視為空——升級後的既有 Workspace 不該把當初那段
	// 說明文字當成真指令注入（見 legacy_bootstrap.go）。
	if content := string(data); !isUneditedLegacyTemplate(name, content) {
		return content, nil
	}
	return "", nil
}
