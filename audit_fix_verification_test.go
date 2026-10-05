package rosetta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
)

// --- G6: assistant turns that would degenerate to empty must error ---

// An OpenAI-chat assistant turn carrying only thinking/redacted-thinking
// blocks is not replayable there; it must fail loudly rather than send a
// degenerate empty message.
func TestAssistantThinkingOnlyTurnRejectedOpenAIChat(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)
	req := &ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{
			User("hi"),
			AssistantBlocks(Thinking("secret reasoning", "sig")),
		},
	}
	_, err := p.encodeMessages(req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("thinking-only assistant turn: err = %v, want ErrInvalidRequest", err)
	}
}

// The Anthropic analogue: an assistant turn whose only thinking block is
// unsigned is dropped, leaving nothing to replay; that is an error too.
func TestAssistantUnsignedThinkingOnlyTurnRejectedAnthropic(t *testing.T) {
	c := newClientWithProtocol(t, ProtoAnthropic)
	p := c.provider.(*anthropicProvider)
	req := &ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			User("hi"),
			AssistantBlocks(Thinking("reasoning with no signature", "")),
		},
	}
	_, _, err := p.encodeMessages(req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsigned-thinking-only assistant turn: err = %v, want ErrInvalidRequest", err)
	}
}

// --- G5: a refusal arriving alongside content must not be dropped ---

func TestRefusalCoexistsWithContentOpenAIChat(t *testing.T) {
	resp, err := decodeOpenAIChatResponse([]byte(
		`{"choices":[{"message":{"content":"partial answer","refusal":"I can't finish that"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, b := range resp.Content {
		if b.Type == BlockText {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("content blocks = %+v, want both the content and the refusal text", resp.Content)
	}
	if got := resp.Text(); !strings.Contains(got, "partial answer") || !strings.Contains(got, "I can't finish that") {
		t.Fatalf("Text() = %q, want both content and refusal", got)
	}
}

// --- G11: in-band 200 errors are flagged, not mistaken for success ---

func TestInBandErrorFlaggedOnUnaryDecoders(t *testing.T) {
	tests := []struct {
		name   string
		decode func() error
	}{
		{
			name: "openai-chat",
			decode: func() error {
				_, err := decodeOpenAIChatResponse([]byte(
					`{"error":{"message":"boom","type":"server_error"}}`))
				return err
			},
		},
		{
			name: "anthropic",
			decode: func() error {
				_, err := decodeAnthropicResponse([]byte(
					`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decode()
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if !apiErr.InBand {
				t.Fatal("InBand must be true so callers gating on StatusCode>=400 still see the failure")
			}
			if apiErr.StatusCode != 200 {
				t.Fatalf("StatusCode = %d, want 200 (the transport succeeded)", apiErr.StatusCode)
			}
		})
	}
}

// --- G9: a caller-abandoned stream is not a missing-usage observation ---

func TestCallerClosedStreamNotCountedUsageMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One content event, then hold the connection open so Close() wins
		// before any usage-carrying terminal event.
		io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	tr := NewMemoryUsageTracker()
	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithHTTPClient(srv.Client()),
		WithUsageTracker(tr),
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatalf("expected a first event, err = %v", stream.Err())
	}
	// Abandon mid-stream: no usage was ever reported, but this is a caller
	// choice, not a provider that omitted usage.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	snap := tr.Snapshot()
	if snap.UsageMissing != 0 {
		t.Fatalf("caller-closed stream must not count as UsageMissing: %+v", snap)
	}
}

// --- validate() must not mutate the caller's request ---

func TestChatDoesNotMutateCallerRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()))
		req := &ChatRequest{
			Model:    "m",
			Messages: []Message{User("hi")},
			Extra:    map[string]any{"context": "x"},
		}
		if _, err := c.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.extraJSON != nil {
			t.Fatalf("Chat wrote unexported state into the caller's request (extraJSON len=%d)", len(req.extraJSON))
		}
	})
}

// A nil request still yields ErrInvalidRequest rather than a panic.
func TestChatNilRequestRejected(t *testing.T) {
	c := newTestClient(t)
	_, err := c.Chat(context.Background(), nil)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil request: err = %v, want ErrInvalidRequest", err)
	}
}

// newClientWithProtocol builds a client pinned to a protocol (no endpoint
// probe), for provider-level unit tests.
func newClientWithProtocol(t *testing.T, p Protocol) *Client {
	t.Helper()
	c, err := NewClient(
		WithEndpoint("http://api.test/v1"),
		WithAPIKey("k"),
		WithProtocol(p),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
