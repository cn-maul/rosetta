package rosetta

// Plumbing shared by the auxiliary (non-chat) API families: embeddings and
// rerank. Both speak OpenAI-style JSON over Bearer auth, ride on the
// embedding endpoint override (WithEmbeddingEndpoint / WithEmbeddingAPIKey)
// and are safe to retry (their payloads carry no side effects).

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// embedBase resolves the base URL for auxiliary calls: the explicit
// embedding override, else the main endpoint.
func (c *Client) embedBase() string {
	if c.settings.embedEndpoint != "" {
		return c.settings.embedEndpoint
	}
	return c.settings.endpoint
}

// embedKey resolves the credential for auxiliary calls.
func (c *Client) embedKey() string {
	if c.settings.embedAPIKey != "" {
		return c.settings.embedAPIKey
	}
	return c.settings.apiKey
}

// auxUnsupported reports whether auxiliary calls are impossible on this
// client: an Anthropic-protocol main endpoint has no /embeddings or
// /rerank, so an explicit override is required there.
func (c *Client) auxUnsupported() bool {
	return c.settings.protocol == ProtoAnthropic && c.settings.embedEndpoint == ""
}

// auxHeaders builds the Bearer-auth header set for auxiliary calls.
func auxHeaders(c *Client) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	if key := c.embedKey(); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return h
}

// postAuxJSON posts an OpenAI-style JSON payload against the auxiliary
// endpoint and returns the raw 200 body. Responses can be large (one
// embedding vector per input), so the read cap is generous.
func postAuxJSON(ctx context.Context, c *Client, path string, payload map[string]any) ([]byte, error) {
	const method = http.MethodPost
	url := joinEndpoint(c.embedBase(), path)
	call := &httpx.Call{
		Method:      method,
		URL:         url,
		Header:      auxHeaders(c),
		Body:        func() ([]byte, error) { return json.Marshal(payload) },
		RetryPolicy: httpx.RetryIdempotent,
	}
	resp, err := c.http.Do(ctx, call)
	if err != nil {
		return nil, transport(err, method, url)
	}
	body, rerr := httpx.ReadBody(resp.Body, 64<<20)
	resp.Body.Close()
	if rerr != nil {
		return nil, transport(rerr, method, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
	}
	return body, nil
}
