package core

import (
	"fmt"
	"strings"
)

// ToolErrorKind 是一次 Tool 失敗的**領域類型**，回答「這是哪一類失敗」。
//
// **與 ToolResult.Retryable 正交。** 一個回答「要不要重試」、一個回答「這是哪一類
// 失敗」，兩者互不推導：sandbox 拒絕與參數錯誤都不可重試，upstream 故障可重試，而
// 「不可重試」本身說不出下一步該做什麼。本票新增這個型別時**完全沒有動** Retryable
// 的判定（TestToolErrorKindDoesNotDisturbRetryable 釘住這一點）。
//
// **零值是「未分類」，這是刻意的。** 未分類的失敗回填內容與這個型別出現之前**逐位元組
// 相同**（Guidance 回空字串），所以遷移可以逐個 Tool 進行，不必一次改完 93 個 ToolResult
// 字面。與 SandboxDecision 的零值取向不同——那裡零值是「擋下來」（fail closed，漏填
// 是安全問題），這裡零值是「什麼都不附加」（漏填只是少了一段指引，不會改變任何既有
// 行為）。兩個型別的零值各自對著自己的失敗代價選，不是同一條規則。
//
// **七種類型的常數與指引一次立齊，但生產路徑上只有三類真的會被產生。** ticket #51
// 只遷 sandbox、not_found、invalid_args；permission、timeout、upstream、limit_reached
// 的常數先佔位，等後續逐個 Tool 遷移時才有產生點。這是 issue #38 第二項「零值代表
// 未分類、遷移逐個 Tool 進行」的直接後果，不是漏接線——分類粒度本身是那份 spec 的
// 決策，把七類一次寫齊才不會日後各憑各的直覺再切一次。
type ToolErrorKind int

const (
	// ToolErrorUnclassified 是零值：這次失敗還沒有被分類。行為等同本票之前——
	// 回填內容只有原始錯誤，不附任何指引。
	ToolErrorUnclassified ToolErrorKind = iota

	// ToolErrorSandbox 是 Sandbox 校驗拒絕（HTTP 域名、檔案路徑、Shell 命令）。
	//
	// **這一類的 Guidance 刻意回空字串，理由是這一類的「下一步」不只一種。** 本專案的
	// Sandbox 拒絕有三種形態，各自該對 LLM 說的話完全不同：
	//
	//  1. **白名單拒絕** — 路徑／域名／命令不在清單裡。訊息指名被拒的是什麼，以及該把
	//     它加進 Workspace config.yaml 的哪一段（file.allowed_paths、
	//     http.allowed_domains、shell.allowed_commands）。shell 那則還多一句 issue #36
	//     依真實觀測加上的「不要逐一嘗試其他命令名，直接告訴使用者」。
	//  2. **輸入本身有問題** — 絕對路徑、命令含路徑分隔符、URL 解析不了。這些**不是**
	//     改設定能放行的，該改的是參數；訊息因此說的是「請改用相對路徑」「只接受不含
	//     路徑的名字」這類話。
	//  3. **不會放行的安全限制** — 路徑穿越出 Workspace 根、符號連結、裝置檔／具名管道
	//     ／socket、非 http/https 的 scheme。這些一律拒絕，訊息要讓 LLM 知道別再試，
	//     換一條路或告訴使用者。
	//
	// **一句通用指引服務不了這三種**：叫人去改設定對第 2、3 種是錯的建議，泛泛地說
	// 「這被拒絕了」則是廢話。所以這一類的下一步由**每一則訊息自己帶**，通用指引留空。
	//
	// 這也正好滿足另一條要求：#36 那則專屬訊息不被覆蓋、也不被重複附加
	// （TestShellWhitelistSpecificMessageKept 釘住）。空字串讓這件事對**整個類別**一致
	// 成立，不需要一個「這則已有專屬指引」的旗標——旗標會有人忘了設。
	//
	// **維護契約（代價要講清楚）**：這一類沒有兜底，新增 Sandbox 拒絕點時訊息品質得靠
	// 寫的人守住。標準是**給出符合該拒絕原因的可行下一步**——照上面三種形態對號入座，
	// 別把第 2、3 種寫成「請加進白名單」。
	ToolErrorSandbox

	// ToolErrorNotFound 是目標不存在：檔案、目錄或 MCP server。
	ToolErrorNotFound

	// ToolErrorInvalidArgs 是參數 JSON 不合法，或欄位缺漏、型別不對。
	ToolErrorInvalidArgs

	// ToolErrorPermission 是作業系統層拒絕（與 ToolErrorSandbox 不同：那是本專案的
	// 應用層白名單，這是 OS 的權限位）。
	ToolErrorPermission

	// ToolErrorTimeout 是逾時。
	ToolErrorTimeout

	// ToolErrorUpstream 是對端故障：HTTP 目標或 MCP server。
	ToolErrorUpstream

	// ToolErrorLimitReached 是已達上限：admission slot 已滿，或超出回填上限。
	ToolErrorLimitReached
)

// Guidance 回傳這一類失敗要附給 LLM 的**下一步指引**；空字串代表這一類沒有通用指引。
//
// **這是個純函式，那正是它與散落在各處的錯誤字串最大的差別**（issue #38 第二項）：
// 二十個 Tool 裡的二十段字串沒有辦法整批檢查，收成一張表就可以——哪一類有指引、指引
// 彼此是否真的不同，都能表格驅動測（見 toolerror_test.go）。
//
// **措辭是對 LLM 說的，不是對使用者說的。** issue #36 量到的差別就在這裡：同一個模型、
// 同樣形狀的失敗，訊息只說「不被允許」時它換了 10 個命令名用光 max_iterations；訊息
// 明講「不要逐一嘗試，直接告訴使用者」之後 1 次就收斂。所以每一段都要回答兩件事——
// **這次該做什麼**，以及**什麼時候該停下來問人**。
//
// **措辭有三條硬規則**，都是踩過才寫下來的：
//
//  1. **不指名任何 Tool。** 可用的 Tool 由 Profile 的 tools 欄位過濾，這裡看不到那份
//     清單。寫「例如用 list_dir」會讓一個只有 read_file 的 Agent 去呼叫不存在的 Tool，
//     多燒一次 iteration——正好是 issue #36 要治的那個病。要動態建議得把工具清單傳進
//     來，那就不是純函式了，本票不走那條路。
//  2. **不假設重試歷史。** 這個型別與 Retryable 正交，同一個類型可能配 Retryable 為
//     真或假；寫「循環已經替你重試過」在後者身上就是說謊。只能陳述誰負責重試，不能
//     陳述重試發生過。
//  3. **不對世界下斷言。** 「這不是暫時性故障」管不了別的行程等一下會不會把檔案建出來。
//     能說的是**同一個呼叫再送一次會怎樣**，那是我們自己這條鏈路上的事實。
//
// 未列出的值（含零值與日後新增卻忘了寫指引的常數）一律安靜地回空字串：不 panic，也
// 不附一段看起來像指引的預設話——後者比沒有更糟，它會用一句萬用廢話取代真正的分類。
func (k ToolErrorKind) Guidance() string {
	switch k {
	case ToolErrorNotFound:
		return "這個目標不存在。重送同一個呼叫不會讓它出現——先確認上層路徑實際有什麼，" +
			"再用確認過的確切名字呼叫一次；沒有辦法確認、或確認後它真的不在，" +
			"就直接告訴使用者，不要逐一猜名字。"
	case ToolErrorInvalidArgs:
		return "參數不符合這個 Tool 宣告的 input schema。" +
			"對照工具宣告修正欄位名稱、型別與必填項後再呼叫一次；" +
			"不要改用別的 Tool 繞過去，那通常只是換一種錯法。"
	case ToolErrorPermission:
		return "作業系統層拒絕了這次存取。重送同一個呼叫不會改變權限——" +
			"請直接告訴使用者是哪一個目標需要調整，由他決定要不要改。"
	case ToolErrorTimeout:
		return "這次執行逾時了。重送同一個呼叫多半會再逾時；" +
			"改用範圍更小、更快完成的做法，或告訴使用者這件事需要更長的時間。"
	case ToolErrorUpstream:
		return "對端服務故障，問題不在你送的參數。重送同一個呼叫通常不會改變結果——" +
			"暫時性故障的重試由 ReAct 循環自己處理，不必你在對話層再試一次。" +
			"改用別的做法，或告訴使用者這個服務目前不可用。"
	case ToolErrorLimitReached:
		return "已達上限。若是同時執行的數量滿了，先做別的事、稍後再回來；" +
			"若是這次請求本身太大，縮小範圍再呼叫一次。" +
			"重複送出完全相同的呼叫不會讓上限變寬。"
	}
	// ToolErrorUnclassified 與 ToolErrorSandbox 都落在這裡，理由各自寫在常數上。
	return ""
}

// toolGuidanceSeparator 把指引與原始錯誤隔成兩段。
//
// 用空行而不是一個空格：原始錯誤本身可能就是多行（shell 的 stderr、MCP 的錯誤物件），
// 接在同一段裡 LLM 分不出哪一句是 Tool 說的、哪一句是我們說的。
const toolGuidanceSeparator = "\n\n"

// toolMessageContent 是 Tool 結果回填給 LLM 的**單一組裝點**，順序固定為
// 「原始錯誤 → 類型指引」。
//
// **單一組裝點是本票的交付物之一**（issue #38 第二項、第三項）：死循環守衛（issue #38
// 第三項、ticket #54）接在同一個地方、排在類型指引之後，日後調整措辭或順序都只有這裡
// 要改，不必去找三個散落的拼接點。
//
// **只有回填走這裡，審計不走。** 審計記的是「Tool 實際回報了什麼」，我們附加的指引不
// 屬於那個事實，所以 ReActLoop.execute 落庫時用的是 result.Error 原文（見 react.go；
// TestToolErrorGuidanceReachesLLMButNotAudit 從同一次執行的兩邊同時斷言這個分岔）。
//
// retries 是實際重試次數，只有可重試的失敗才會大於零——那段措辭與位置都維持本票之前
// 的原樣，未分類（零值）的結果因此逐位元組不變。
//
// repeated 是死循環守衛回報的連續失敗次數（ticket #54），大於零才附提示、且一定排在
// 類型指引**之後**。兩段話的分工是刻意的：類型指引說的是「這一類失敗該怎麼辦」，
// 死循環提示說的是「你已經在這條路上原地打轉」——後者只有在前者顯然沒被聽進去時才
// 該出現，順序反過來等於先喊停再解釋，LLM 會少掉判斷的依據。
func toolMessageContent(result ToolResult, retries, repeated int) string {
	if result.OK {
		return result.Content
	}

	var b strings.Builder
	if retries > 0 && result.Retryable {
		fmt.Fprintf(&b, "Tool 執行失敗（已重試 %d 次）: %s", retries, result.Error)
	} else {
		b.WriteString("Tool 執行失敗: ")
		b.WriteString(result.Error)
	}
	if guidance := result.ErrorKind.Guidance(); guidance != "" {
		b.WriteString(toolGuidanceSeparator)
		b.WriteString(guidance)
	}
	if repeated > 0 {
		b.WriteString(toolGuidanceSeparator)
		b.WriteString(loopGuardNotice(repeated))
	}
	return b.String()
}

// loopGuardNotice 是死循環守衛達門檻時附加的提示（ticket #54）。
//
// 措辭沿用 ToolErrorKind.Guidance 那三條硬規則（不指名任何 Tool、不假設重試歷史、
// 不對世界下斷言），另外多守一條**本票特有**的：
//
//   - **明說「換寫法不算換做法」。** 守衛存在的理由就是 LLM 會調鍵序、多加空白再送
//     一次同樣的呼叫；不把這件事講出來，它完全可以照著「請改變策略」照做，然後送出
//     一組等價參數，下一輪再收到同一段提示。這句話是把規範化規則本身告訴 LLM——
//     沒有它，守衛只擋得住循環的症狀，擋不住成因。
//
// 帶上實際次數而不是含糊地說「很多次」：具體的數字讓 LLM 判斷得出自己已經走多遠，
// 也讓這段話在對話記錄裡可查證。
//
// **與 sandbox 那類的專屬訊息並存**：那一類沒有通用指引（見 ToolErrorSandbox），
// 訊息末尾的專屬建議在單次拒絕時不會被接上任何東西（TestShellWhitelistSpecificMessageKept
// 釘住）。連續三次之後才附這段是刻意的——那時專屬建議顯然沒有奏效，加碼不是稀釋。
func loopGuardNotice(repeated int) string {
	return fmt.Sprintf("你已經用等價的參數連續呼叫這個 Tool 失敗 %d 次——"+
		"調整鍵序或空白不算換了做法。"+
		"再送一次相同或等價的參數不會有不同結果：請改變策略（換參數、換途徑，"+
		"或先取得缺少的資訊），或者直接告訴使用者卡在哪裡、需要他提供什麼。", repeated)
}
