// Profile 的 bootstrap 欄位的整合測試（ticket #17）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Bootstrap 三檔在
// seam 之下用真實檔案（t.TempDir()，憲法 4.3）。
//
// 本票驗的是「載入**哪些**」，不是「誰蓋過誰」：拼接順序恆由 ADR-0003 決定，
// 欄位的書寫順序不得影響它（見 TestBootstrapFieldOrderDoesNotAffectPromptOrder）。
package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// 三份 Bootstrap 檔案的內容，各自可辨識——斷言看的是「這段有沒有進 prompt」。
const (
	bootAgents   = "本專案的慣例是測試先行"
	bootUser     = "使用者偏好繁體中文回覆"
	bootSoul     = "你是 Nova，說話浮誇愛用比喻"
	bootIdentity = "你是 Oryx，回答力求精確"
)

// profileWithBootstrap 回傳一份指定了 bootstrap 欄位與 identity.prompt 的 Profile。
// bootstrap 傳 nil 即「欄位省略」，傳 []string{} 即「明確的空清單」——這兩者是
// 不同的語義，helper 必須原樣轉交、不得把 nil 正規化成空切片。
func profileWithBootstrap(bootstrap []string, identityPrompt string) *core.Profile {
	prof := testProfile()
	prof.Identity.Prompt = identityPrompt
	prof.Bootstrap = bootstrap
	return prof
}

// TestProfileBootstrapFieldTriState 是本票的主表：三態（省略／列出／空清單）乘上
// 各種列法。三份檔案在每一列都預先寫好，唯一的變數是 bootstrap 欄位本身——這樣
// 「沒進 prompt」只可能是欄位造成的，不是檔案不在。
func TestProfileBootstrapFieldTriState(t *testing.T) {
	tests := []struct {
		name string
		// bootstrap 為 nil 代表欄位省略；[]string{} 代表明確的空清單。
		bootstrap      []string
		identityPrompt string
		wantIn         []string
		wantNotIn      []string
	}{
		{
			name:      "省略：載入預設三檔（既有 Profile 免遷移）",
			bootstrap: nil,
			wantIn:    []string{bootSoul, bootAgents, bootUser},
		},
		{
			name:      "空清單：一份都不載入（與省略是不同結果）",
			bootstrap: []string{},
			wantNotIn: []string{bootSoul, bootAgents, bootUser},
		},
		{
			name:      "只列 AGENTS.md：未列的不進 prompt",
			bootstrap: []string{"AGENTS.md"},
			wantIn:    []string{bootAgents},
			wantNotIn: []string{bootSoul, bootUser},
		},
		{
			name:      "列兩份：覆寫而非疊加，第三份不進",
			bootstrap: []string{"AGENTS.md", "USER.md"},
			wantIn:    []string{bootAgents, bootUser},
			wantNotIn: []string{bootSoul},
		},
		{
			name:      "只列 SOUL.md：人格層由它供給",
			bootstrap: []string{"SOUL.md"},
			wantIn:    []string{bootSoul},
			wantNotIn: []string{bootAgents, bootUser},
		},
		{
			name:      "列出全部三份：等同省略的結果",
			bootstrap: []string{"SOUL.md", "AGENTS.md", "USER.md"},
			wantIn:    []string{bootSoul, bootAgents, bootUser},
		},
		{
			// 欄位決定「載入哪些」，不得推翻 ADR-0003 的互斥：列了 SOUL.md 也
			// 不會讓它跟 identity.prompt 疊成雙重人格。
			name:           "列了 SOUL.md 但 identity.prompt 存在：互斥仍然勝出",
			bootstrap:      []string{"SOUL.md"},
			identityPrompt: bootIdentity,
			wantIn:         []string{bootIdentity},
			wantNotIn:      []string{bootSoul},
		},
		{
			name:           "空清單 ＋ identity.prompt：人格仍在，其餘皆空",
			bootstrap:      []string{},
			identityPrompt: bootIdentity,
			wantIn:         []string{bootIdentity},
			wantNotIn:      []string{bootSoul, bootAgents, bootUser},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedBootstrap(t, dir, "AGENTS.md", bootAgents)
			seedBootstrap(t, dir, "USER.md", bootUser)
			seedBootstrap(t, dir, "SOUL.md", bootSoul)

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap(tt.bootstrap, tt.identityPrompt))
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("system prompt 遺失 %q: %q", want, got)
				}
			}
			for _, notWant := range tt.wantNotIn {
				if strings.Contains(got, notWant) {
					t.Errorf("system prompt 不該含 %q（bootstrap 欄位是覆寫，不是疊加）: %q", notWant, got)
				}
			}
		})
	}
}

// TestBootstrapFieldOrderDoesNotAffectPromptOrder 釘住本票最容易寫錯的一條：
// 欄位決定**載入哪些**，順序恆由 ADR-0003 決定。把欄位倒過來寫，送往 LLM 邊界的
// system prompt 順序不得改變——否則 ADR-0003 就從固定契約降級成預設值，它
// Consequences 要求的可測性也跟著失效。
func TestBootstrapFieldOrderDoesNotAffectPromptOrder(t *testing.T) {
	// ADR-0003 的順序是 SOUL → AGENTS → USER；這裡刻意完全倒過來寫。
	orders := [][]string{
		{"USER.md", "AGENTS.md", "SOUL.md"},
		{"AGENTS.md", "SOUL.md", "USER.md"},
		{"SOUL.md", "AGENTS.md", "USER.md"},
	}

	for _, order := range orders {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedBootstrap(t, dir, "AGENTS.md", bootAgents)
			seedBootstrap(t, dir, "USER.md", bootUser)
			seedBootstrap(t, dir, "SOUL.md", bootSoul)

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap(order, ""))
			got := promptAfterTurn(t, agent, core.NewSession("cli", "local", "default"), &reqs, "早安")

			var last int
			for _, want := range []string{bootSoul, bootAgents, bootUser} {
				at := strings.Index(got, want)
				if at < 0 {
					t.Fatalf("system prompt 遺失 %q: %q", want, got)
				}
				if at < last {
					t.Errorf("順序被欄位書寫順序帶偏：%q 出現在前一層之前（ADR-0003 是固定契約）\n%s", want, got)
				}
				last = at
			}
		})
	}
}

// TestTwoProfilesDifferentBootstrapDifferentPrompt 是本票的使用者故事：不同 Agent
// 帶不同的專案說明與偏好。同一個 Workspace、同一則訊息，兩份 Profile 產生的
// system prompt 必須不同。
func TestTwoProfilesDifferentBootstrapDifferentPrompt(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs,
		readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", bootAgents)
	seedBootstrap(t, dir, "USER.md", bootUser)

	agentOnly := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap([]string{"AGENTS.md"}, bootIdentity))
	userOnly := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap([]string{"USER.md"}, bootIdentity))

	gotAgents := promptAfterTurn(t, agentOnly, core.NewSession("cli", "local", "a"), &reqs, "早安")
	gotUser := promptAfterTurn(t, userOnly, core.NewSession("cli", "local", "b"), &reqs, "早安")

	if gotAgents == gotUser {
		t.Fatalf("兩份引用不同 Bootstrap 檔案的 Profile 產生了相同的 system prompt: %q", gotAgents)
	}
	if !strings.Contains(gotAgents, bootAgents) || strings.Contains(gotAgents, bootUser) {
		t.Errorf("只列 AGENTS.md 的 Profile 拿到的 prompt 不對: %q", gotAgents)
	}
	if !strings.Contains(gotUser, bootUser) || strings.Contains(gotUser, bootAgents) {
		t.Errorf("只列 USER.md 的 Profile 拿到的 prompt 不對: %q", gotUser)
	}
}

// TestExplicitFileDeletedMidSessionFailsNextTurn 釘住「列出即必須存在」是**每個
// turn** 的規則，不是只在啟動時驗一次。
//
// Bootstrap 每個 turn 重讀（#16 定案），刪檔也是一種編輯。啟動時驗過就算的話，
// 「改內容會生效、刪檔卻靜默降級」——Agent 會在使用者不知情下少掉一段他明確要求的
// 上下文，正是 fail fast 要避免的「半殘運作、對話中途才發現」。
//
// 省略欄位的那一列是對照組：預設三檔缺檔仍視為該層為空，#16 的行為不因本票改變。
func TestExplicitFileDeletedMidSessionFailsNextTurn(t *testing.T) {
	tests := []struct {
		name string
		// bootstrap 為 nil 代表欄位省略。
		bootstrap       []string
		wantSecondError bool
	}{
		{name: "明確列出：刪檔後下一個 turn 失敗", bootstrap: []string{"USER.md"}, wantSecondError: true},
		{name: "欄位省略：刪檔後照常（缺檔視為該層為空）", bootstrap: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs,
				readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedBootstrap(t, dir, "USER.md", bootUser)

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap(tt.bootstrap, bootIdentity))
			session := core.NewSession("cli", "local", "default")

			// 第一個 turn：檔案還在，內容進 prompt。
			got := promptAfterTurn(t, agent, session, &reqs, "早安")
			if !strings.Contains(got, bootUser) {
				t.Fatalf("第一個 turn 的 prompt 應含 USER.md 的內容: %q", got)
			}

			if err := os.Remove(filepath.Join(dir, "USER.md")); err != nil {
				t.Fatal(err)
			}

			// 第二個 turn：明確列出的話必須失敗，省略的話照常但該層變空。
			_, err := agent.Process(t.Context(), session, "午安")
			if tt.wantSecondError {
				if err == nil {
					t.Fatal("明確列出的檔案被刪掉後，下一個 turn 應失敗而不是靜默少一層")
				}
				if !strings.Contains(err.Error(), "USER.md") {
					t.Errorf("錯誤訊息 %q 未指名是哪一份", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("省略欄位時缺檔應照常: %v", err)
			}
			if second := systemPrompt(t, reqs[len(reqs)-1]); strings.Contains(second, bootUser) {
				t.Errorf("檔案已刪，第二個 turn 的 prompt 不該還有它（每 turn 重讀）: %q", second)
			}
		})
	}
}

// TestUnknownBootstrapNameFailsTurn 釘住手組的 Profile 也擋得住筆誤。LoadProfile
// 已經在載入時擋過一次，但不是每個 Profile 都經過它（測試手組的、日後
// ProfileRegistry 走的路徑都不是）——少一道就會把筆誤變成一個安靜的空 prompt。
func TestUnknownBootstrapNameFailsTurn(t *testing.T) {
	var reqs [][]byte
	srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
	root, dir := bootstrapWorkspace(t)
	seedBootstrap(t, dir, "AGENTS.md", bootAgents)

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap([]string{"Agents.md"}, bootIdentity))
	_, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安")
	if err == nil {
		t.Fatal("bootstrap 含未知檔名時該 turn 應失敗，不得靜默載入空值")
	}
	if !strings.Contains(err.Error(), "Agents.md") {
		t.Errorf("錯誤訊息 %q 未指名是哪個值", err.Error())
	}
}

// TestUnselectedBrokenFileDoesNotBreakTurn 釘住「未選中的檔案根本不會被讀」：
// 一份壞掉（做成目錄，讀取端會判定不是普通檔）的 AGENTS.md，在 bootstrap 欄位
// 沒有列到它時不得害 turn 失敗。這是 sel 的實際效力——只把「有沒有進 prompt」
// 驗過的話，一個「照讀但丟棄」的實作也會綠。
func TestUnselectedBrokenFileDoesNotBreakTurn(t *testing.T) {
	tests := []struct {
		name      string
		bootstrap []string
		wantErr   bool
	}{
		{name: "未列到壞檔：照常完成", bootstrap: []string{"USER.md"}},
		{name: "空清單：照常完成", bootstrap: []string{}},
		{name: "列到壞檔：該 turn 失敗", bootstrap: []string{"AGENTS.md"}, wantErr: true},
		{name: "省略欄位：預設三檔含壞檔，該 turn 失敗", bootstrap: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs, readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedBootstrap(t, dir, "USER.md", bootUser)
			// 把 AGENTS.md 做成目錄：它存在（存在性校驗會過）但不是普通檔，
			// 讀取端一碰就報錯。
			mkdirBootstrap(t, dir, "AGENTS.md")

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithBootstrap(tt.bootstrap, bootIdentity))
			_, err := agent.Process(t.Context(), core.NewSession("cli", "local", "default"), "早安")
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望該 turn 失敗，實際成功")
				}
				return
			}
			if err != nil {
				t.Fatalf("未選中的壞檔不該影響這個 turn: %v", err)
			}
		})
	}
}
