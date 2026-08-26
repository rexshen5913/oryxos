package core

import (
	"encoding/json"
	"path"
	"strings"
)

// loopGuard 是死循環守衛：數同一個 Tool 帶**等價參數**的連續失敗次數，達門檻時讓
// 回填給 LLM 的內容多帶一段要求改變策略的提示（issue #38 第三項、ticket #54）。
//
// **生命週期是單次 Run，不是這個結構本身。** 它由 ReActLoop.Run 在每個 turn 開頭
// 建一個新的，計數因此不跨 turn 保留：使用者在兩個 turn 之間可能已經把缺的目錄建
// 好、或換了問法，上一輪的失敗說明不了這一輪。把它掛成 ReActLoop 的欄位會同時違反
// 這一點與憲法 5.2——那等於用共享狀態把上一個 turn 的事實帶進下一個。
//
// **它數的是「邏輯呼叫」，不是 executeWithRetry 內部的重試。** 那一層處理的是瞬時
// 網路故障（同一次呼叫退避重試三次），這一層看的是 LLM 在對話層一再選擇同一條路。
// 兩者量級不同，本票完全沒有動 Retryable 與 maxToolRetries。
type loopGuard struct {
	threshold int
	failures  map[loopGuardKey]int
}

// loopGuardKey 標識「同一條路」：同一個 Tool ＋ 同一組等價參數。
//
// 用結構而不是把兩者拼成一個字串：拼接得挑一個分隔符，而任何分隔符都可能出現在參數
// 內容裡（寫檔內容是使用者資料，什麼字元都有可能）。結構的欄位比對沒有這個問題。
type loopGuardKey struct {
	tool string
	// args 是**規範化後的參數本身，不是雜湊**（ticket #54 明訂）。除錯時要看得出
	// 是哪個參數在循環，雜湊過的 key 在日誌裡等於一串沒有用的亂碼。
	args string
}

// newLoopGuard 以連續失敗門檻建一個守衛。
func newLoopGuard(threshold int) *loopGuard {
	return &loopGuard{threshold: threshold, failures: make(map[loopGuardKey]int)}
}

// observe 記一次 Tool 呼叫的結果，回傳規範化後的參數與**達門檻時的連續失敗次數**。
//
// 次數為 0 代表不該觸發（這次成功，或還沒數到門檻）；達門檻時回傳實際次數，讓呼叫端
// 把它寫進提示與日誌——「已經連續失敗 3 次」比「失敗了很多次」更能讓 LLM 判斷該收手。
//
// **任一次成功清空整張表**，不只清當次那個 key：成功代表 LLM 已經跳出了循環，讓別的
// key 帶著舊計數留下來，會在後面某次無關的失敗上誤觸發，等於教它別再用那個 Tool。
func (g *loopGuard) observe(call ToolCall, result ToolResult) (string, int) {
	if result.OK {
		clear(g.failures)
		return "", 0
	}
	key := loopGuardKey{tool: call.Name, args: normalizeToolArgs(call.Arguments)}
	g.failures[key]++
	if g.failures[key] < g.threshold {
		return key.args, 0
	}
	return key.args, g.failures[key]
}

// pathLikeArgKeys 是要額外做路徑標準化的參數欄位名。
//
// **只有 path 一個，而且這是刻意的。** 那是本專案內建 Tool 唯一的路徑欄位名
// （read_file、write_file、list_dir 都用它）。MCP server 若用別的名字（file_path
// 之類），它的路徑等價寫法就規範化不到——那是**已知限制**，不是遺漏。預先把別人
// 可能用的欄位名一條條猜進來，猜錯的部分會永遠留在這裡沒人敢刪（憲法 3.1 YAGNI）。
var pathLikeArgKeys = []string{"path"}

// normalizeToolArgs 把 Tool 參數收斂成守衛比對用的形式。
//
// **這是守衛的核心**：少了它，LLM 換個等價寫法（調鍵序、多加空白）就繞過去了，
// 門檻永遠數不到。四條規則與各自的理由：
//
//  1. **反序列化再序列化**收斂鍵序與結構空白——json.Marshal 對 map 依 key 排序輸出，
//     鍵序就是在那裡收斂的。
//  2. **path 欄位額外套與 Tool 相同的路徑 clean**：`./a.txt` 與 `a.txt` 走到
//     CheckFilePath 會得到同一個 rel，是同一個檔案。
//  3. **所有字串值的內容原樣保留，path 也不例外。** 見 normalizePathArg 的說明——
//     這一條與 ticket 原文的措辭有出入，理由記在那裡。
//  4. **數字保留原始字面、尾隨內容一律不接受**——兩者都由 decodeToolArgs 負責，
//     它們的理由與踩過的坑寫在那裡（redact.go）。去敏走的是同一個入口。
//  5. **解析不成功時退回去除前後空白**，不 panic、不上拋（憲法 5.1）。守衛是輔助
//     機制，它不該有能力讓一個 turn 失敗。
func normalizeToolArgs(args string) string {
	// 解析與去敏共用同一個入口（見 decodeToolArgs）：數字保留原始字面，尾隨內容
	// 一律視為不合法。兩邊對「什麼算一份合法參數」的答案必須一致，否則守衛的 key
	// 與日誌記下的那份會是不同的東西。
	v, ok := decodeToolArgs(args)
	if !ok {
		return strings.TrimSpace(args)
	}
	// 只有 JSON 物件才有欄位可標準化；陣列與純量是合法 JSON，但沒有 path 可談，
	// 走到下面重新序列化收斂空白就夠了。
	if obj, ok := v.(map[string]any); ok {
		for _, k := range pathLikeArgKeys {
			if s, ok := obj[k].(string); ok {
				obj[k] = normalizePathArg(s)
			}
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(args)
	}
	return string(out)
}

// normalizePathArg 對路徑值套用與 Tool 相同的標準化。
//
// **刻意不做 TrimSpace，這與 ticket #54 的原文措辭有出入，理由記在這裡。**
//
// ticket 說「路徑類欄位額外做去空白與路徑標準化」，那句話有一個隱含前提：路徑裡的
// 空白沒有語義（它把「內容裡的空白有語義」寫成了對比）。追 internal/tool/sandbox.go
// 的 CheckFilePath 會發現前提不成立——它對原始路徑只做
// `filepath.Clean(filepath.FromSlash(rawPath))`，而 filepath.Clean("notes/missing ")
// 回的是 "notes/missing "，尾端空白原樣保留。`a.txt` 與 `a.txt ` 因此是磁碟上兩個
// 不同的檔案（POSIX 檔名本來就允許空白）。
//
// **守衛的等價定義必須與 Tool 的實際行為對齊**：兩組參數會讓 Tool 去動不同的檔案，
// 它們就不是「同一條路的等價寫法」。合併它們會在兩個不同檔案各失敗一次時誤觸發，
// 叫 LLM 停止一件它其實才剛開始做的事——那比漏判更糟，因為它打斷的是正常流程。
//
// 用 path.Clean 而不是 filepath.Clean：Tool 的路徑參數一律是相對 Workspace 根的
// slash 路徑，而本專案不支援 Windows（ADR-0006）。在 Unix 上 filepath.FromSlash 是
// no-op、filepath.Clean 就是 path.Clean，兩者對這些路徑逐字等價——所以這裡「與 Tool
// 相同」是真的相同，不是近似。
//
// **空字串不進 Clean**：path.Clean("") 回 "."，那會把「漏填 path」與「path 就是當前
// 目錄」併成同一個 key。兩者是不同的失敗，該分得開。
func normalizePathArg(s string) string {
	if s == "" {
		return s
	}
	return path.Clean(s)
}
