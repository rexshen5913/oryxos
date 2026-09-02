package eval_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/eval"
)

// micro 讓表格寫得出「這一筆算得出成本」。
func micro(v int64) *int64 { return &v }

// TestNewRecordCarriesEveryField 釘住結構化結果的七個欄位都從輸入接上了。
//
// 驗收條件列的是七項（用例名、通過與否、iteration 數、Tool 失敗數、token 用量、
// 耗時、成本），漏接任何一項的症狀都一樣：歷史檔案裡那一欄恆為零值，而回歸比對
// 會看著一排 0 得出「沒有退步」的結論。
func TestNewRecordCarriesEveryField(t *testing.T) {
	got := eval.NewRecord(
		eval.Case{Name: "讀檔案並回答"},
		eval.Verdict{Passed: true},
		eval.RunResult{Iterations: 3, ToolFailures: 1, TotalTokens: 1234, CostMicroUSD: micro(4600)},
		1500*time.Millisecond,
	)

	if got.Case != "讀檔案並回答" {
		t.Errorf("Case = %q，期望 %q", got.Case, "讀檔案並回答")
	}
	if !got.Passed {
		t.Error("Passed = false，期望 true")
	}
	if got.Iterations != 3 {
		t.Errorf("Iterations = %d，期望 3", got.Iterations)
	}
	if got.ToolFailures != 1 {
		t.Errorf("ToolFailures = %d，期望 1", got.ToolFailures)
	}
	if got.TotalTokens != 1234 {
		t.Errorf("TotalTokens = %d，期望 1234", got.TotalTokens)
	}
	if got.ElapsedMs != 1500 {
		t.Errorf("ElapsedMs = %d，期望 1500", got.ElapsedMs)
	}
	if got.CostMicroUSD == nil || *got.CostMicroUSD != 4600 {
		t.Errorf("CostMicroUSD = %v，期望 4600", got.CostMicroUSD)
	}
}

// TestNewRecordKeepsCostUnavailableEmpty 是驗收條件「未配置定價時成本欄位為空而非
// 0」在組裝這一端的守衛。
//
// 這條容易在轉型時被弄丟：`*int64` 一旦被解引用成 int64，nil 就變成了 0，而 0 在
// 報表上讀起來是「這次沒花錢」——與 ticket #49 的資料語義正好相反。
func TestNewRecordKeepsCostUnavailableEmpty(t *testing.T) {
	got := eval.NewRecord(eval.Case{Name: "x"}, eval.Verdict{Passed: true},
		eval.RunResult{Iterations: 1}, time.Second)
	if got.CostMicroUSD != nil {
		t.Errorf("CostMicroUSD = %d，期望空值（沒算 ≠ 不用錢）", *got.CostMicroUSD)
	}
}

// TestRecordSummary 釘住那一行結構化結果**印給人看**的形狀。
//
// 斷言的是輸出裡出現了哪些片段，不是整行的字面相等：措辭日後會調，而這裡要守的是
// 「七項資訊都在這一行裡」與「成本算不出來時不得印成 0」。
func TestRecordSummary(t *testing.T) {
	tests := []struct {
		name        string
		record      eval.Record
		wantParts   []string
		wantAbsent  []string
		wantFailure bool // 期望這一行讀起來是「未通過」
	}{
		{
			name: "通過且算得出成本",
			record: eval.Record{
				Case: "讀檔案並回答", Passed: true,
				Iterations: 3, ToolFailures: 1, TotalTokens: 1234,
				ElapsedMs: 1500, CostMicroUSD: micro(4600),
			},
			// 4600 微美元 = 0.004600 美元。**單位不得在顯示時被壓成美分或美元整數**，
			// 那會讓一次低於一美分的呼叫顯示成 0（與 ticket #49 存整數微美元同一個理由）。
			wantParts: []string{"讀檔案並回答", "3", "1", "1234", "1.5s", "0.004600"},
		},
		{
			name: "未通過",
			record: eval.Record{
				Case: "退步了", Passed: false,
				Iterations: 10, ToolFailures: 4, TotalTokens: 9000,
				ElapsedMs: 20000, CostMicroUSD: micro(1),
			},
			wantParts:   []string{"退步了", "10", "4", "9000", "20s"},
			wantFailure: true,
		},
		{
			name: "成本算不出來時不得印成 0",
			record: eval.Record{
				Case: "沒配定價", Passed: true,
				Iterations: 1, TotalTokens: 100, ElapsedMs: 900,
			},
			wantParts: []string{"沒配定價", "未計算"},
			// 一個 $ 金額都不該出現：印 $0.000000 會讓報表看起來很省，而真相是沒算。
			wantAbsent: []string{"$"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.record.Summary()
			for _, part := range tt.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("Summary() = %q，期望含 %q", got, part)
				}
			}
			for _, part := range tt.wantAbsent {
				if strings.Contains(got, part) {
					t.Errorf("Summary() = %q，不該含 %q", got, part)
				}
			}
			// 「通過」是「未通過」的子字串，所以通過與否要用「未通過」這個字串來判，
			// 不能只查有沒有「通過」——後者對兩種結果都成立。
			if gotFailure := strings.Contains(got, "未通過"); gotFailure != tt.wantFailure {
				t.Errorf("Summary() = %q，期望讀起來是未通過 = %v", got, tt.wantFailure)
			}
		})
	}
}

// TestAppendRecordAccumulates 是驗收條件「歷史檔案累積而非覆寫」。
//
// **這是本票整個歷史檔案存在的理由。** 覆寫的話，「比上次多用了兩個 iteration」這句話
// 永遠說不出口——沒有上次。而覆寫的症狀是安靜的：檔案一直都在，看起來很正常，只是
// 裡面永遠只有最後一次。
func TestAppendRecordAccumulates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")

	// 兩次「執行」，各寫一筆同名用例——這正是回歸比對要看的形狀。
	first := eval.Record{Case: "讀檔案並回答", Passed: true, Iterations: 1, TotalTokens: 100, ElapsedMs: 900}
	second := eval.Record{Case: "讀檔案並回答", Passed: false, Iterations: 3, TotalTokens: 300, ElapsedMs: 2000}
	for _, rec := range []eval.Record{first, second} {
		if err := eval.AppendRecord(path, rec); err != nil {
			t.Fatalf("AppendRecord: %v", err)
		}
	}

	got, err := eval.LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("讀回 %d 筆，期望 2（累積而非覆寫）", len(got))
	}
	// 順序要與寫入順序一致：回歸比對靠的就是「最後一筆是這次、前一筆是上次」。
	if got[0].Iterations != 1 || got[1].Iterations != 3 {
		t.Errorf("iteration 依序 = %d, %d，期望 1, 3", got[0].Iterations, got[1].Iterations)
	}
	if !got[0].Passed || got[1].Passed {
		t.Errorf("通過與否依序 = %v, %v，期望 true, false", got[0].Passed, got[1].Passed)
	}
}

// TestAppendRecordSurvivesNewAssertionKinds 是驗收條件「補一格斷言追加多筆後仍可
// 完整解析」。
//
// 用例加一條斷言不會改變歷史檔案的欄位，但它會改變**用例的通過與否**——一份已經累積
// 了幾十行的歷史檔案，必須在那之後仍然整份讀得回來，而不是從變動那一行開始壞掉。
// 一行一筆的格式正是為了這件事：任何一行都不依賴前後文。
func TestAppendRecordSurvivesNewAssertionKinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")

	// 前兩筆是加斷言之前的執行，第三筆是加了一條 max_iterations 之後的執行。
	before := []eval.Record{
		{Case: "同一條用例", Passed: true, Iterations: 5, TotalTokens: 500, ElapsedMs: 1000, CostMicroUSD: micro(120)},
		{Case: "同一條用例", Passed: true, Iterations: 5, TotalTokens: 510, ElapsedMs: 1100, CostMicroUSD: micro(121)},
	}
	after := eval.Record{Case: "同一條用例", Passed: false, Iterations: 8, ToolFailures: 2, TotalTokens: 900, ElapsedMs: 3000}
	for _, rec := range append(append([]eval.Record{}, before...), after) {
		if err := eval.AppendRecord(path, rec); err != nil {
			t.Fatalf("AppendRecord: %v", err)
		}
	}

	got, err := eval.LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("讀回 %d 筆，期望 3", len(got))
	}
	// 早期那幾筆的成本要原樣讀得回來，最後那筆的空成本也要**仍然是空的**：
	// 讀回時把 null 變成 0，等於在歷史裡憑空造出一次「不用錢」的執行。
	if got[0].CostMicroUSD == nil || *got[0].CostMicroUSD != 120 {
		t.Errorf("第一筆成本 = %v，期望 120", got[0].CostMicroUSD)
	}
	if got[2].CostMicroUSD != nil {
		t.Errorf("第三筆成本 = %d，期望空值", *got[2].CostMicroUSD)
	}
	if got[2].Iterations != 8 || got[2].ToolFailures != 2 {
		t.Errorf("第三筆 iteration/Tool 失敗 = %d/%d，期望 8/2", got[2].Iterations, got[2].ToolFailures)
	}
}

// TestAppendRecordCreatesParentDir 釘住輸出目錄不存在時自己建起來。
//
// 評測跑完才發現歷史寫不進去，等於那一次真實 Provider 的錢白花了——指標沒留下來。
func TestAppendRecordCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "還沒建的目錄", "history.jsonl")
	if err := eval.AppendRecord(path, eval.Record{Case: "x", Passed: true}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	got, err := eval.LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("讀回 %d 筆，期望 1", len(got))
	}
}

// TestLoadHistoryRejectsBrokenLine 釘住壞掉的一行要**明確報錯並指出是第幾行**，
// 不是安靜跳過。
//
// 安靜跳過的代價：一份被外部工具寫壞的歷史檔案會少掉幾筆，而回歸比對照樣得出結論
// ——用一份殘缺的資料。錯誤要被顯式處理（憲法 5.1），這裡的處理方式是不讓呼叫端
// 拿到一份它以為完整的清單。
func TestLoadHistoryRejectsBrokenLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := eval.AppendRecord(path, eval.Record{Case: "好的那筆", Passed: true}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("開啟歷史檔案: %v", err)
	}
	if _, err := f.WriteString("{這不是 JSON\n"); err != nil {
		t.Fatalf("寫入壞行: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("關閉歷史檔案: %v", err)
	}

	_, err = eval.LoadHistory(path)
	if err == nil {
		t.Fatal("期望報錯，實際成功")
	}
	// 第 2 行才是壞的；錯誤訊息不指出行號的話，一份幾百行的歷史檔案要人一行一行找。
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("錯誤訊息 = %q，期望指出是第 2 行", err)
	}
}

// TestLoadHistoryToleratesBlankLines 釘住空行被略過。
//
// 每筆記錄後面都跟一個換行，所以檔案結尾必然有一個空段——把它當成壞行的話，一份
// **完全正常**的歷史檔案永遠讀不回來。
func TestLoadHistoryToleratesBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	for i := 0; i < 2; i++ {
		if err := eval.AppendRecord(path, eval.Record{Case: "x", Passed: true, Iterations: i}); err != nil {
			t.Fatalf("AppendRecord: %v", err)
		}
	}
	got, err := eval.LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("讀回 %d 筆，期望 2（結尾的換行不算一筆）", len(got))
	}
}

// TestLoadHistoryMissingFileIsExplicit 釘住檔案不存在時回一個**辨認得出來**的錯誤。
//
// 回 (nil, nil) 會讓「還沒有歷史」與「路徑打錯」長得一模一樣，而後者是使用者最常犯
// 的錯。包成可用 errors.Is 判斷的形式，呼叫端要當成「還沒有歷史」處理時仍然做得到。
func TestLoadHistoryMissingFileIsExplicit(t *testing.T) {
	_, err := eval.LoadHistory(filepath.Join(t.TempDir(), "沒有這個檔案.jsonl"))
	if err == nil {
		t.Fatal("期望報錯，實際成功")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("錯誤 = %v，期望可用 errors.Is 判成檔案不存在", err)
	}
}
