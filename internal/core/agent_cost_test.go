// 成本歸集的整合測試（ticket #49）：從 AgentService.Process 這個既有 seam 驅動，
// LLM 以 httptest 回放（ADR-0002），SQLite 用真的，斷言直接查 llm_calls 的成本欄位
// ——那是這張票的外部產物。
//
// 與 pricing_test.go 的分工：那裡驗算術（給定用量與單價算出什麼），這裡驗**鏈路**
// （Provider 回應裡的 token 用量有沒有一路走到落庫的那個數字）。兩層都要有，因為
// 算術正確但沒接上、或接上了但取錯欄位，都是綠色的單元測試看不見的失敗。
package core_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
	"github.com/rexshen5913/oryxos/internal/provider"
)

// testPrices 是 testProfile()（openai／gpt-4o-mini）用得到的定價表。
//
// 單價取 3／15／0.3 與 core 的單元測試一致，讓兩層的期望值可以互相對照著讀。
var testPrices = core.PriceList{
	"openai": {"gpt-4o-mini": {Input: 3, Output: 15, CachedInput: costPtr(0.3)}},
}

// costPtr 讓定價寫得出「明確配置這個價格」與「沒寫這個欄位」的差別。
func costPtr(f float64) *float64 { return &f }

// newCostAgentOn 組出帶定價表的 AgentService；logger 由呼叫端給，未知模型那格要
// 從它落下的結構化日誌斷言。
func newCostAgentOn(t *testing.T, baseURL string, prices core.PriceList,
	logger *slog.Logger, st *testStore) *core.AgentService {
	t.Helper()
	svc := provider.NewService(map[string]provider.Config{
		"openai": {APIKey: "test-key", BaseURL: baseURL},
	}, discardLogger())
	return core.NewAgentService(testProfile(), svc, noTools(t), newMemory(t, st.sessions()),
		st.audit, noBootstrap(t), core.NopEventSink{}, prices, logger)
}

// queryLLMCallCosts 查 llm_calls 的成本欄位，按起始時間排序。
//
// 另寫一支而不是擴充既有的 queryLLMCalls：那支服務的是 ticket #12 的欄位對應斷言，
// 兩者各自獨立，這張票改壞了不會連帶弄紅那邊。
func queryLLMCallCosts(t *testing.T, dbPath string) []sql.NullInt64 {
	t.Helper()
	var got []sql.NullInt64
	eachRow(t, dbPath, `SELECT cost_micro_usd FROM llm_calls ORDER BY started_at, call_id`,
		func(rows *sql.Rows) {
			var c sql.NullInt64
			if err := rows.Scan(&c); err != nil {
				t.Fatalf("掃描 cost_micro_usd: %v", err)
			}
			got = append(got, c)
		})
	return got
}

// TestCostRecordedForBothUsageShapes 是本票主場景：兩次呼叫、兩種 usage 形狀，
// 成本都算得出來並落進 llm_calls。
//
// 兩種形狀一起驗是 AC 明訂的：go-openai 的 PromptTokensDetails 是**指標**，回應
// 沒帶這個欄位時為 nil。只錄有 details 的那種，取值前漏了判空也不會有人發現，直到
// 某個 Provider 不回這個欄位時整條對話 panic。
func TestCostRecordedForBothUsageShapes(t *testing.T) {
	srv := newReplayServer(t,
		readFixture(t, "reply_cost_plain.json"),
		readFixture(t, "reply_cost_cached.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newCostAgentOn(t, srv.URL, testPrices, discardLogger(), db)
	session := activeSession(t, db.sessions())

	for i, msg := range []string{"第一個問題", "第二個問題"} {
		if _, err := agent.Process(context.Background(), session, msg); err != nil {
			t.Fatalf("第 %d 次 Process: %v", i+1, err)
		}
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) != 2 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 2", len(costs))
	}

	// 第一次沒有 prompt_tokens_details：180×3 + 40×15 = 540+600。
	// 第二次 1000 個輸入 token 有 800 個命中快取，未快取的只有 200 個：
	// 200×3 + 800×0.3 + 50×15 = 600+240+750。若快取那 800 個被當成未快取計價，
	// 這格會得到 3990——差距足以看出公式有沒有先相減。
	for i, want := range []int64{1140, 1590} {
		if !costs[i].Valid {
			t.Errorf("llm_calls[%d] cost_micro_usd 是空值, 期望 %d", i, want)
			continue
		}
		if costs[i].Int64 != want {
			t.Errorf("llm_calls[%d] cost_micro_usd = %d, 期望 %d", i, costs[i].Int64, want)
		}
	}
}

// TestCostNullWhenModelNotPriced 驗配置了定價但模型不在表內時寫 NULL、不寫 0，
// 並落一筆警告日誌。
//
// 「不寫 0」是這格的重點：0 會讓成本報表看起來很省，而真相是沒算——「沒算」與
// 「不用錢」必須在資料上分得開。日誌則是管理員發現自己漏配了定價的唯一途徑。
func TestCostNullWhenModelNotPriced(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_cost_plain.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	var logBuf bytes.Buffer
	// 定價表認得這個 Provider，但沒有 testProfile() 用的那個模型。
	prices := core.PriceList{"openai": {"some-other-model": {Input: 3, Output: 15}}}
	agent := newCostAgentOn(t, srv.URL, prices, slog.New(slog.NewJSONHandler(&logBuf, nil)), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) != 1 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 1", len(costs))
	}
	if costs[0].Valid {
		t.Errorf("cost_micro_usd = %d, 期望空值——沒有定價就不該編一個數字出來", costs[0].Int64)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "llm_cost_not_priced") {
		t.Errorf("未落成本缺定價的警告日誌: %s", logs)
	}
	if !strings.Contains(logs, "gpt-4o-mini") {
		t.Errorf("警告日誌未帶模型名，管理員無從得知該補哪一筆定價: %s", logs)
	}
}

// TestCostNullWithoutPriceList 驗完全沒有配置定價的既有 Workspace 照常運作，
// 成本落 NULL。「省略即不計價」這條 AC 的執行側。
func TestCostNullWithoutPriceList(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_cost_plain.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newCostAgentOn(t, srv.URL, nil, discardLogger(), db)
	session := activeSession(t, db.sessions())

	resp, err := agent.Process(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("沒有定價表不該讓對話失敗: %v", err)
	}
	if resp == "" {
		t.Error("回應是空的")
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) != 1 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 1", len(costs))
	}
	if costs[0].Valid {
		t.Errorf("cost_micro_usd = %d, 期望空值", costs[0].Int64)
	}
}

// TestCostRecordedOnFailedCall 驗 Provider 呼叫失敗時成本仍然被記錄。
//
// 「回應不含任何 choice」是真實會發生的失敗，而那次請求的 token **已經被計費了**
// ——Provider 端不會因為回應不合用就退錢。漏算會讓失敗與重試的花費在報表上憑空
// 消失，而那正是成本最容易失控的那一段。
func TestCostRecordedOnFailedCall(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_no_choice.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	agent := newCostAgentOn(t, srv.URL, testPrices, discardLogger(), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err == nil {
		t.Fatal("回應沒有 choices，Process 應該失敗——這格的前提是那次呼叫真的失敗了")
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) != 1 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 1——失敗的呼叫一樣要留下審計", len(costs))
	}
	if !costs[0].Valid {
		t.Fatal("失敗呼叫的 cost_micro_usd 是空值——那些 token 已經被計費了")
	}
	// 88 個輸入 token、0 個輸出：88×3 + 0×15。
	if costs[0].Int64 != 264 {
		t.Errorf("cost_micro_usd = %d, 期望 264", costs[0].Int64)
	}
}

// TestCostNullWhenUsageUnknown 驗**沒有用量資訊時落 NULL，不是 0**。
//
// Provider 在連線失敗、逾時、上游 5xx 這類錯誤下回傳的是零值回應，沒有 usage。
// 那次請求可能根本沒送達（因此沒計費），也可能送達了而我們不知道用量——兩種都是
// 「未知」，而不是「零」。對它計價會算出 0，在報表上留下一個具體但錯誤的數字，
// 與整張票「沒算不能寫成不用錢」的判準直接抵觸（外部審查抓到的）。
//
// 與 TestCostRecordedOnFailedCall 構成一對：那格的失敗**帶回了** usage（回應不含
// choice），所以要計價；這格的失敗沒有，所以不能。分界不在成敗，在有沒有用量。
func TestCostNullWhenUsageUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 上游故障：沒有回應主體，因此沒有任何 token 用量可讀。
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	var logBuf bytes.Buffer
	agent := newCostAgentOn(t, srv.URL, testPrices, slog.New(slog.NewJSONHandler(&logBuf, nil)), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err == nil {
		t.Fatal("上游回 500，Process 應該失敗——這格的前提是那次呼叫真的失敗了")
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) == 0 {
		t.Fatal("失敗的呼叫也要留下審計")
	}
	for i, c := range costs {
		if c.Valid {
			t.Errorf("llm_calls[%d] cost_micro_usd = %d, 期望空值——沒有用量就不知道花了多少", i, c.Int64)
		}
	}

	// 定價是齊的，缺的是用量。落「缺定價」的警告會把管理員導向錯誤的方向。
	if strings.Contains(logBuf.String(), "llm_cost_not_priced") {
		t.Errorf("落了缺定價的警告，但這次缺的是用量不是定價: %s", logBuf.String())
	}
}

// TestCostUncomputableLogsItsOwnReason 驗**定價存在但算不出來**時，日誌不會謊報成
// 「缺定價」。
//
// CostMicroUSD 回空值有三種原因，而它們對管理員的處置完全不同：沒定價要去補設定檔、
// 用量矛盾要去看 Provider、總額超範圍要去檢查單價是不是多打了幾個零。全部記成
// llm_cost_not_priced，會把後兩種情況的管理員送去改一份本來就正確的設定檔。
//
// 這與 TestCostNullWhenUsageUnknown 是同一條原則的兩半：那格管「完全沒有用量」，
// 這格管「用量與定價都在、但乘出來的數字表達不了」。第一輪只修了前一半（外部審查
// 第三輪抓到）。
func TestCostUncomputableLogsItsOwnReason(t *testing.T) {
	srv := newReplayServer(t, readFixture(t, "reply_cost_plain.json"))

	dbPath := filepath.Join(t.TempDir(), "oryxos.db")
	db := openStore(t, dbPath)
	var logBuf bytes.Buffer
	// 定價**有**，只是大到讓乘積溢位：180 個 token × 1e308 是 +Inf。
	prices := core.PriceList{"openai": {"gpt-4o-mini": {Input: 1e308, Output: 1e308}}}
	agent := newCostAgentOn(t, srv.URL, prices, slog.New(slog.NewJSONHandler(&logBuf, nil)), db)
	session := activeSession(t, db.sessions())

	if _, err := agent.Process(context.Background(), session, "你好"); err != nil {
		t.Fatalf("算不出成本不該讓對話失敗: %v", err)
	}

	db.flush(t)
	costs := queryLLMCallCosts(t, dbPath)
	if len(costs) != 1 {
		t.Fatalf("llm_calls 資料列數 = %d, 期望 1", len(costs))
	}
	if costs[0].Valid {
		t.Errorf("cost_micro_usd = %d, 期望空值——溢位的乘積不是一個成本", costs[0].Int64)
	}

	logs := logBuf.String()
	if strings.Contains(logs, "llm_cost_not_priced") {
		t.Errorf("記成了缺定價，但定價就在設定裡——管理員會去改一份正確的設定檔: %s", logs)
	}
	if !strings.Contains(logs, "llm_cost_uncomputable") {
		t.Errorf("未落算不出成本的警告: %s", logs)
	}
	if !strings.Contains(logs, string(core.CostUnavailableOutOfRange)) {
		t.Errorf("警告未帶原因，管理員無從得知該查哪裡: %s", logs)
	}
}
