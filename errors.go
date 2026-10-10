package rosetta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
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
	ErrStreamTruncated = errors.New("rosetta: stream truncated")
	// ErrStreamOverflow is returned when a single stream's accumulated
	// content exceeds the safety caps (total bytes or distinct blocks), which
	// bounds memory against a hostile or buggy provider. The partial response
	// stays available via Stream.Partial; match with errors.Is.
	ErrStreamOverflow = errors.New("rosetta: stream accumulation exceeded safety limits")
	// ErrUpstreamMalformed is returned when the upstream answered with a
	// success status (or a 200-equivalent) but its response body could not be
	// decoded into the shape the protocol requires — a truncated body, an HTML
	// error page from an intermediary, a half-written JSON document.
	//
	// # Why this needs to be a distinct sentinel
	//
	// The direction of blame decides the caller's action, and for this failure
	// the two directions are opposite:
	//
	//   - ErrInvalidRequest means *the caller* built something the SDK or the
	//     provider rejected. Retrying it unchanged fails identically, and
	//     blaming the provider would hide the caller's bug.
	//   - ErrUpstreamMalformed means *the upstream* produced an unusable
	//     answer to a request that was fine. The caller's only useful moves are
	//     to retry elsewhere (failover), back off, or surface the fault — and
	//     none of them is "fix your request".
	//
	// Before this sentinel existed these failures were wrapped in a bare
	// fmt.Errorf, so no caller could classify them: a gateway's error mapper
	// found no sentinel, no *APIError and no *TransportError, fell through to
	// its "internal gateway error" bucket, and returned 500 while also
	// declining to fail over — misreporting an upstream fault as the gateway's
	// own and leaving the request pinned to the broken upstream.
	//
	// # Scope: this covers unary response decoding, NOT mid-stream event parsing
	//
	// Deliberately not attached to stream-event decode failures (see
	// provider_openai_chat.go / provider_anthropic.go /
	// provider_openai_responses.go, each marked "NOT ErrUpstreamMalformed").
	// A stream error may arrive after content was already delivered, and
	// "retry the upstream" is then actively harmful — it duplicates output and
	// double-charges. Because one sentinel cannot express "safe to retry" for
	// a failure whose safety depends on how far the caller got, the streams
	// keep an untyped error and callers gate on their own committed state.
	// Tagging them here would turn the natural
	// `errors.Is(err, ErrUpstreamMalformed) -> failover` mapping into a
	// correctness bug.
	//
	// The original decode error is preserved with %w so errors.As can still
	// reach the underlying *json.SyntaxError / *json.UnmarshalTypeError for
	// diagnostics. Those carry an offset and at most a field name — never the
	// body itself — so wrapping them does not put response content into logs;
	// the malformed bytes are deliberately not attached.
	ErrUpstreamMalformed = errors.New("rosetta: upstream response could not be decoded")
	// ErrStreamIdleTimeout is the recommended cause for Stream.Abort when a
	// watchdog aborts a stream that delivered no event within its idle
	// window. The SDK never raises it itself — WithTimeout is unary-only
	// and an established stream is governed by the caller — so matching it
	// always means a caller-side watchdog fired. The partial response
	// stays available via Stream.Partial.
	ErrStreamIdleTimeout = errors.New("rosetta: stream idle timeout (caller watchdog)")
	// ErrStreamAborted is the default cause used when Stream.Abort is
	// called with a nil error. Matching it means the caller aborted the
	// stream for an unspecified reason; prefer passing an explicit cause
	// (e.g. ErrStreamIdleTimeout) so downstream code can distinguish.
	ErrStreamAborted = errors.New("rosetta: stream aborted by caller")
)

// TransportError wraps a lower-level network failure (DNS, connect, TLS,
// read) with the request context it occurred in.
type TransportError struct {
	Method string
	URL    string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("rosetta: transport error during %s %s: %v", e.Method, safeURL(e.URL), e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// APIError is a structured error returned by the remote API. It is produced
// for every non-2xx response whose body could be interpreted.
type APIError struct {
	StatusCode int
	Code       string // provider-specific error code, when present
	Type       string // provider-specific error type, when present
	Param      string // offending request parameter, when the provider names it
	Message    string
	RequestID  string // from X-Request-Id / request-id headers
	Method     string
	URL        string
	// Retryable marks statuses worth retrying (408/429/5xx/529). It is a
	// hint for callers: the SDK auto-retries only when the request's retry
	// policy allows it (GET-like methods by default; non-idempotent POSTs
	// like chat require an explicit policy).
	Retryable bool
	// RetryAfter is the server-stated wait, parsed from the Retry-After
	// response header (delay-seconds or HTTP-date) and capped at 60s.
	// Zero means the server gave none (or the value was unparsable).
	//
	// It is a hint for callers — cooldown scheduling, header pass-through,
	// backoff elsewhere — not an instruction: the SDK's own retry loop has
	// already honored whatever it is going to honor, and this field is set
	// even on the final error a caller receives after retries ran out.
	// Only status-code failures from the transport carry it; in-band (200)
	// errors never do, since the HTTP layer succeeded.
	RetryAfter time.Duration
	// InBand marks an error that arrived inside a 200 response body or a
	// stream event rather than as a non-2xx HTTP status. Such errors carry
	// StatusCode=200 (the transport succeeded) but are real failures; callers
	// that gate on StatusCode >= 400 must also check InBand (G11).
	InBand bool
	// Category is a coarse attribution hint derived from the status code
	// and the provider's structured error fields (code/type). It tells a
	// caller which *thing* failed — the account's money, the account's
	// quota window, the rate limit, the model's availability, or content
	// policy — so decisions like "rest this credential" or "fail over to
	// another model" don't have to re-parse provider bodies.
	//
	// It is advisory: CatUnclassified means "no known canonical signal
	// matched", not "none of the above applies". Callers must treat it as
	// a hint layered on StatusCode, never as a replacement for it.
	Category ErrorCategory
	// AffectsModel is the model the provider explicitly named as the
	// reason for the failure (empty when it didn't). Set for model-level
	// failures — "model not found", "model not available in your plan",
	// "model decommissioned" — so a caller can stop offering that model on
	// this credential without resting the credential itself for its other
	// models.
	AffectsModel string
	Raw          json.RawMessage // original response body (may be truncated)
}

// ErrorCategory is a coarse attribution hint for an APIError: which kind of
// thing the provider said failed. Values are matched against canonical
// protocol signals only — documented status codes and the structured
// code/type fields of OpenAI and Anthropic error bodies. Deliberately NOT
// matched: free-text message keywords (English or otherwise). A message
// wordlist is unbounded vendor-private surface that drifts with every
// provider's rewording; a gateway that needs it can layer its own hints on
// top of Raw, which is always available.
type ErrorCategory int8

const (
	// CatUnclassified means no canonical signal matched. It is the zero
	// value and must stay first.
	CatUnclassified ErrorCategory = iota
	// CatOutOfCredit: the account has no money. OpenAI 429 with
	// code "insufficient_quota"; Anthropic 400 type "billing_error"
	// (its docs say "you've hit your maximum spend") and 402 credit_too_low.
	// Not retryable against the same account until it is topped up.
	CatOutOfCredit
	// CatRateLimited: the request was throttled. 429 without the
	// insufficient_quota code; Retry-After often carries the wait.
	// Retryable — possibly against a different credential.
	CatRateLimited
	// CatQuotaExhausted: a subscription usage window is spent (distinct
	// from pay-as-you-go credit). Anthropic 429 type "rate_limit_error"
	// whose message names a daily/weekly/monthly/billing window reset.
	// Recovers when the window resets.
	CatQuotaExhausted
	// CatModelUnavailable: the model itself cannot be served on this
	// account/endpoint — not found, not in the plan, or decommissioned.
	// OpenAI 404 code "model_not_found" / "model_decommissioned"; Anthropic
	// 404 type "not_found_error" naming the model. AffectsModel is set
	// when the provider names one. The credential's other models are fine.
	CatModelUnavailable
	// CatContentRefused: the request content was refused (safety system,
	// prompt injection filter, moderation). OpenAI 400 type
	// "content_filter" / code "content_policy_violation"; Anthropic 400
	// type "request_blocked" (its web firewall) — the account is innocent:
	// retrying the same prompt on another credential fails the same way.
	CatContentRefused
	// CatAuthFailed: the credential itself was rejected. 401, or 403 that
	// is not a content refusal. The key is bad, expired or lacks access;
	// no other request will succeed with it until it is fixed.
	CatAuthFailed
)

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rosetta: %s %s -> %d", e.Method, safeURL(e.URL), e.StatusCode)
	// Type/Code/Message are taken verbatim from the provider body, which can
	// echo the caller's key or prompt; mask credential patterns before the
	// string reaches any log.
	if t := httpx.MaskSecrets(e.Type); t != "" {
		b.WriteString(" (" + t)
		if c := httpx.MaskSecrets(e.Code); c != "" {
			b.WriteString("/" + c)
		}
		b.WriteString(")")
	}
	if m := httpx.MaskSecrets(e.Message); m != "" {
		b.WriteString(": " + m)
	}
	return b.String()
}

// withRetryAfter fills an APIError's RetryAfter from the response headers.
// Applied at every non-2xx return so the server-stated wait reaches the
// caller uniformly across protocols and call kinds. Nil or empty headers
// leave it zero.
func withRetryAfter(e *APIError, h http.Header) *APIError {
	if e == nil {
		return nil
	}
	e.RetryAfter = httpx.RetryAfterOf(h)
	return e
}

// classify fills Category and AffectsModel from the status code and the
// structured code/type fields already parsed onto e. Canonical signals
// only — see ErrorCategory for why message keywords are deliberately not
// consulted (except the one Anthropic rate_limit_error case where the
// canonical type is shared between "slow down" and "window spent" and the
// distinction is only in the message's reset-time phrasing, which is
// itself documented API surface: "Please try again in 1hr" style window
// resets).
//
// Called at the end of every error-body parse, so classification is
// uniform across the three protocols and the auxiliary APIs.
func (e *APIError) classify() {
	if e == nil {
		return
	}
	code, typ := e.Code, e.Type
	switch {
	case e.StatusCode == http.StatusUnauthorized:
		e.Category = CatAuthFailed
	case e.StatusCode == http.StatusForbidden && typ != "request_blocked":
		e.Category = CatAuthFailed

	// Money: OpenAI's documented insufficient_quota (billing hard limit),
	// Anthropic's billing_error / credit_too_low.
	//
	// Either field alone is enough: OpenAI sends code "insufficient_quota"
	// on some deployments and type "insufficient_quota" on others, and
	// third-party relays pick one or the other. Requiring both would miss
	// the single most important case a caller needs to get right.
	case e.StatusCode == 402 && (code == "credit_too_low" || typ == "credit_too_low" || typ == "billing_error"):
		e.Category = CatOutOfCredit
	case code == "insufficient_quota" || typ == "insufficient_quota":
		e.Category = CatOutOfCredit
	case typ == "billing_error":
		e.Category = CatOutOfCredit

	// Throttling: 429s that are not the credit case. Anthropic's
	// rate_limit_error covers both "too fast" and "daily/weekly window
	// spent"; the window case says a reset time, which is the documented
	// distinction between the two.
	case e.StatusCode == http.StatusTooManyRequests:
		if typ == "rate_limit_error" && windowResetPhrases.MatchString(e.Message) {
			e.Category = CatQuotaExhausted
		} else {
			e.Category = CatRateLimited
		}

	// Model-level unavailability. OpenAI's codes; Anthropic's 404 names
	// the model in its message ("model: <id> is not available"), which is
	// documented behavior, so the model id is recoverable from there.
	case e.StatusCode == http.StatusNotFound && (code == "model_not_found" || code == "model_decommissioned" || typ == "not_found_error"):
		e.Category = CatModelUnavailable
		e.AffectsModel = e.namedModel()

	// Content policy. request_blocked is Anthropic's firewall, not a
	// permission failure — the account is fine.
	case typ == "request_blocked", typ == "content_filter", code == "content_policy_violation":
		e.Category = CatContentRefused
	}
}

// windowResetPhrases distinguishes Anthropic's "usage window spent" 429s
// from its "slow down" 429s. Both arrive as type rate_limit_error; the
// window case carries a reset deadline in the message, per Anthropic's
// documented error shapes ("Please try again in 8h", "...until 3pm
// Monday", "resets at 9am Pacific"). Time-unit and reset phrasings only —
// no vendor names or private wording.
var windowResetPhrases = regexp.MustCompile(
	`(?i)\b(\d+\s*(h|hr|hrs|hour|hours|m|min|mins|minute|minutes|d|day|days|week|weeks)\b|until\s+\S+|resets?\b|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)

// namedModel extracts the model id the provider named in the message.
// Anthropic's 404 says "model: <id> is not available..."; OpenAI's
// model_not_found says "The model '<id>' does not exist". Both quoted and
// colon forms are canonical document shapes.
func (e *APIError) namedModel() string {
	if m := quotedModelRe.FindStringSubmatch(e.Message); len(m) > 1 {
		return m[1]
	}
	if m := colonModelRe.FindStringSubmatch(e.Message); len(m) > 1 {
		return m[1]
	}
	return ""
}

var (
	quotedModelRe = regexp.MustCompile(`(?i)\bmodel\s+'([^']+)'`)
	colonModelRe  = regexp.MustCompile(`(?i)\bmodel:\s*(\S+)`)
)

// transport wraps a network error with request context.
func transport(err error, method, url string) error {
	return &TransportError{Method: method, URL: url, Err: err}
}

// attachRequest fills an APIError's request context from a
// (method, url, requestID) triple, taken as loose strings so the unary
// decoders can accept it optionally without breaking zero-arg callers.
func attachRequest(e *APIError, rc []string) {
	if e == nil {
		return
	}
	if len(rc) > 0 {
		e.Method = rc[0]
	}
	if len(rc) > 1 {
		e.URL = rc[1]
	}
	if len(rc) > 2 {
		e.RequestID = rc[2]
	}
}

// Response body caps, unified across adapters. A single tight cap rejects
// legitimate large payloads; these bound a runaway gateway per category.
const (
	// bodyLimit bounds a unary completion, error, or /models catalog body.
	// 8MiB accommodates long completions and large tool-call arguments that
	// a 1MiB cap wrongly rejected while still capping a misbehaving server.
	bodyLimit = 8 << 20
	// auxBodyLimit bounds embedding/rerank result bodies, which are large by
	// design (one vector per input).
	auxBodyLimit = 64 << 20
)

// readBody reads a response body up to limit and closes it. An over-limit
// body becomes a distinct size error rather than a transport failure, so a
// caller can tell "the gateway streamed an oversized response" apart from a
// network break; errors.Is against httpx.ErrBodyTooLarge still matches.
func readBody(resp *http.Response, method, url string, limit int64) ([]byte, error) {
	body, err := httpx.ReadBody(resp.Body, limit)
	resp.Body.Close()
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			return nil, fmt.Errorf("rosetta: %s %s response body exceeds %d bytes: %w",
				method, displayEndpoint(url), limit, err)
		}
		return nil, transport(err, method, url)
	}
	return body, nil
}

// bufferedJSONResponse reports whether a 2xx stream response carries a
// buffered JSON body rather than an event stream: the Content-Type is
// present but is not text/event-stream. An absent Content-Type is treated as
// (legacy) SSE, so streams from servers that omit the header are not
// misrouted. A non-event-stream 2xx to a stream:true request holds either a
// complete completion or an error the SSE scanner would silently drop
// (audit B3).
func bufferedJSONResponse(contentType string) bool {
	if contentType == "" {
		return false
	}
	return !strings.Contains(strings.ToLower(contentType), "text/event-stream")
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
	raw := truncateBody(httpx.MaskSecretBytes(body))
	if !json.Valid(raw) { // short non-JSON body: degrade to a JSON string
		s, _ := json.Marshal(string(raw))
		return json.RawMessage(s)
	}
	return raw // truncateBody already degraded an oversized body to a string
}

// safeURL renders a request URL for an error string: displayEndpoint strips
// userinfo/query/fragment, and httpx.MaskSecrets catches path-embedded
// credentials (e.g. a proxy routing key as …/proxy/sk-ant-…) that survive
// normalization (audit C9). The credential regexp lives in httpx so the
// transport's log redaction and this package's error redaction share one
// definition.
func safeURL(u string) string { return httpx.MaskSecrets(displayEndpoint(u)) }

// redactJSON masks sensitive values in a valid-JSON body while keeping it
// valid JSON: sensitive-keyed values become "[redacted]" and key-material
// patterns are masked everywhere. It decodes numbers with UseNumber so large
// integers and unusual exponents round-trip byte-for-byte instead of being
// corrupted through float64. On any decode/marshal surprise it still masks
// credential patterns over the original bytes rather than returning them
// untouched — a value that json.Valid accepts but the decoder rejects (an
// out-of-range number literal) must never switch redaction off.
func redactJSON(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return httpx.MaskSecretBytes(raw)
	}
	redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return httpx.MaskSecretBytes(raw)
	}
	return httpx.MaskSecretBytes(out)
}

func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			// Redact any value type under a sensitive key: credentials
			// smuggled as a number or nested object must not survive.
			if isSensitiveKey(k) {
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
	case strings.Contains(low, "key"), strings.Contains(low, "secret"),
		strings.Contains(low, "token"), strings.Contains(low, "password"),
		strings.Contains(low, "authorization"), strings.Contains(low, "credential"),
		strings.Contains(low, "bearer"), strings.Contains(low, "access"),
		strings.Contains(low, "session"), strings.Contains(low, "cookie"):
		return true
	}
	return false
}
