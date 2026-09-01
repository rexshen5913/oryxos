package config

import "github.com/rexshen5913/oryxos/internal/core"

// PriceListOf 把設定檔各 Provider 的定價段攤平成 core 的定價表。
//
// 為什麼需要這一層轉換：設定檔的形狀（yaml 欄位名）是 config 的知識，計價的算術是
// core 的職責，兩邊各自持有一份型別、在這裡對接——形狀與 LoadMcpServers 產出
// core.McpServerSpec 完全一致。
//
// 「省略即不計價」由 core 那端保證：查不到定價就回空值、成本欄位落 NULL，不是寫 0
// （那會讓報表看起來很省）。這裡跳過沒有定價段的 Provider **只是不建一個空 map**，
// 語義上進不進表都一樣——突變測試證實拿掉這個 continue 行為不變。
func PriceListOf(providers map[string]ProviderConfig) core.PriceList {
	list := make(core.PriceList, len(providers))
	for name, pc := range providers {
		if len(pc.Pricing) == 0 {
			continue
		}
		models := make(map[string]core.ModelPricing, len(pc.Pricing))
		for model, price := range pc.Pricing {
			// input 與 output 必填由 validatePricing 在載入時保證。這裡仍然檢查，
			// 因為本函式是 exported 的：手組、未經 Load 的 ProviderConfig 也可能傳進來，
			// 直接解引用會 panic。跳過不完整的項與「省略即不計價」是同一個結果。
			if price.Input == nil || price.Output == nil {
				continue
			}
			models[model] = core.ModelPricing{
				Input:       *price.Input,
				Output:      *price.Output,
				CachedInput: price.CachedInput,
			}
		}
		list[name] = models
	}
	return list
}
