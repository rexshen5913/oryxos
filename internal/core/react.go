package core

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

const (
	// maxToolRetries 是單次 Tool 呼叫失敗後的重試次數上限（需求 8.2：
	// 可重試失敗按指數退避重試、最多三次）。
	maxToolRetries = 3
	// toolRetryBaseDelay 是指數退避的起始等待（100ms→200ms→400ms）：
	// 針對瞬時網路故障，重試耗盡的額外延遲控制在一秒內。
	toolRetryBaseDelay = 100 * time.Millisecond
)

// ReActLoop 是 Agent 的核心工作機制：呼叫 LLM、視回應決定呼叫 Tool 或給出
// 最終回應，直到終止或達最大迭代次數。自行實作、完全可控（憲法 2.1）。
type ReActLoop struct {
	provider  ProviderService
	tools     ToolExecutor
	memory    MemoryService
	audit     AuditStore
	bootstrap ContextLoader
	// events 播報循環內的執行過程。播報點與既有的審計落點**相鄰**：兩者記的是同一
	// 批事實，放在一起可以避免日後加事件時漏掉其中一邊。
	events EventSink
	// prices 是 Workspace 的定價表，用來把 token 用量換算成成本（ticket #49）。
	//
	// **允許為 nil**，與上面幾個依賴不同：nil 代表這個 Workspace 沒有配置定價，
	// 每次呼叫的成本因此落 NULL。map 的零值讀取本來就安全，不需要一個 NopPriceList
	// 來表達「沒有」——那是 EventSink 那種介面才需要的形狀。
	prices PriceList
	// logger 落引擎層的結構化日誌。目前只有 Skill 段截斷這一種——它是「持續
	// 存在的降級」，每個 turn 都成立，得在每個 turn 記得到（見 Run）。
	logger *slog.Logger
}

// NewReActLoop 以 provider、Tool 子集、Memory 門面、審計儲存、上下文載入器、
// 事件流、定價表與 logger 建立 ReAct 循環；除 prices 外都不得為 nil（不關心執行
// 過程的呼叫端傳 NopEventSink，沒有配置定價的 Workspace 傳 nil）。
func NewReActLoop(provider ProviderService, tools ToolExecutor, memory MemoryService, audit AuditStore, bootstrap ContextLoader, events EventSink, prices PriceList, logger *slog.Logger) *ReActLoop {
	return &ReActLoop{provider: provider, tools: tools, memory: memory, audit: audit, bootstrap: bootstrap, events: events, prices: prices, logger: logger}
}

// Run 對 session 的當前對話歷史跑 ReAct 循環，回傳 Agent 的最終回應。每輪把
// LLM 回應與 Tool 結果追加到 session 對話歷史（完整可查）：LLM 沒有 Tool 呼叫
// 即為最終回應；有則按宣告順序逐一執行（不並行），結果以 tool 訊息回填後進入
// 下一輪。Tool 失敗（含 SandboxViolation）不是硬錯誤：可重試的失敗先按指數
// 退避重試，重試耗盡或不可重試時錯誤作為 tool 結果回填給 LLM，由 LLM 決定
// 下一步。達 settings.max_iterations 時強制終止，回覆固定提示語＋最後一輪
// LLM 內容（若有），turn 保留不算錯誤——已執行的 Tool 結果留在歷史。
func (l *ReActLoop) Run(ctx context.Context, profile *Profile, session *Session) (string, error) {
	maxIterations := profile.Settings.effectiveMaxIterations()
	defs := l.tools.Definitions()

	// Bootstrap 與長期記憶都在**進入迭代迴圈之前**取一次快照，同一 turn 內的
	// 後續迭代重用它：system prompt 在 turn 內固定，LLM 第二次迭代看到的前提與
	// 它第一次決策時一致，組裝函式也維持無檔案 I/O。使用者手改檔案則在**下一個
	// turn** 生效——每個 turn 重讀、不緩存（技術方案 §5.3）。
	//
	// 被快照的只有這兩者——**對話歷史絕不快照**，每次組裝都從當前 Session 重新
	// 取，否則第二次迭代看不到本 turn 剛回填的 tool 結果，ReAct 循環直接壞掉。
	//
	// 要讀哪幾份由 Profile 的 bootstrap 欄位與 ADR-0003 的互斥共同決定；沒被選中的
	// 檔案完全不碰，一個用不到的檔案壞掉不該讓每個 turn 都失敗。
	sel, err := profile.BootstrapSelection()
	if err != nil {
		return "", fmt.Errorf("解析 Profile %s 的 bootstrap 欄位: %w", profile.Name, err)
	}
	boot, err := l.bootstrap.Bootstrap(ctx, sel)
	if err != nil {
		return "", err // 載入端已帶足夠的上下文，不重複包裝
	}
	longTerm, err := l.memory.LongTermMemory(ctx)
	if err != nil {
		return "", fmt.Errorf("載入長期記憶: %w", err)
	}
	// Skill 段與上面兩者同一條規則：每個 turn 重讀、在迭代迴圈之外取一次快照。
	// 載入失敗 fail 該 turn，不靜默降級成「這個 Agent 沒有技能」。
	refs, err := profile.SkillRefs()
	if err != nil {
		return "", fmt.Errorf("解析 Profile %s 的 skills 欄位: %w", profile.Name, err)
	}
	skills, err := l.bootstrap.Skills(ctx, refs)
	if err != nil {
		return "", err // 載入端已指名是哪一份 Skill，不重複包裝
	}
	// 截斷要**每個 turn** 記，不能只靠啟動時算的那一次：description 每個 turn 重讀，
	// 使用者在對話中途把某份寫長就可能跨過上限，而啟動時的快照對此一無所知。
	// 這裡落結構化日誌、不寫 CLI：對話進行中插播提醒會打斷使用者，而日誌是這類
	// 「持續存在的降級」該待的地方；啟動時的 CLI 提醒仍在（見 cmd/oryxos/chat.go）。
	skillSection, dropped := ComposeSkillSection(skills)
	if dropped > 0 {
		l.logger.Warn("skill_section_truncated",
			"profile", profile.Name, "declared", len(skills), "dropped", dropped,
			"limit_runes", MaxSkillSectionRunes, "phase", "turn")
	}
	// Skill 段叫 LLM 用 load_skill 取回正文（見 skillSectionIntro），但那個 Tool 在
	// 不在可用集合裡是**組裝點**決定的。兩邊分屬不同 package，中間沒有東西盯著的話，
	// 日後多一個組裝點忘了那條推導就會安靜地重現這條鏈路最該避免的失敗形態：LLM 被
	// 叫去呼叫一個工具清單裡不存在的 Tool，然後拿描述硬編出步驟。
	//
	// **在送出任何請求之前**擋下來、fail 這個 turn。偵測到之後還把那份自相矛盾的
	// prompt 送出去，等於明知會壞還讓它壞——那正是這道檢查要防的事。這是組裝點的
	// bug（使用者改不動的東西），照本專案對配置不一致的既有判準一律 fail fast：
	// Registry.Subset 對未註冊的 Tool、Bootstrap 對列出卻缺檔的檔案都是這樣。
	if len(skills) > 0 && !slices.ContainsFunc(defs, func(d ToolDefinition) bool { return d.Name == LoadSkillToolName }) {
		l.logger.Warn("skill_section_promises_missing_tool",
			"profile", profile.Name, "tool", LoadSkillToolName, "declared_skills", len(skills))
		return "", fmt.Errorf("Profile %s 宣告了 %d 份 Skill，但 %s 不在這個 Agent 的可用工具裡"+
			"（Skill 段的引言承諾了它）：組裝點必須把它加進 Tool 子集",
			profile.Name, len(skills), LoadSkillToolName)
	}

	// 死循環守衛的計數**只活在這一次 Run 之內**（見 loopGuard）。用區域變數而不是
	// ReActLoop 的欄位，這條性質就由語言本身保證：不必靠誰記得在 turn 結尾清空，
	// 也不會有兩個併發的 turn 共用一份計數（憲法 5.2）。
	maxRepeatedFailures := profile.Settings.effectiveMaxRepeatedToolFailures()
	guard := newLoopGuard(maxRepeatedFailures)

	// 上下文預算在迴圈之前取一次：它是 Profile 的設定，turn 之內不會變，取出來是
	// 為了讓下面的日誌說得出用的是哪個數字（組裝函式自己也讀得到，但它不落日誌）。
	maxContextRunes := profile.Settings.effectiveMaxContextRunes()

	var lastContent string // 最後一輪 LLM 內容，強制終止時作為已知進度附上
	for i := range maxIterations {
		// 播報在呼叫**之前**：這則事件的用途是讓等待中的使用者看到「它還在跑、
		// 現在是第幾輪」，等呼叫回來才播報就晚了整整一次 LLM 往返。
		EmitEvent(ctx, l.events, l.logger, Event{Kind: EventIteration, Iteration: i + 1})
		// 上下文壓縮與 Skill 段截斷同一個形狀（見 ComposeSkillSection）：組裝函式回報
		// 降級量，由這裡落日誌。壓縮發生在**每個 iteration**——它取決於當下的對話
		// 歷史長度，而歷史在 turn 之內每輪都在長，turn 開始時算一次會漏掉後面幾輪。
		msgs, compacted := buildMessages(profile, session, boot, longTerm, skillSection)
		if compacted > 0 {
			// 落警告日誌而不是播事件：這是「使用者看不見卻在花錢」的降級，該讓維運
			// 查得到；CLI 上等著看的是執行進度，不是預算會計（EmitEvent 服務的對象）。
			l.logger.Warn("context_compacted",
				"profile", profile.Name, "compacted", compacted,
				"budget_runes", maxContextRunes, "iteration", i+1)
		}
		started := time.Now()
		resp, err := l.provider.Chat(ctx, ChatRequest{
			Provider:    profile.Provider.Name,
			Model:       profile.Provider.Model,
			Temperature: profile.Provider.Temperature,
			Messages:    msgs,
			Tools:       defs,
		})
		// 每次 LLM 呼叫都落審計，成敗都記——審計記的是已發生的事實，失敗那次
		// 尤其要留下來。這一行在錯誤處理之前，turn 失敗 rollback 也不會抹掉它。
		l.recordLLMCall(ctx, profile, session, started, resp, err)
		if err != nil {
			return "", fmt.Errorf("呼叫 LLM: %w", err)
		}
		session.Append(Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		// 只有 tool_calls、content 為空的那一輪不播報：這則事件說的是「LLM 產出了
		// 文字」，空字串不是產出，播出去只會在 CLI 上多一行空白。
		if resp.Content != "" {
			EmitEvent(ctx, l.events, l.logger, Event{Kind: EventAssistantText, Text: resp.Content})
		}
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}
		lastContent = resp.Content

		for _, call := range resp.ToolCalls {
			result, retries, err := l.executeWithRetry(ctx, profile, session, call)
			if err != nil {
				return "", err
			}
			// 守衛在**回填組裝之前**觀測：這一次要不要附提示，取決於含這一次在內的
			// 連續失敗次數；等內容組好再回頭改，就得把已經拼完的字串拆開。
			normalized, repeated := guard.observe(call, result)
			if repeated > 0 {
				// 落警告日誌而不是播事件：這是給維運與日後評測指標看的降級訊號
				// （ticket #54 明言它要能成為評測指標之一），不是使用者在 CLI 上等著
				// 看的執行進度——後者才是 EmitEvent 服務的對象。
				//
				// args 記的是**規範化後的參數本身、不做雜湊**，除錯時才看得出是哪一組
				// 參數在循環；但它仍然過 RedactArgs——所有落盤路徑共用同一套去敏規則
				// （見 redact.go），日誌不因為「這只是除錯資訊」就例外。
				l.logger.Warn("tool_loop_guard_tripped",
					"session_id", session.ID, "profile", profile.Name,
					"tool", call.Name, "args", RedactArgs(normalized),
					"repeated", repeated, "threshold", maxRepeatedFailures)
			}
			// 回填內容一律經**單一組裝點**（見 toolMessageContent）：原始錯誤在前、
			// 該類型給 LLM 的指引居中、死循環提示在後。審計不走這裡，它記的是
			// result.Error 原文（見 execute），兩邊刻意分岔。
			session.Append(Message{
				Role:       RoleTool,
				Content:    toolMessageContent(result, retries, repeated),
				ToolCallID: call.ID,
			})
		}
	}

	// 強制終止不是錯誤：固定提示語＋已知進度回覆使用者並留在歷史，
	// 已執行的 Tool 結果不因終止而被 rollback 丟棄（ticket #6 已定案）。
	reply := fmt.Sprintf("已達最大迭代次數 %d，仍未完成任務，已強制終止。", maxIterations)
	if lastContent != "" {
		reply += "最後一輪進度：" + lastContent
	}
	session.Append(Message{Role: RoleAssistant, Content: reply})
	return reply, nil
}

// executeWithRetry 執行一次 Tool 呼叫；結果標示可重試的失敗時按指數退避重試
// （最多 maxToolRetries 次），回傳最終結果與實際重試次數。退避等待走 ctx：
// 取消／逾時立即中斷並回傳錯誤（憲法 5.3）——此時本輪已嘗試執行過該 Tool，
// 錯誤註記外部效果不會因回退而撤銷。
func (l *ReActLoop) executeWithRetry(ctx context.Context, profile *Profile, session *Session, call ToolCall) (ToolResult, int, error) {
	result := l.execute(ctx, profile, session, call)
	retries := 0
	delay := toolRetryBaseDelay
	for !result.OK && result.Retryable && retries < maxToolRetries {
		select {
		case <-ctx.Done():
			return ToolResult{}, retries, fmt.Errorf("Tool %s 失敗後的重試等待被取消（本輪已嘗試執行過該 Tool，其外部效果不會因回退而撤銷）: %w", call.Name, ctx.Err())
		case <-time.After(delay):
		}
		retries++
		delay *= 2
		// 播報在退避等待**之後**、實際執行之前。放在等待之前能讓使用者早幾百毫秒
		// 看到訊息，但 ctx 在等待中被取消時就會播出一次沒有發生過的重試——事件記的
		// 是已發生的事實，寧可晚一點也不要記錯。
		EmitEvent(ctx, l.events, l.logger, Event{Kind: EventToolRetrying, ToolName: call.Name, Iteration: retries})
		result = l.execute(ctx, profile, session, call)
	}
	return result, retries, nil
}

// execute 執行一次 Tool 呼叫並落審計。每次實際執行都記一筆（重試因此有多筆），
// 與既有 tool_invocation 結構化日誌的語義一致——它們記的是同一件事。
func (l *ReActLoop) execute(ctx context.Context, profile *Profile, session *Session, call ToolCall) ToolResult {
	started := time.Now()
	result := l.tools.Execute(ctx, call)
	// 參數與錯誤訊息落庫前先去敏，與結構化日誌共用同一套規則（core.RedactArgs）。
	// db 檔是使用者會直接打開、隨 Workspace 備份搬遷的東西，比日誌更持久；
	// 審計要的是「呼叫了誰、結果如何」，密鑰不在其中。
	//
	// 這裡與 sessions.messages_json 存原始 arguments 不衝突：那份原文是**必要的**
	// ——恢復的對話要能原樣重放給 LLM，改過的參數會讓歷史失真。審計記錄不重放，
	// 沒有這個必要性，就不該多留一份明文。
	inv := ToolInvocation{
		SessionID:   session.ID,
		ProfileName: profile.Name,
		ToolName:    call.Name,
		Parameters:  RedactArgs(call.Arguments),
		Status:      auditStatus(ctx, !result.OK),
		StartedAt:   started,
		CompletedAt: time.Now(),
	}
	if result.OK {
		inv.Result = result.Content
	} else {
		inv.Error = RedactErrorText(result.Error)
	}
	l.audit.RecordToolInvocation(ctx, inv)
	return result
}

// recordLLMCall 落一筆 LLM 呼叫的審計記錄；err 非 nil 時記失敗（ctx 逾時記 timeout）。
//
// 成本在這裡算而不是另外包一層裝飾器：這裡本來就是**所有 Provider 呼叫的必經之處**，
// 而且 token 用量與成本是同一筆事實的兩種寫法，分開記會多出一個「兩邊對不上」的
// 可能。也因此 ProviderService 維持單一實作（憲法 2.2、2.3）。
//
// **失敗的呼叫一樣計價**：那些 token 已經被 Provider 計費了，錯誤路徑的 resp 也帶回
// 了用量（見 provider.Service.Chat）。漏算會讓失敗重試的成本在報表上憑空消失。
func (l *ReActLoop) recordLLMCall(ctx context.Context, profile *Profile, session *Session, started time.Time, resp ChatResponse, err error) {
	// **沒有用量資訊就不計價。** Provider 在連線失敗、逾時、上游 5xx 這類錯誤下回傳
	// 的是零值回應，沒有 usage：那次請求可能根本沒送達（因此沒計費），也可能送達了
	// 而我們不知道用量——兩種都是「未知」，不是「零」。對它計價會算出 0，在報表上
	// 留下一個具體但錯誤的數字，與本票「沒算不能寫成不用錢」的判準直接抵觸。
	//
	// 分界不在成敗：回應不含 choice 那條失敗路徑**帶回了** usage（見
	// provider.Service.Chat），那些 token 已經被計費，一樣要算。
	var cost *int64
	if resp.Usage != (TokenUsage{}) {
		var why CostUnavailable
		cost, why = l.prices.CostMicroUSD(profile.Provider.Name, profile.Provider.Model, resp.Usage)
		// 落警告而不是靜默：成本欄位是空的，管理員得知道為什麼。這是「使用者看不見
		// 卻在花錢」的降級，與 context_compacted 同一類，所以走同一條路（結構化日誌，
		// 不是事件流——CLI 上等著看的是執行進度）。
		//
		// **原因決定寫哪一則**：三種原因對管理員的處置完全不同，全部記成「缺定價」
		// 會把後兩種情況的人送去改一份本來就正確的設定檔。
		switch why {
		case "":
			// 算出來了，沒有降級可報。
		case CostUnavailableNoPricing:
			l.logger.Warn("llm_cost_not_priced",
				"profile", profile.Name, "provider", profile.Provider.Name,
				"model", profile.Provider.Model)
		default:
			// 定價在、用量也在，但算不出一個能落庫的數字。帶上原因，管理員才知道
			// 該去看 Provider 回報的用量，還是去檢查單價是不是多打了幾個零。
			l.logger.Warn("llm_cost_uncomputable",
				"profile", profile.Name, "provider", profile.Provider.Name,
				"model", profile.Provider.Model, "reason", string(why))
		}
	}
	// **只在有用量時判斷原因**：完全沒有 usage 時（連線失敗、逾時）連「算不算得出來」
	// 都問不了，那不是定價或用量的問題，Provider 的失敗本身已經有自己的錯誤日誌。
	l.audit.RecordLLMCall(ctx, LLMCall{
		SessionID:    session.ID,
		Provider:     profile.Provider.Name,
		Model:        profile.Provider.Model,
		Usage:        resp.Usage,
		Latency:      time.Since(started),
		Status:       auditStatus(ctx, err != nil),
		StartedAt:    started,
		CompletedAt:  time.Now(),
		CostMicroUSD: cost,
	})
}

// buildMessages 組裝一次 LLM 呼叫的訊息序列：system prompt 依 ADR-0003 的順序
// 拼出（見 composeSystemPrompt），加上按 max_history_turns 截斷後的近期對話歷史，
// 最後套上下文壓縮（見 compactToolResults），回傳序列與被壓的條數。
//
// **兩層截斷的順序是定死的**：先 turn 級（哪幾輪還算數），再內容級（還算數的那幾輪
// 裡每條留多長）。反過來的話會先花力氣壓一批馬上要被整輪丟掉的訊息，而且內容級的
// 預算會被那些訊息吃掉，留給真正要送出去的那幾輪反而更少。
//
// boot 與 longTerm 都是呼叫端在 turn 開始時取好的快照，當參數傳入——本函式不碰檔案，
// 維持無 I/O、好測（技術方案 §4.2）。對話歷史則每次都從當前 session 重新取，
// 含本 turn 內剛追加的 assistant 與 tool 訊息。
func buildMessages(profile *Profile, session *Session, boot BootstrapContext, longTerm, skillSection string) ([]Message, int) {
	history := truncateHistory(session.Messages, profile.Settings.effectiveMaxHistoryTurns())
	msgs := make([]Message, 0, len(history)+1)
	if system := composeSystemPrompt(profile.Identity.Prompt, boot, longTerm, skillSection); system != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: system})
	}
	msgs = append(msgs, history...)
	return compactToolResults(msgs, profile.Settings.effectiveMaxContextRunes())
}

// truncateHistory 保留近期 maxTurns 輪對話（一輪自一條 user 訊息起算，當前輪
// 計入 N），超出丟棄、不做總結壓縮。截斷點永遠落在 user 訊息邊界：不會把
// assistant 的 tool_calls 與對應 tool 結果攔腰切開（OpenAI 兼容協議要求成對
// 出現）。只影響進 prompt 的訊息，Session 歷史本身不動（完整可查）。
func truncateHistory(msgs []Message, maxTurns int) []Message {
	seen := 0
	for i, m := range slices.Backward(msgs) {
		if m.Role == RoleUser {
			seen++
			if seen == maxTurns {
				return msgs[i:]
			}
		}
	}
	return msgs
}
