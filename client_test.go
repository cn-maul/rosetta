package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestChatRequestValidation(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil request err = %v", err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty request err = %v", err)
	}
	if _, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty messages err = %v", err)
	}
}

func TestGateThinking(t *testing.T) {
	mi := ModelInfo{ID: "m1", Known: true, SupportsThinking: false}
	c := newTestClient(t, WithModelInfo(mi))
	req := &ChatRequest{Model: "m1", Messages: []Message{User("hi")}, Thinking: &ThinkingConfig{Effort: EffortHigh}}

	if _, err := c.prepare(req); !errors.Is(err, ErrThinkingUnsupported) {
		t.Fatalf("err = %v, want ErrThinkingUnsupported", err)
	}

	// Unknown models are not judged (no guessing for custom models).
	req2 := &ChatRequest{Model: "custom", Messages: []Message{User("hi")}, Thinking: &ThinkingConfig{Effort: EffortHigh}}
	if _, err := c.prepare(req2); err != nil {
		t.Fatalf("unknown model must pass: %v", err)
	}

	// Thinking-capable models pass.
	c2 := newTestClient(t, WithModelInfo(ModelInfo{ID: "m1", Known: true, SupportsThinking: true}))
	if _, err := c2.prepare(req); err != nil {
		t.Fatalf("capable model must pass: %v", err)
	}

	// Fallback mode degrades silently and does not mutate the caller's
	// request.
	c3 := newTestClient(t, WithModelInfo(mi), WithThinkingFallback(true))
	gated, err := c3.prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	if gated.Thinking != nil {
		t.Fatal("thinking must be dropped in fallback mode")
	}
	if req.Thinking == nil {
		t.Fatal("caller's request must not be mutated")
	}
}

func TestCheckContext(t *testing.T) {
	mi := ModelInfo{ID: "m1", Known: true, ContextWindow: 20}
	long := &ChatRequest{Model: "m1", Messages: []Message{User(strings.Repeat("a", 400))}, MaxOutputTokens: 100}

	c := newTestClient(t, WithModelInfo(mi)) // default: warn only
	if _, err := c.prepare(long); err != nil {
		t.Fatalf("default must warn, not fail: %v", err)
	}

	c2 := newTestClient(t, WithModelInfo(mi), WithStrictContextCheck(true))
	if _, err := c2.prepare(long); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("strict mode err = %v, want ErrContextTooLong", err)
	}

	// Within the window passes.
	small := &ChatRequest{Model: "m1", Messages: []Message{User("hi")}, MaxOutputTokens: 5}
	if _, err := c2.prepare(small); err != nil {
		t.Fatalf("small request err = %v", err)
	}
}

// WithModelsFile and WithModelInfo must merge into one manual layer —
// the file must not silently overwrite the code-level entries.
func TestModelsFileAndManualMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{"models":[{"id":"from-file","context_window":1000}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t,
		WithModelsFile(path),
		WithModelInfo(ModelInfo{ID: "from-code", ContextWindow: 2000}),
	)
	for _, want := range []string{"from-file", "from-code"} {
		if _, ok := c.registry.Lookup(want); !ok {
			t.Fatalf("model %q missing after merge", want)
		}
	}
}

// Full unary round trip against an in-memory server, inside a synctest
// bubble (no real network, no real waits).
func TestChatE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer k" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","model":"gpt-4o","choices":[{"message":{"content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`)
		}))
		tr := NewMemoryUsageTracker()
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithUsageTracker(tr))
		resp, err := c.Chat(context.Background(), &ChatRequest{
			Model:    "gpt-4o",
			Messages: []Message{User("hi")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text() != "hello there" || resp.StopReason != StopEnd {
			t.Fatalf("resp = %+v", resp)
		}
		if resp.Usage.TotalTokens != 12 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
		snap := tr.Snapshot()
		if snap.TotalRequests != 1 || snap.TotalTokens != 12 || snap.ByModel["gpt-4o"].Requests != 1 {
			t.Fatalf("stats = %+v", snap)
		}
	})
}

// Full streaming round trip; usage is recorded when the stream ends and
// no goroutine is left behind.
func TestChatStreamE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"st\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"ream\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n"+
				"data: [DONE]\n\n")
		}))
		tr := NewMemoryUsageTracker()
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithUsageTracker(tr))
		base := runtime.NumGoroutine()

		stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()

		var sb strings.Builder
		for stream.Next() {
			if ev := stream.Event(); ev.Type == EventTextDelta {
				sb.WriteString(ev.Text)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if sb.String() != "stream" {
			t.Fatalf("text = %q", sb.String())
		}
		if u := stream.Usage(); u.TotalTokens != 7 {
			t.Fatalf("usage = %+v", u)
		}
		if p := stream.Partial(); p.StopReason != StopEnd {
			t.Fatalf("partial stop = %s", p.StopReason)
		}

		// Usage reaches the tracker at end-of-stream.
		snap := tr.Snapshot()
		if snap.TotalRequests != 1 || snap.TotalTokens != 7 || snap.UsageMissing != 0 {
			t.Fatalf("stats = %+v", snap)
		}

		stream.Close()
		srv.Close() // drain server and pooled connections
		synctest.Wait()
		if now := runtime.NumGoroutine(); now > base {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d before, %d after\n%s", base, now, buf[:n])
		}
	})
}

// A 400 that matches a known optional-field rejection triggers exactly
// one sanitizing retry, and the downgrade sticks for later requests.
func TestChatSanitizeRetryE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts int
		var bodies []string
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(body))
			attempts++
			if attempts == 1 {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"reasoning_effort is unrecognized","type":"invalid_request_error"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()))
		req := &ChatRequest{
			Model:    "m",
			Messages: []Message{User("hi")},
			Thinking: &ThinkingConfig{Effort: EffortHigh},
		}
		resp, err := c.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text() != "ok" || attempts != 2 {
			t.Fatalf("resp=%+v attempts=%d", resp, attempts)
		}
		if strings.Contains(bodies[0], "reasoning_effort") == false || strings.Contains(bodies[1], "reasoning_effort") {
			t.Fatalf("sanitize retry did not drop the field: %v", bodies)
		}

		// Sticky: the next request starts without reasoning_effort and
		// succeeds on the first attempt (no extra 400 round trip).
		before := len(bodies)
		if _, err := c.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(bodies) != before+1 || strings.Contains(bodies[before], "reasoning_effort") {
			t.Fatalf("sticky state lost: bodies=%v", bodies[before:])
		}
	})
}

// Anthropic /models pagination: every page is fetched via after_id until
// has_more is false.
func TestAnthropicListModelsPaginationE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var afters []string
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(404)
				return
			}
			afters = append(afters, r.URL.Query().Get("after_id"))
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("after_id") == "" {
				io.WriteString(w, `{"data":[{"id":"claude-a"},{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"claude-c"}],"has_more":false,"last_id":"claude-c"}`)
		}))
		c := newTestClient(t, WithProtocol(ProtoAnthropic), WithHTTPClient(srv.Client()))
		models, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 3 {
			t.Fatalf("models = %+v", models)
		}
		ids := map[string]bool{}
		for _, m := range models {
			ids[m.ID] = true
		}
		for _, want := range []string{"claude-a", "claude-b", "claude-c"} {
			if !ids[want] {
				t.Fatalf("missing %q in %+v", want, models)
			}
		}
		if len(afters) != 2 || afters[0] != "" || afters[1] != "claude-b" {
			t.Fatalf("after_id trail = %v", afters)
		}
	})
}

// ModelInfo's best-effort discovery honors WithTimeout.
func TestModelInfoAppliesTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Second)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithTimeout(1*time.Second))
		start := time.Now()
		_, err := c.ModelInfo(context.Background(), "unknown-model")
		if !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("err = %v, want ErrUnknownModel", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("elapsed = %s, timeout not applied", d)
		}
	})
}

func TestDetectProtocol(t *testing.T) {
	// OpenAI-shaped catalog.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"data":[{"object":"model","id":"gpt-4o"}]}`)
	}))
	defer srv.Close()
	proto, err := DetectProtocol(context.Background(), srv.URL, "k")
	if err != nil || proto != ProtoOpenAIChat {
		t.Fatalf("proto=%s err=%v", proto, err)
	}

	// Anthropic-shaped catalog, reachable only via x-api-key (the first
	// Bearer probe is rejected).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"data":[{"type":"model","id":"claude-x"}]}`)
	}))
	defer srv2.Close()
	proto, err = DetectProtocol(context.Background(), srv2.URL, "k")
	if err != nil || proto != ProtoAnthropic {
		t.Fatalf("proto=%s err=%v", proto, err)
	}

	// Undetectable endpoints fall back to OpenAI Chat.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv3.Close()
	proto, err = DetectProtocol(context.Background(), srv3.URL, "")
	if err != nil || proto != ProtoOpenAIChat {
		t.Fatalf("fallback proto=%s err=%v", proto, err)
	}
}

func TestDetectClientPinsProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"type":"model","id":"claude-x"}]}`)
	}))
	defer srv.Close()
	c, err := DetectClient(context.Background(), WithEndpoint(srv.URL), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Protocol() != ProtoAnthropic {
		t.Fatalf("protocol = %s", c.Protocol())
	}
}

// roundTripCounter wraps a transport to observe whether it was used.
type roundTripCounter struct {
	base  http.RoundTripper
	calls int
}

func (rt *roundTripCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	return rt.base.RoundTrip(req)
}

// The probe must run through the caller's HTTP client, not a throwaway
// default one.
func TestDetectClientUsesCustomHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"object":"model","id":"gpt-4o"}]}`)
	}))
	defer srv.Close()
	rt := &roundTripCounter{base: srv.Client().Transport}
	if _, err := DetectClient(context.Background(),
		WithEndpoint(srv.URL), WithAPIKey("k"),
		WithHTTPClient(&http.Client{Transport: rt}),
	); err != nil {
		t.Fatal(err)
	}
	if rt.calls == 0 {
		t.Fatal("custom HTTP client was not used for the probe")
	}
}

// WithTimeout must bound the probe; a hanging endpoint fails fast instead
// of silently falling back.
func TestDetectClientAppliesTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Second)
		}))
		_, err := DetectClient(context.Background(),
			WithEndpoint(srv.URL), WithAPIKey("k"),
			WithHTTPClient(srv.Client()), WithTimeout(1*time.Second),
		)
		if err == nil {
			t.Fatal("hanging endpoint must fail under WithTimeout, not fall back")
		}
	})
}

// DetectClient must not write its protocol pin into the caller's options
// backing array: the caller's later appends would then silently pick up a
// stale WithProtocol.
func TestDetectClientDoesNotMutateCallerOpts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"type":"model","id":"claude-x"}]}`)
	}))
	defer srv.Close()

	// Pre-allocated cap > len so an unbounded append inside DetectClient
	// would write into our backing array.
	opts := make([]Option, 0, 4)
	opts = append(opts, WithEndpoint(srv.URL), WithAPIKey("k"))
	if _, err := DetectClient(context.Background(), opts...); err != nil {
		t.Fatal(err)
	}
	// The caller appends its own protocol pin. If DetectClient had written
	// into the shared array, the third slot would already hold a stale
	// WithProtocol(ProtoAnthropic).
	opts = append(opts, WithProtocol(ProtoOpenAIChat))
	c, err := NewClient(opts...)
	if err != nil {
		t.Fatal(err)
	}
	if c.Protocol() != ProtoOpenAIChat {
		t.Fatalf("caller options mutated by DetectClient: protocol = %s", c.Protocol())
	}
}

func TestAPIErrorParsing(t *testing.T) {
	// The sentinel-style APIError carries the status, method and URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Header().Set("Retry-After", "1")
		io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, WithEndpoint(srv.URL), WithMaxRetries(0))
	_, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 429 || !apiErr.Retryable || apiErr.Method != "POST" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "429") {
		t.Fatalf("Error() = %q", apiErr.Error())
	}
	var te *TransportError
	if errors.As(err, &te) {
		t.Fatal("APIError must not match TransportError")
	}
	_ = json.Marshal // keep import if unused elsewhere
}
