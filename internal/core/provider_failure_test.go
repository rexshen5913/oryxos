// Provider 失敗的可辨識錯誤（ticket #79，spec #73 第八節）。Web Service 依它把 turn 失敗分類成
// 503 provider_error：呼叫端看到 503，就知道是上游暫時不可用、可以稍後重試，而不是請求寫錯了。
package core_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestProviderFailureIsRecognizable 釘住兩件事：
//
//   - **Provider 呼叫失敗的錯誤鏈含 core.ErrProviderFailed**，其他原因的 turn 失敗則不含。
//   - **錯誤文字一個字都不變**：chat 印給使用者的就是這段文字。標記若以前綴的形式疊上去，
//     每一則 Provider 錯誤都會多出一段同義的話，所以用開頭比對，而不只是「含有某個子串」。
func TestProviderFailureIsRecognizable(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) *core.AgentService
		// wantProvider 為真時，期望錯誤鏈含 ErrProviderFailed。
		wantProvider bool
		// wantPrefix 是錯誤訊息的開頭：加上標記之前就是這段文字。
		wantPrefix string
	}{
		{
			name: "Provider 回非 2xx",
			setup: func(t *testing.T) *core.AgentService {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
				}))
				t.Cleanup(srv.Close)
				return newAgent(t, srv.URL, discardLogger())
			},
			wantProvider: true,
			wantPrefix:   "呼叫 LLM: Provider openai 呼叫失敗",
		},
		{
			name: "網路錯誤（端點不可達）",
			setup: func(t *testing.T) *core.AgentService {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				srv.Close()
				return newAgent(t, srv.URL, discardLogger())
			},
			wantProvider: true,
			wantPrefix:   "呼叫 LLM: Provider openai 呼叫失敗",
		},
		{
			// LLM 呼叫成功，失敗發生在之後的持久化：這不是 Provider 的問題，重試不會比較好。
			name: "持久化失敗（跟 Provider 無關）",
			setup: func(t *testing.T) *core.AgentService {
				srv := newReplayServer(t, readFixture(t, "reply_direct.json"))
				st := newStore(t)
				agent := newAgentOn(t, srv.URL, discardLogger(), st)
				if err := st.close(); err != nil {
					t.Fatalf("關閉 SQLite: %v", err)
				}
				return agent
			},
			wantProvider: false,
			wantPrefix:   "持久化 Session",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.setup(t).Process(context.Background(), core.NewSession("cli", "local", "default"), "你好")
			if err == nil {
				t.Fatal("期望這個 turn 失敗，Process 卻成功返回")
			}
			if got := errors.Is(err, core.ErrProviderFailed); got != tt.wantProvider {
				t.Errorf("errors.Is(err, ErrProviderFailed) = %v, 期望 %v\n錯誤: %v", got, tt.wantProvider, err)
			}
			if !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Errorf("錯誤訊息 = %q, 期望以 %q 開頭（標記不該改動既有的錯誤文字）", err.Error(), tt.wantPrefix)
			}
		})
	}
}
