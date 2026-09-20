package rosetta

// MultimediaTokenEstimates overrides the flat per-block token estimates
// used by the context-window check. The built-in defaults are deliberately
// coarse (multimedia tokenization is provider- and resolution-specific);
// tune them when the default warnings are too noisy or too lax for your
// provider's actual billing. Zero fields keep the defaults.
type MultimediaTokenEstimates struct {
	// Image is the flat estimate per image block. Default 1500.
	Image int
	// Audio is the flat estimate per audio block. Default 500.
	Audio int
	// File is the flat estimate per document block. Default 3000.
	File int
}

// resolve fills zero fields with the built-in defaults.
func (e MultimediaTokenEstimates) resolve() MultimediaTokenEstimates {
	if e.Image <= 0 {
		e.Image = 1500
	}
	if e.Audio <= 0 {
		e.Audio = 500
	}
	if e.File <= 0 {
		e.File = 3000
	}
	return e
}

// EstimateTokens gives a rough token count for a piece of text. CJK
// characters are ≈1 token each; ASCII text ≈4 characters per token. The
// estimate rounds up so it errs on the high side (a context-window
// warning must never undercount) and is NOT a tokenizer — plug a real one
// in at the call site if precision matters.
func EstimateTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// estimateInputTokens approximates the prompt size of a request, including
// per-message overhead, flat per-block multimedia estimates and the tool
// definitions offered to the model.
func (r *ChatRequest) estimateInputTokens(est MultimediaTokenEstimates) int {
	est = est.resolve()
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
				// The signature is replayed verbatim on every turn, so it
				// occupies real prompt tokens; ignoring it undercounts a
				// multi-turn extended-thinking conversation (audit B15).
				total += EstimateTokens(b.Thinking) + EstimateTokens(b.Signature)
			case BlockRedactedThinking:
				// Redacted reasoning rides in the Thinking field as an opaque
				// base64 payload that is also replayed unchanged.
				total += EstimateTokens(b.Thinking)
			case BlockImage:
				total += est.Image
			case BlockAudio:
				// Flat per-clip estimate; audio tokenization is
				// provider-specific and duration is not carried on the block.
				total += est.Audio
			case BlockFile:
				// Conservative flat estimate for a typical small document;
				// real PDF cost varies by page count and content.
				total += est.File
			case BlockToolCall:
				total += EstimateTokens(b.Arguments) + 16
			case BlockToolResult:
				total += EstimateTokens(b.Content)
			}
		}
	}
	for _, t := range r.Tools {
		// Per-tool overhead plus the name, description and schema text.
		total += 24 + EstimateTokens(t.Name) + EstimateTokens(t.Description)
		total += EstimateTokens(string(t.Parameters))
	}
	return total
}
