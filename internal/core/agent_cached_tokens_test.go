// 命中快取的 token 落庫的整合測試（ticket #56）：從 AgentService.Process 這個既有
// seam 驅動，LLM 以 httptest 回放（ADR-0002），SQLite 用真的。
//
// 與 agent_cost_test.go（ticket #49）的分工：那裡驗「成本算出來的數字有沒有落庫」，
// 這裡驗**那個數字的輸入齊不齊**——成本算得再對，少一個輸入就沒有人能從表上驗證它。
//
// 因此本檔的主斷言不是「某一欄等於某個值」，而是**拿落庫的那幾欄重算一次，要得到
// 落庫的那個成本**。這正是 ticket #56 的問題陳述：管理員拿 llm_calls 手算會得到兩倍
// 以上的數字，合理的反應是懷疑成本欄位算錯了——而它沒錯，是表上少了一個輸入。
package core_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// llmCallUsageRow 是 llm_calls 上與成本覆算有關的那幾欄。
//
// cachedPromptTokens 可空而其餘不可空，對應的是欄位語義本身：三個 token 計數欄位
// 是 NOT NULL，新欄位可空**只為了讓既有資料列留 NULL**（那些呼叫發生時本欄還不
// 存在）。新寫入的每一列都該是具體整數，所以這裡讀成 sql.NullInt64 是為了讓
// 「不小心落成 NULL」被斷言抓到，不是預期它會是 NULL。
type llmCallUsageRow struct {
	promptTokens       int
	cachedPromptTokens sql.NullInt64
	completionTokens   int
	cost               sql.NullInt64
}

// queryLLMCallUsage 查 llm_calls 的用量與成本欄位，按起始時間排序。
//
// 另寫一支而不是擴充 queryLLMCallCosts：那支服務 ticket #49 的成本斷言，兩者各自
// 獨立，這張票改壞了不該連帶弄紅那邊。
func queryLLMCallUsage(t *testing.T, dbPath string) []llmCallUsageRow {
	t.Helper()
	var got []llmCallUsageRow
	eachRow(t, dbPath,
		`SELECT prompt_tokens, cached_prompt_tokens, completion_tokens, cost_micro_usd
		   FROM llm_calls ORDER BY started_at, call_id`,
		func(rows *sql.Rows) {
			var r llmCallUsageRow
			if err := rows.Scan(&r.promptTokens, &r.cachedPromptTokens,
				&r.completionTokens, &r.cost); err != nil {
				t.Fatalf("掃描 llm_calls 用量欄位: %v", err)
			}
			got = append(got, r)
		})
	return got
}

// TestCachedPromptTokensRecordedForBothUsageShapes 是本票主場景：三種 usage 形狀，
// 命中快取的 token 都落庫，且**用落庫的欄位重算得回落庫的成本**。
//
// 三格涵蓋的是 Provider 回應的三種真實形狀，缺一不可：
//   - 沒有 prompt_tokens_details（go-openai 的該欄位是指標，nil 是常見情況）
//   - 有 details 且命中大部分（快取真的生效，也是 ticket #56 量到 42% 落差的形狀）
//   - 失敗但帶回 usage（回應不含 choice；那些 token 已經被計費，見 ticket #49）
//
// 覆算那一步才是這張票的驗收：任一欄取錯或漏寫，重算的結果就對不上。舉例來說，
// 快取那格若把 cached 落成 0，重算會得到 1000×3+50×15=3750 而不是 1590。
func TestCachedPromptTokensRecordedForBothUsageShapes(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		// wantProcessErr 是這一格的 Process 該不該失敗。失敗的呼叫一樣要留下審計，
		// 而且 token 已經被計費——負向路徑必須跟正向一起釘住。
		wantProcessErr bool
		wantPrompt     int
		wantCached     int64
		wantCompletion int
	}{
		{
			name:           "回應沒帶 prompt_tokens_details",
			fixture:        "reply_cost_plain.json",
			wantPrompt:     180,
			wantCached:     0, // 沒帶明細就是沒有命中快取；那正是計價當時吃進去的值
			wantCompletion: 40,
		},
		{
			name:           "回應帶明細且大部分命中快取",
			fixture:        "reply_cost_cached.json",
			wantPrompt:     1000,
			wantCached:     800,
			wantCompletion: 50,
		},
		{
			name:           "呼叫失敗但帶回用量",
			fixture:        "reply_no_choice.json",
			wantProcessErr: true,
			wantPrompt:     88,
			wantCached:     0,
			wantCompletion: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newReplayServer(t, readFixture(t, tt.fixture))

			dbPath := filepath.Join(t.TempDir(), "oryxos.db")
			db := openStore(t, dbPath)
			agent := newCostAgentOn(t, srv.URL, testPrices, discardLogger(), db)
			session := activeSession(t, db.sessions())

			_, err := agent.Process(context.Background(), session, "你好")
			if tt.wantProcessErr && err == nil {
				t.Fatal("Process 應該失敗——這格的前提是那次呼叫真的失敗了")
			}
			if !tt.wantProcessErr && err != nil {
				t.Fatalf("Process: %v", err)
			}

			db.flush(t)
			rows := queryLLMCallUsage(t, dbPath)
			if len(rows) != 1 {
				t.Fatalf("llm_calls 資料列數 = %d, 期望 1", len(rows))
			}
			got := rows[0]

			// 先斷言欄位本身，失敗訊息才說得出是哪個數字錯了。
			if !got.cachedPromptTokens.Valid {
				t.Fatalf("cached_prompt_tokens 是 SQL NULL, 期望 %d——"+
					"NULL 只保留給本欄位存在之前寫下的舊資料列", tt.wantCached)
			}
			if got.cachedPromptTokens.Int64 != tt.wantCached {
				t.Errorf("cached_prompt_tokens = %d, 期望 %d",
					got.cachedPromptTokens.Int64, tt.wantCached)
			}
			if got.promptTokens != tt.wantPrompt {
				t.Errorf("prompt_tokens = %d, 期望 %d", got.promptTokens, tt.wantPrompt)
			}
			if got.completionTokens != tt.wantCompletion {
				t.Errorf("completion_tokens = %d, 期望 %d", got.completionTokens, tt.wantCompletion)
			}

			// 主斷言：**只用表上的欄位**重算，要得到表上的成本。
			//
			// 重算刻意走 production 的 PriceList.CostMicroUSD：這一格要證明的不是
			// 算術正確（那由 pricing_test.go 守），而是**這張表帶著足夠的輸入**讓
			// 那個算術重跑得起來。用同一支函式反而讓「輸入齊不齊」成為唯一變因。
			if !got.cost.Valid {
				t.Fatalf("cost_micro_usd 是空值——本格的定價是齊的，成本該算得出來")
			}
			recomputed, why := testPrices.CostMicroUSD("openai", "gpt-4o-mini", core.TokenUsage{
				PromptTokens:       got.promptTokens,
				CachedPromptTokens: int(got.cachedPromptTokens.Int64),
				CompletionTokens:   got.completionTokens,
			})
			if recomputed == nil {
				t.Fatalf("拿表上的欄位重算不出成本, 原因 %q——稽核者手上只有這些欄位", why)
			}
			if *recomputed != got.cost.Int64 {
				t.Errorf("拿表上的欄位重算得到 %d, 表上的 cost_micro_usd 是 %d——"+
					"成本欄位的輸入沒有全部落庫，稽核者會誤判成本算錯了",
					*recomputed, got.cost.Int64)
			}
		})
	}
}

// TestCachedPromptTokensZeroWhenUsageUnknown 驗**完全沒有用量資訊時本欄仍寫 0**，
// 與 prompt_tokens 那三欄同進退。
//
// 上游回 500 這條路徑沒有任何 usage 可讀，成本因此落 NULL（ticket #49 的
// TestCostNullWhenUsageUnknown 守著那一半）。本格守的是另一半：那一列的三個 token
// 計數欄位都是 0，cached_prompt_tokens 沒有理由獨自變成 NULL——它是同一個用量家族的
// 成員，不是 cost_micro_usd 那種「算出來的值」。
//
// 為什麼要單獨一格：這條路徑的成本是 NULL，覆算那個主斷言在它身上問不出東西，
// 所以進不了上面那張表。
func TestCachedPromptTokensZeroWhenUsageUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newCostAgentOn(t, srv.URL, testPrices, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err == nil {
		t.Fatal("上游回 500，Process 應該失敗——這格的前提是那次呼叫真的失敗了")
	}

	db.flush(t)
	rows := queryLLMCallUsage(t, dbPath)
	if len(rows) == 0 {
		t.Fatal("失敗的呼叫也要留下審計")
	}
	for i, r := range rows {
		if !r.cachedPromptTokens.Valid {
			t.Errorf("llm_calls[%d] cached_prompt_tokens 是 SQL NULL, 期望 0——"+
				"prompt_tokens 那三欄在同一列也是 0，本欄沒有理由獨自變成空值", i)
			continue
		}
		if r.cachedPromptTokens.Int64 != 0 {
			t.Errorf("llm_calls[%d] cached_prompt_tokens = %d, 期望 0",
				i, r.cachedPromptTokens.Int64)
		}
	}
}
