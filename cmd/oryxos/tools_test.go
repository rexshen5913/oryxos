// `oryxos tools` 在組裝點的測試（issue #27 方向二）。
//
// 這個命令要回答的是使用者接一台新 MCP server 時的第一個問題：**Profile 的 tools 欄位
// 該寫什麼名字**。在此之前沒有任何查詢途徑——#24 驗收時得另外寫一支 Python 腳本起子
// 進程、送 initialize 與 tools/list，才知道 github server 那 26 個工具叫什麼。
//
// MCP 那一側一律起真實的本地 stdio server 子進程（見 mcp_server_test.go），不 mock 協議
// （憲法 4.3、ADR-0002）。這個命令不呼叫 LLM，所以沒有回放伺服器。
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestToolsListsEverythingWritableIntoProfile 是本命令的主場景：**列出來的東西就是
// Profile 的 tools 可以寫的東西**，而且帶著描述。
//
// 內建 Tool 與 MCP 工具一起列，因為它們本來就在同一個 Registry 裡，而使用者真正的問題
// 是「tools 可以寫什麼」——只列 MCP 的話，他仍然不知道 recall_memory 叫什麼名字。
//
// 描述必須跟著出來：一台 server 給 26 個名字相近的工具（list_pull_requests、
// get_pull_request、merge_pull_request），光看名字挑不出要哪一個。
func TestToolsListsEverythingWritableIntoProfile(t *testing.T) {
	// base_url 指向一個不存在的位址：這個命令不該碰 Provider，若哪天碰了會立刻現形。
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "github", "list_pull_requests", "merge_pull_request"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - github\ntools:\n  - github__list_pull_requests\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("oryxos tools: %v", err)
	}
	got := out.String()

	// MCP 工具：**含 Profile 沒列到的那些**。沒列到的才是使用者需要查的。
	for _, want := range []string{"github__list_pull_requests", "github__merge_pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("輸出沒有 MCP 工具 %s:\n%s", want, got)
		}
	}
	// 內建 Tool、Memory Tool、原生 Go Tool 示例都在同一個 Registry，一起列才回答得了
	// 「tools 可以寫什麼」。
	// 三個 File Tool 與 shell 在這裡不需要任何額外接線：它們經 RegisterBuiltins 進
	// 同一個 Registry，於是自動被列出來——「新的內建 Tool 不另闢鏈路」這句話的斷言
	// 就是這一格。使用者要據這份清單知道 Profile 的 tools 欄位可以寫哪些名字，所以
	// **用途說明也要跟著出來**（下面那格）。
	for _, want := range []string{"http_get", "read_file", "write_file", "list_dir", "shell", "save_memory", "recall_memory", "text_stats", "load_skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("輸出沒有內建 Tool %s:\n%s", want, got)
		}
	}
	// 描述：光有名字挑不出要哪一個。
	if !strings.Contains(got, "把收到的 text 原樣回覆") {
		t.Errorf("輸出沒有 MCP 工具的描述:\n%s", got)
	}
	// 四個新 Tool 的**用途**也要列得出來（US 41）：使用者是靠這一欄決定 tools 欄位
	// 要寫哪幾個名字的，只有名字等於要他自己猜 write_file 會不會追加、shell 支不支援
	// 管線。比對各自描述裡最有辨識度的那句。
	for _, want := range []string{
		"回傳內容與是否被截斷",   // read_file
		"覆寫該檔案原有的全部內容", // write_file
		"是否為目錄與大小",     // list_dir
		"不經 shell 直譯器", // shell
	} {
		if !strings.Contains(got, want) {
			t.Errorf("輸出沒有帶出新內建 Tool 的用途說明 %q:\n%s", want, got)
		}
	}
}

// toolLine 回傳輸出裡含 name 的那一行，用來斷言該行**自己**的標記。
//
// 逐行取而不是對整段 Contains：這裡要驗的是「哪些有標記、哪些沒有」，對整段做
// Contains("✓") 的話，只要輸出裡任何一行有標記就會通過。
func toolLine(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("輸出沒有 %s:\n%s", name, out)
	return ""
}

// TestToolsGroupsBySourceAndMarksProfileTools 釘住輸出的兩個可讀性性質。
//
// **按來源分組**：使用者跑這個命令的當下多半剛接上一台新 server，想看的是「**這台**
// 給了我什麼」。五十個名字混在一份字母序清單裡，那個問題答不出來。
//
// **標記 Profile 已經列了哪些**：接第二台 server 時，真正要找的是「還沒加進去的是哪些」。
// 沒有標記的話使用者得自己拿 Profile 逐行比對，而那正是這個命令該替他做的事。
//
// 兩台 server 而不是一台：一台的話「各自成組」與「全部倒在同一個標題底下」長得一樣。
func TestToolsGroupsBySourceAndMarksProfileTools(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "github", "list_pull_requests", "merge_pull_request")+
		testMcpServerEntry(t, "slack", "post_message"))
	// Profile 只列了兩台各一個工具：其餘的正是使用者要查的。
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - github\n  - slack\n"+
		"tools:\n  - github__list_pull_requests\n  - save_memory\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("oryxos tools: %v", err)
	}
	got := out.String()

	// 每台 server 各自成組，且組標題帶著 server 名——使用者要照著它寫 tools 的前綴。
	githubHdr := strings.Index(got, "MCP server: github")
	slackHdr := strings.Index(got, "MCP server: slack")
	if githubHdr < 0 || slackHdr < 0 {
		t.Fatalf("輸出沒有按 server 分組:\n%s", got)
	}
	// github 的工具落在 github 那一組裡，沒有溢到 slack 組去。
	if i := strings.Index(got, "github__merge_pull_request"); i < githubHdr || i > slackHdr {
		t.Errorf("github__merge_pull_request 沒有落在 github 那一組裡:\n%s", got)
	}
	if i := strings.Index(got, "slack__post_message"); i < slackHdr {
		t.Errorf("slack__post_message 沒有落在 slack 那一組裡:\n%s", got)
	}

	// ✓ 只標在 Profile 真的列了的那些上。
	marked := []string{"github__list_pull_requests", "save_memory"}
	unmarked := []string{"github__merge_pull_request", "slack__post_message", "http_get"}
	for _, name := range marked {
		if line := toolLine(t, got, name); !strings.Contains(line, "✓") {
			t.Errorf("%s 已在 Profile 的 tools 裡，卻沒有標記:\n%s", name, line)
		}
	}
	for _, name := range unmarked {
		if line := toolLine(t, got, name); strings.Contains(line, "✓") {
			t.Errorf("%s 不在 Profile 的 tools 裡，卻被標成已加入:\n%s", name, line)
		}
	}
	// 標記本身要有說明，否則使用者不知道那個符號是什麼意思。
	if !strings.Contains(got, "default") {
		t.Errorf("輸出沒有說明標記是相對於哪一份 Profile:\n%s", got)
	}
}

// TestToolsDegradesWhenServerUnavailable 釘住**一台 server 連不上不會讓整個命令失敗**。
//
// 沿用 #22 在 chat 建立的降級語義：連不上就跳過、警示可見、其餘照常。對這個命令來說
// 更要如此——使用者往往正是**因為某台 server 有問題**才來跑它。整個命令失敗的話，
// 他連還活著的那幾台有什麼工具都看不到，等於在最需要診斷資訊的時候把資訊收走。
func TestToolsDegradesWhenServerUnavailable(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "alive", "echo")+
		"  broken:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-server]\n")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - alive\n  - broken\ntools:\n  - alive__echo\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("一台 server 連不上不該讓整個命令失敗: %v", err)
	}
	got := out.String()

	// 1. 連不上這件事看得見，而且指名是哪一台、原因是什麼。
	if !strings.Contains(got, "警告") || !strings.Contains(got, "broken") {
		t.Errorf("輸出沒有可見的連線失敗警示:\n%s", got)
	}
	if !strings.Contains(got, "/nonexistent/oryxos-mcp-server") {
		t.Errorf("警示沒有帶上原始錯誤，使用者不知道要修什麼:\n%s", got)
	}
	// 2. 活著那台的工具照樣列得出來——降級的範圍剛好是壞掉的那一台。
	if !strings.Contains(got, "alive__echo") {
		t.Errorf("活著的 server 的工具沒有列出來:\n%s", got)
	}
	// 3. 內建 Tool 完全不受外部 server 影響。
	if !strings.Contains(got, "save_memory") {
		t.Errorf("內建 Tool 沒有列出來:\n%s", got)
	}
}

// TestToolsClosesMcpSubprocessesOnExit 釘住**命令退出前把 MCP 子進程收乾淨**。
//
// 這是 chat 沒有的風險形態：chat 起來之後長時間活著，子進程的存在是預期的；而 tools
// 印完就退，忘了收尾的話每跑一次就留一隻孤兒，而且完全沒有跡象——直到使用者某天發現
// 一堆 node 進程掛在那裡。
//
// marker 由 server 在 stdin 被關閉（MCP stdio 的「請你收工」訊號）而退出前寫下，所以
// 它存在即代表整條收尾走完，不只是「我們沒有 crash」。
func TestToolsClosesMcpSubprocessesOnExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive.exited")
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntryWithExitMarker(t, "alive", marker, "echo"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - alive\ntools:\n  - alive__echo\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("oryxos tools: %v", err)
	}
	// 先確認子進程真的起來過，否則下面那條會因為「根本沒 spawn」而假綠。
	if !strings.Contains(out.String(), "alive__echo") {
		t.Fatalf("server 沒有連上，這條測試驗不到收尾:\n%s", out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("命令返回時 MCP 子進程還沒被收掉（marker %s 不存在）——每跑一次就留一隻孤兒", marker)
	}
}

// TestChatUnknownToolErrorPointsAtToolsCommand 是方向一在組裝點的完整故事：**打錯字的
// 當下就看得到正確答案，以及去哪裡看完整清單**。
//
// 形態取自 #24 真實踩到的那次——`github__list_prs`，把 pull_requests 記成了 prs。
// 在此之前錯誤只說「未註冊」，使用者的下一步是翻文件或自己寫腳本；現在同一個錯誤同時
// 給出這台 server 真正提供的名字，以及 `oryxos tools`。
//
// 白名單本身沒有放寬：這裡仍然**擋下啟動**，第一條斷言就是這件事。
func TestChatUnknownToolErrorPointsAtToolsCommand(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "github", "list_pull_requests", "merge_pull_request"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - github\ntools:\n  - github__list_prs\n")

	var out bytes.Buffer
	err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "hi"})
	if err == nil {
		t.Fatal("引用未註冊的 Tool 應該擋下啟動——白名單被放寬了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "github__list_prs") {
		t.Errorf("錯誤沒有指出是哪個名字寫錯了:\n%s", msg)
	}
	if !strings.Contains(msg, "github__list_pull_requests") {
		t.Errorf("錯誤沒有列出這台 server 真正提供的工具名:\n%s", msg)
	}
	if !strings.Contains(msg, "oryxos tools") {
		t.Errorf("錯誤沒有指向查詢途徑，使用者仍不知道下一步該做什麼:\n%s", msg)
	}
}

// TestToolsWorksEvenWhenProfileToolsAreWrong 釘住**這個命令不能被它要診斷的問題擋住**。
//
// 使用者跑它的時機，正是 Profile 的 tools 寫錯、chat 起不來的那一刻。若這個命令也對
// 未註冊的名字報錯，兩邊會互相指向對方——chat 說「執行 oryxos tools」，tools 說「這個
// Tool 未註冊」——使用者卡死在中間，而這個命令存在的唯一理由就沒了。
//
// 機制上它靠的是**不呼叫 Registry.Subset**：Subset 是「哪些進得了這個 Agent 的可用
// 子集」的白名單校驗，而列清單不需要那道關。這條測試守的就是這個區分。
func TestToolsWorksEvenWhenProfileToolsAreWrong(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "github", "list_pull_requests"))
	// 兩種寫錯都放進來：MCP 工具名打錯、以及一個根本不存在的內建 Tool。
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - github\ntools:\n  - github__list_prs\n  - no_such_tool\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("Profile 的 tools 寫錯時這個命令仍必須可用，否則使用者卡死在兩個命令中間: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "github__list_pull_requests") {
		t.Errorf("沒有列出正確的工具名，使用者拿不到他要的答案:\n%s", got)
	}
	// ✓ 是「Profile 已經列了」，不是「Profile 想列」：寫錯的名字不該讓正確的那個被標記，
	// 否則使用者會以為自己沒寫錯。
	if line := toolLine(t, got, "github__list_pull_requests"); strings.Contains(line, "✓") {
		t.Errorf("Profile 寫的是 github__list_prs，正確的那個不該被標成已加入:\n%s", line)
	}
}

// TestToolsWorksWithoutProviderCredentials 釘住**沒設 API key 也查得到工具**。
//
// 這是最可能跑這個命令的時刻：剛 `oryxos init`、正在拼 Profile，Provider 的憑證還沒設。
// 列工具與 LLM 無關，被 Provider 憑證擋下等於在使用者最需要它的時候把門關上。
//
// 沒有這條的話，setupChatWorkspace 的 t.Setenv 會一直把這個缺陷蓋住——其餘每一條
// tools 測試都在「key 已設定」的環境裡跑。
func TestToolsWorksWithoutProviderCredentials(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "github", "list_pull_requests"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - github\ntools:\n  - github__list_pull_requests\n")
	// config.yaml 的 api_key 是 ${OPENROUTER_API_KEY}，unset 之後就是 stock Workspace
	// 剛建好的狀態。t.Setenv 已註冊測試結束時還原。
	os.Unsetenv("OPENROUTER_API_KEY")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("沒設 Provider 憑證時仍必須查得到工具: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "github__list_pull_requests") {
		t.Errorf("沒有列出 MCP 工具:\n%s", got)
	}
}

// TestToolsGroupsOverlappingServerPrefixesCorrectly 釘住**前綴重疊時分組不串台**。
//
// server 名沒有字元限制，`foo` 與 `foo__bar` 可以同時宣告；`foo__bar__echo` 若以第一個
// 匹配到的前綴歸類，就會被掛到 foo 底下。使用者照著分組標題抄前綴，抄到的是錯的那台。
func TestToolsGroupsOverlappingServerPrefixesCorrectly(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "foo", "alpha")+
		testMcpServerEntry(t, "foo__bar", "echo"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - foo\n  - foo__bar\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("oryxos tools: %v", err)
	}
	got := out.String()

	// 標題帶換行才分得開：「MCP server: foo」是「MCP server: foo__bar」的前綴。
	fooHdr := strings.Index(got, "MCP server: foo\n")
	barHdr := strings.Index(got, "MCP server: foo__bar\n")
	if fooHdr < 0 || barHdr < 0 {
		t.Fatalf("兩台 server 沒有各自成組:\n%s", got)
	}
	if i := strings.Index(got, "foo__bar__echo"); i < barHdr {
		t.Errorf("foo__bar__echo 被歸到了別台 server 底下（foo__bar 的組標題在第 %d 字元，它在第 %d）:\n%s",
			barHdr, i, got)
	}
	if i := strings.Index(got, "foo__alpha"); i < fooHdr || i > barHdr {
		t.Errorf("foo__alpha 沒有落在 foo 那一組裡:\n%s", got)
	}
}

// TestChatKeepsHealthyServerToolsWhenPrefixOverlaps 釘住**降級不得誤傷健康 server**。
//
// `foo` 連不上、`foo__bar` 好好的，而 Profile 用的是 `foo__bar__echo`。以字串前綴判斷
// 歸屬的話，`foo__bar__echo` 會被當成 `foo` 的工具——於是一台**根本沒壞**的 server 的
// 工具被警告成不可用，還從 Agent 的可用子集裡刪掉。那不只是訊息難看，是能力真的少了。
//
// 判準必須是「這個名字有沒有被註冊」，不是「名字長得像誰的」：註冊了就代表有一台健康
// 的 server 真的提供它，不管誰提供的。
func TestChatKeepsHealthyServerToolsWhenPrefixOverlaps(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)

	writeMcpServers(t, dir, "mcp_servers:\n"+
		"  foo:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-foo]\n"+
		testMcpServerEntry(t, "foo__bar", "echo"))
	// Profile 同時列了兩台的工具：健康那台的該留、掛掉那台的該走。放在同一份 Profile
	// 裡，正反兩面才對得起來——只驗一面的話，一個「全留」或「全刪」的實作都能矇混過去。
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - foo\n  - foo__bar\n"+
		"tools:\n  - foo__bar__echo\n  - foo__alpha\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "hi"}); err != nil {
		t.Fatalf("foo 連不上不該讓啟動失敗: %v", err)
	}
	got := out.String()

	names := llmToolNames(t, reqs, 0)
	// 1. 健康那台的工具真的還在 Agent 手上——這是能力有沒有退化的唯一實證。
	if !slices.Contains(names, "foo__bar__echo") {
		t.Errorf("foo__bar 是健康的，它的工具卻從可用子集裡消失了: %v", names)
	}
	// 2. 掛掉那台的工具確實走了——降級本身仍要生效。
	if slices.Contains(names, "foo__alpha") {
		t.Errorf("foo 連不上，它的工具卻還在可用子集裡: %v", names)
	}
	// 3. 使用者被告知少了哪些，而且**只**少了那些。
	if !strings.Contains(got, "foo__alpha") {
		t.Errorf("沒有告訴使用者 foo__alpha 這次不可用:\n%s", got)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "不可用") && strings.Contains(line, "foo__bar__echo") {
			t.Errorf("健康 server 的工具被講成不可用——輸出與實際能力自相矛盾:\n%s", line)
		}
	}
	// 4. foo 真的連不上這件事仍要講。
	if !strings.Contains(got, "警告") || !strings.Contains(got, "foo") {
		t.Errorf("連不上的 server 沒有被指名:\n%s", got)
	}
}

// TestToolsDoesNotContradictItselfOnPrefixOverlap 是同一情境在 `oryxos tools` 的那一面：
// 不能一邊把工具列成健康可用、一邊又警告它不可用。
func TestToolsDoesNotContradictItselfOnPrefixOverlap(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		"  foo:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-foo]\n"+
		testMcpServerEntry(t, "foo__bar", "echo"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - foo\n  - foo__bar\ntools:\n  - foo__bar__echo\n")

	var out bytes.Buffer
	if err := runTools(context.Background(), &out, dir, toolsOptions{profileName: "default"}); err != nil {
		t.Fatalf("oryxos tools: %v", err)
	}
	got := out.String()

	// 它被列在健康的 foo__bar 底下、而且標成已選取。
	if line := toolLine(t, got, "foo__bar__echo"); !strings.Contains(line, "✓") {
		t.Errorf("foo__bar__echo 在 Profile 的 tools 裡，卻沒被標成已加入:\n%s", line)
	}
	// 同一份輸出就不該再說它不可用。
	if strings.Contains(got, "不可用") {
		t.Errorf("輸出一邊把工具列成可用、一邊又說它不可用:\n%s", got)
	}
}

// TestChatStillFailsFastOnTypoWhileDegradingRealFailure 釘住降級**不會順手吞掉打錯字**。
//
// 兩者同時發生：`foo` 真的連不上（環境問題，該降級），而 `github__list_prs` 是打錯字
// （設定錯誤，該擋下啟動）。判準是「這個名字屬不屬於某台連不上的 server」——不是
// 「這次有沒有 server 連不上」。
//
// 弄混的代價是安靜的：把打錯字的一併降級，Agent 會照常起來、少一個工具，使用者拿到的
// 是「它就是不會做那件事」，而真正的原因（少打了幾個字）沒有任何人告訴他。
func TestChatStillFailsFastOnTypoWhileDegradingRealFailure(t *testing.T) {
	dir := setupChatWorkspace(t, "http://provider.invalid")
	writeMcpServers(t, dir, "mcp_servers:\n"+
		"  foo:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-foo]\n"+
		testMcpServerEntry(t, "github", "list_pull_requests"))
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - foo\n  - github\n"+
		"tools:\n  - foo__alpha\n  - github__list_prs\n")

	var out bytes.Buffer
	err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "hi"})
	if err == nil {
		t.Fatal("打錯字的工具名被降級吞掉了——Agent 會安靜地少一個工具，沒有人知道為什麼")
	}
	if !strings.Contains(err.Error(), "github__list_prs") {
		t.Errorf("錯誤沒有指出是哪個名字寫錯了:\n%v", err)
	}
	// 同一次啟動裡，真正連不上的那台仍然走降級（不是被 fail fast 蓋過去）。
	if got := out.String(); !strings.Contains(got, "foo") || !strings.Contains(got, "警告") {
		t.Errorf("真的連不上的 server 沒有被降級警示:\n%s", got)
	}
}

// TestChatKeepsToolRegisteredByHealthyServerUnderFailedPrefix 釘住降級的**第一判準**：
// 已經被註冊的名字絕不刪除，不管它長得像誰的。
//
// 這一格是前綴啟發式救不了的那種：連不上的是 `foo__bar`，健康的是 `foo`，而 `foo` 提供
// 一個名叫 `bar__echo` 的工具——註冊名剛好是 `foo__bar__echo`。以「最長的 server 名前綴」
// 判斷會指向連不上的 `foo__bar`，於是一個**真的拿得到**的工具被刪掉。
//
// 名字重疊到這個程度很罕見，但判準本身是對的那個：有東西註冊了它，就代表真的有一台
// 健康的 server 提供它。前綴匹配只該用在「沒有人註冊」的名字上。
func TestChatKeepsToolRegisteredByHealthyServerUnderFailedPrefix(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "chat_reply_1.json"))
	dir := setupChatWorkspace(t, srv.URL)

	// 健康的 foo 提供 bar__echo；連不上的是 foo__bar。兩者拼出同一個註冊名。
	writeMcpServers(t, dir, "mcp_servers:\n"+
		testMcpServerEntry(t, "foo", "bar__echo")+
		"  foo__bar:\n    transport: stdio\n    command: [/nonexistent/oryxos-mcp-foobar]\n")
	writeProfile(t, dir, "provider:\n  name: openrouter\n  model: m\n"+
		"mcp_servers:\n  - foo\n  - foo__bar\ntools:\n  - foo__bar__echo\n")

	var out bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader(""), &out, dir,
		chatOptions{profileName: "default", message: "hi"}); err != nil {
		t.Fatalf("foo__bar 連不上不該讓啟動失敗: %v", err)
	}

	if names := llmToolNames(t, reqs, 0); !slices.Contains(names, "foo__bar__echo") {
		t.Errorf("foo__bar__echo 是健康的 foo 註冊的，卻因為名字像 foo__bar 的而被刪掉了: %v", names)
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "不可用") && strings.Contains(line, "foo__bar__echo") {
			t.Errorf("拿得到的工具被講成不可用:\n%s", line)
		}
	}
}
