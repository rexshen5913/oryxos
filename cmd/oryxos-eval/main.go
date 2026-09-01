// oryxos-eval 是 OryxOS 的評測二進制：讀 YAML 用例宣告，驅動**真實 Agent** 跑一輪，
// 用宣告式斷言判卷並印出通過與否。
//
// **它會呼叫真實 Provider 並產生費用。** 這也是它是一支獨立二進制、而不是 `go test`
// 的一部分的原因（憲法 4.4）。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// costWarning 在**每一次執行的最前面**印出來，不只寫在 --help 裡。
//
// 說明文字只有主動去查的人會看到；真正會誤觸的是「照著 Makefile 敲了一行」的人，
// 而他唯一會看的地方是終端機。
const costWarning = "警告：評測會呼叫真實 Provider 並產生費用（每個用例至少一次 LLM 呼叫）。"

type options struct {
	casesPath string
	workspace string
	outDir    string
}

func newRootCmd() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "oryxos-eval",
		Short: "跑 YAML 宣告的評測用例（會呼叫真實 Provider 並產生費用）",
		Long: "讀入 YAML 用例宣告，為每個用例建立一個乾淨的 Workspace、佈置初始檔案，\n" +
			"把任務送給真實 Agent 跑一輪，再用宣告式斷言判卷。\n\n" +
			costWarning + "\n\n" +
			"它刻意不是 go test 的一部分：自動化測試中 LLM 一律回放錄製回應（憲法 4.4），\n" +
			"而評測的價值恰恰在於用真實模型。",
		Args:         cobra.NoArgs,
		SilenceUsage: true, // 執行期錯誤（缺憑證、Provider 故障）與用法無關
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.casesPath, "cases", "evals", "用例檔或用例目錄（目錄下的 *.yaml，依檔名排序）")
	cmd.Flags().StringVar(&opts.workspace, "workspace", ".oryxos", "來源 Workspace：config.yaml 與 profiles/ 從這裡複製")
	cmd.Flags().StringVar(&opts.outDir, "out-dir", "",
		"每次執行的輸出目錄建在它底下（省略時用系統暫存目錄）。執行後刻意不刪，失敗時要進去翻")
	return cmd
}

// run 逐一跑完用例並印出結果；有任何一個用例未通過就回傳錯誤（讓退出碼非零）。
func run(ctx context.Context, out io.Writer, opts options) error {
	// 全部用例先解析完再開跑：一份寫壞的 YAML 應該在**送出任何請求之前**被擋下。
	// 跑到第三個才發現第四個的斷言種類拼錯，前三次的錢已經花了。
	cases, err := eval.LoadCases(opts.casesPath)
	if err != nil {
		return err
	}

	// **--out-dir 是放 run 目錄的地方，不是 run 目錄本身。** 每次執行都在它底下開一個
	// 全新的 run 目錄，所以同一個 --out-dir 重跑時，用例的 Workspace 一定是新建的
	// （Codex 審查抓到：固定用 case-01／case-02 會讓第二次執行沿用上一次的 oryxos.db、
	// active Session 與長期記憶）。PrepareWorkspace 那邊另有一道拒絕沿用的把關，兩者
	// 分工是「這裡讓正常使用永遠碰不到那個錯誤，那裡讓它不可能被繞過」。
	if opts.outDir != "" {
		if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
			return fmt.Errorf("建立評測輸出目錄 %s: %w", opts.outDir, err)
		}
	}
	// opts.outDir 為空時 MkdirTemp 用系統暫存目錄。
	runDir, err := os.MkdirTemp(opts.outDir, "oryxos-eval-")
	if err != nil {
		return fmt.Errorf("建立評測輸出目錄: %w", err)
	}

	fmt.Fprintf(out, "%s\n\n來源 Workspace：%s\n用例：%s（%d 條）\n輸出目錄：%s\n\n",
		costWarning, opts.workspace, opts.casesPath, len(cases), runDir)

	passed := 0
	for i, c := range cases {
		// 目錄名用序號而不是用例名：用例名是使用者寫的自由文字，可以含斜線或其他
		// 在路徑裡有意義的字元。序號旁邊就印著名字，對照不困難。
		caseRoot := filepath.Join(runDir, fmt.Sprintf("case-%02d", i+1))
		started := time.Now()
		result, err := eval.RunCase(ctx, opts.workspace, caseRoot, c)
		elapsed := time.Since(started).Round(time.Millisecond)
		if err != nil {
			// **執行錯誤就停，不繼續跑。** 這類錯誤（缺憑證、Profile 壞掉、Provider
			// 故障）幾乎都會讓後面每一條用例以同樣的方式失敗，而每試一條都要花錢。
			// 斷言未通過則不同——那是評測本來就要回報的結果，會繼續跑完。
			fmt.Fprintf(out, "[%d/%d] %s … 執行錯誤（%s）\n        %v\n        Workspace：%s\n",
				i+1, len(cases), c.Name, elapsed, err, caseRoot)
			return fmt.Errorf("評測中止於第 %d 條用例: %w", i+1, err)
		}

		verdict := eval.Grade(c, result)
		status := "未通過"
		if verdict.Passed {
			status = "通過"
			passed++
		}
		fmt.Fprintf(out, "[%d/%d] %s … %s（%s）\n", i+1, len(cases), c.Name, status, elapsed)
		for _, reason := range verdict.Failures {
			fmt.Fprintf(out, "        - %s\n", reason)
		}
		if !verdict.Passed {
			// 失敗時才印回應與 Tool 軌跡：通過的用例印這些只會淹掉真正要看的那幾條。
			fmt.Fprintf(out, "        回應：%s\n        呼叫過的 Tool：%v\n        Workspace：%s\n",
				result.Reply, result.ToolsCalled, caseRoot)
		}
	}

	fmt.Fprintf(out, "\n結果：%d 通過 / %d 未通過（共 %d）\n", passed, len(cases)-passed, len(cases))
	if passed != len(cases) {
		return fmt.Errorf("%d 條用例未通過", len(cases)-passed)
	}
	return nil
}

func main() {
	// Ctrl+C／SIGTERM 取消 context，讓進行中的 LLM 呼叫可中斷（憲法 5.3）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
