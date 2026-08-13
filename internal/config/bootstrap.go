package config

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Bootstrap 三檔就住在 Workspace 根（技術方案 §8.3）——本 package 知道的是這個
// **位置**，檔名本身是 Profile bootstrap 欄位的合法值、由 core 定義（單一來源）。
const (
	agentsFile = core.BootstrapAgentsFile
	userFile   = core.BootstrapUserFile
	soulFile   = core.BootstrapSoulFile
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

// selectedFiles 回傳這組選擇要碰的檔名，順序固定（讓錯誤訊息可預期；與 prompt 的
// 拼接順序無關，後者由 core 的 composeSystemPrompt 決定）。
//
// 載入與啟動校驗共用它：兩處看到的「哪些檔案算數」因此必然一致。分開寫的話，一份
// 被 ADR-0003 互斥排除的 SOUL.md 會出現「壞掉可以跑、缺檔卻起不來」這種說不通的
// 不對稱——同一份用不到的檔案不該有兩種待遇。
func selectedFiles(sel core.BootstrapSelection) []string {
	names := make([]string, 0, 3)
	if sel.Agents {
		names = append(names, agentsFile)
	}
	if sel.User {
		names = append(names, userFile)
	}
	if sel.Soul {
		names = append(names, soulFile)
	}
	return names
}

// ValidateBootstrapFiles 校驗這組選擇裡**明確要求**的 Bootstrap 檔案都存在，供組裝
// 點在啟動時呼叫。sel.Explicit 為假（bootstrap 欄位省略）時不做任何檢查——那是
// 「載入預設三檔」，缺檔視為該層為空。
//
// 這是載入路徑那條規則的**提前**回報，不是它的替代：真正的把關在 BootstrapLoader
// 的每個 turn（見 core.BootstrapSelection.Explicit）。提前一步的價值是使用者連
// 一句話都還沒打就知道設定錯了，不必等到第一個 turn 才發現——AC 要的「啟動即報錯」
// 正是這個。
//
// 這裡只回答「在不在」一個問題。符號連結、非普通檔、權限不足都留給讀取路徑處理
// ——它已經對每一種都有清楚的錯誤訊息，在這裡再定義一次「什麼算可用的檔案」會變成
// 兩個來源。名稱是否合法則更早、在 core 的 Profile 校驗就擋掉了。
func ValidateBootstrapFiles(root *os.Root, sel core.BootstrapSelection) error {
	if !sel.Explicit {
		return nil
	}
	for _, name := range selectedFiles(sel) {
		if _, err := root.Lstat(name); err != nil {
			return fmt.Errorf("bootstrap 列出的 %s 不存在或無法存取: %w", name, err)
		}
	}
	return nil
}

// Bootstrap 讀回一份 Bootstrap 快照。每次呼叫都真的讀檔——不緩存，使用者手改
// 下一個 turn 就生效（載入頻率由呼叫端決定，見 core.ContextLoader）。
func (l *BootstrapLoader) Bootstrap(ctx context.Context, sel core.BootstrapSelection) (core.BootstrapContext, error) {
	var boot core.BootstrapContext
	// 只讀被選中的那些——沒選中的**完全不碰**，壞掉也不該讓這個 turn 失敗
	// （見 core.BootstrapSelection）。
	into := map[string]*string{
		agentsFile: &boot.Agents,
		userFile:   &boot.User,
		soulFile:   &boot.Soul,
	}

	for _, name := range selectedFiles(sel) {
		// 每讀一份前檢查一次：取消要能在中途生效，不是只在進門時看一眼
		// （憲法 5.3）。
		if err := ctx.Err(); err != nil {
			return core.BootstrapContext{}, fmt.Errorf("載入 Bootstrap 上下文: %w", err)
		}
		content, err := l.read(name, sel.Explicit)
		if err != nil {
			return core.BootstrapContext{}, err
		}
		*into[name] = content
	}
	return boot, nil
}

// read 讀回一份 Bootstrap 檔案的內容。
//
// 「不存在」與「讀不到」是**不同**的事：後者是真實故障（權限不足、路徑不是普通檔、
// I/O 錯誤），一律以錯誤上拋讓呼叫端 fail 該 turn——把故障吞成空值會讓 Agent 在
// 使用者不知情下失去上下文。
//
// 「不存在」則看 mustExist：Profile 明確列出的檔案缺一份就是設定錯誤（每個 turn
// 都判，不是只在啟動時），省略欄位時的預設三檔缺檔則視為該層為空、對話照常
// （使用者只寫其中一兩份是常態）。**每個 turn 都判**是必要的：Bootstrap 每個 turn
// 重讀，啟動後才被刪掉的檔案若在這裡被當成空值，Agent 就會安靜地少掉一段明確要求
// 的上下文——那正是 fail fast 要避免的「半殘運作、對話中途才發現」。
func (l *BootstrapLoader) read(name string, mustExist bool) (string, error) {
	info, err := l.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if mustExist {
			return "", fmt.Errorf("Profile 的 bootstrap 明確列出的 %s 不存在: %w", name, err)
		}
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
