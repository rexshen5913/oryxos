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
	// logger 落引擎層的結構化日誌。目前只有 Skill 段截斷這一種——它是「持續
	// 存在的降級」，每個 turn 都成立，得在每個 turn 記得到（見 Run）。
	logger *slog.Logger
}

// NewReActLoop 以 provider、Tool 子集、Memory 門面、審計儲存、上下文載入器與
// logger 建立 ReAct 循環；六者都不得為 nil。
func NewReActLoop(provider ProviderService, tools ToolExecutor, memory MemoryService, audit AuditStore, bootstrap ContextLoader, logger *slog.Logger) *ReActLoop {
	return &ReActLoop{provider: provider, tools: tools, memory: memory, audit: audit, bootstrap: bootstrap, logger: logger}
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

	var lastContent string // 最後一輪 LLM 內容，強制終止時作為已知進度附上
	for range maxIterations {
		started := time.Now()
		resp, err := l.provider.Chat(ctx, ChatRequest{
			Provider:    profile.Provider.Name,
			Model:       profile.Provider.Model,
			Temperature: profile.Provider.Temperature,
			Messages:    buildMessages(profile, session, boot, longTerm, skillSection),
			Tools:       defs,
		})
		// 每次 LLM 呼叫都落審計，成敗都記——審計記的是已發生的事實，失敗那次
		// 尤其要留下來。這一行在錯誤處理之前，turn 失敗 rollback 也不會抹掉它。
		l.recordLLMCall(ctx, profile, session, started, resp, err)
		if err != nil {
			return "", fmt.Errorf("呼叫 LLM: %w", err)
		}
		session.Append(Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}
		lastContent = resp.Content

		for _, call := range resp.ToolCalls {
			result, retries, err := l.executeWithRetry(ctx, profile, session, call)
			if err != nil {
				return "", err
			}
			// 回填內容一律經**單一組裝點**（見 toolMessageContent）：原始錯誤在前、
			// 該類型給 LLM 的指引在後。審計不走這裡，它記的是 result.Error 原文
			// （見 execute），兩邊刻意分岔。
			session.Append(Message{
				Role:       RoleTool,
				Content:    toolMessageContent(result, retries),
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
func (l *ReActLoop) recordLLMCall(ctx context.Context, profile *Profile, session *Session, started time.Time, resp ChatResponse, err error) {
	l.audit.RecordLLMCall(ctx, LLMCall{
		SessionID:   session.ID,
		Provider:    profile.Provider.Name,
		Model:       profile.Provider.Model,
		Usage:       resp.Usage,
		Latency:     time.Since(started),
		Status:      auditStatus(ctx, err != nil),
		StartedAt:   started,
		CompletedAt: time.Now(),
	})
}

// buildMessages 組裝一次 LLM 呼叫的訊息序列：system prompt 依 ADR-0003 的順序
// 拼出（見 composeSystemPrompt），加上按 max_history_turns 截斷後的近期對話歷史。
//
// boot 與 longTerm 都是呼叫端在 turn 開始時取好的快照，當參數傳入——本函式不碰檔案，
// 維持無 I/O、好測（技術方案 §4.2）。對話歷史則每次都從當前 session 重新取，
// 含本 turn 內剛追加的 assistant 與 tool 訊息。
func buildMessages(profile *Profile, session *Session, boot BootstrapContext, longTerm, skillSection string) []Message {
	history := truncateHistory(session.Messages, profile.Settings.effectiveMaxHistoryTurns())
	msgs := make([]Message, 0, len(history)+1)
	if system := composeSystemPrompt(profile.Identity.Prompt, boot, longTerm, skillSection); system != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: system})
	}
	return append(msgs, history...)
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
