package rosetta

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// EmbeddingRequest asks for embedding vectors of one or more inputs.
// It rides on the OpenAI embeddings wire format (POST /embeddings), spoken
// by OpenAI, Ollama, vLLM, Qwen, GLM, Moonshot, SiliconFlow and most other
// compatible services. Embeddings have no streaming and no thinking; usage
// accounting covers input tokens only.
type EmbeddingRequest struct {
	// Model is the provider embedding model id, e.g. "text-embedding-3-small",
	// "bge-m3", "nomic-embed-text".
	Model string
	// Input is the list of texts to embed; one vector is returned per entry,
	// in order.
	Input []string
	// Dimensions requests a shortened output vector (OpenAI
	// text-embedding-3 family and compatible services; 0 = provider default).
	Dimensions int
	// User is an optional end-user identifier passed through for abuse
	// monitoring.
	User string
	// Extra is merged into the protocol payload last (same semantics as
	// ChatRequest.Extra).
	Extra map[string]any
}

func (r *EmbeddingRequest) validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil request", ErrInvalidRequest)
	}
	if r.Model == "" {
		return fmt.Errorf("%w: Model is required", ErrInvalidRequest)
	}
	if len(r.Input) == 0 {
		return fmt.Errorf("%w: Input must not be empty", ErrInvalidRequest)
	}
	if r.Dimensions < 0 {
		return fmt.Errorf("%w: Dimensions must not be negative", ErrInvalidRequest)
	}
	return nil
}

// Embedding is one vector: its position in the request's Input and the
// embedding values themselves.
type Embedding struct {
	Index     int
	Embedding []float32
}

// EmbeddingResponse is the decoded embeddings reply. Raw keeps the
// original body (truncated) for diagnostics.
type EmbeddingResponse struct {
	Model string
	Data  []Embedding
	Usage Usage
	Raw   json.RawMessage
}

// Embed computes embedding vectors via POST /embeddings. The call goes to
// the embedding endpoint override when configured (WithEmbeddingEndpoint),
// else the main endpoint; Anthropic-protocol clients require the override
// because Anthropic serves no embeddings API. Usage, when returned, is
// recorded into the configured tracker.
func (c *Client) Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if c.auxUnsupported() {
		return nil, fmt.Errorf("%w: anthropic has no embeddings API; point WithEmbeddingEndpoint at an OpenAI-compatible embedding service", ErrNotSupported)
	}
	if c.settings.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
		defer cancel()
	}
	payload := map[string]any{
		"model": req.Model,
		"input": req.Input,
	}
	if req.Dimensions > 0 {
		payload["dimensions"] = req.Dimensions
	}
	if req.User != "" {
		payload["user"] = req.User
	}
	if err := mergeExtra(payload, req.Extra, c.settings.extraOverrides, embeddingsReservedPayloadKeys); err != nil {
		return nil, err
	}
	body, err := postAuxJSON(ctx, c, "/embeddings", payload)
	if err != nil {
		return nil, err
	}
	// WithExtraOverrides(true) lets Extra replace input/dimensions; the
	// response must then be validated against what is actually on the
	// wire, not the original request fields. Normalize through JSON so an
	// override of any shape ([]any, float64) is read, not just the SDK's
	// []string/int — and an override to an empty list is a hard error, not a
	// skipped count check (audit C20).
	expected := len(req.Input)
	if n, ok := wireSliceLen(payload["input"]); ok {
		if n == 0 {
			return nil, fmt.Errorf("%w: embeddings input is empty after Extra override", ErrInvalidRequest)
		}
		expected = n
	}
	dimensions := req.Dimensions
	if v, ok := wireInt(payload["dimensions"]); ok {
		dimensions = v
	}
	resp, err := decodeEmbeddingsResponse(body, expected, dimensions)
	if err != nil {
		return nil, err
	}
	c.record(req.Model, resp.Usage, resp.Usage.IsZero())
	return resp, nil
}

// decodeEmbeddingsResponse decodes and sanity-checks the reply: one
// non-empty vector per input, with unique in-range indices, all elements
// numeric, a consistent dimensionality across the batch (equal to a
// requested Dimensions when one was sent), and non-negative usage. A
// provider answer that fails these checks is a protocol error, not a
// success with missing data — callers would otherwise embed "silently
// missing" vectors into downstream indexes.
func decodeEmbeddingsResponse(body []byte, expected, dimensions int) (*EmbeddingResponse, error) {
	var wire struct {
		Model string `json:"model"`
		Data  []struct {
			Index     *int               `json:"index"`
			Embedding *[]json.RawMessage `json:"embedding"`
		} `json:"data"`
		Usage *struct {
			InputTokens  *int64 `json:"input_tokens"`
			PromptTokens *int64 `json:"prompt_tokens"`
			TotalTokens  *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("rosetta: decoding embeddings response: %w", err)
	}
	if len(wire.Data) == 0 {
		return nil, fmt.Errorf("rosetta: embeddings response contains no data")
	}
	if expected > 0 && len(wire.Data) != expected {
		return nil, fmt.Errorf("rosetta: embeddings response has %d vectors for %d inputs", len(wire.Data), expected)
	}
	seen := make(map[int]bool, len(wire.Data))
	out := &EmbeddingResponse{Model: wire.Model, Raw: truncateBody(body)}
	out.Data = make([]Embedding, 0, len(wire.Data))
	wantDim := dimensions // 0 = derive from the first vector
	for i, d := range wire.Data {
		if d.Index == nil {
			return nil, fmt.Errorf("rosetta: embeddings response vector %d is missing its index", i)
		}
		idx := *d.Index
		if idx < 0 || (expected > 0 && idx >= expected) {
			return nil, fmt.Errorf("rosetta: embeddings response has out-of-range index %d", idx)
		}
		if seen[idx] {
			return nil, fmt.Errorf("rosetta: embeddings response repeats index %d", idx)
		}
		seen[idx] = true
		if d.Embedding == nil || len(*d.Embedding) == 0 {
			return nil, fmt.Errorf("rosetta: embeddings response has an empty vector at index %d", idx)
		}
		vec := make([]float32, 0, len(*d.Embedding))
		for j, raw := range *d.Embedding {
			if trimmed := strings.TrimSpace(string(raw)); trimmed == "null" || trimmed == "" {
				return nil, fmt.Errorf("rosetta: embeddings response vector %d element %d is null", idx, j)
			}
			var f float32
			if err := json.Unmarshal(raw, &f); err != nil {
				return nil, fmt.Errorf("rosetta: embeddings response vector %d element %d is not a number", idx, j)
			}
			vec = append(vec, f)
		}
		if wantDim == 0 {
			wantDim = len(vec)
		}
		if len(vec) != wantDim {
			return nil, fmt.Errorf("rosetta: embeddings response vector %d has dimension %d, want %d", idx, len(vec), wantDim)
		}
		out.Data = append(out.Data, Embedding{Index: idx, Embedding: vec})
	}
	// The contract is "one vector per input, in order": restore request
	// order even if the provider shuffled data.
	slices.SortFunc(out.Data, func(a, b Embedding) int { return a.Index - b.Index })
	if wire.Usage != nil {
		// Vendors disagree between input_tokens and prompt_tokens; accept
		// both. total_tokens alone is not treated as input.
		input := int64(0)
		if wire.Usage.InputTokens != nil {
			input = *wire.Usage.InputTokens
		} else if wire.Usage.PromptTokens != nil {
			input = *wire.Usage.PromptTokens
		}
		total := int64(0)
		if wire.Usage.TotalTokens != nil {
			total = *wire.Usage.TotalTokens
		}
		if input < 0 || total < 0 {
			return nil, fmt.Errorf("rosetta: embeddings response has negative token usage")
		}
		u := Usage{InputTokens: input}
		u.TotalTokens = total
		if u.TotalTokens == 0 {
			u.TotalTokens = u.InputTokens
		}
		out.Usage = u
	}
	return out, nil
}
