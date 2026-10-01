package rosetta

import "encoding/json"

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
// per-message overhead, flat per-block multimedia estimates, the tool
// definitions offered to the model, and a serialized-length fallback for
// Extra (which reaches the payload after every typed field is rendered).
//
// Text tokens are accumulated as raw rune counts and rounded once at the
// end, rather than calling EstimateTokens per block: rounding up each small
// block and summing would systematically overestimate a prompt made of many
// short blocks (M8).
func (r *ChatRequest) estimateInputTokens(est MultimediaTokenEstimates) int {
	est = est.resolve()
	total := 0
	ascii, other := 0, 0
	addText := func(s string) {
		a, o := tokenCounts(s)
		ascii += a
		other += o
	}
	if r.System != "" {
		addText(r.System)
		total += 4
	}
	for _, m := range r.Messages {
		total += 4
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				addText(b.Text)
			case BlockThinking:
				// The signature is replayed verbatim on every turn, so it
				// occupies real prompt tokens; ignoring it undercounts a
				// multi-turn extended-thinking conversation (audit B15).
				addText(b.Thinking)
				addText(b.Signature)
			case BlockRedactedThinking:
				// Redacted reasoning rides in the Thinking field as an opaque
				// base64 payload that is also replayed unchanged.
				addText(b.Thinking)
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
				addText(b.Arguments)
				total += 16
			case BlockToolResult:
				addText(b.Content)
			}
		}
	}
	for _, t := range r.Tools {
		// Per-tool overhead plus the name, description and schema text.
		total += 24
		addText(t.Name)
		addText(t.Description)
		addText(string(t.Parameters))
	}
	if len(r.Extra) > 0 {
		// Extra is the documented escape hatch for provider-specific fields
		// and is merged into the payload after the typed fields, so a caller
		// can inject arbitrary context (a whole document, a long tool
		// catalog) that the walk above never sees. Falling back to the
		// serialized length keeps the gate from being bypassed entirely;
		// it over-counts structure and keys, which suits an estimate that
		// must err on the high side. The serialization is reused from
		// validate() when available so a large Extra is not re-marshaled
		// (M7).
		b := r.extraJSON
		if b == nil {
			if b2, err := json.Marshal(r.Extra); err == nil {
				b = b2
			}
		}
		if b != nil {
			addText(string(b))
		}
	}
	total += (ascii+3)/4 + other
	return total
}

// tokenCounts returns the raw ASCII and non-ASCII rune counts of text,
// without the per-block ceiling that EstimateTokens applies.
func tokenCounts(text string) (ascii, other int) {
	for _, r := range text {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return
}
