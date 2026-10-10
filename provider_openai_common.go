package rosetta

// Shared helpers for the two OpenAI-family adapters (Chat Completions and
// Responses), which share Bearer auth and the /models catalog.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// fileDataURL normalizes inline file content into the data: URL form the
// OpenAI protocols expect ("data:<media>;base64,<data>"). A data: URL
// passes through; raw base64 gets the media type applied, defaulting to
// application/pdf (the document type both OpenAI protocols document).
// http(s) URLs are rejected: those protocols want inline data or an
// uploaded file_id (Anthropic is the one that accepts URL sources).
func fileDataURL(b Block) (string, error) {
	if strings.HasPrefix(b.FileData, "data:") {
		return b.FileData, nil
	}
	if strings.HasPrefix(b.FileData, "http://") || strings.HasPrefix(b.FileData, "https://") {
		return "", fmt.Errorf("%w: openai protocols accept inline file data or FileID, not a URL; use an http(s) URL only with Anthropic", ErrInvalidRequest)
	}
	if b.FileData == "" {
		return "", fmt.Errorf("%w: file block needs FileData or FileID", ErrInvalidRequest)
	}
	media := b.MimeType
	if media == "" {
		media = "application/pdf"
	}
	return "data:" + media + ";base64," + b.FileData, nil
}

// rejectionHit reports whether a 400 APIError rejects one of the named
// optional fields. A provider-supplied error.param is authoritative — it
// names the rejected field directly — so it is matched first; only when it
// is absent does the matcher fall back to scanning the message for a field
// name plus a rejection hint, which keeps unrelated 400s from misfiring.
// Shared by both OpenAI-family sanitizers so the compat ladder behaves
// identically whichever field the gateway labels.
func rejectionHit(apiErr *APIError, names ...string) bool {
	if param := strings.ToLower(apiErr.Param); param != "" {
		for _, n := range names {
			if strings.Contains(param, n) {
				return true
			}
		}
		return false
	}
	low := strings.ToLower(apiErr.Message)
	return containsAny(low, names...) && containsAny(low, hintWords...)
}

// openAIHeaders builds the common header set for OpenAI-family calls.
func openAIHeaders(c *Client, accept string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	if c.settings.apiKey != "" {
		h.Set("Authorization", "Bearer "+c.settings.apiKey)
	}
	return h
}

// listOpenAIModels queries GET /models (shape shared by both OpenAI
// protocols and most compatible services). Entries carry no capability
// metadata (Known=false).
func listOpenAIModels(ctx context.Context, c *Client) ([]ModelInfo, error) {
	const method = http.MethodGet
	url := joinEndpoint(c.settings.endpoint, "/models")
	call := &httpx.Call{Method: method, URL: url, Header: openAIHeaders(c, "application/json")}
	resp, err := c.http.Do(ctx, call)
	if err != nil {
		return nil, transport(err, method, url)
	}
	body, err := readBody(resp, method, url, bodyLimit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, withRetryAfter(parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id")), resp.Header)
	}
	var list struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		// /models catalog decode failure: the upstream body is not the shape
		// this protocol requires — an upstream fault (see
		// ErrUpstreamMalformed), not a caller mistake.
		return nil, fmt.Errorf("%w: model list: %w", ErrUpstreamMalformed, err)
	}
	// A 200 with no "data" field (or an explicit null) is a malformed catalog,
	// not an empty one. Treating it as empty would let SetRemote wipe every
	// previously learned remote entry (audit B11).
	//
	// Tagged with the same sentinel as the decode failure above: this is the
	// post-condition of the very same decode step, and {"data":null} is no
	// more a catalog than `{` is. Leaving it untagged would recreate the exact
	// classification hole one line below the fix.
	//
	// Contrast with the *content* checks further down (empty vector, wrong
	// dimension, non-numeric element): those inspect a body that DID decode to
	// the required shape, so they are deliberately left untagged — the
	// sentinel means "could not be decoded into the required shape", and
	// stretching it to "decoded but semantically unacceptable" would make
	// errors.Is(ErrUpstreamMalformed) useless as a failover signal.
	if list.Data == nil {
		return nil, fmt.Errorf("%w: model list response is missing the required \"data\" field", ErrUpstreamMalformed)
	}
	models := make([]ModelInfo, 0, len(*list.Data))
	for _, m := range *list.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, ModelInfo{ID: m.ID, Protocol: c.settings.protocol})
	}
	return models, nil
}
