package eval

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// Requires 是用例對 Workspace 環境的前置條件宣告（issue #59）。
//
// 用例宣告得出「要呼叫 read_file」，卻宣告不出「這個 Workspace 允不允許 read_file」
// ——後者散在 Profile 的 tools 與 config.yaml 的兩份白名單裡。配置漂掉時，失敗的形態
// 完全不指向原因：
//
//	[2/7] read_file 讀出檔案內容並回答 … 未通過
//	        - tool_called：未呼叫 "read_file"（實際：一個 Tool 都沒呼叫）
//
// 這段輸出沒有一個字提到白名單，看的人合理的第一個猜測是「模型退步了」。而且它是
// **花完錢之後**才看得到的失敗。
//
// **選填**（issue #59 定案）。沒宣告的用例照原本的方式跑，既有用例零遷移；代價是它
// 得不到保護，那是選填本來就換來的。必填的話 01-reply-only 這種不碰 Tool 的用例得寫
// 一個空段，schema 變重換到的保護有限。
type Requires struct {
	// Tools 是 Profile 的 tools 必須涵蓋的 Tool 名。
	Tools []string `yaml:"tools"`
	// Paths 是 config.yaml 的 file.allowed_paths 必須涵蓋的路徑。
	Paths []string `yaml:"paths"`
	// Commands 是 config.yaml 的 shell.allowed_commands 必須涵蓋的程式名。
	Commands []string `yaml:"commands"`
}

// requiresFields 是 requires 段支援的全部欄位，**與 Requires 的 yaml tag 一一對應**。
//
// 顯式列出而不用反射取（憲法 2.3）。加欄位時這裡要一起加——漏了的話新欄位會在解析時
// 被自己的白名單擋下，那是立刻看得見的失敗，比反向的靜默忽略安全。
var requiresFields = []string{"tools", "paths", "commands"}

// UnmarshalYAML 讓 requires 的欄位成為一份**封閉的白名單**：不認得的欄位一律報錯。
//
// **沒有它的話，拼錯的欄位會被靜默忽略**（Codex 審查抓到）：`requires.path` 少一個 s，
// yaml.Unmarshal 收不下、三段都空、IsZero() 為真、整個校驗被跳過——而寫的人以為有保護。
// 那是這個功能最糟的失敗形態，它退回 issue #59 開票時的場景（花完錢才看到一個不指向
// 原因的失敗），只是這次連校驗都沒發生。
//
// 與 AssertionKind 同一條規則：「一個被安靜忽略的斷言，會讓評測宣稱一個它根本沒檢查
// 的性質成立——那比沒有評測更糟，因為報表上是綠的。」
func (r *Requires) UnmarshalYAML(node *yaml.Node) error {
	// `requires:` 寫了但沒有內容時是 null 節點，與整段省略是同一件事，放行。
	//
	// **這一段目前經 ParseCase 走不到**：實測 yaml.v3 對空的 `requires:` 根本不呼叫
	// UnmarshalYAML，直接給零值。保留它是因為那是**第三方庫的行為**，不是本程式的
	// 結構——換版本或換解析路徑時，沒有這一段會讓空的 requires: 被判成「必須是一組
	// 欄位」，而那是錯的。TestRequiresUnmarshalNullNode 直接呼叫這個方法守住它。
	if node.Tag == "!!null" {
		// **清空 receiver**（Codex 審查抓到）：null 的語義是「沒有前置條件」，只
		// return nil 會讓舊值殘留。解碼進一個重用的結構體時，殘留的條件會讓校驗檢查
		// 一份根本不屬於這份用例的宣告——而那份宣告可能剛好滿足，於是靜默放行。
		*r = Requires{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("requires 必須是一組欄位（支援 %s）", strings.Join(requiresFields, "、"))
	}
	// Content 在 mapping node 上是 key、value 交替排列。
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !slices.Contains(requiresFields, key) {
			return fmt.Errorf("requires 不認得的欄位 %q（支援 %s）；拼錯的欄位會讓整段前置條件被靜默略過，所以一律擋下",
				key, strings.Join(requiresFields, "、"))
		}
	}
	// plain 是一個沒有 UnmarshalYAML 方法的同構型別，用來避免遞迴呼叫自己。
	type plain Requires
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*r = Requires(p)
	return nil
}

// IsZero 回答這份宣告是不是完全沒有內容。
//
// 選填的語義需要它：`requires:` 寫了但三段都空，與整段沒寫是同一件事。
func (r Requires) IsZero() bool {
	return len(r.Tools) == 0 && len(r.Paths) == 0 && len(r.Commands) == 0
}

// validate 校驗這份宣告**本身**寫得合不合法——與 Workspace 的實際配置無關。
//
// 它擋的是「用例寫壞了」：空白的 Tool 名、絕對路徑、穿越出 Workspace 的路徑、帶路徑的
// 程式名。這些條目**不論 Workspace 怎麼配置都不會滿足**，所以在解析層就該擋下，與斷言
// 種類、setup.files 路徑同一條規則（一份寫壞的用例應該在送出任何請求之前被擋下）。
//
// CheckRequires 那一側對同樣的輸入也會給出方向正確的訊息。**兩層不互相取代**：解析層
// 讓正常使用永遠碰不到那種訊息，CheckRequires 那一道則是為了繞過解析直接呼叫的情形
// ——與 checkLimit 對 parseLimit 的處理同一條理由。
func (r Requires) validate() error {
	for i, name := range r.Tools {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("requires.tools[%d] 不得為空白（Tool 名要與 Profile 的 tools 字面相符）", i)
		}
	}
	// 判準與 CheckRequires 同源：連涵蓋整個 Workspace 的白名單都放行不了，問題就在
	// 這個宣告本身。CheckFilePath 對三種不合法輸入的訊息已經說得出原因與修法。
	permissive := tool.NewSandboxChecker(tool.SandboxConfig{AllowedPaths: []string{"."}})
	for i, path := range r.Paths {
		if _, _, err := permissive.CheckFilePath(path); err != nil {
			return fmt.Errorf("requires.paths[%d]: %w", i, err)
		}
	}
	for i, name := range r.Commands {
		if len(tool.EffectiveAllowedCommands([]string{name})) == 0 {
			return fmt.Errorf("requires.commands[%d] 的 %q 不是合法的程式名（不得是空白、也不得含路徑分隔符——寫 wc 而不是 /usr/bin/wc）", i, name)
		}
	}
	return nil
}

// Environment 是校驗前置條件時看得到的 Workspace 環境。
//
// **刻意是純資料，不是 Workspace 路徑。** 校驗的時機在 RunCase 裡，而 run.go 沒有
// 自動化測試（憲法 4.4）。把環境收斂成這四個欄位，判斷邏輯就能全部搬到測得到的這一側，
// 那邊只剩一行沒有分支的呼叫——與 ticket #50 把判卷切成純函式是同一條理由。
type Environment struct {
	// ProfileName 只用於錯誤訊息：要說得出該去改哪一份 Profile。
	ProfileName string
	// ProfileTools 是該 Profile 的 tools。
	ProfileTools []string
	// AllowedPaths 是 config.yaml 的 file.allowed_paths 原值（未收斂）。
	AllowedPaths []string
	// AllowedCommands 是 config.yaml 的 shell.allowed_commands 原值（未收斂）。
	AllowedCommands []string
}

// CheckRequires 校驗 Workspace 環境是否滿足用例宣告的前置條件，滿足時回傳 nil。
//
// **每一項都檢查，不在第一項不滿足時就停**（與 Grade 同一條理由）。這個校驗雖然發生
// 在花錢之前，但修一項、再跑一次、才看到第二項一樣消耗人。
//
// 三類的比對語義刻意不同，且都與**執行時的那一段邏輯同源**——校驗說可以就必須真的
// 可以，否則它會製造一種新的困惑：校驗放行了，Tool 卻在執行時被拒。
//
//   - tools：字面完全相等。Tool 名是註冊表上的固定字串，沒有涵蓋關係。
//   - paths：**子樹涵蓋**，走 SandboxChecker.CheckFilePath。字面比對會在
//     `allowed_paths: ["."]` 這種涵蓋一切的設定上誤報「缺 notes」，把一個完全正常的
//     Workspace 判成配置錯誤。
//   - commands：**先用 EffectiveAllowedCommands 驗證這個名字本身合不合法**（單獨餵進去，
//     收出空的就代表它永遠不會出現在任何 effective 白名單裡），合法之後對原始白名單做
//     字面完全相等的比對。第二步不再收斂一次：收斂只剔除條目、不修改保留下來的，對一個
//     已確認合法的名字，收斂前後結果必然相同。
func CheckRequires(req Requires, env Environment) error {
	if req.IsZero() {
		return nil
	}

	// **宣告本身寫壞了就先擋，不往下比對。**
	//
	// 一個不合法的條目——空白的 Tool 名、絕對路徑、帶路徑的程式名——不論 Workspace
	// 怎麼配置都不會滿足。往下走會產生指向 config.yaml 或 profiles/<name>.yaml 的訊息，
	// 而那個修法永遠不會奏效：照著做、再跑一次、看到同一則訊息（Codex 審查抓到）。
	//
	// **空白 Tool 名更糟**：Profile 若剛好也含著同一個空白值，字面比對會命中、校驗
	// 直接放行——一個根本不是 Tool 名的宣告被當成滿足了。
	//
	// 入口重用解析層的同一份判準，三類一次涵蓋。分散在各迴圈裡的前置檢查漏過 tools
	// 一次（paths 與 commands 有、tools 沒有），一份判準不會再漏第二次。
	if err := req.validate(); err != nil {
		return fmt.Errorf("用例宣告的 requires 本身不合法：%w；這一項不論 Workspace 怎麼配置都不會滿足，請改寫用例 YAML", err)
	}

	var unmet []string

	for _, name := range req.Tools {
		if !slices.Contains(env.ProfileTools, name) {
			unmet = append(unmet, fmt.Sprintf(
				"requires.tools 的 %q 不在 Profile %q 的 tools（請加進 profiles/%s.yaml 的 tools）",
				name, env.ProfileName, env.ProfileName))
		}
	}

	// SandboxChecker 是純字串判斷、不碰檔案系統（見 CheckFilePath 的說明），所以這裡
	// 建一個來比對不會讓 CheckRequires 失去純函式的性質。
	checker := tool.NewSandboxChecker(tool.SandboxConfig{
		AllowedPaths:    env.AllowedPaths,
		AllowedCommands: env.AllowedCommands,
	})
	// 走到這裡每一條 path 都已確認合法，所以這個迴圈只回答一個問題：這個 Workspace
	// 放行了嗎。不滿足就是配置的事，訊息指向 config.yaml 是對的。
	for _, path := range req.Paths {
		if decision, _, _ := checker.CheckFilePath(path); decision != tool.SandboxAllow {
			unmet = append(unmet, fmt.Sprintf(
				"requires.paths 的 %q 不在 file.allowed_paths 的涵蓋範圍（請把它或它的上層目錄加進 Workspace config.yaml 的 file.allowed_paths）",
				path))
		}
	}

	// **比對用原值，不收斂。** 入口的 validate 已經確保 name 本身合法（非空白、無路徑
	// 分隔符），而 EffectiveAllowedCommands 只**剔除**條目、不修改保留下來的——所以一個
	// 合法的 name 在白名單裡就不會被剔除、不在就還是不在，收斂前後結果必然相同。
	//
	// 收斂那個函式用在 validate 那一側判斷「這個名字本身合不合法」，那裡有測試守著；
	// 寫在這裡是等價的，留著只會讓人以為它擋著什麼。
	for _, name := range req.Commands {
		if !slices.Contains(env.AllowedCommands, name) {
			unmet = append(unmet, fmt.Sprintf(
				"requires.commands 的 %q 不在 shell.allowed_commands（請加進 Workspace config.yaml 的 shell.allowed_commands）",
				name))
		}
	}

	if len(unmet) == 0 {
		return nil
	}
	// 縮排配合 cmd/oryxos-eval 的執行錯誤輸出（那一層用 8 個空格起頭），多出來的行
	// 自己對齊，否則第二項之後會貼在行首、看起來像另一則錯誤。
	return fmt.Errorf("Workspace 環境不滿足用例宣告的 requires：\n        - %s",
		strings.Join(unmet, "\n        - "))
}
