package rosetta

// EstimateTokens gives a rough token count for a piece of text. CJK
// characters are ≈1 token each; ASCII text ≈4 characters per token. The
// estimate is deliberately conservative and is NOT a tokenizer — plug a
// real one in at the call site if precision matters.
func EstimateTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return ascii/4 + other
}

// estimateInputTokens approximates the prompt size of a request, including
// per-message overhead and a flat estimate per image.
func (r *ChatRequest) estimateInputTokens() int {
	total := 0
	if r.System != "" {
		total += EstimateTokens(r.System) + 4
	}
	for _, m := range r.Messages {
		total += 4
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				total += EstimateTokens(b.Text)
			case BlockThinking:
				total += EstimateTokens(b.Thinking)
			case BlockImage:
				total += 1500
			case BlockToolCall:
				total += EstimateTokens(b.Arguments) + 16
			case BlockToolResult:
				total += EstimateTokens(b.Content)
			}
		}
	}
	return total
}
