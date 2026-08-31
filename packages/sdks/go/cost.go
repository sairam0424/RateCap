package ratecap

// EstimateLLMCost mirrors the AWS Bedrock/LiteLLM token-cost estimate
// (input tokens + max output tokens the model is allowed to generate) —
// ratecap-core stays transport/schema-agnostic; it never parses any LLM
// provider's request or response, it only ever sees the resulting int.
func EstimateLLMCost(inputTokens, maxTokens int) int {
	cost := inputTokens + maxTokens
	if cost < 0 {
		return 0
	}
	return cost
}
