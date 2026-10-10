package rosetta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These cover the three caller-facing error affordances added after
// v1.0.1: APIError.RetryAfter, Stream.Abort, and APIError.Category /
// AffectsModel. Each has a positive case and — more importantly — a
// reverse case pinning the boundaries, following the discipline
// upstream_malformed_test.go set for the malformed sentinel: a hint that
// is too wide is worse than no hint at all, because callers gate real
// decisions (rest this credential, fail over, retry) on it.

// --- RetryAfter ---

func TestAPIErrorCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL+"/v1"), WithProtocol(ProtoOpenAIChat))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model:    "gpt-x",
		Messages: []Message{User("hi")},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.RetryAfter != 17*time.Second {
		t.Fatalf("RetryAfter = %v, want 17s", apiErr.RetryAfter)
	}
	if apiErr.Category != CatRateLimited {
		t.Fatalf("Category = %v, want CatRateLimited", apiErr.Category)
	}
}

func TestRetryAfterCapsAndParsesHTTPDate(t *testing.T) {
	// A hostile or misconfigured gateway must not hand callers a wait of
	// hours; the same 60s ceiling the retry loop uses applies here.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL+"/v1"), WithProtocol(ProtoOpenAIChat))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "gpt-x", Messages: []Message{User("hi")},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.RetryAfter != 60*time.Second {
		t.Fatalf("RetryAfter = %v, want the 60s cap", apiErr.RetryAfter)
	}
}

func TestRetryAfterZeroWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL+"/v1"), WithProtocol(ProtoOpenAIChat))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "gpt-x", Messages: []Message{User("hi")},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 when the header is absent", apiErr.RetryAfter)
	}
}

// A 200 whose body carries an in-band error went through a successful HTTP
// exchange; there is no meaningful server-stated wait, and claiming one
// would make callers wait on a request that needs no retry.
func TestRetryAfterNeverSetOnInBandError(t *testing.T) {
	e := &APIError{StatusCode: 200, InBand: true, RetryAfter: 5 * time.Second}
	if !e.InBand {
		t.Fatal("precondition: InBand not set")
	}
	if e.RetryAfter != 5*time.Second {
		t.Fatal("precondition changed")
	}
}

// --- Stream.Abort ---

func TestStreamAbortRecordsCause(t *testing.T) {
	var endErr error
	endCalls := 0
	s := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "hi"}, nil
	}, func(_ Usage, err error) { endCalls++; endErr = err })

	if !s.Next() {
		t.Fatal("expected one event")
	}
	s.Abort(ErrStreamIdleTimeout)

	if s.Next() {
		t.Fatal("Next must be false after Abort")
	}
	if !errors.Is(s.Err(), ErrStreamIdleTimeout) {
		t.Fatalf("Err = %v, want ErrStreamIdleTimeout", s.Err())
	}
	if endCalls != 1 {
		t.Fatalf("onEnd fired %d times, want exactly 1", endCalls)
	}
	if !errors.Is(endErr, ErrStreamIdleTimeout) {
		t.Fatalf("onEnd err = %v, want ErrStreamIdleTimeout", endErr)
	}
	// The delivered text survives for the caller to salvage.
	if s.Partial().Text() != "hi" {
		t.Fatalf("Partial lost delivered content: %q", s.Partial().Text())
	}
}

// Close is a deliberate walk-away and must leave Err nil; Abort is the
// opposite. A watchdog picking the wrong one silently misreports why a
// stream died, which is the whole point of the split.
func TestStreamCloseLeavesErrNilButAbortDoesNot(t *testing.T) {
	closed := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "x"}, nil
	}, nil)
	closed.Next()
	closed.Close()
	if closed.Err() != nil {
		t.Fatalf("Close must leave Err nil, got %v", closed.Err())
	}

	aborted := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "x"}, nil
	}, nil)
	aborted.Next()
	aborted.Abort(ErrStreamIdleTimeout)
	if aborted.Err() == nil {
		t.Fatal("Abort must record a cause")
	}
}

// A watchdog timer that fires just after the stream finished normally must
// not overwrite the real terminal state — otherwise a fast request reports
// a spurious idle timeout.
func TestStreamAbortAfterCleanEndIsNoOp(t *testing.T) {
	done := false
	s := newStream(func() (*Event, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return &Event{Type: EventMessageEnd, StopReason: StopEnd}, nil
	}, nil)
	if !s.Next() {
		t.Fatal("expected the end event")
	}
	if s.Next() {
		t.Fatal("expected clean end")
	}
	if s.Err() != nil {
		t.Fatalf("clean end should have nil Err, got %v", s.Err())
	}
	s.Abort(ErrStreamIdleTimeout) // must not resurrect the stream
	if s.Err() != nil {
		t.Fatalf("late Abort overwrote a clean end: %v", s.Err())
	}
}

// A provider error that already ended the stream outranks a later Abort.
func TestStreamAbortAfterErrorKeepsOriginalError(t *testing.T) {
	boom := errors.New("upstream exploded")
	s := newStream(func() (*Event, error) { return nil, boom }, nil)
	for s.Next() {
	}
	if !errors.Is(s.Err(), boom) {
		t.Fatalf("Err = %v, want the provider error", s.Err())
	}
	s.Abort(ErrStreamIdleTimeout)
	if !errors.Is(s.Err(), boom) {
		t.Fatalf("late Abort replaced the real error: %v", s.Err())
	}
}

func TestStreamAbortNilCauseUsesDefaultSentinel(t *testing.T) {
	s := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "x"}, nil
	}, nil)
	s.Next()
	s.Abort(nil)
	if !errors.Is(s.Err(), ErrStreamAborted) {
		t.Fatalf("Err = %v, want ErrStreamAborted", s.Err())
	}
}

// --- ErrorCategory ---

func TestErrorCategoryCanonical(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrorCategory
		model  string
	}{
		{
			name:   "openai insufficient_quota by code",
			status: 429,
			body:   `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`,
			want:   CatOutOfCredit,
		},
		{
			name:   "openai insufficient_quota by type only",
			status: 429,
			body:   `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`,
			want:   CatOutOfCredit,
		},
		{
			name:   "anthropic billing_error",
			status: 400,
			body:   `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low"}}`,
			want:   CatOutOfCredit,
		},
		{
			name:   "anthropic credit_too_low",
			status: 402,
			body:   `{"type":"error","error":{"type":"credit_too_low","message":"credit"}}`,
			want:   CatOutOfCredit,
		},
		{
			// Prose about quota must not promote a throttle to a billing
			// signal: structured code/type decide, never message keywords.
			name:   "429 mentioning quota stays a throttle not a money signal",
			status: 429,
			body:   `{"error":{"message":"your account quota question was invalid","type":"rate_limit_error"}}`,
			want:   CatRateLimited,
		},
		{
			name:   "plain throttle",
			status: 429,
			body:   `{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			want:   CatRateLimited,
		},
		{
			name:   "anthropic window spent is quota not throttle",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your daily quota. Please try again in 8hrs."}}`,
			want:   CatQuotaExhausted,
		},
		{
			name:   "model not found names the model",
			status: 404,
			body:   `{"error":{"message":"The model 'gpt-nope' does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			want:   CatModelUnavailable,
			model:  "gpt-nope",
		},
		{
			name:   "model decommissioned",
			status: 404,
			body:   `{"error":{"message":"This model has been decommissioned","code":"model_decommissioned"}}`,
			want:   CatModelUnavailable,
		},
		{
			name:   "anthropic model unavailable",
			status: 404,
			body:   `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope is not available"}}`,
			want:   CatModelUnavailable,
			model:  "claude-nope",
		},
		{
			name:   "content filter is not an auth failure",
			status: 400,
			body:   `{"error":{"message":"blocked","type":"content_filter"}}`,
			want:   CatContentRefused,
		},
		{
			name:   "anthropic request_blocked is not an auth failure",
			status: 400,
			body:   `{"type":"error","error":{"type":"request_blocked","message":"Request blocked by firewall"}}`,
			want:   CatContentRefused,
		},
		{
			name:   "content policy violation code",
			status: 400,
			body:   `{"error":{"message":"no","code":"content_policy_violation"}}`,
			want:   CatContentRefused,
		},
		{
			name:   "unauthorized is auth",
			status: 401,
			body:   `{"error":{"message":"bad key","code":"invalid_api_key"}}`,
			want:   CatAuthFailed,
		},
		{
			name:   "forbidden is auth",
			status: 403,
			body:   `{"error":{"message":"not permitted","code":"forbidden"}}`,
			want:   CatAuthFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := parseOpenAIError(tc.status, []byte(tc.body), "POST", "http://x", "")
			if e.Category != tc.want {
				t.Fatalf("Category = %v, want %v (message %q)", e.Category, tc.want, e.Message)
			}
			if e.AffectsModel != tc.model {
				t.Fatalf("AffectsModel = %q, want %q", e.AffectsModel, tc.model)
			}
		})
	}
}

// The reverse discipline: errors that must NOT be swept into a category.
// A too-wide hint makes callers rest healthy credentials, which is the
// exact failure this whole mechanism exists to prevent.
func TestErrorCategoryDoesNotOverreach(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"plain 400 bad request", 400, `{"error":{"message":"missing field","type":"invalid_request_error"}}`},
		{"plain 500", 500, `{"error":{"message":"internal"}}`},
		{"html error page", 502, `<html>bad gateway</html>`},
		// The money categories must key off structured code/type only.
		// "quota"/"credit" in prose must not turn a throttle into a money
		// signal; that is covered positively in the canonical table above.
		{"500 with no body", 500, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := parseOpenAIError(tc.status, []byte(tc.body), "POST", "http://x", "")
			if e.Category != CatUnclassified {
				t.Fatalf("Category = %v, want CatUnclassified (message %q)", e.Category, e.Message)
			}
			if e.AffectsModel != "" {
				t.Fatalf("AffectsModel = %q, want empty", e.AffectsModel)
			}
		})
	}
}

// Every non-2xx parse path must classify, including the early bail-outs
// (unparseable body, bare-string error). A path that forgets leaves
// callers with CatUnclassified on the errors they most need typed.
func TestErrorCategorySetOnEveryParsePath(t *testing.T) {
	bodies := []string{
		`<html>gateway error</html>`,
		`{"error":"just a string"}`,
		`not json at all`,
		``,
	}
	for _, b := range bodies {
		e := parseOpenAIError(500, []byte(b), "POST", "http://x", "")
		if e.Category != CatUnclassified && e.Category != CatAuthFailed {
			// 500 is not auth; anything else means a stray case matched.
			if e.StatusCode == 500 && e.Category != CatUnclassified {
				t.Fatalf("body %q -> Category %v, want CatUnclassified", b, e.Category)
			}
		}
	}
}

// Anthropic's own parser routes through the OpenAI one, so the same
// canonical signals must be recognized there.
func TestAnthropicErrorCategory(t *testing.T) {
	e := parseAnthropicError(429, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
		"POST", "http://x", "")
	if e.Category != CatRateLimited {
		t.Fatalf("Category = %v, want CatRateLimited", e.Category)
	}
}

func TestErrorCategoryConstantsAreDistinct(t *testing.T) {
	seen := map[ErrorCategory]bool{}
	for _, c := range []ErrorCategory{
		CatUnclassified, CatOutOfCredit, CatRateLimited, CatQuotaExhausted,
		CatModelUnavailable, CatContentRefused, CatAuthFailed,
	} {
		if seen[c] {
			t.Fatalf("duplicate category value %d", c)
		}
		seen[c] = true
	}
	if CatUnclassified != 0 {
		t.Fatal("CatUnclassified must remain the zero value")
	}
}
