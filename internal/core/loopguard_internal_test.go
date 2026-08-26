// 死循環守衛的參數規範化單元測試（ticket #54）。
//
// **這是 internal/core 的第一個內部測試檔，理由記在這裡。** normalizeToolArgs 是
// 未匯出的純函式，要驗它只有兩條路：開一個內部測試檔，或為了測試把它匯出。選前者
// ——internal/tool、internal/config、memory、storage 都已經有內部測試檔，不是新
// 發明；為測試而匯出才是真的擴大 API 表面，而規範化規則是守衛的實作細節，不是
// core 對外的承諾。
//
// core 的整合測試仍全部走 core_test 與 AgentService.Process 這個既有 seam
// （見 agent_loop_guard_test.go），這個檔案不驅動任何鏈路，只驗一個純函式。
package core

import "testing"

// TestNormalizeToolArgsEquivalence 驗規範化的**核心性質**：哪些寫法該收斂成同一個
// key、哪些不該。
//
// 斷言寫成「兩個輸入規範化後相不相等」而不是「輸出等於某個字串」，因為守衛真正
// 依賴的就是這個等價關係——輸出長什麼樣是實作細節，換一種規範化寫法只要等價關係
// 不變，這些格子就該保持綠色。
func TestNormalizeToolArgsEquivalence(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		same bool
	}{
		{
			// LLM 重送同一個呼叫時鍵序常常不同，這是繞過守衛最容易發生的一種。
			name: "鍵序不同視為同一 key",
			a:    `{"path":"notes/a.md","content":"X"}`,
			b:    `{"content":"X","path":"notes/a.md"}`,
			same: true,
		},
		{
			name: "JSON 結構空白不同視為同一 key",
			a:    `{"path":"notes/a.md"}`,
			b:    "{ \"path\" : \"notes/a.md\" }",
			same: true,
		},
		{
			name: "路徑等價寫法視為同一 key",
			a:    `{"path":"./a.txt"}`,
			b:    `{"path":"a.txt"}`,
			same: true,
		},
		{
			// **這一格原本寫成 same: true，那是錯的，理由記在這裡。**
			//
			// ticket #54 的原文說「路徑類欄位額外做去空白與路徑標準化」，但那句話有一個
			// 隱含前提：路徑裡的空白沒有語義。追 internal/tool/sandbox.go 的
			// CheckFilePath 會發現前提不成立——它對原始路徑只做 filepath.Clean，而
			// filepath.Clean("notes/missing ") 回的是 "notes/missing "，尾端空白原樣
			// 保留。`a.txt` 與 `a.txt ` 因此是**磁碟上兩個不同的檔案**。
			//
			// 守衛的等價定義必須與 Tool 的實際行為對齊：兩組參數會讓 Tool 做不同的事，
			// 它們就不是「同一條路的等價寫法」，不該共用失敗計數。合併它們會在兩個不同
			// 檔案各失敗一次時誤觸發，叫 LLM 停止一件它其實才剛開始做的事。
			name: "path 前後空白是不同的檔案，不得合併",
			a:    `{"path":" a.txt "}`,
			b:    `{"path":"a.txt"}`,
			same: false,
		},
		{
			// JSON 數字若先解成 float64 再序列化，超過 2^53 的整數會被截到同一個值
			// （實測 9007199254740993 → 9007199254740992）。MCP Tool 用 numeric ID
			// 當參數時，兩個不同的 ID 就會共用失敗計數。
			name: "超過 float64 精度的整數不得碰撞",
			a:    `{"id":9007199254740992}`,
			b:    `{"id":9007199254740993}`,
			same: false,
		},
		{
			// **尾隨的閉合括號是最陰險的一種，理由記在這裡。**
			//
			// Decoder.More() 回答的是「當前 array/object 裡還有沒有下一個元素」，不是
			// 「文件結束了沒有」。`{"a":1}]` 的尾隨 `]` 會被它讀成「當前容器結束」而回
			// false（實測確認），於是一個不合法的參數字串安靜地與合法的收斂成同一個 key
			// ——而 `{"a":1} 別的` 那種明顯的垃圾反而擋得住。
			//
			// 正確的做法是再 Decode 一次、要求錯誤剛好是 io.EOF。
			name: "尾隨的閉合括號不得被安靜丟掉",
			a:    `{"path":"a.txt"}`,
			b:    `{"path":"a.txt"}]`,
			same: false,
		},
		{
			// 同上，換一種閉合括號——兩者走的是 More() 的同一個盲點。
			name: "尾隨的右大括號不得被安靜丟掉",
			a:    `[1,2]`,
			b:    `[1,2]]`,
			same: false,
		},
		{
			// **反向的護欄**：尾隨空白不是「內容」，`{"a":1}\n` 是合法的 JSON 文件。
			// 少了這一格，一個把檢查寫得過嚴的實作（例如要求輸入逐字結束於 `}`）也會
			// 通過上面那些格子，卻讓 LLM 多送一個換行就繞過守衛。
			name: "尾隨空白不算尾隨內容",
			a:    `{"path":"a.txt"}`,
			b:    "{\"path\":\"a.txt\"}\n\t ",
			same: true,
		},
		{
			// 解析只吃掉第一個 JSON 值、不檢查後面還有沒有東西的話，尾隨內容會被安靜
			// 丟掉，兩個不同的參數字串於是收斂成同一個 key。
			name: "尾隨內容不得被安靜丟掉",
			a:    `{"path":"a.txt"}`,
			b:    `{"path":"a.txt"} 還有別的`,
			same: false,
		},
		{
			// **這一格是規則的邊界，不是可有可無的補充。** 內容裡的空白有語義：
			// 一個結尾多空白的檔案與不多的是兩份不同的檔案，把它們併成同一個 key
			// 會讓守衛在「LLM 正在逐步修正內容」時誤觸發。
			name: "寫檔內容的空白差異視為不同 key",
			a:    `{"path":"notes/a.md","content":"X"}`,
			b:    `{"path":"notes/a.md","content":"X "}`,
			same: false,
		},
		{
			// **移除 path 的 TrimSpace 之後，這一格才是守住那條界線的人。**
			// 原本靠「content 的前後空白差異」驗，但 path.Clean 本來就不動空白，
			// 把 content 加進路徑欄位清單也不會讓那一格轉紅——理由等於沒人守。
			//
			// 斜線才是 Clean 真正會動的東西：內容裡的 `//`（URL、程式碼、路徑字面
			// 都可能有）會被收成 `/`，兩份不同的檔案內容於是共用同一個 key。
			name: "寫檔內容裡的斜線不被當成路徑收斂",
			a:    `{"path":"notes/a.md","content":"x//y"}`,
			b:    `{"path":"notes/a.md","content":"x/y"}`,
			same: false,
		},
		{
			name: "非 JSON 參數退回去除前後空白",
			a:    "  這不是 JSON  ",
			b:    "這不是 JSON",
			same: true,
		},
		{
			// 路徑標準化只認 path 這個欄位名，其餘欄位的字串值原樣保留。
			// 這一格守住那條界線：換個欄位名就不該再被 Clean。
			name: "非 path 欄位的路徑寫法不被標準化",
			a:    `{"file_path":"./a.txt"}`,
			b:    `{"file_path":"a.txt"}`,
			same: false,
		},
		{
			name: "不同 path 仍是不同 key",
			a:    `{"path":"notes/a.md"}`,
			b:    `{"path":"notes/b.md"}`,
			same: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA, gotB := normalizeToolArgs(tt.a), normalizeToolArgs(tt.b)
			if same := gotA == gotB; same != tt.same {
				t.Errorf("規範化後相等 = %v, 期望 %v\n  a=%q → %q\n  b=%q → %q",
					same, tt.same, tt.a, gotA, tt.b, gotB)
			}
		})
	}
}

// TestNormalizeToolArgsExactOutput 驗幾個**確切輸出**：這些格子關心的不是等價關係，
// 而是「輸出本身仍然是人看得懂的那串參數」（ticket #54 要求 key 不做雜湊，日誌裡
// 要看得出是哪個參數在循環），以及不合法輸入不會炸掉整個 turn。
func TestNormalizeToolArgsExactOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// 規範化後仍是可讀的 JSON——這就是「不做雜湊」的具體樣子。
			// 收斂的是鍵序、結構空白與路徑寫法，**不含字串值裡的空白**。
			name: "合法 JSON 收斂成可讀的正規形式",
			in:   "{ \"path\" : \"./notes/a.md\" , \"content\" : \"X\" }",
			want: `{"content":"X","path":"notes/a.md"}`,
		},
		{
			// 大整數原樣輸出，不經 float64 中轉。
			name: "大整數保留原始字面",
			in:   `{"id":9007199254740993}`,
			want: `{"id":9007199254740993}`,
		},
		{
			// 尾隨內容讓整串參數不是一個合法的 JSON 文件，走回退分支。
			name: "尾隨內容退回去除前後空白",
			in:   `  {"path":"a.md"} 還有別的  `,
			want: `{"path":"a.md"} 還有別的`,
		},
		{
			// 尾隨閉合括號同樣不是合法的 JSON 文件，一樣走回退分支。
			name: "尾隨閉合括號退回去除前後空白",
			in:   `  {"path":"a.md"}]  `,
			want: `{"path":"a.md"}]`,
		},
		{
			// **path.Clean("") 會回 "."**，那會把「漏填 path」與「path 是當前目錄」
			// 併成同一個 key。空字串直接原樣留著，讓兩種失敗分得開。
			name: "空 path 不被標準化成當前目錄",
			in:   `{"path":""}`,
			want: `{"path":""}`,
		},
		{
			// 非法 JSON 走回退分支：不 panic、不上拋（憲法 5.1），只做最保守的
			// 去空白。守衛是輔助機制，它不該有能力讓一個 turn 失敗。
			name: "截斷的 JSON 退回去除前後空白",
			in:   `  {"path":"a.md"  `,
			want: `{"path":"a.md"`,
		},
		{
			// JSON 陣列與純量都是合法 JSON 但不是物件，沒有 path 欄位可標準化，
			// 收斂鍵序與空白之後原樣輸出。
			name: "合法但非物件的 JSON 只收斂空白",
			in:   `[1,  2]`,
			want: `[1,2]`,
		},
		{
			name: "空參數維持空字串",
			in:   "   ",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeToolArgs(tt.in); got != tt.want {
				t.Errorf("normalizeToolArgs(%q) = %q, 期望 %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestLoopGuardCountsAndResets 驗守衛的計數語義：同一個 key 連續失敗才累積、
// 達門檻才回報、**任一次成功清空整張表**。
//
// 這一支與整合測試的分工：整合測試證明守衛真的接進了 ReAct 循環、提示真的送到
// LLM；這一支證明計數規則本身正確，不必為了數到門檻跑三輪 LLM 回放。
func TestLoopGuardCountsAndResets(t *testing.T) {
	const threshold = 3
	call := ToolCall{Name: "write_file", Arguments: `{"path":"notes/a.md","content":"X"}`}
	// 等價寫法：鍵序與空白都不同，規範化後與 call 是同一個 key。
	equivalent := ToolCall{Name: "write_file", Arguments: "{ \"content\" : \"X\" , \"path\" : \"./notes/a.md\" }"}
	// **參數完全相同、只有 Tool 名不同。** 這樣才驗得到 Tool 名真的納入了 key——
	// 參數也跟著不同的話，就算實作漏掉 Tool 名，兩者照樣會分開計數。
	other := ToolCall{Name: "read_file", Arguments: call.Arguments}
	fail := ToolResult{Error: "boom"}
	ok := ToolResult{OK: true, Content: "done"}

	g := newLoopGuard(threshold)

	if _, n := g.observe(call, fail); n != 0 {
		t.Errorf("第一次失敗回報 %d, 期望 0（未達門檻）", n)
	}
	// 換等價寫法仍累積到同一個 key——這是規範化存在的理由。
	if _, n := g.observe(equivalent, fail); n != 0 {
		t.Errorf("第二次失敗（等價寫法）回報 %d, 期望 0（未達門檻）", n)
	}
	// 另一個 Tool 各自計數，不干擾上面那個 key；這裡讓它也累積到門檻前一次，
	// 下面才驗得到「成功清空的是整張表」。
	for i := 1; i <= threshold-1; i++ {
		if _, n := g.observe(other, fail); n != 0 {
			t.Errorf("其他 Tool 第 %d 次失敗回報 %d, 期望 0", i, n)
		}
	}
	normalized, n := g.observe(call, fail)
	if n != threshold {
		t.Fatalf("第三次失敗回報 %d, 期望 %d（達門檻）", n, threshold)
	}
	// 回報的是規範化後的參數本身（不是雜湊），日誌才看得出是哪個參數在循環。
	if want := `{"content":"X","path":"notes/a.md"}`; normalized != want {
		t.Errorf("回報的規範化參數 = %q, 期望 %q", normalized, want)
	}

	// 一次成功清空**整張表**，不只清這個 key：LLM 換路成功代表它跳出了循環，
	// 讓別的 key 帶著舊計數繼續累積，會在後面某次無關的失敗上誤觸發。
	if _, n := g.observe(call, ok); n != 0 {
		t.Errorf("成功時回報 %d, 期望 0", n)
	}
	// other 在成功前已累積到門檻前一次。沒清整張表的話，這一次就會觸發。
	if _, n := g.observe(other, fail); n != 0 {
		t.Errorf("成功歸零後其他 Tool 的失敗回報 %d, 期望 0——成功應清空整張表，不只清當次那個 key", n)
	}
	if _, n := g.observe(call, fail); n != 0 {
		t.Errorf("成功歸零後的第一次失敗回報 %d, 期望 0", n)
	}
}
