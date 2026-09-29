// server.go 實作 `oryxos server`：一次載入 Workspace 裡的全部 Profile，啟動 Web Service（ticket #75）。
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/web"
)

// serverOptions 是 server 命令的選項。
//
// 兩個旗標的去向不同：--addr 在 listener 建立之前就用完了（見 newServerCmd）；--turn-timeout 則一路
// 帶進 runServer，決定 turn 上限、寫入期限與優雅關閉的等待（見 turnTimeoutOf）。
//
// 三個讀取期限與寫入期限都不開旗標（spec #73 沒有要求），命令路徑一律用預設值；它們放在這裡，是為了
// 讓測試經由 runServer 這個 seam 把期限設短，不必真的等上 10 秒。**零值代表用預設值**，見
// newHTTPServer。
type serverOptions struct {
	addr string

	// readHeaderTimeout 是讀完請求標頭的上限，擋「只送半份標頭」的連線。
	readHeaderTimeout time.Duration
	// readTimeout 是讀完整個請求（標頭加 body）的上限，擋「標頭完整、body 不送完」的連線。
	readTimeout time.Duration
	// idleTimeout 是 keep-alive 連線在兩個請求之間最多能閒置多久。
	idleTimeout time.Duration
	// writeTimeout 是寫完一個回應的上限，擋「送出請求之後不讀回應」的連線。
	writeTimeout time.Duration
	// turnTimeout 是單一 turn 的時間上限（--turn-timeout）。
	turnTimeout time.Duration
}

const (
	// defaultTurnTimeout 是 --turn-timeout 的預設值（spec #73 第二節，技術方案 §7.4）。
	defaultTurnTimeout = 60 * time.Second
	// responseWriteGrace 是 turn 跑滿上限之後，還留給寫出回應的時間（見 newHTTPServer）。
	responseWriteGrace = 30 * time.Second
	// shutdownGrace 是優雅關閉時，在 handler 的最長時間之外多等的緩衝（見 shutdownWait）。
	shutdownGrace = 10 * time.Second
)

// turnTimeoutOf 回傳這次啟動的 turn 時間上限：沒填（測試的零值）就用預設值。命令路徑一律有值，
// 0 與負值在 newServerCmd 就被拒絕了。
func turnTimeoutOf(opts serverOptions) time.Duration {
	return cmp.Or(opts.turnTimeout, defaultTurnTimeout)
}

// handlerBudget 是一個請求的 handler 最多會跑多久：**讀 body 最晚在讀取期限到時結束，turn 從那之後
// 才開始計時、最多跑滿 turn 上限**。寫入期限與優雅關閉的等待都以它為基準，而不是只以 turn 上限為基準
// （Spec 審查）：body 傳得慢的請求，turn 開始得晚，只留 turn 上限的話，它逾時的那一刻兩個期限都已經到了。
func handlerBudget(opts serverOptions) time.Duration {
	return cmp.Or(opts.readTimeout, defaultReadTimeout) + turnTimeoutOf(opts)
}

// shutdownWait 是優雅關閉時，等進行中請求結束的上限：**handler 的最長時間再加一段緩衝**（spec #73
// 第二節的「一個 turn 時間上限再加一段緩衝」，turn 之前讀 body 的時間也算進來，見 handlerBudget）。
//
// 按下 Ctrl+C 的那一刻，可能有一個請求剛讀完標頭，它最多跑滿 handlerBudget。等得比這短，進行中的
// turn 會被切斷，呼叫端拿不到回應、那一輪白跑；緩衝留給 turn 結束之後寫回應與收尾。
func shutdownWait(opts serverOptions) time.Duration {
	return handlerBudget(opts) + shutdownGrace
}

// authReminder 是啟動時「未啟用認證」的提醒（spec #73 使用者故事 11）。
//
// **提醒本身不看監聽位址、一律印**：只在「監聽所有介面」時才印的話，改成 127.0.0.1 的人會以為安全
// 了；但 CORS 全開，他瀏覽的任何網頁仍可能經由瀏覽器打到本機。
//
// **只有後半句的緩解建議看位址**：已經只監聽 loopback 時，「改用 --addr 127.0.0.1:8080」是做過的
// 事，還指定了一個沒在用的埠，所以換成說明只對本機開放也擋不住的那條路。位址不是 TCP 位址時（不會
// 發生在 net.Listen("tcp", …) 上）照「可能對外」處理，寧可多給一句建議。
func authReminder(addr net.Addr) string {
	const head = "提醒：未啟用認證，任何連得到這個位址的人都能驅動 Agent（含它的 Profile 開放的 Tool）；"
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return head + "只對本機開放也一樣：CORS 全開，你瀏覽的網頁仍可能經由瀏覽器打到它，完整風險說明見 SECURITY.md。\n"
	}
	return head + "只想在本機使用請改用 --addr 127.0.0.1:8080，完整風險說明見 SECURITY.md。\n"
}

// 三個讀取期限的預設值。核心階段的請求 body 只是一段 JSON，正常的請求遠遠用不到這麼久；
// 期限只在連線慢得不正常時才起作用。
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// newHTTPServer 組出帶讀取期限的 http.Server。
//
// **沒填的期限換成預設值，不是不設期限**：http.Server 的零值就是不設期限，照搬的話，忘記填選項
// 的呼叫端會悄悄拿到一台可以被慢速連線佔滿的 server。一條只送半份標頭、或 body 送一半就停住的
// 連線，會一直佔著一個 goroutine 與一個檔案描述符，累積到上限，health 就連不進來了。cmp.Or 回傳
// 第一個非零的值。
//
// **handler 沒讀 body 也逃不過 readTimeout**：net/http 寫出回應之前，會先排空 handler 沒讀的
// body（server.go 的 chunkWriter.writeHeader）。body 送不完時，排空會卡住，靠這個期限把它切斷。
//
// **readTimeout 不會切斷跑很久的 handler**：body 一讀到結尾，net/http 就清掉連線的讀取期限
// （server.go 的 startBackgroundRead），請求的 context 不會因為這個期限到了而被取消，所以 #79 的
// turn 可以跑得比它久。
//
// **WriteTimeout 擋的是「送出請求之後不讀回應」的連線**（#77 的 GET /memory 之後才需要）：回應大到
// TCP 緩衝裝不下時，寫入會停在那裡等 client 讀，client 一直不讀，連線、goroutine 與編碼好的回應就
// 一直被佔著。#75 的端點都是幾百 bytes 的小回應，緩衝吸收得了；/memory 回傳整份檔案，大小沒有上限，
// 這個前提就不成立了。
//
// **整台 server 設一個值，不是只替查詢端點設**：用 http.ResponseController 逐端點設也做得到，但每一層
// 中介層都得能 Unwrap 到底層連線，每個端點也得記得套上；整台設一個值，404、405、預檢這些由中介層
// 直接寫出的回應也一併涵蓋。net/http 每讀到一個新請求就重新起算這個期限。
//
// **沒填時是「handler 的最長時間＋responseWriteGrace」**（spec #73 第二節，ticket #79）：WriteTimeout
// 從讀完請求標頭就開始計時，涵蓋整個 handler，包括 turn 之前讀 body 的那一段（見 handlerBudget）。
// 它若不大於 handler 的最長時間，turn 逾時之後，504 回應還沒寫出去，連線就先被切斷了。寬限的 30 秒是
// 寫出回應本身的預算，也就是 #77 那個「不讀回應的 client」最多能佔住連線多久。明確填了（只有測試會
// 填）就照用。
func newHTTPServer(handler http.Handler, opts serverOptions) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cmp.Or(opts.readHeaderTimeout, defaultReadHeaderTimeout),
		ReadTimeout:       cmp.Or(opts.readTimeout, defaultReadTimeout),
		IdleTimeout:       cmp.Or(opts.idleTimeout, defaultIdleTimeout),
		WriteTimeout:      cmp.Or(opts.writeTimeout, handlerBudget(opts)+responseWriteGrace),
	}
}

func newServerCmd() *cobra.Command {
	var opts serverOptions
	cmd := &cobra.Command{
		Use:   "server",
		Short: "啟動 Web Service（HTTP API），同時載入 Workspace 裡的全部 Profile",
		Long: "在已初始化的 Workspace 中啟動常駐的 HTTP 服務，profiles/ 底下每份 Profile 對應的\n" +
			"Agent 同時可用。Ctrl+C 或 SIGTERM 會先等進行中的請求結束再退出。\n" +
			"核心階段沒有認證：任何連得到監聽位址的人都能驅動 Agent，風險說明見 SECURITY.md。",
		Args: cobra.NoArgs,
		// 執行期錯誤（未初始化、埠號被佔用、Profile 壞掉）與用法無關，不倒 Usage 沖淡錯誤訊息。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 在綁定位址之前就擋下：0 會讓每一個 turn 一開始就逾時，負值同理。
			if opts.turnTimeout <= 0 {
				return fmt.Errorf("--turn-timeout 必須大於 0，收到 %v", opts.turnTimeout)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("取得當前目錄: %w", err)
			}
			// listener 在這裡綁好再交給 runServer，而不是讓 runServer 自己綁：測試要能交一個本機
			// 隨機埠進去（spec #73 的主 seam）。綁不上屬於 Workspace 層級的錯誤，命令直接回報。
			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(cmd.Context(), "tcp", opts.addr)
			if err != nil {
				return fmt.Errorf("監聽 %s: %w", opts.addr, err)
			}
			return runServer(cmd.Context(), cmd.OutOrStdout(), cwd, opts, listener)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", ":8080",
		"監聽位址；預設監聽所有網路介面的 8080 埠，只對本機開放請用 127.0.0.1:8080")
	cmd.Flags().DurationVar(&opts.turnTimeout, "turn-timeout", defaultTurnTimeout,
		"單一 turn 的時間上限；推理型模型跑滿 iteration 上限時可能需要調長")
	return cmd
}

// runServer 組一次進程層級、對 profiles/ 底下每份 Profile 各組一次 Profile 層級（見 assembly.go），
// 然後在 listener 上服務，直到 ctx 取消再優雅關閉。
//
// **錯誤分兩級**（spec #73 第三節）：Workspace 層級的錯誤（Workspace 不存在、config.yaml 解析不了、
// SQLite 或日誌檔打不開）直接讓啟動失敗；Profile 層級的錯誤只讓那一份不可用，可用數為 0 才失敗。
// Provider 憑證逐個展開（credentialsPerProvider），所以缺憑證屬於後者。
//
// **listener 一交進來，所有權就歸這裡**：不管從哪條路離開都會關掉它，啟動失敗時埠號不會被一個
// 起不來的進程佔著。
//
// **收尾分兩段，順序不能反**：先 Shutdown（不再接受新連線、等進行中的請求結束），再交給
// processAssembly.Close（審計、SQLite、MCP 連線與日誌檔，順序見那裡）。反過來的話，還在處理中的
// 請求會撞上已經關掉的日誌檔與資料庫。Shutdown 在函式本體裡同步做完才返回，Close 排在 defer 裡，
// 所以一定在它之後。
func runServer(ctx context.Context, out io.Writer, baseDir string, opts serverOptions, listener net.Listener) (err error) {
	defer func() {
		// 正常關閉時 Shutdown 已經關過它，再關一次會回 net.ErrClosed，那不算錯誤。
		if cerr := listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
			err = fmt.Errorf("關閉監聽: %w", cerr)
		}
	}()
	startedAt := time.Now()

	fmt.Fprintf(out, "OryxOS server 監聽 %s\n", listener.Addr())
	fmt.Fprint(out, authReminder(listener.Addr()))

	proc, err := assembleProcess(ctx, out, baseDir, credentialsPerProvider)
	var profiles []*profileAssembly
	defer func() {
		if cerr := proc.Close(profiles...); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err != nil {
		return err
	}

	names, err := profileNames(proc.ws)
	if err != nil {
		return err
	}
	// **一份 Profile 的錯誤只讓那一份不可用**（spec #73 第三節）：每一份都載入完，才判斷能不能啟動。
	// 一個人的設定錯誤不該讓整個實例的 Agent 都停擺。
	entries := make([]web.ProfileEntry, 0, len(names))
	for _, name := range names {
		prof, assembled, err := loadServerProfile(ctx, out, proc, name)
		entry := web.ProfileEntry{Name: name, Profile: prof}
		if err != nil {
			// **去敏只做一次，三條輸出路徑用同一段文字**：啟動輸出、錯誤日誌、profiles 端點的 error
			// 欄位。各自去敏的話，改了其中一條、漏了另一條，也沒有人會發現。原因是使用者手寫的字串
			// 拼出來的（tools 裡一個打錯的名字會被原樣帶出來），規則與審計、事件流同一套。
			entry.Reason = core.RedactErrorText(err.Error())
			fmt.Fprintf(out, "Profile %s 不可用：%s\n", name, entry.Reason)
			proc.logger.Error("profile_unavailable", "profile", name, "error", entry.Reason)
			closeUnavailableProfile(proc, name, assembled)
		} else {
			profiles = append(profiles, assembled)
			// 取自 Profile 過濾後的 Executor，也就是送給 LLM 的那一份：MCP 降級與自動加入的 load_skill
			// 都已經反映在裡面。拿 Profile 的 tools 欄位原文來列，會把連不上的 MCP 工具也列成可用。
			entry.Agent = assembled.agent
			for _, info := range assembled.executor.Tools() {
				entry.Tools = append(entry.Tools, web.ToolEntry{Name: info.Name, Description: info.Description, Server: info.Server})
			}
			fmt.Fprintf(out, "Profile %s 已載入（Provider %s，模型 %s）\n", name, prof.Provider.Name, prof.Provider.Model)
		}
		entries = append(entries, entry)
	}
	if err := requireAvailableProfile(entries); err != nil {
		return err
	}

	// 交給 web 的只有 Provider 的名字，憑證與 base_url 在這裡就不往下傳（見 web.Options.Providers）。
	// 用 make 建一個非 nil 的切片：沒有任何 Provider 時，JSON 裡是 [] 而不是 null。
	providers := make([]string, 0, len(proc.cfg.Providers))
	for providerName := range proc.cfg.Providers {
		providers = append(providers, providerName)
	}
	slices.Sort(providers) // map 的走訪順序每次都不同，排序之後回應才可重現

	srv := newHTTPServer(web.NewHandler(web.Options{
		Logger:      proc.logger,
		Version:     buildVersion(),
		StartedAt:   startedAt,
		Profiles:    entries,
		Providers:   providers,
		LongTerm:    proc.longTerm,
		Sessions:    proc.sessions,
		TurnTimeout: turnTimeoutOf(opts),
	}), opts)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	// 可用的就是組起來、收進 profiles 的那幾份；與 entries 的 Reason 在同一個 if／else 裡決定，
	// 兩邊不會對不上。
	fmt.Fprintf(out, "就緒：%d 份 Profile 可用、%d 份不可用，開始接受請求（Ctrl+C 停止）\n",
		len(profiles), len(entries)-len(profiles))

	select {
	case err := <-serveErr:
		// Serve 只會因為 listener 出了無法恢復的錯誤而自己返回。這時已經不再接受連線，繼續等 ctx
		// 只會讓進程看起來還活著。
		return errors.Join(fmt.Errorf("HTTP 服務中止: %w", err), forceClose(srv))
	case <-ctx.Done():
	}

	fmt.Fprintln(out, "正在關閉：不再接受新連線，等進行中的請求結束")
	// **關閉用的 context 不能直接用 ctx**：ctx 已經取消了，Shutdown 拿到它會一刻都不等，進行中的
	// 請求被當場切斷。WithoutCancel 保留 ctx 帶的值、拿掉取消，上限另外由 shutdownWait 給。
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait(opts))
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 等不到進行中的請求結束：強制切斷剩下的連線，再往下收尾。
		return errors.Join(fmt.Errorf("優雅關閉逾時: %w", err), forceClose(srv))
	}
	// Shutdown 一開始，Serve 就返回 ErrServerClosed。讀掉它，確認 Serve 的 goroutine 已經結束。
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服務中止: %w", err)
	}
	return nil
}

// forceClose 立刻切斷 server 剩下的連線，用在等不到、或不必再等的時候。
//
// 錯誤要包上「這一步在做什麼」：它會和造成關閉的那個錯誤 errors.Join 在一起，一句裸的
// 「use of closed network connection」分不出是哪一步冒出來的。沒有錯誤時回 nil，errors.Join
// 會略過它。
func forceClose(srv *http.Server) error {
	if err := srv.Close(); err != nil {
		return fmt.Errorf("強制關閉 HTTP 連線: %w", err)
	}
	return nil
}

// loadServerProfile 載入名為 name 的 Profile 並組成可運作的 Agent。
//
// 回傳的 Profile 在 YAML 讀不出來時是 nil；讀得出來但之後才失敗時非 nil，profiles 端點靠它列出
// 描述與 Provider。組裝的半成品在失敗時也可能非 nil（MCP 已經連上），交給 closeUnavailableProfile。
//
// **錯誤包上「哪一步」，不包「哪一份」**：呼叫端把它印在「Profile X 不可用：」之後、列在「X：」
// 之後，再包上名字只會讓同一個名字出現兩次。
func loadServerProfile(ctx context.Context, out io.Writer, proc *processAssembly, name string) (
	*core.Profile, *profileAssembly, error) {
	prof, err := core.LoadProfile(filepath.Join(proc.ws, "profiles", name+".yaml"))
	if err != nil {
		// 解析成功、校驗失敗時 prof 非 nil（見 core.LoadProfile）：照樣交出去給 profiles 端點列描述，
		// 但不往下組裝。
		return prof, nil, fmt.Errorf("載入 Profile 設定檔: %w", err)
	}
	// **檔名與 name 欄位必須一致，只有 server 這樣要求**（spec #73 第三節）。對外的名字是檔名，會出現
	// 在 URL 上；Session 的 profile_name 卻取自 name 欄位。兩者不一致時，同一個 Agent 在 URL 上和
	// 資料庫裡是兩個名字。比對放在組裝之前：不一致的 Profile 不必起任何 MCP 子進程。chat 不跟著
	// 收緊，留給 CLI 命令那份 spec 決定（spec #73 Further Notes）。
	if prof.Name != name {
		return prof, nil, fmt.Errorf("檔名 %s.yaml 與 name 欄位 %q 不一致；server 以檔名作為對外的名字，"+
			"Session 記的卻是 name 欄位，請讓兩者相同", name, prof.Name)
	}
	// 跟這份 Profile 有關的提醒，行首一律帶上它的名字：多份 Profile 的提醒印在一起，不指名就
	// 分不出是誰的（spec #73 第二節）。
	//
	// **指名是在外面包一層 writer，不是去改 assembleProfile 的措辭**。改措辭會連 chat 的輸出
	// 一起變；包一層的話，提醒本身一個字都不動，chat 與 server 對同一份設定給出同一句診斷。
	//
	// 事件流傳不做事的實作：Web Service 的回應是同步阻塞的，SSE 屬擴展階段（spec #73 Out of Scope）。
	assembled, err := assembleProfile(ctx, &linePrefixWriter{out: out, prefix: "[Profile " + name + "] "},
		proc, prof, core.NopEventSink{})
	if err != nil {
		return prof, assembled, fmt.Errorf("組裝 Agent: %w", err)
	}
	return prof, assembled, nil
}

// closeUnavailableProfile 收掉一份不可用的 Profile 已經起來的 MCP 子進程。
//
// **現在就收，不等到 server 關閉**：這份 Profile 在這次啟動中永遠用不到它們。留到關閉時收的話，
// 一個 tools 打錯字的 Profile 會讓幾個 MCP 子進程閒置到下一次重啟。會走到這裡的是 MCP 已經連上、
// 之後才在 Subset 擋下的那種失敗。
//
// 收失敗只落錯誤日誌、不讓啟動失敗：這份 Profile 已經不可用，清理失敗不該連累其他 Agent。
// McpClientService.Close 對每條連線都試過、也清空了清單，關閉時再收一次不會有不同的結果。
func closeUnavailableProfile(proc *processAssembly, name string, assembled *profileAssembly) {
	if assembled == nil {
		return
	}
	if err := assembled.mcpClients.Close(); err != nil {
		proc.logger.Error("mcp_close_failed", "profile", name, "error", core.RedactErrorText(err.Error()))
	}
}

// requireAvailableProfile 在一份可用的 Profile 都沒有時回傳錯誤，列出每一份的原因。
//
// **可用數為 0 時不啟動**（spec #73 第三節，使用者故事 6）：一個 Agent 都沒有的 server 看起來
// 服務正常，實際上什麼都做不了。profiles/ 底下沒有任何 YAML 也算這種情況。
//
// **錯誤由去敏過的原因組成，不以 %w 包原始錯誤**：這個錯誤會原樣印到終端機，包原始錯誤等於
// 繞過去敏。錯誤日誌的 profile_unavailable 記的也是同一段去敏文字——原始錯誤刻意不落在任何
// 地方，憑證不該因為多了一條輸出路徑就被保存下來。
func requireAvailableProfile(entries []web.ProfileEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("%s/profiles 底下沒有任何 Profile（*.yaml），server 沒有 Agent 可以服務", workspaceDir)
	}
	var reasons []string
	for _, entry := range entries {
		if entry.Available() {
			return nil
		}
		reasons = append(reasons, fmt.Sprintf("  %s：%s", entry.Name, entry.Reason))
	}
	return fmt.Errorf("沒有任何可用的 Profile，server 不啟動：\n%s", strings.Join(reasons, "\n"))
}

// profileNames 列出 profiles/ 底下每份 YAML 的檔名（去掉 .yaml），依檔名排序（os.ReadDir 的保證）。
//
// **Profile 對外的名字是檔名**（spec #73 第三節），與 `chat --profile` 用同一套定位方式。
func profileNames(ws string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(ws, "profiles"))
	if err != nil {
		return nil, fmt.Errorf("列出 Profile: %w", err)
	}
	var names []string
	for _, entry := range entries {
		name, isYAML := strings.CutSuffix(entry.Name(), ".yaml")
		if isYAML && !entry.IsDir() {
			names = append(names, name)
		}
	}
	return names, nil
}

// buildVersion 取自 Go 寫進二進制的建置資訊，因此不需要全域變數，也不需要建置時用 -ldflags
// 塞值（憲法 5.2）。
//
// **值由 Go 工具鏈決定，不是這裡決定**。實測：在 git 工作區裡 `go build` 會蓋上從 VCS 推導的
// pseudo-version（例如 `v0.0.0-20260915084300-65c831836114+dirty`）；`go test` 編出的二進制則是
// `(devel)`。spec #73 寫「本機開發建置時為 (devel)」，只對後者成立。
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		// 只有不是以 module 模式建置的二進制才會走到這裡，本專案的建置都是 module 模式；
		// 留著是因為 ok 為假時 info 是 nil。
		return "(devel)"
	}
	return info.Main.Version
}

// linePrefixWriter 在每一行的行首加上 prefix，讓 Profile 層級的啟動提醒指名是哪份 Profile。
//
// 它不是 goroutine 安全的，也不需要是：Profile 是一份一份依序組的；唯一從別的 goroutine 來的
// 輸出是 MCP 連線失敗的即時警示，而 tool.ConnectMcpServers 會先把它收回單一執行緒才呼叫。
type linePrefixWriter struct {
	out    io.Writer
	prefix string
	// midLine 表示上一次寫到一半、還沒遇到換行：接下來那一段不是新的一行，不加 prefix。
	midLine bool
}

// Write 回傳的位元組數只算 p 本身，不含加上去的 prefix：io.Writer 的契約是「p 裡寫出了多少」。
func (w *linePrefixWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if !w.midLine {
			if _, err := io.WriteString(w.out, w.prefix); err != nil {
				return written, fmt.Errorf("寫出 Profile 名前綴: %w", err)
			}
		}
		line := p
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			line = p[:i+1]
		}
		n, err := w.out.Write(line)
		written += n
		if err != nil {
			return written, fmt.Errorf("寫出啟動提醒: %w", err)
		}
		w.midLine = line[len(line)-1] != '\n'
		p = p[len(line):]
	}
	return written, nil
}
