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

// shutdownTimeout 是優雅關閉時，等進行中請求結束的上限。
//
// spec #73 第二節定案的上限是「一個 turn 時間上限再加一段緩衝」，但 turn 時間上限（--turn-timeout）
// 屬於 #79。#75 的端點都是瞬間完成的查詢，這個值只是「按 Ctrl+C 不會卡住」的上限；#79 落地時改成
// turn 上限加緩衝。
const shutdownTimeout = 10 * time.Second

// serverOptions 是 server 命令的選項。
//
// 唯一的旗標 --addr 在 listener 建立之前就用完了（見 newServerCmd）。三個讀取期限不開旗標（spec
// #73 沒有要求），命令路徑一律用預設值；它們放在這裡，是為了讓測試經由 runServer 這個 seam 把
// 期限設短，不必真的等上 10 秒。**零值代表用預設值**，見 newHTTPServer。
type serverOptions struct {
	addr string

	// readHeaderTimeout 是讀完請求標頭的上限，擋「只送半份標頭」的連線。
	readHeaderTimeout time.Duration
	// readTimeout 是讀完整個請求（標頭加 body）的上限，擋「標頭完整、body 不送完」的連線。
	readTimeout time.Duration
	// idleTimeout 是 keep-alive 連線在兩個請求之間最多能閒置多久。
	idleTimeout time.Duration
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
// 不設 WriteTimeout：spec #73 定案它必須大於 turn 時間上限，而 turn 時間上限（--turn-timeout）
// 屬於 #79。
func newHTTPServer(handler http.Handler, opts serverOptions) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cmp.Or(opts.readHeaderTimeout, defaultReadHeaderTimeout),
		ReadTimeout:       cmp.Or(opts.readTimeout, defaultReadTimeout),
		IdleTimeout:       cmp.Or(opts.idleTimeout, defaultIdleTimeout),
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
	return cmd
}

// runServer 組一次進程層級、對 profiles/ 底下每份 Profile 各組一次 Profile 層級（見 assembly.go），
// 然後在 listener 上服務，直到 ctx 取消再優雅關閉。
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
	// **這行提醒不看監聽位址、一律印**（spec #73 使用者故事 11）。只在「監聽所有介面」時才印的話，
	// 改成 127.0.0.1 的人會以為安全了；但 CORS 全開，他瀏覽的任何網頁照樣能經由瀏覽器打到本機。
	fmt.Fprintf(out, "提醒：未啟用認證，任何連得到這個位址的人都能驅動 Agent（含它的 Profile 開放的 Tool）；"+
		"只想在本機使用請改用 --addr 127.0.0.1:8080，完整風險說明見 SECURITY.md。\n")

	proc, err := assembleProcess(ctx, out, baseDir)
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
	for _, name := range names {
		prof, err := core.LoadProfile(filepath.Join(proc.ws, "profiles", name+".yaml"))
		if err != nil {
			return fmt.Errorf("載入 Profile %s: %w", name, err)
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
		// 先收進清單、再檢查錯誤：組到一半失敗時 MCP 子進程可能已經起來了，半成品也要交給 Close。
		profiles = append(profiles, assembled)
		if err != nil {
			// #75 的暫時語義：任何一份組不起來就不啟動，與 chat 相同。#76 會放寬成只讓那一份不可用。
			//
			// **包上檔名，因為 assembleProfile 的錯誤不一定指名**：tools 校驗失敗會寫「Profile X 的…」，
			// MCP server 缺憑證、Tool registry 組裝失敗則不會。已經指名的那幾種會重複一次名字，
			// 用這個代價換「每一種失敗都指得出是哪一份」。
			return fmt.Errorf("組裝 Profile %s: %w", name, err)
		}
		fmt.Fprintf(out, "Profile %s 已載入（Provider %s，模型 %s）\n", name, prof.Provider.Name, prof.Provider.Model)
	}

	// 交給 web 的只有 Provider 的名字，憑證與 base_url 在這裡就不往下傳（見 web.Options.Providers）。
	// 用 make 建一個非 nil 的切片：沒有任何 Provider 時，JSON 裡是 [] 而不是 null。
	providers := make([]string, 0, len(proc.cfg.Providers))
	for providerName := range proc.cfg.Providers {
		providers = append(providers, providerName)
	}
	slices.Sort(providers) // map 的走訪順序每次都不同，排序之後回應才可重現

	srv := newHTTPServer(web.NewHandler(web.Options{
		Logger:    proc.logger,
		Version:   buildVersion(),
		StartedAt: startedAt,
		Profiles:  names,
		Providers: providers,
	}), opts)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	fmt.Fprintf(out, "就緒：%d 份 Profile 已載入，開始接受請求（Ctrl+C 停止）\n", len(names))

	select {
	case err := <-serveErr:
		// Serve 只會因為 listener 出了無法恢復的錯誤而自己返回。這時已經不再接受連線，繼續等 ctx
		// 只會讓進程看起來還活著。
		return errors.Join(fmt.Errorf("HTTP 服務中止: %w", err), forceClose(srv))
	case <-ctx.Done():
	}

	fmt.Fprintln(out, "正在關閉：不再接受新連線，等進行中的請求結束")
	// **關閉用的 context 不能直接用 ctx**：ctx 已經取消了，Shutdown 拿到它會一刻都不等，進行中的
	// 請求被當場切斷。WithoutCancel 保留 ctx 帶的值、拿掉取消，上限另外由 shutdownTimeout 給。
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
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
