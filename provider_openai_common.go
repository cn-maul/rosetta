package rosetta

// Shared helpers for the two OpenAI-family adapters (Chat Completions and
// Responses), which share Bearer auth and the /models catalog.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cn-maul/rosetta/internal/httpx"
)

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
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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
