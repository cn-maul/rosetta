package rosetta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DetectProtocol determines which protocol an endpoint speaks by active
// probing of GET /models: OpenAI-style catalogs carry object:"model"
// entries, Anthropic's carry type:"model". Both auth styles are tried
// (Bearer first, then x-api-key). It falls back to OpenAI Chat, the
// de-facto compatibility lingua franca — Responses-capable endpoints
// also serve /chat/completions. Override with WithProtocol when
// Responses semantics are required or probing is impossible.
func DetectProtocol(ctx context.Context, endpoint, apiKey string) (Protocol, error) {
	return detectByProbe(ctx, endpoint, apiKey, &http.Client{Timeout: 10 * time.Second})
}

// detectByProbe probes GET /models, first with Bearer auth then with
// x-api-key, and classifies the catalog shape. The caller supplies the
// HTTP client so custom transports and deadlines are honored.
func detectByProbe(ctx context.Context, endpoint, apiKey string, hc *http.Client) (Protocol, error) {
	probeURL := joinEndpoint(endpoint, "/models")
	try := func(auth string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/json")
		if apiKey != "" {
			if auth == "bearer" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			} else {
				req.Header.Set("x-api-key", apiKey)
				req.Header.Set("anthropic-version", anthropicVersion)
			}
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, body, err
	}
	for _, auth := range []string{"bearer", "x-api-key"} {
		status, body, err := try(auth)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		if status != http.StatusOK {
			continue
		}
		var doc struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &doc) != nil || len(doc.Data) == 0 {
			continue
		}
		switch doc.Data[0]["type"] {
		case "model":
			return ProtoAnthropic, nil
		}
		switch doc.Data[0]["object"] {
		case "model":
			return ProtoOpenAIChat, nil
		}
	}
	return ProtoOpenAIChat, nil
}

// DetectClient builds a Client with automatic protocol detection: the
// endpoint is classified via DetectProtocol and the result is pinned as
// the client's protocol. Other options behave exactly as in NewClient;
// WithHTTPClient and WithTimeout apply to the probe too.
func DetectClient(ctx context.Context, opts ...Option) (*Client, error) {
	st, err := buildSettings(opts)
	if err != nil {
		return nil, err
	}
	if st.endpoint == "" {
		return nil, ErrNoEndpoint
	}
	hc := st.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if st.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, st.timeout)
		defer cancel()
	}
	proto, err := detectByProbe(ctx, st.endpoint, st.apiKey, hc)
	if err != nil {
		return nil, fmt.Errorf("rosetta: detecting protocol for %s: %w", st.endpoint, err)
	}
	// Force a copy so the appended option cannot leak into the caller's
	// backing array.
	opts = append(opts[:len(opts):len(opts)], WithProtocol(proto))
	return NewClient(opts...)
}
