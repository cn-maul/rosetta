package rosetta

// Regression tests for the audit fixes: registry snapshot isolation and
// deterministic alias handling, endpoint query rejection, Anthropic auth
// header scoping, embedding/rerank response validation, refresh error
// propagation and body-too-large detection.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// Mutating the caller's input slice (or a returned ModelInfo) must never
// change registry state behind its lock.
func TestRegistryAliasesAreSnapshots(t *testing.T) {
	r := newRegistry()
	infos := []ModelInfo{{ID: "m1", Aliases: []string{"alias-a"}}}
	if err := r.SetManual(infos); err != nil {
		t.Fatal(err)
	}
	infos[0].Aliases[0] = "hijacked"
	if _, ok := r.Lookup("hijacked"); ok {
		t.Fatal("mutating the input slice must not affect the registry")
	}

	mi, _ := r.Lookup("m1")
	mi.Aliases[0] = "hijacked"
	if _, ok := r.Lookup("hijacked"); ok {
		t.Fatal("mutating a returned ModelInfo must not affect the registry")
	}

	list := r.List()
	for _, m := range list {
		m.Aliases[0] = "hijacked"
	}
	if _, ok := r.Lookup("hijacked"); ok {
		t.Fatal("mutating List() entries must not affect the registry")
	}
}

func TestEndpointQueryRejected(t *testing.T) {
	_, err := NewClient(WithEndpoint("https://api.test/v1?token=secret"), WithAPIKey("k"))
	if err == nil {
		t.Fatal("endpoint with query string must be rejected at construction")
	}
	_, err = NewClient(WithProtocol(ProtoAnthropic), WithEndpoint("https://api.test/v1"),
		WithAPIKey("k"), WithEmbeddingEndpoint("https://embed.test/v1?key=secret"))
	if err == nil {
		t.Fatal("embedding endpoint with query string must be rejected")
	}
}

func TestAnthropicAuthHeaders(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	h := p.headers(false)
	if h.Get("x-api-key") != "k" {
		t.Fatal("x-api-key must always be sent")
	}
	if h.Get("Authorization") != "" {
		t.Fatal("Authorization must not be sent by default (credential scoping)")
	}

	c2 := newTestClient(t, WithProtocol(ProtoAnthropic), WithAnthropicBearerAuth(true))
	p2 := c2.provider.(*anthropicProvider)
	h2 := p2.headers(false)
	if h2.Get("x-api-key") != "k" || h2.Get("Authorization") != "Bearer k" {
		t.Fatalf("bearer opt-in headers = %v", h2)
	}
}

func TestEmbedDecodeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty data", `{"data":[]}`, "no data"},
		{"missing data", `{}`, "no data"},
		{"count mismatch", `{"data":[{"index":0,"embedding":[1]}]}`, "1 vectors for 2 inputs"},
		{"out of range index", `{"data":[{"index":0,"embedding":[1]},{"index":5,"embedding":[1]}]}`, "out-of-range index"},
		{"duplicate index", `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`, "repeats index"},
		{"empty vector", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[]}]}`, "empty vector"},
	}
	for _, tc := range cases {
		if _, err := decodeEmbeddingsResponse([]byte(tc.body), 2, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}

	// input_tokens-only usage variant (as served by some compatible
	// services) must populate InputTokens.
	resp, err := decodeEmbeddingsResponse([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"input_tokens":10}}`), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestEmbedNegativeDimensions(t *testing.T) {
	c := newTestClient(t)
	_, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "m", Input: []string{"x"}, Dimensions: -1})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestRerankDecodeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty results", `{"results":[]}`, "no results"},
		{"out of range index", `{"results":[{"index":9,"relevance_score":0.5}]}`, "out-of-range index"},
		{"negative index", `{"results":[{"index":-1,"relevance_score":0.5}]}`, "out-of-range index"},
		{"duplicate index", `{"results":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.9}]}`, "repeats index"},
	}
	for _, tc := range cases {
		if _, err := decodeRerankResponse([]byte(tc.body), 3); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestRerankNegativeTopNAndSearchUnits(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "q", Documents: []string{"d"}, TopN: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("TopN -1: err = %v, want ErrInvalidRequest", err)
	}

	// search_units are billed units, not tokens: only input_tokens may
	// feed the token statistics.
	resp, err := decodeRerankResponse([]byte(`{"results":[{"index":0,"relevance_score":0.5}],"meta":{"billed_units":{"search_units":1}}}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v, want zero tokens for search_units-only billing", resp.Usage)
	}
}

// A failed refresh must be reported to every waiter, not just to the
// goroutine that started it — and concurrent refreshes must share a
// single /models request while one is in flight.
func TestRefreshModelsErrorPropagation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		arrived := make(chan struct{})
		release := make(chan struct{})
		var mu sync.Mutex
		hits := 0
		var once sync.Once
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			once.Do(func() { close(arrived) })
			<-release
			w.WriteHeader(500)
			io.WriteString(w, `{"error":{"message":"catalog unavailable","type":"server_error"}}`)
		}))
		// Retries are off so the hit count maps 1:1 to refreshes.
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithMaxRetries(0))

		const waiters = 5
		errs := make([]error, waiters)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[0] = c.refreshModels(context.Background())
		}()
		<-arrived // first refresh is in flight, blocked on the handler

		for i := 1; i < waiters; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = c.refreshModels(context.Background())
			}(i)
		}
		// Give the waiters a chance to (wrongly) start their own refreshes.
		synctest.Wait()
		close(release)
		wg.Wait()

		mu.Lock()
		n := hits
		mu.Unlock()
		if n != 1 {
			t.Fatalf("/models hit %d times, want 1 (concurrent refreshes must share one request)", n)
		}
		for i, err := range errs {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
				t.Errorf("waiter %d: err = %v, want the refresh's 500 APIError (failure must not read as success)", i, err)
			}
		}
	})
}

// ModelInfo must surface ErrUnknownModel after a failed remote discovery,
// never a stale success.
func TestModelInfoUnknownAfterFailedRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()))
		if _, err := c.ModelInfo(context.Background(), "nope"); !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("err = %v, want ErrUnknownModel", err)
		}
	})
}

// ---- phase-3 hardening: unified validation, Extra protection, stream
// ---- concurrency, estimator configurability, thinking revocation ----

func TestChatRequestStructuralValidation(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		req  ChatRequest
	}{
		{"bad role", ChatRequest{Model: "m", Messages: []Message{{Role: "dev", Blocks: []Block{{Type: BlockText, Text: "x"}}}}}},
		{"no blocks", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser}}}},
		{"empty text block", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText}}}}}},
		{"image without URL", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockImage}}}}}},
		{"audio bad format", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockAudio, AudioData: "AA", AudioFormat: "flac"}}}}}},
		{"file without data", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockFile}}}}}},
		{"tool result without id", ChatRequest{Model: "m", Messages: []Message{{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, Content: "ok"}}}}}},
		{"invalid tool args", ChatRequest{Model: "m", Messages: []Message{{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolCall, ToolCallID: "1", ToolName: "f", Arguments: "{oops"}}}}}},
		{"unknown block type", ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: "video"}}}}}},
		{"negative max output", ChatRequest{Model: "m", Messages: []Message{User("hi")}, MaxOutputTokens: -1}},
		{"temperature out of range", ChatRequest{Model: "m", Messages: []Message{User("hi")}, Temperature: f(2.5)}},
		{"topP out of range", ChatRequest{Model: "m", Messages: []Message{User("hi")}, TopP: f(1.5)}},
		{"bad effort", ChatRequest{Model: "m", Messages: []Message{User("hi")}, Thinking: &ThinkingConfig{Effort: "maximal"}}},
		{"negative budget", ChatRequest{Model: "m", Messages: []Message{User("hi")}, Thinking: &ThinkingConfig{BudgetTokens: -5}}},
	}
	for _, tc := range cases {
		if err := tc.req.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}

	// Well-formed requests still pass, including effort unset and a valid
	// tool-call replay.
	ok := ChatRequest{Model: "m", Messages: []Message{
		User("hi"),
		AssistantBlocks(ToolCall("1", "f", `{"a":1}`)),
		ToolResult("1", "f", "done"),
	}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestExtraReservedKeys(t *testing.T) {
	steal := func() *ChatRequest {
		return &ChatRequest{
			Model:    "m",
			Messages: []Message{User("hi")},
			Extra:    map[string]any{"stream": false},
		}
	}
	c := newTestClient(t)
	if _, err := c.Chat(context.Background(), steal()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("reserved Extra key: err = %v, want ErrInvalidRequest", err)
	}

	// The opt-in restores override semantics: the request gets past
	// payload build (here it fails at the HTTP layer instead).
	c2 := newTestClient(t, WithExtraOverrides(true))
	_, err := c2.Chat(context.Background(), steal())
	var terr *TransportError
	if !errors.As(err, &terr) {
		t.Fatalf("WithExtraOverrides must lift the reserved-key check, got: %v", err)
	}
}

// Close must be callable from another goroutine while Next is blocked,
// without races or double-release (run under -race).
func TestStreamCloseConcurrentWithNext(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	s := newStream(func() (*Event, error) {
		atomic.AddInt32(&calls, 1)
		close(blocked)
		<-release
		return &Event{Type: EventTextDelta, Text: "late"}, nil
	}, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Next() // blocks inside the producer
	}()
	<-blocked
	s.Close()
	close(release)
	wg.Wait()

	if err := s.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
	if s.Next() {
		t.Fatal("Next after Close must return false")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("producer ran %d times, want 1 (no re-entry after Close)", n)
	}
}

func TestMultimediaEstimatesOverride(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{{Type: BlockImage, ImageURL: "https://x/y.png"}},
	}}}
	if got := req.estimateInputTokens(MultimediaTokenEstimates{}); got != 4+1500 {
		t.Fatalf("default image estimate = %d, want %d", got, 4+1500)
	}
	if got := req.estimateInputTokens(MultimediaTokenEstimates{Image: 100}); got != 4+100 {
		t.Fatalf("overridden image estimate = %d, want %d", got, 4+100)
	}
}

func TestDisableThinkingRevocation(t *testing.T) {
	r := newRegistry()
	if err := r.SetRemote([]ModelInfo{{ID: "m1", SupportsThinking: true, Known: true}}); err != nil {
		t.Fatal(err)
	}
	// A sparse manual entry alone cannot revoke (OR semantics)…
	if err := r.SetManual([]ModelInfo{{ID: "m1"}}); err != nil {
		t.Fatal(err)
	}
	if mi, _ := r.Lookup("m1"); !mi.SupportsThinking {
		t.Fatal("sparse manual entry must inherit SupportsThinking=true")
	}
	// …but DisableThinking is the explicit veto.
	if err := r.SetManual([]ModelInfo{{ID: "m1", DisableThinking: true}}); err != nil {
		t.Fatal(err)
	}
	mi, _ := r.Lookup("m1")
	if mi.SupportsThinking {
		t.Fatal("DisableThinking must revoke the inherited thinking claim")
	}
	if !mi.Known {
		t.Fatal("Known must survive the merge")
	}
}

func TestReadBodyLimit(t *testing.T) {
	b, err := httpx.ReadBody(strings.NewReader("hello"), 5)
	if err != nil || string(b) != "hello" {
		t.Fatalf("b=%q err=%v", b, err)
	}
	_, err = httpx.ReadBody(strings.NewReader("hello!"), 5)
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	// Exactly at the limit passes; the sentinel byte proves overflow.
	b, err = httpx.ReadBody(strings.NewReader(strings.Repeat("x", 5)), 5)
	if err != nil || len(b) != 5 {
		t.Fatalf("exact limit: b=%d err=%v", len(b), err)
	}
}
