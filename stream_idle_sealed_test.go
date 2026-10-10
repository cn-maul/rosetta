package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// --- WithStreamIdleTimeout ---

// A producer that never yields must not hang Next forever when a watchdog
// is armed: the stream ends with ErrStreamIdleTimeout, distinguishable
// from both a clean end and a truncation, and Partial survives.
func TestStreamIdleTimeoutAbortsSilentStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		s := newStream(func() (*Event, error) {
			<-release // a provider that goes silent mid-stream
			return nil, io.EOF
		}, nil)
		s.setIdle(30 * time.Second)

		if s.Next() {
			t.Fatal("expected the watchdog to end the stream")
		}
		if !errors.Is(s.Err(), ErrStreamIdleTimeout) {
			t.Fatalf("Err = %v, want ErrStreamIdleTimeout", s.Err())
		}
		// It must be an idle timeout, not a truncation: the sentinel is
		// the only thing that tells them apart.
		if errors.Is(s.Err(), ErrStreamTruncated) {
			t.Fatal("idle timeout must not masquerade as truncation")
		}
	})
}

// The window is per-read, not per-stream: events that keep arriving inside
// it must never trip the watchdog, however long the stream runs overall.
func TestStreamIdleTimeoutIsPerReadNotPerStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i := 0
		s := newStream(func() (*Event, error) {
			i++
			if i > 5 {
				return nil, io.EOF
			}
			time.Sleep(20 * time.Second) // each gap is under the window
			return &Event{Type: EventTextDelta, Text: "x"}, nil
		}, nil)
		s.setIdle(30 * time.Second)

		n := 0
		for s.Next() {
			n++
		}
		if n != 5 {
			t.Fatalf("delivered %d events, want 5 — the watchdog cut a live stream", n)
		}
		if s.Err() != nil {
			t.Fatalf("Err = %v, want nil for a stream that stayed alive", s.Err())
		}
	})
}

// Default (no watchdog) must behave exactly as before: the setting is zero
// and Next stays a plain blocking read, so the caller's context is what ends
// a stalled stream. The watchdog is opt-in.
func TestStreamWithoutWatchdogKeepsBlockingRead(t *testing.T) {
	s := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "x"}, nil
	}, nil)
	if s.idle != 0 {
		t.Fatalf("idle = %v, want 0 by default", s.idle)
	}
	if !s.Next() {
		t.Fatalf("plain read failed: %v", s.Err())
	}
	if s.Err() != nil {
		t.Fatalf("Err = %v, want nil", s.Err())
	}
}

// A negative duration is nonsense input; it must disable the watchdog
// rather than fire instantly on every read.
func TestSetIdleClampsNegativeToDisabled(t *testing.T) {
	s := newStream(func() (*Event, error) { return nil, io.EOF }, nil)
	s.setIdle(-time.Second)
	if s.idle != 0 {
		t.Fatalf("idle = %v, want 0 for a negative window", s.idle)
	}
}

func TestWithStreamIdleTimeoutOption(t *testing.T) {
	c := newTestClient(t, WithStreamIdleTimeout(45*time.Second))
	if c.settings.streamIdle != 45*time.Second {
		t.Fatalf("streamIdle = %v, want 45s", c.settings.streamIdle)
	}
	c2 := newTestClient(t, WithStreamIdleTimeout(-time.Second))
	if c2.settings.streamIdle != 0 {
		t.Fatalf("negative must disable, got %v", c2.settings.streamIdle)
	}
	c3 := newTestClient(t)
	if c3.settings.streamIdle != 0 {
		t.Fatalf("default must be disabled, got %v", c3.settings.streamIdle)
	}
}

// Note: an end-to-end variant (a real httptest server that sends headers
// then goes silent) is deliberately not here — under synctest the producer
// goroutine stays blocked on its socket read, which deadlocks the bubble.
// The watchdog path is covered deterministically above; the option plumbing
// by TestWithStreamIdleTimeoutOption.

// --- sealed reasoning (OpenAI Responses) ---

const sealedBody = `{
  "id":"resp_1","model":"gpt-x","status":"completed",
  "output":[
    {"type":"reasoning","id":"rs_abc","summary":[{"type":"summary_text","text":"thinking hard"}],
     "encrypted_content":"ENCRYPTED-BLOB"},
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
  ],
  "usage":{"input_tokens":10,"output_tokens":5}
}`

func TestResponsesSealedReasoningSurvivesDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sealedBody)
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL+"/v1"), WithProtocol(ProtoOpenAIResponses))
	resp, err := c.Chat(context.Background(), &ChatRequest{
		Model: "gpt-x", Messages: []Message{User("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sealed *Block
	for i := range resp.Content {
		if resp.Content[i].Type == BlockThinking {
			sealed = &resp.Content[i]
		}
	}
	if sealed == nil {
		t.Fatal("no thinking block decoded")
	}
	if sealed.Sealed != "ENCRYPTED-BLOB" {
		t.Fatalf("Sealed = %q, want the encrypted_content verbatim", sealed.Sealed)
	}
	if sealed.SealedBy != "openai-responses" {
		t.Fatalf("SealedBy = %q, want openai-responses", sealed.SealedBy)
	}
	// The readable summary must stay readable and must not be mixed into
	// the opaque seal.
	if sealed.Thinking != "thinking hard" {
		t.Fatalf("Thinking = %q, want the summary", sealed.Thinking)
	}
}

// A reasoning item with an encrypted payload and no readable text is still
// state the next turn needs; it must not be dropped as "empty".
func TestResponsesSealedReasoningWithNoSummaryIsKept(t *testing.T) {
	body := `{"id":"resp_1","model":"gpt-x","status":"completed",
	 "output":[{"type":"reasoning","id":"rs_a","encrypted_content":"BLOB"}]}`
	resp, err := decodeResponsesResponse([]byte(body), "POST", "http://x", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Sealed != "BLOB" {
		t.Fatalf("a seal-only reasoning item was dropped: %+v", resp.Content)
	}
}

// The whole point: the sealed payload must go back on the wire, or the
// conversation cannot continue across turns.
func TestResponsesSealedReasoningIsReplayed(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sealedBody)
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL+"/v1"), WithProtocol(ProtoOpenAIResponses))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "gpt-x",
		Messages: []Message{
			User("hi"),
			{Role: RoleAssistant, Blocks: []Block{
				SealedThinking("thinking hard", "ENCRYPTED-BLOB", "openai-responses"),
			}},
			User("more"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := got["input"].([]any)
	var found bool
	for _, it := range input {
		m, _ := it.(map[string]any)
		if m["type"] == "reasoning" {
			found = true
			if m["encrypted_content"] != "ENCRYPTED-BLOB" {
				t.Fatalf("replayed reasoning item = %v, want the seal verbatim", m)
			}
			sum, _ := m["summary"].([]any)
			if len(sum) != 1 {
				t.Fatalf("summary dropped on replay: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("no reasoning item in the replayed input: %v", input)
	}
}

// A seal belongs to the family that issued it. Replaying an OpenAI
// encrypted blob to Anthropic (or any other family) would send foreign
// opaque state to a provider that never produced it.
// A seal belongs to the family that issued it. An OpenAI encrypted blob
// sent to Anthropic must never reach the wire — Anthropic dropped the
// block (it cannot replay another family's seal) and, since that left the
// assistant turn empty, rejects the turn loudly rather than silently
// dropping the conversation state (G6). Either way the seal is not leaked.
func TestSealedReasoningIsNotSentToAnotherFamily(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","model":"claude","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, WithEndpoint(srv.URL), WithProtocol(ProtoAnthropic))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "claude",
		Messages: []Message{
			User("hi"),
			{Role: RoleAssistant, Blocks: []Block{
				SealedThinking("summary", "OPENAI-ONLY-SEAL", "openai-responses"),
			}},
			User("more"),
		},
	})
	// No request may reach the wire carrying the foreign seal; whether the
	// turn is rejected outright (G6) or the block is dropped, the payload
	// must not be there.
	if len(body) > 0 && strings.Contains(string(body), "OPENAI-ONLY-SEAL") {
		t.Fatalf("an OpenAI seal leaked onto the Anthropic wire: %s", body)
	}
	_ = err // an ErrInvalidRequest here is the honest G6 outcome
}

// A sealed block still counts as content: a message carrying only a seal
// is valid, and one carrying nothing is still rejected.
func TestSealedThinkingValidatesAsContent(t *testing.T) {
	only := Message{Role: RoleAssistant, Blocks: []Block{
		SealedThinking("", "BLOB", "openai-responses"),
	}}
	if err := only.validate(); err != nil {
		t.Fatalf("a seal-only assistant turn must be valid: %v", err)
	}
	empty := Message{Role: RoleAssistant, Blocks: []Block{}}
	if err := empty.validate(); err == nil {
		t.Fatal("an empty assistant turn must still be rejected")
	}
}

// Chat and Anthropic must not invent a Responses seal on decode: the
// field is that family's alone.
func TestOtherProtocolsLeaveSealUnset(t *testing.T) {
	body := `{"id":"1","model":"m","choices":[{"message":{"content":"hi"}}]}`
	resp, err := decodeOpenAIChatResponse([]byte(body), "POST", "http://x", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.Content {
		if b.Sealed != "" || b.SealedBy != "" {
			t.Fatalf("chat decode invented a seal: %+v", b)
		}
	}
}
