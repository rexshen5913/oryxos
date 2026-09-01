// init 產出的設定檔定價段模板測試（ticket #49）。
//
// 沿用 TestInitMcpServersTemplate 的形狀與理由：既有註解引導使用者，模板本身又要能
// 被自己的載入器讀進來。這條不能省——一份縮排寫錯的模板會讓每個新 Workspace 一啟動
// 就報錯，而那是 init 自己造出來的。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/config"
)

// TestInitConfigTemplateLoadsWithPricingSection 驗加了定價段的模板仍然解析得動，
// 且**預設不帶任何定價**——範例留在註解裡。
//
// 「預設不帶定價」是行為的一部分，不只是排版偏好：預填一個沒查證過的價格，成本報表
// 會錯得無聲無息，那比沒有數字更難發現。空的定價段則讓成本欄位落 NULL，管理員一看
// 就知道還沒配置。
func TestInitConfigTemplateLoadsWithPricingSection(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	path := filepath.Join(dir, ".oryxos", "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("init 產出的設定檔讀不進來: %v", err)
	}
	pc, ok := cfg.Providers["openrouter"]
	if !ok {
		t.Fatalf("模板沒有宣告 openrouter: %+v", cfg.Providers)
	}
	if len(pc.Pricing) != 0 {
		t.Errorf("模板預填了 %d 筆定價，期望 0（範例要留在註解裡）: %v", len(pc.Pricing), pc.Pricing)
	}

	// 模板叫使用者「拿掉每行開頭的 # 就能用」，那句話必須是真的。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取設定檔: %v", err)
	}
	if !strings.Contains(string(data), "#     pricing:") {
		t.Errorf("模板裡沒有註解掉的定價範例，使用者不知道這個能力存在")
	}
}

// TestInitPricingExampleIsValid 把註解裡那段範例的原始 YAML 真的餵回載入器。
//
// 這一格才是「拿掉註解就能用」的證據：註解掉的內容永遠不會被解析，寫錯了也沒人知道
// ——只有照著模板做的人才會踩到。模板裡那段由 pricingExample 逐行加前綴產生，所以
// 驗這份原始 YAML 就等於驗註解裡的內容。
//
// 同時驗**縮排是對的**：pricingExample 帶四個空格的前導縮排，拼在 provider 之下要
// 剛好合法。縮排少一層會解析成 Provider 的兄弟節點、多一層則直接語法錯誤。
func TestInitPricingExampleIsValid(t *testing.T) {
	full := "providers:\n  openrouter:\n    api_key: k\n" + pricingExample
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatalf("寫入範例: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("模板註解裡的範例是壞的 YAML——照著它做的人會啟動失敗: %v", err)
	}
	price, ok := cfg.Providers["openrouter"].Pricing["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatalf("範例沒有解析出定價: %+v", cfg.Providers["openrouter"])
	}
	// 三個欄位名都要對得上。cached_input 特別重要：欄位名錯了會被靜默忽略，
	// 快取 token 於是全額計價，而帳面上完全看不出算錯了。
	// 三個欄位都是指標（「沒寫」與「寫 0」要分得開），所以先確認都有值再比。
	if price.Input == nil || price.Output == nil || price.CachedInput == nil {
		t.Fatalf("範例有欄位沒解析出來: %+v", price)
	}
	if *price.Input != 3 || *price.Output != 15 || *price.CachedInput != 0.3 {
		t.Errorf("範例解析出的定價 = {%v %v %v}, 期望 {3 15 0.3}",
			*price.Input, *price.Output, *price.CachedInput)
	}
}
