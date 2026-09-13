package rosetta

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// Sentinel errors returned by the SDK. Use errors.Is to match.
var (
	// ErrNoEndpoint is returned when no endpoint could be determined.
	ErrNoEndpoint = errors.New("rosetta: endpoint is required (WithEndpoint)")
	// ErrNoAPIKey is returned when no API key was configured. Local
	// servers that ignore auth still expect a non-empty placeholder.
	ErrNoAPIKey = errors.New("rosetta: api key is required (WithAPIKey)")
	// ErrUnknownModel is returned when a model id cannot be resolved.
	ErrUnknownModel = errors.New("rosetta: unknown model")
	// ErrContextTooLong is returned (strict mode only) when the prompt
	// is estimated to exceed the model context window.
	ErrContextTooLong = errors.New("rosetta: prompt exceeds model context window")
	// ErrThinkingUnsupported is returned when thinking was requested but
	// the model cannot think and no fallback is configured.
	ErrThinkingUnsupported = errors.New("rosetta: model does not support thinking")
	// ErrInvalidRequest is returned for structurally invalid requests.
	ErrInvalidRequest = errors.New("rosetta: invalid request")
	// ErrNotSupported is returned when an operation cannot be served by the
	// configured protocol/endpoint combination — e.g. Embed on an
	// Anthropic-protocol client without WithEmbeddingEndpoint.
	ErrNotSupported = errors.New("rosetta: operation not supported with this configuration")
	// ErrStreamTruncated is returned when a stream ends (EOF) without the
	// provider's terminal event — a cut connection or gateway timeout, not
	// a clean finish. The partial response stays available via
	// Stream.Partial; match with errors.Is.
	ErrStreamTruncated = errors.New("stream truncated")
)

// TransportError wraps a lower-level network failure (DNS, connect, TLS,
// read) with the request context it occurred in.
type TransportError struct {
	Method string
	URL    string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("rosetta: transport error during %s %s: %v", e.Method, e.URL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// APIError is a structured error returned by the remote API. It is produced
// for every non-2xx response whose body could be interpreted.
type APIError struct {
	StatusCode int
	Code       string // provider-specific error code, when present
	Type       string // provider-specific error type, when present
	Message    string
	RequestID  string // from X-Request-Id / request-id headers
	Method     string
	URL        string
	// Retryable marks statuses worth retrying (408/429/5xx/529). It is a
	// hint for callers: the SDK auto-retries only when the request's retry
	// policy allows it (GET-like methods by default; non-idempotent POSTs
	// like chat require an explicit policy).
	Retryable bool
	Raw       json.RawMessage // original response body (may be truncated)
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rosetta: %s %s -> %d", e.Method, e.URL, e.StatusCode)
	if e.Type != "" {
		b.WriteString(" (" + e.Type)
		if e.Code != "" {
			b.WriteString("/" + e.Code)
		}
		b.WriteString(")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// transport wraps a network error with request context.
func transport(err error, method, url string) error {
	return &TransportError{Method: method, URL: url, Err: err}
}

// retryableStatus reports whether an HTTP status is worth retrying. The
// list lives in the transport layer; this is a thin alias so error typing
// and retry decisions can never drift apart.
func retryableStatus(code int) bool {
	return httpx.RetryableStatus(code)
}

// truncateBody bounds a successfully-decoded response body for storage in
// Raw, backing off to a rune boundary so the result stays valid UTF-8.
// The input is known-valid JSON (callers only reach it after a successful
// Unmarshal), so no validity scan is performed: bodies at or under the
// cap pass through untouched, larger ones are truncated and — since
// truncation breaks JSON validity — stored as a JSON string so Raw never
// breaks re-marshaling.
func truncateBody(body []byte) json.RawMessage {
	const max = 4 << 10
	if len(body) <= max {
		return json.RawMessage(body)
	}
	b := body[:max]
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	s, _ := json.Marshal(string(b))
	return json.RawMessage(s)
}

// safeTruncateBody wraps response bodies of unknown provenance (error
// pages, gateway HTML) for storage in Raw: structured bodies get sensitive
// values redacted before storage — error bodies sometimes echo the
// caller's API key, auth header or prompt, and Raw travels into logs and
// telemetry — and anything that is not valid JSON is degraded to a JSON
// string. Redaction runs BEFORE truncation: once an oversized body is
// degraded to a JSON string by truncateBody, structured redaction can no
// longer reach the sensitive keys inside it.
func safeTruncateBody(body []byte) json.RawMessage {
	if json.Valid(body) {
		return truncateBody(redactJSON(body))
	}
	// Mask key material on the full body, then truncate and degrade to a
	// JSON string.
	raw := truncateBody(apiKeyRe.ReplaceAll(body, []byte("sk-***")))
	if !json.Valid(raw) { // short non-JSON body: degrade to a JSON string
		s, _ := json.Marshal(string(raw))
		return json.RawMessage(s)
	}
	return raw // truncateBody already degraded an oversized body to a string
}

// apiKeyRe matches OpenAI-style key material echoed inside error messages.
var apiKeyRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)

// redactJSON masks sensitive values in a valid-JSON body while keeping it
// valid JSON: sensitive-keyed string values become "[redacted]" and
// key-material patterns are masked everywhere. Best effort — on any
// structural surprise the original (truncated) body is returned.
func redactJSON(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return apiKeyRe.ReplaceAll(out, []byte("sk-***"))
}

func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if _, ok := val.(string); ok && isSensitiveKey(k) {
				t[k] = "[redacted]"
				continue
			}
			redactValue(val)
		}
	case []any:
		for _, item := range t {
			redactValue(item)
		}
	}
}

func isSensitiveKey(k string) bool {
	low := strings.ToLower(k)
	low = strings.ReplaceAll(low, "-", "")
	low = strings.ReplaceAll(low, "_", "")
	switch {
	case strings.Contains(low, "apikey"), strings.Contains(low, "secret"),
		strings.Contains(low, "token"), strings.Contains(low, "password"),
		strings.Contains(low, "authorization"), strings.Contains(low, "credential"):
		return true
	}
	return false
}
