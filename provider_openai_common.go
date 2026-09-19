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
		return nil, parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("rosetta: decoding model list: %w", err)
	}
	models := make([]ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, ModelInfo{ID: m.ID, Protocol: c.settings.protocol})
	}
	return models, nil
}
