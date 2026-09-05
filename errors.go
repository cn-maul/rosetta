package rosetta

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel errors returned by the SDK. Use errors.Is to match.
var (
	// ErrNoEndpoint is returned when no endpoint could be determined.
	ErrNoEndpoint = errors.New("rosetta: endpoint is required (WithEndpoint/WithVendor)")
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
	// ErrInvalidRequest is returned for structurally invalid ChatRequests.
	ErrInvalidRequest = errors.New("rosetta: invalid request")
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
	Retryable  bool
	Raw        json.RawMessage // original response body (may be truncated)
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

// retryableStatus reports whether an HTTP status is worth retrying.
// 529 is Anthropic's non-standard "overloaded" status.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// truncateBody bounds an error body stored in APIError.Raw.
func truncateBody(body []byte) json.RawMessage {
	const max = 4 << 10
	if len(body) > max {
		return json.RawMessage(body[:max])
	}
	return json.RawMessage(body)
}

// genericAPIError builds an APIError for a body that could not be parsed.
func genericAPIError(status int, method, url, body string) *APIError {
	msg := strings.TrimSpace(body)
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{
		StatusCode: status,
		Message:    msg,
		Method:     method,
		URL:        url,
		Retryable:  retryableStatus(status),
	}
}
