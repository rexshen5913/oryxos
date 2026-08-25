// 系統提示詞前綴確定性的整合測試（ticket #45）：沿用既有兩個 seam——一律從
// AgentService.Process 驅動，LLM 以 httptest 回放（ADR-0002）——Bootstrap 三檔、
// MEMORY.md 與 SKILL.md 在 seam 之下用真實檔案（t.TempDir()，憲法 4.3）。**不開新
// seam**：斷言對象就是既有測試一直在看的那個外部產物，送往 LLM 邊界請求的 system
// 訊息。
//
// 釘的是 composeSystemPrompt 註解裡那兩條不變式中的**第 2 條（整條鏈路）**：磁碟與
// 設定上的來源沒變時，一個 turn 走完 Bootstrap 載入、長期記憶讀取、Skill 載入與各段
// 組裝之後，送往 Provider 的 system 訊息必須**位元組級相同**。
//
// **第 1 條（composeSystemPrompt 自己是純函式）不需要本檔，也守不住本檔要守的事。**
// 上游若把 Skill 段改成 `map` 迭代，那個函式收到的參數本身就變了——它忠實地產出不同
// 輸出，純函式那條完全沒被違反，而前綴已經在變。所以驗證點必須放在鏈路的出口，也就是
// 既有 seam 上的 Provider 請求。
//
// 這條在本票之前很可能已經成立（拼接順序由 ADR-0003 定死、Skill 段的快照機制保證
// turn 內固定），但沒有測試釘住——日後有人往裡面加時間戳、`map` 迭代序或其他不確定
// 內容，有前綴快取的 Provider 上會**靜默**失效（費用上升，沒有任何測試轉紅）。本檔
// 就是那盞紅燈。
//
// **前提要完整：固定的是「有效輸入」，不是磁碟上的一切。** 要固定的有兩類——
// **Profile 的相關設定**（identity.prompt、bootstrap 欄位、skills 欄位，後兩者決定讀
// 哪幾份、依什麼順序），以及**有效輸入本身**（被選取的 Bootstrap 的載入結果、長期記憶
// 的輸出、被引用的 Skill 的 name 與 description 及其順序）。本檔的做法是每支測試只建
// 一次 Profile、整支共用，檔案則只在反向格刻意改。
//
// **有效輸入不等於「磁碟上的所有檔案」，本檔自己就是那個示範**：
// seedDeterminismWorkspace **無條件**寫兩份 SKILL.md，而「無 Skill 段」那一格一份都
// 不引用——檔案在，卻不是有效輸入，prompt 裡正確地沒有 Skill 段。同理，SKILL.md 的
// 正文永遠不進 prompt（漸進揭露第一層只取 name 與 description）。完整的排除清單與
// 截斷造成的邊界見 composeSystemPrompt 的註解。
//
// 所以本檔**每一處位元組穩定性斷言**都建立在這兩類全部固定之上，那是斷言成立的前提、
// 不是佈置時的隨手選擇。
//
// **契約是單向的：有效來源相同 ⇒ prompt 相同。反過來不成立**——組裝路上有
// strings.TrimSpace、換行折疊與 Skill 段截斷，來源變了而 prompt 正確地不變是可能的
// （細節見 composeSystemPrompt 的註解）。所以下面那支反向格證明的是**三個具體變更**
// 確實傳到了 prompt，不是一條普遍的單射律；它的作用是排除「函式退化成常數」，不是
// 宣稱任何來源變動都必然改變前綴。
//
// **反向格是唯一的例外，而且只是部分例外**：它刻意改一份**被選取的** Bootstrap 檔案
// 的內容（那是有效輸入，所以 prompt 必須跟著變），其餘一切照樣固定，斷言的方向也反
// 過來。變動範圍受控才能斷定改變確實來自那一份檔案，而不是別處漏了什麼在飄。
//
// 「必須跟著變」這一半在本檔只驗 Bootstrap（就是那個反向格）；長期記憶與 Skill 分別
// 由既有的 TestLongTermMemoryRereadEachTurn、TestSkillRereadEveryTurn 守著，不重複驗。
//
// 「位元組相等」在本檔一律以 Go 的字串比較表達：`==` 對 string 比的就是位元組序列，
// 轉成 []byte 再比只是多一步、不會更嚴格。
package core_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// 本檔專用的內容常數。與 agent_profile_bootstrap_test.go 的 bootAgents／bootUser
// 分開是刻意的：反向格要「改掉某一份的內容」，共用常數會讓改動的意圖看不出來。
const (
	detLongTerm = "使用者的後端統一用 Go"
	detDescA    = "把昨天的 PR 整理成摘要。需要做每日 PR 摘要時使用。"
	detDescB    = "發訊息到 Slack 頻道。需要通知團隊時使用。"
)

// seedDeterminismWorkspace 佈置一個五層都有東西的 Workspace：AGENTS.md、USER.md、
// MEMORY.md 與兩份 SKILL.md（人格層由 Profile 的 identity.prompt 供給）。
//
// Skill 檔案**無條件**寫下去、由 Profile 決定引不引用：這樣「無 Skill 段」那一格
// 的差別只來自 Profile 一個變數，不是「檔案不在」。
func seedDeterminismWorkspace(t *testing.T, dir string) {
	t.Helper()
	seedBootstrap(t, dir, "AGENTS.md", bootAgents)
	seedBootstrap(t, dir, "USER.md", bootUser)
	seedMemory(t, filepath.Join(dir, memoryRelPath), "## 2026-08-01\n\n- "+detLongTerm+"\n")
	seedSkill(t, dir, "daily-pr-digest", detDescA, "正文 A")
	seedSkill(t, dir, "slack-post", detDescB, "正文 B")
}

// TestSystemPromptPrefixByteStableWhenInputsUnchanged 是本票主場景：**有效輸入全部
// 不變**時，兩次送往 Provider 的 system 訊息位元組完全相同。佈置上用的是比「有效輸入
// 不變」更強、也更好讀的條件——同一份 Profile 物件從頭用到尾，Bootstrap 三檔、
// MEMORY.md 與兩份 SKILL.md 在兩個 turn 之間一個位元組都沒動。
//
// 名字帶上 WhenInputsUnchanged 是刻意的：叫「AcrossTurns」會讀成一句無條件的主張，
// 而 turn 之間輸入變了 prompt 本來就該變（見檔頭）。前提寫進名字，下一個讀者才不會
// 拿這支測試去論證一件它沒有測的事。
//
// 兩格涵蓋「有 Skill 段」與「無 Skill 段」——Skill 段是目前最可能引入不確定排序的
// 地方（它是唯一一層由 N 個元素拼出來的），但沒有它的 Profile 也得成立，否則不確定
// 性只是被一個沒人引用的欄位遮住了。
func TestSystemPromptPrefixByteStableWhenInputsUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		skills []string
	}{
		{name: "無 Skill 段", skills: nil},
		{name: "有 Skill 段", skills: []string{"daily-pr-digest", "slack-post"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs,
				readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedDeterminismWorkspace(t, dir)

			agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills(tt.skills...))
			session := core.NewSession("cli", "local", "default")

			first := promptAfterTurn(t, agent, session, &reqs, "早安")
			second := promptAfterTurn(t, agent, session, &reqs, "午安")

			// 空字串對自己也相等——先確認真的組出了東西，這一格才有意義。
			// （更深一層的「不是在測一個空函式」由反向格承擔，見下一支測試。）
			if first == "" {
				t.Fatal("system prompt 為空——本 Workspace 至少有 Bootstrap 與長期記憶兩層")
			}
			// 這一格若轉紅，先去看 composeSystemPrompt 這條鏈路上是不是混進了時間戳
			// 或 map 迭代序：有前綴快取的 Provider 就是靠這條不變式命中的。
			if first != second {
				t.Errorf("兩個 turn 的 system prompt 不同——有前綴快取的 Provider 上會靜默失效\n第一次:\n%s\n第二次:\n%s",
					first, second)
			}
			// 兩格確實是不同形狀的 Profile，不是同一件事跑兩遍。
			if got, want := strings.Contains(first, "daily-pr-digest"), len(tt.skills) > 0; got != want {
				t.Errorf("Skill 段存在與否 = %v，Profile 引用了 %d 份 Skill：%s", got, len(tt.skills), first)
			}
		})
	}
}

// TestSystemPromptPrefixChangesWhenBootstrapChanges 是反向格：這三個具體的 Bootstrap
// 內容變更，前綴**必須**跟著變。
//
// **「這三個具體變更」是刻意的措辭。** 本測試不主張、也證明不了「任何來源變動都會改變
// 前綴」——組裝路上的 TrimSpace、換行折疊與 Skill 段截斷讓那句話本來就不成立（見
// composeSystemPrompt 的註解）。這三格挑的都是能穿過正規化的整段替換。
//
// 它的作用是排除**退化**：沒有這一格，上一支測試可能只是在測一個永遠回傳同一段（甚至
// 空）字串的函式——那種函式也「位元組級穩定」，卻早就把 Bootstrap 弄丟了。三格分別動
// 三份檔案，因為三層各有自己的取用路徑（SOUL.md 那條還多一道與 identity.prompt 的
// 互斥）。
func TestSystemPromptPrefixChangesWhenBootstrapChanges(t *testing.T) {
	tests := []struct {
		name string
		// identityPrompt 為空時 SOUL.md 才生效（ADR-0003 的互斥），所以動 SOUL.md
		// 的那一格必須把人格層讓出來。
		identityPrompt string
		file           string
		before, after  string
	}{
		{
			name:           "AGENTS.md 改變",
			identityPrompt: bootIdentity,
			file:           "AGENTS.md",
			before:         bootAgents,
			after:          "本專案的慣例是先寫 ADR 再動手",
		},
		{
			name:           "USER.md 改變",
			identityPrompt: bootIdentity,
			file:           "USER.md",
			before:         bootUser,
			after:          "使用者偏好英文回覆",
		},
		{
			name:           "SOUL.md 改變（identity.prompt 缺席時才生效）",
			identityPrompt: "",
			file:           "SOUL.md",
			before:         bootSoul,
			after:          "你是 Nova，說話簡潔克制",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqs [][]byte
			srv := newRecordingReplayServer(t, &reqs,
				readFixture(t, "reply_direct.json"), readFixture(t, "reply_direct.json"))
			root, dir := bootstrapWorkspace(t)
			seedBootstrap(t, dir, "AGENTS.md", bootAgents)
			seedBootstrap(t, dir, "USER.md", bootUser)
			seedBootstrap(t, dir, "SOUL.md", bootSoul)

			agent := newBootstrapAgent(t, srv.URL, root, tt.identityPrompt)
			session := core.NewSession("cli", "local", "default")

			first := promptAfterTurn(t, agent, session, &reqs, "早安")
			// Bootstrap 每個 turn 重讀（技術方案 §5.3），所以改檔在下一個 turn 生效。
			seedBootstrap(t, dir, tt.file, tt.after)
			second := promptAfterTurn(t, agent, session, &reqs, "午安")

			if first == second {
				t.Fatalf("%s 的內容換掉了，system prompt 卻沒變——那段根本沒進 prompt:\n%s",
					tt.file, first)
			}
			if !strings.Contains(second, tt.after) {
				t.Errorf("第二個 turn 的 system prompt 沒有新內容 %q:\n%s", tt.after, second)
			}
			if strings.Contains(second, tt.before) {
				t.Errorf("第二個 turn 的 system prompt 仍帶著舊內容 %q——Bootstrap 被緩存了:\n%s",
					tt.before, second)
			}
		})
	}
}

// TestSkillSectionOrderStableAcrossTurns 釘住 Skill 段這一層的排序穩定：多份 Skill、
// 連續跑多個 turn，每次的 system prompt 位元組相同，且順序等於 Profile 的宣告順序。
// 前提與第一支相同（不是與反向格相同——那支刻意改 Bootstrap）：這幾個 turn 之間有效
// 輸入都沒動過，Profile 的 skills 欄位與被引用那幾份的 name／description 尤其。Skill
// 快照每個 turn 重取，欄位或那些欄位的內容改了 prompt 本來就該變（見檔頭）。
//
// **宣告順序刻意不是字典序。** 只驗「每次都一樣」抓不到「被排序了」——`sort` 之後每次
// 也都一樣。既有的 TestComposeSkillSectionKeepsDeclarationOrder 驗的是純函式那一層，
// 這裡驗的是整條鏈路（Profile → ContextLoader.Skills → 組裝 → Provider 請求）重複
// 執行的結果，兩者不重複。
//
// **六份而不是兩份，是為了提高偵測力，不是為了一個保證。** Go 規格對 map 的迭代順序
// 只說「未指定、且不保證兩次迭代之間相同」——它既沒有承諾每次都不同，也沒有承諾隨機
// 或均勻，所以這裡算不出任何「漏抓機率」，寫成數字只會是一個沒有依據的權威假象。能
// 誠實說的是：可區分的排列變多，一個以 map 為基礎的實作要連續數個 turn 都湊出同一個
// 順序就更不容易被放過。突變驗證的實測支持這個選擇（見 implement.md）：同一個 map
// 突變下，只用兩份 Skill 的第一支測試該次沒有轉紅，六份的這一支轉紅了。
func TestSkillSectionOrderStableAcrossTurns(t *testing.T) {
	// 宣告順序（非字典序）——被測的就是這個順序有沒有被原樣搬進 prompt。
	declared := []string{"foxtrot-skill", "alpha-skill", "delta-skill", "charlie-skill", "echo-skill", "bravo-skill"}
	const turns = 5

	var reqs [][]byte
	fixtures := make([]string, turns)
	for i := range fixtures {
		fixtures[i] = readFixture(t, "reply_direct.json")
	}
	srv := newRecordingReplayServer(t, &reqs, fixtures...)

	root, dir := bootstrapWorkspace(t)
	for i, name := range declared {
		seedSkill(t, dir, name, "第 "+string(rune('A'+i))+" 份 Skill 的用途說明。", "正文")
	}

	agent := newBootstrapAgentWithProfile(t, srv.URL, root, profileWithSkills(declared...))
	session := core.NewSession("cli", "local", "default")

	prompts := make([]string, turns)
	for i := range prompts {
		prompts[i] = promptAfterTurn(t, agent, session, &reqs, "第幾次都問同一件事")
	}

	for i, got := range prompts[1:] {
		if got != prompts[0] {
			t.Errorf("第 %d 個 turn 的 system prompt 與第 1 個不同——Skill 段排序不穩定\n第 1 次:\n%s\n第 %d 次:\n%s",
				i+2, prompts[0], i+2, got)
		}
	}

	var last int
	for _, name := range declared {
		at := strings.Index(prompts[0], name)
		if at < 0 {
			t.Fatalf("system prompt 遺失 Skill %q:\n%s", name, prompts[0])
		}
		if at < last {
			t.Errorf("%q 的位置早於前一份——Skill 段順序應等於 Profile 的宣告順序:\n%s", name, prompts[0])
		}
		last = at
	}
}
