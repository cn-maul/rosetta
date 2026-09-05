package rosetta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// knownHosts maps well-known API hosts to their protocol. Hosts not in
// this table fall through to active probing.
var knownHosts = map[string]Protocol{
	"api.openai.com":         ProtoOpenAIChat,
	"api.anthropic.com":      ProtoAnthropic,
	"api.deepseek.com":       ProtoOpenAIChat,
	"api.moonshot.cn":        ProtoOpenAIChat,
	"api.moonshot.ai":        ProtoOpenAIChat,
	"dashscope.aliyuncs.com": ProtoOpenAIChat,
	"open.bigmodel.cn":       ProtoOpenAIChat,
	"api.siliconflow.cn":     ProtoOpenAIChat,
	"openrouter.ai":          ProtoOpenAIChat,
	"api.groq.com":           ProtoOpenAIChat,
	"api.together.xyz":       ProtoOpenAIChat,
	"api.fireworks.ai":       ProtoOpenAIChat,
	"api.x.ai":               ProtoOpenAIChat,
	"api.mistral.ai":         ProtoOpenAIChat,
	"router.huggingface.co":  ProtoOpenAIChat,
}

// DetectProtocol determines which protocol an endpoint speaks:
//
//  1. known-host table (no network);
//  2. active probe of GET /models — OpenAI-style catalogs carry
//     object:"model" entries, Anthropic's carry type:"model";
//  3. falls back to OpenAI Chat, the de-facto compatibility lingua
//     franca (Responses-capable endpoints also serve /chat/completions).
//
// Both /v1/responses and /v1/chat/completions live behind the OpenAI
// family, so detection cannot (and need not) separate them; override with
// WithProtocol when Responses semantics are required.
func DetectProtocol(ctx context.Context, endpoint, apiKey string) (Protocol, error) {
	if p, ok := detectKnownHost(endpoint); ok {
		return p, nil
	}
	return detectByProbe(ctx, endpoint, apiKey)
}

func detectKnownHost(endpoint string) (Protocol, bool) {
	u, err := url.Parse(trimTrailingSlash(endpoint))
	if err != nil {
		return "", false
	}
	p, ok := knownHosts[u.Hostname()]
	return p, ok
}

// detectByProbe probes GET /models, first with Bearer auth then with
// x-api-key, and classifies the catalog shape.
func detectByProbe(ctx context.Context, endpoint, apiKey string) (Protocol, error) {
	probeURL := joinEndpoint(endpoint, "/models")
	client := &http.Client{Timeout: 10 * time.Second}
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
		resp, err := client.Do(req)
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
// the client's protocol. Other options behave exactly as in NewClient.
func DetectClient(ctx context.Context, opts ...Option) (*Client, error) {
	st, err := buildSettings(opts)
	if err != nil {
		return nil, err
	}
	if st.endpoint == "" {
		return nil, ErrNoEndpoint
	}
	proto, err := DetectProtocol(ctx, st.endpoint, st.apiKey)
	if err != nil {
		return nil, fmt.Errorf("rosetta: detecting protocol for %s: %w", st.endpoint, err)
	}
	return NewClient(append(opts, WithProtocol(proto))...)
}
