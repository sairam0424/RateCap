def estimate_llm_cost(input_tokens, max_tokens):
    cost = input_tokens + max_tokens
    return max(cost, 0)
