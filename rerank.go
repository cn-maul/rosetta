package rosetta

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

// RerankRequest scores a list of documents against a query, most relevant
// first in the reply. It rides on the Cohere rerank wire format
// (POST /rerank), spoken by Cohere, Jina, SiliconFlow, Voyage, DashScope
// and vLLM. Usage accounting is best-effort: vendors disagree on where
// token counts live (Cohere reports billed units, Jina a total), so Usage
// carries whatever was found and may be zero.
type RerankRequest struct {
	// Model is the provider rerank model id, e.g. "rerank-v3.5",
	// "jina-reranker-v2-base-multilingual", "bge-reranker-v2-m3".
	Model string
	// Query is the search query the documents are scored against.
	Query string
	// Documents are the candidate texts. Their original order defines the
	// meaning of RerankResult.Index.
	Documents []string
	// TopN caps how many results come back (0 = provider default).
	TopN int
	// ReturnDocuments asks the provider to echo the document text in each
	// result (also available by reading the caller's own slice by index).
	ReturnDocuments bool
	// Extra is merged into the protocol payload last (same semantics as
	// ChatRequest.Extra).
	Extra map[string]any
}

func (r *RerankRequest) validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil request", ErrInvalidRequest)
	}
	if r.Model == "" {
		return fmt.Errorf("%w: Model is required", ErrInvalidRequest)
	}
	if r.Query == "" {
		return fmt.Errorf("%w: Query must not be empty", ErrInvalidRequest)
	}
	if len(r.Documents) == 0 {
		return fmt.Errorf("%w: Documents must not be empty", ErrInvalidRequest)
	}
	if r.TopN < 0 {
		return fmt.Errorf("%w: TopN must not be negative", ErrInvalidRequest)
	}
	return nil
}

// RerankResult is one scored document: its position in the request's
// Documents slice and its relevance score (normally in [0,1], but scores
// are not comparable across models).
type RerankResult struct {
	Index          int
	RelevanceScore float64
	// Document echoes the document text when the provider returned it
	// (ReturnDocuments, or a vendor that always does).
	Document string
}

// RerankResponse is the decoded rerank reply, ordered most relevant first.
type RerankResponse struct {
	ID      string
	Results []RerankResult
	Usage   Usage
	Raw     json.RawMessage
}

// Rerank scores documents via POST /rerank. The call goes to the embedding
// endpoint override when configured (WithEmbeddingEndpoint), else the main
// endpoint; Anthropic-protocol clients require the override because
// Anthropic serves no rerank API.
func (c *Client) Rerank(ctx context.Context, req *RerankRequest) (*RerankResponse, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if c.auxUnsupported() {
		return nil, fmt.Errorf("%w: anthropic has no rerank API; point WithEmbeddingEndpoint at a Cohere-compatible rerank service", ErrNotSupported)
	}
	if c.settings.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
		defer cancel()
	}
	payload := map[string]any{
		"model":     req.Model,
		"query":     req.Query,
		"documents": req.Documents,
	}
	if req.TopN > 0 {
		payload["top_n"] = req.TopN
	}
	if req.ReturnDocuments {
		payload["return_documents"] = true
	}
	if err := mergeExtra(payload, req.Extra, c.settings.extraOverrides, rerankReservedPayloadKeys); err != nil {
		return nil, err
	}
	body, err := postAuxJSON(ctx, c, "/rerank", payload)
	if err != nil {
		return nil, err
	}
	// WithExtraOverrides(true) lets Extra replace documents; validate the
	// reply against what is actually on the wire. Normalize through JSON so
	// an override of any shape is read, and reject an override to an empty
	// list rather than skipping the index range check (audit C20).
	expectedDocs := len(req.Documents)
	if n, ok := wireSliceLen(payload["documents"]); ok {
		if n == 0 {
			return nil, fmt.Errorf("%w: rerank documents is empty after Extra override", ErrInvalidRequest)
		}
		expectedDocs = n
	}
	resp, err := decodeRerankResponse(body, expectedDocs)
	if err != nil {
		return nil, err
	}
	c.record(req.Model, resp.Usage, resp.Usage.IsZero())
	return resp, nil
}

// decodeRerankResponse decodes and sanity-checks the reply: every result
// must reference a real document (in-range, unique index) with a finite,
// present score — missing or null fields are protocol errors, not zeroes,
// since callers index their Documents slice by result Index. Results are
// re-sorted by descending score so the "most relevant first" contract
// holds even when a provider returns them out of order. Only explicit
// token counts feed Usage — billed units such as Cohere's search_units
// are not tokens and must not pollute token statistics.
func decodeRerankResponse(body []byte, expectedDocs int) (*RerankResponse, error) {
	var wire struct {
		ID      string `json:"id"`
		Results []*struct {
			Index          *int     `json:"index"`
			RelevanceScore *float64 `json:"relevance_score"`
			Document       *struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
		// Cohere keeps counts under meta (billed_units or tokens); Jina
		// uses a flat usage object. Accept both.
		Meta *struct {
			BilledUnits *struct {
				InputTokens *int64 `json:"input_tokens"`
				SearchUnits *int64 `json:"search_units"`
			} `json:"billed_units"`
			Tokens *struct {
				InputTokens *int64 `json:"input_tokens"`
			} `json:"tokens"`
		} `json:"meta"`
		Usage *struct {
			TotalTokens *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("rosetta: decoding rerank response: %w", err)
	}
	if len(wire.Results) == 0 {
		return nil, fmt.Errorf("rosetta: rerank response contains no results")
	}
	seen := make(map[int]bool, len(wire.Results))
	out := &RerankResponse{ID: wire.ID, Raw: truncateBody(body)}
	for i, r := range wire.Results {
		if r == nil {
			return nil, fmt.Errorf("rosetta: rerank response result %d is null", i)
		}
		if r.Index == nil {
			return nil, fmt.Errorf("rosetta: rerank response result %d is missing its index", i)
		}
		idx := *r.Index
		if idx < 0 || (expectedDocs > 0 && idx >= expectedDocs) {
			return nil, fmt.Errorf("rosetta: rerank response has out-of-range index %d for %d documents", idx, expectedDocs)
		}
		if seen[idx] {
			return nil, fmt.Errorf("rosetta: rerank response repeats index %d", idx)
		}
		seen[idx] = true
		if r.RelevanceScore == nil {
			return nil, fmt.Errorf("rosetta: rerank response result %d is missing its relevance score", i)
		}
		score := *r.RelevanceScore
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, fmt.Errorf("rosetta: rerank response has a non-finite score at index %d", idx)
		}
		rr := RerankResult{Index: idx, RelevanceScore: score}
		if r.Document != nil {
			rr.Document = r.Document.Text
		}
		out.Results = append(out.Results, rr)
	}
	// Enforce the "most relevant first" contract independent of provider
	// ordering; stable so equal scores keep the provider's order.
	slices.SortStableFunc(out.Results, func(a, b RerankResult) int {
		switch {
		case a.RelevanceScore > b.RelevanceScore:
			return -1
		case a.RelevanceScore < b.RelevanceScore:
			return 1
		default:
			return 0
		}
	})
	u := Usage{}
	switch {
	case wire.Meta != nil && wire.Meta.Tokens != nil && wire.Meta.Tokens.InputTokens != nil:
		u.InputTokens = *wire.Meta.Tokens.InputTokens
	case wire.Meta != nil && wire.Meta.BilledUnits != nil && wire.Meta.BilledUnits.InputTokens != nil:
		u.InputTokens = *wire.Meta.BilledUnits.InputTokens
	case wire.Usage != nil && wire.Usage.TotalTokens != nil:
		u.InputTokens = *wire.Usage.TotalTokens
	}
	if u.InputTokens < 0 {
		return nil, fmt.Errorf("rosetta: rerank response has negative token usage")
	}
	u.TotalTokens = u.InputTokens
	out.Usage = u
	return out, nil
}
