package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/rexshen5913/oryxos/internal/core"
)

// Config 是單一 Provider 的連線配置。
type Config struct {
	APIKey  string
	BaseURL string // 空值時用 OpenAI 官方端點；自動化測試以此指向 httptest.Server（ADR-0002）
}

// llmRequestTimeout 是單次 LLM 呼叫的請求超時上限：Provider 掛住時保證超時
// 報錯而非無限等待；呼叫端仍可用 ctx 更早取消。
const llmRequestTimeout = 120 * time.Second

// Service 實作 core.ProviderService：維護 provider name → go-openai 客戶端實例
// 的顯式 map 註冊表（憲法 2.3），對 ReAct 循環屏蔽各 LLM 服務差異。
type Service struct {
	clients map[string]*openai.Client
	logger  *slog.Logger
}

// NewService 依 configs 為每個 provider name 顯式建立客戶端並註冊。
// logger 用於落每次 LLM 呼叫的結構化日誌，不得為 nil。
func NewService(configs map[string]Config, logger *slog.Logger) *Service {
	clients := make(map[string]*openai.Client, len(configs))
	for name, cfg := range configs {
		oc := openai.DefaultConfig(cfg.APIKey)
		if cfg.BaseURL != "" {
			oc.BaseURL = cfg.BaseURL
		}
		oc.HTTPClient = &http.Client{Timeout: llmRequestTimeout}
		clients[name] = openai.NewClientWithConfig(oc)
	}
	return &Service{clients: clients, logger: logger}
}

// Chat 以 req.Provider 對應的客戶端完成一次 LLM 呼叫。只做協議轉換（憲法 2.2）；
// 故障直接報錯，不重試、不 fallback（技術方案 §3.3）。
func (s *Service) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	client, ok := s.clients[req.Provider]
	if !ok {
		return core.ChatResponse{}, fmt.Errorf("Provider %q 未註冊（檢查 Workspace 設定檔的 providers 段）", req.Provider)
	}

	start := time.Now()
	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		Messages:    toOpenAIMessages(req.Messages),
	})
	latency := time.Since(start)
	if err != nil {
		s.logger.ErrorContext(ctx, "llm_call",
			"provider", req.Provider,
			"model", req.Model,
			"latency_ms", latency.Milliseconds(),
			"error", err.Error(),
		)
		return core.ChatResponse{}, fmt.Errorf("Provider %s 呼叫失敗: %w", req.Provider, err)
	}

	s.logger.InfoContext(ctx, "llm_call",
		"provider", req.Provider,
		"model", req.Model,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
		"latency_ms", latency.Milliseconds(),
	)

	if len(resp.Choices) == 0 {
		return core.ChatResponse{}, fmt.Errorf("Provider %s 回應不含任何 choice", req.Provider)
	}
	return fromOpenAIMessage(resp.Choices[0].Message), nil
}

// toOpenAIMessages 把 Session 訊息轉成 OpenAI 兼容協議格式。tool 訊息與
// assistant 的 tool_calls 轉換隨 Tool 執行於後續 ticket 補上。
func toOpenAIMessages(msgs []core.Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, openai.ChatCompletionMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}
	return out
}

// fromOpenAIMessage 把 LLM 回應訊息轉回 OryxOS 內部格式，raw tool_calls
// 原樣交回，循環與調度由 core 控制（憲法 2.2）。
func fromOpenAIMessage(m openai.ChatCompletionMessage) core.ChatResponse {
	resp := core.ChatResponse{Content: m.Content}
	for _, tc := range m.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return resp
}
