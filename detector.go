package rosetta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// detectBodyLimit bounds the /models body read during probing. Catalogs can
// be large; the limit is generous enough to hold the first page (which
// carries the classification field) without truncating mid-JSON.
const detectBodyLimit = 4 << 20

// DetectProtocol determines which protocol an endpoint speaks by active
// probing of GET /models: OpenAI-style catalogs carry object:"model"
// entries, Anthropic's carry type:"model". Both auth styles are tried
// (Bearer first, then x-api-key). When nothing can be classified it falls
// back to OpenAI Chat, the de-facto compatibility lingua franca —
// Responses-capable endpoints also serve /chat/completions. Override with
// WithProtocol when Responses semantics are required or probing is
// impossible.
func DetectProtocol(ctx context.Context, endpoint, apiKey string) (Protocol, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return "", fmt.Errorf("rosetta: invalid endpoint %q: %w", displayEndpoint(endpoint), err)
	}
	proto, _, err := detectByProbe(ctx, endpoint, apiKey, newProbeClient(), nil)
	if err != nil {
		return "", err
	}
	return proto, nil
}

// newProbeClient returns an HTTP client for probing that refuses to
// re-send credentials on cross-host redirects (net/http only strips
// Authorization/Cookie automatically, never x-api-key).
func newProbeClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: httpx.CrossHostSafeRedirect}
}

// detectByProbe probes GET /models, first with Bearer auth then with
// x-api-key, and classifies the catalog shape. The caller supplies the
// HTTP client so custom transports and deadlines are honored. The bool
// result reports whether a definitive classification was made; when false
// the returned protocol is the OpenAI Chat fallback. logger (optional) is
// told why detection was inconclusive.
func detectByProbe(ctx context.Context, endpoint, apiKey string, hc *http.Client, logger *slog.Logger) (Protocol, bool, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	probeURL := joinEndpoint(endpoint, "/models")
	// classify reads the live body so an oversized catalog still classifies
	// from its first page (see classifyCatalog).
	try := func(auth string) (Protocol, bool, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return "", false, 0, err
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
			return "", false, 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", false, resp.StatusCode, nil
		}
		proto, ok := classifyCatalog(io.LimitReader(resp.Body, detectBodyLimit))
		return proto, ok, resp.StatusCode, nil
	}
	for _, auth := range []string{"bearer", "x-api-key"} {
		proto, ok, status, err := try(auth)
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			logger.Warn("rosetta: protocol probe transport failure", "auth", auth, "err", err.Error())
			continue
		}
		if status != http.StatusOK {
			logger.Debug("rosetta: protocol probe non-200", "auth", auth, "status", status)
			continue
		}
		if ok {
			return proto, true, nil
		}
		logger.Debug("rosetta: protocol probe could not classify /models catalog", "auth", auth)
	}
	logger.Warn("rosetta: protocol undetermined, defaulting to OpenAI Chat", "endpoint", displayEndpoint(endpoint))
	return ProtoOpenAIChat, false, nil
}

// classifyCatalog streams a /models catalog and classifies it from the first
// element of its "data" array: Anthropic entries carry type:"model", OpenAI
// entries carry object:"model". Streaming (rather than unmarshaling the whole
// body) lets a catalog larger than the read limit still classify from its
// first page, and tolerates unrelated top-level keys in any order.
func classifyCatalog(r io.Reader) (Protocol, bool) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := keyTok.(string)
		if key != "data" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return "", false
			}
			continue
		}
		dt, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, ok := dt.(json.Delim); !ok || d != '[' {
			return "", false
		}
		if !dec.More() {
			return "", false
		}
		var first map[string]json.RawMessage
		if dec.Decode(&first) != nil {
			return "", false
		}
		if v, ok := first["type"]; ok && string(v) == `"model"` {
			return ProtoAnthropic, true
		}
		if v, ok := first["object"]; ok && string(v) == `"model"` {
			return ProtoOpenAIChat, true
		}
		return "", false
	}
	return "", false
}

// DetectClient builds a Client with automatic protocol detection: the
// endpoint is classified via the /models probe and the result is pinned as
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
	// An explicit WithProtocol wins: do not probe, and do not let a probe
	// result override the caller's pinned protocol (B12).
	if st.protocolSet {
		return NewClient(opts...)
	}
	hc := st.httpClient
	if hc == nil {
		hc = newProbeClient()
	} else {
		// Copy so the redirect guard does not mutate the caller's client.
		guarded := *hc
		if guarded.CheckRedirect == nil {
			guarded.CheckRedirect = httpx.CrossHostSafeRedirect
		}
		if guarded.Timeout == 0 {
			guarded.Timeout = 10 * time.Second
		}
		hc = &guarded
	}
	if st.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, st.timeout)
		defer cancel()
	}
	proto, _, err := detectByProbe(ctx, st.endpoint, st.apiKey, hc, st.logger)
	if err != nil {
		return nil, fmt.Errorf("rosetta: detecting protocol for %s: %w", displayEndpoint(st.endpoint), err)
	}
	// Force a copy so the appended option cannot leak into the caller's
	// backing array.
	opts = append(opts[:len(opts):len(opts)], WithProtocol(proto))
	return NewClient(opts...)
}
