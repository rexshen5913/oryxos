package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record 是一次用例執行的結構化結果，歷史檔案裡一行一筆。
//
// **為什麼要有這個型別**：沒有它的話，評測只回答得了是非題——這一次通過了沒有。而
// 退步不是是非題，是程度問題（issue #36 那個 10→1 的 iteration 數，改回 10 之後回應
// 依然正確、Tool 依然被呼叫，兩條布林斷言全都照樣綠燈）。要說出「比上次多用了兩個
// iteration」，就必須有「上次」——所以指標要留下來，而且要**累積**。
//
// 欄位就是驗收條件列的那七項，不多不少。刻意不加時間戳：歷史檔案是**追加**的，行的
// 先後本身就是時間順序，多一個欄位只是多一個要維護的東西（憲法 3.1）。也刻意不存
// 未通過原因：那是給當下看的診斷，而這份檔案是給日後比對用的指標。
type Record struct {
	Case         string `json:"case"`
	Passed       bool   `json:"passed"`
	Iterations   int    `json:"iterations"`
	ToolFailures int    `json:"tool_failures"`
	TotalTokens  int    `json:"total_tokens"`
	ElapsedMs    int64  `json:"elapsed_ms"`
	// CostMicroUSD 是成本，單位百萬分之一美元；**JSON 裡是 null 而不是 0 代表沒算**。
	//
	// 指標型別是這條語義唯一活得下來的形式：換成 int64 的話，nil 在序列化時會變成 0，
	// 而 0 在報表上讀起來是「這次不用錢」——與 ticket #49 落庫時 NULL 的語義正好相反。
	CostMicroUSD *int64 `json:"cost_micro_usd"`
}

// NewRecord 把一次執行的三份輸入收斂成一筆結構化結果。
//
// 耗時由呼叫端量而不是在這裡量：它涵蓋的是整個 RunCase（含建 Workspace 與查審計表），
// 那個範圍只有呼叫端知道從哪裡開始。
func NewRecord(c Case, v Verdict, r RunResult, elapsed time.Duration) Record {
	return Record{
		Case:         c.Name,
		Passed:       v.Passed,
		Iterations:   r.Iterations,
		ToolFailures: r.ToolFailures,
		TotalTokens:  r.TotalTokens,
		ElapsedMs:    elapsed.Milliseconds(),
		CostMicroUSD: r.CostMicroUSD,
	}
}

// Summary 是印給人看的那一行，七項資訊都在同一行裡。
//
// 與寫進歷史檔案的 JSON 是**同一筆資料的兩種呈現**，不是兩份各自維護的東西：JSON 給
// 程式讀，這一行給正在等結果的人讀。組裝放在這裡而不是 cmd/oryxos-eval，是為了讓它
// 測得到——特別是成本算不出來時不得印成 0 這條，那是在終端機上一眼看不出錯的那種錯。
func (r Record) Summary() string {
	status := "未通過"
	if r.Passed {
		status = "通過"
	}
	return fmt.Sprintf("%s … %s（iteration %d、Tool 失敗 %d、token %d、耗時 %s、成本 %s）",
		r.Case, status, r.Iterations, r.ToolFailures, r.TotalTokens,
		(time.Duration(r.ElapsedMs) * time.Millisecond).String(), r.costText())
}

// costText 把微美元的成本印成美元金額；算不出來時明說「未計算」。
//
// 六位小數不是排版偏好：單價的單位是每百萬 token 幾美元，單次呼叫常低於一美分，
// 少印幾位會讓一次真實的花費顯示成 0.00——那正是 ticket #49 存整數微美元要防的事。
func (r Record) costText() string {
	if r.CostMicroUSD == nil {
		return "未計算"
	}
	return fmt.Sprintf("$%.6f", float64(*r.CostMicroUSD)/1e6)
}

// AppendRecord 把一筆結果**追加**到歷史檔案末尾，一行一筆（JSON Lines）。
//
// **追加而非覆寫是這個檔案存在的全部意義**，所以用 O_APPEND 而不是先讀進來、加一筆、
// 再整份寫回：後者在中途失敗時會留下一份被截斷的歷史，而前者最壞只是少了最後一行。
//
// 為什麼是一行一筆而不是一整份 JSON 陣列：陣列要追加就得先把整份讀回來、改掉結尾的
// `]`、再寫回去，於是每次執行都在改寫全部歷史。一行一筆則是純追加，而且**任何一行都
// 不依賴前後文**——這正是「補一格斷言之後舊記錄仍然完整可解析」那條驗收條件成立的
// 原因（見 LoadHistory）。
func AppendRecord(path string, rec Record) (err error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 目錄不存在就建起來：評測跑完才發現指標寫不進去，等於那一次真實 Provider
		// 呼叫的錢白花了。
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("建立歷史檔案目錄 %s: %w", dir, err)
		}
	}
	// 先序列化再開檔：序列化失敗時連檔案都不必碰，也就不會留下一個空檔案。
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化用例 %s 的評測結果: %w", rec.Case, err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("開啟歷史檔案 %s: %w", path, err)
	}
	defer func() {
		// 關檔的錯誤不能吞：緩衝的內容是在關檔時才真正寫出去的，吞掉它等於宣稱一筆
		// 其實沒落地的記錄寫成功了。
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("關閉歷史檔案 %s: %w", path, cerr)
		}
	}()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("寫入歷史檔案 %s: %w", path, err)
	}
	return nil
}

// LoadHistory 把整份歷史檔案讀回成記錄清單，順序即寫入順序。
//
// **壞掉的一行明確報錯並指出行號，不安靜跳過。** 跳過的話，一份被外部工具寫壞的歷史
// 會少掉幾筆，而比對照樣得出結論——用一份殘缺的資料。錯誤要被顯式處理（憲法 5.1），
// 這裡的處理方式是不讓呼叫端拿到一份它以為完整的清單。
//
// 檔案不存在時回傳的錯誤包著 fs.ErrNotExist：呼叫端要把它當成「還沒有歷史」處理時，
// 用 errors.Is 判得出來；而回 (nil, nil) 會讓「還沒有歷史」與「路徑打錯」長得一模一樣。
func LoadHistory(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取歷史檔案 %s: %w", path, err)
	}

	var records []Record
	for i, line := range strings.Split(string(data), "\n") {
		// 空行略過：每筆記錄後面都跟一個換行，所以檔案結尾必然有一個空段——把它當成
		// 壞行的話，一份完全正常的歷史檔案永遠讀不回來。
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("解析歷史檔案 %s 第 %d 行: %w", path, i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}
