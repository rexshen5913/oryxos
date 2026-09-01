.PHONY: test build eval

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -o oryxos ./cmd/oryxos

# eval 跑評測 harness：讀 evals/ 下的 YAML 用例，在乾淨的 Workspace 裡驅動真實 Agent
# 跑一輪並判卷。
#
# **它會呼叫真實 Provider 並產生費用**，也需要 config.yaml 引用的 API 憑證環境變數
# （例如 OPENROUTER_API_KEY）已經設好。
#
# 這是它刻意不掛在 test 之下、也不進 CI 的原因（憲法 4.4）：自動化測試中 LLM 一律回放
# 錄製回應，評測則相反——它的價值就在於用真實模型。誤觸的代價是一張真實帳單，而且測試
# 照樣綠燈，所以這條線由 internal/eval 的 TestMakeTestDoesNotTriggerEval 守著。
#
# 傳旗標用 EVAL_FLAGS，例如：
#   make eval EVAL_FLAGS="--cases evals/01-reply-only.yaml --workspace .oryxos"
eval:
	CGO_ENABLED=0 go run ./cmd/oryxos-eval $(EVAL_FLAGS)
