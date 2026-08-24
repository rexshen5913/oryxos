package tool

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 本檔是 shell 生命週期測試用的**探針模式**：測試把自己的二進制以探針名字連結進一個
// 臨時 PATH 目錄，shell 真的去執行它（憲法 4.3，不 mock exec）。
//
// **參數走 argv，不走環境變數。** shell 的 Env 是白名單式的（只有 PATH／HOME／LANG），
// 自訂環境變數傳不進子進程——既有的 MCP 探針靠 env 傳參，這裡不能照抄那一半。
const (
	// probeHangArg：`<probe> hang <startedMarker> <releaseFile>`。寫下 startedMarker
	// 表示「我真的被啟動了」，然後卡住直到 releaseFile 出現才退出。
	//
	// 「卡住」是這些測試的核心素材：admission slot 的上限、逾時的三道防線，驗的都是
	// 「一個不會自己結束的命令」下的行為，而測試必須能決定它**何時**結束。
	probeHangArg = "hang"

	// probeSpawnDescendantArg：`<probe> spawn-descendant <pidFile> <detached>`。
	// 生一個**繼承 stdout／stderr** 的後代、把它的 PID 寫進 pidFile，然後自己卡住。
	// detached 為 "1" 時讓那個後代自成 process group（脫離我們的射程）。
	probeSpawnDescendantArg = "spawn-descendant"

	// probeHangPollInterval 是探針等待 releaseFile 的輪詢間隔。取得夠短，讓「測試放行
	// 之後多久真的退出」不會變成別的斷言的雜訊。
	probeHangPollInterval = 10 * time.Millisecond

	// probeHangMaxLife 是探針的自我了斷上限。**這是測試衛生，不是被測行為**：萬一
	// 回收路徑整個失效，探針仍會自己退場，不把殘留進程留給後續測試或這台機器。
	probeHangMaxLife = 3 * time.Minute
)

// runShellLifecycleProbe 處理 shell 生命週期測試的探針模式，處理過回 true。
//
// 由 TestMain 在解析測試旗標**之前**呼叫（見 mcp_server_test.go 的說明）。
func runShellLifecycleProbe(args []string) bool {
	switch {
	case len(args) >= 3 && args[0] == probeHangArg:
		hangProbe(args[1], args[2])
		return true
	case len(args) >= 3 && args[0] == probeSpawnDescendantArg:
		spawnShellDescendant(args[1], args[2] == "1")
		// 生完後代自己也卡住：要驗的是「父進程被逾時砍掉時，後代怎麼樣」，父進程
		// 先自己退場的話砍的就不是同一件事了。releaseFile 給一個不會出現的路徑。
		hangProbe("", filepath.Join(os.TempDir(), "oryxos-shell-probe-never-released"))
		return true
	}
	return false
}

// hangProbe 寫下「我啟動了」的證據，然後等 releaseFile 出現才退出（exit code 0）。
//
// startedMarker 為空字串時不寫——spawn-descendant 那條路徑用 pidFile 當啟動證據。
func hangProbe(startedMarker, releaseFile string) {
	if startedMarker != "" {
		// 失敗只能忽略：這裡是子進程，沒有 *testing.T 可以回報。marker 沒寫出來的
		// 後果是「啟動了幾個」的斷言轉紅，那正確地指向有東西壞了。
		_ = os.WriteFile(startedMarker, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	deadline := time.Now().Add(probeHangMaxLife)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releaseFile); err == nil {
			return
		}
		time.Sleep(probeHangPollInterval)
	}
}
