package rosetta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// RG-18: hooks observe every call with status, latency and request id, and
// the configured request id reaches the wire.
func TestHooksObserveCall(t *testing.T) {
	var gotReqID string
	var reqSeen, respSeen bool
	var status int
	var latency time.Duration

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqID = r.Header.Get("X-Request-Id")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Request-Id", "upstream-rid")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithProtocol(ProtoOpenAIChat),
		WithRequestIDFunc(func() string { return "rid-1" }),
		WithHooks(Hooks{
			OnRequest: func(ri RequestInfo) {
				reqSeen = true
				if ri.Method != http.MethodPost || ri.URL == "" {
					t.Errorf("request info = %+v", ri)
				}
			},
			OnResponse: func(ri ResponseInfo) {
				respSeen = true
				status = ri.StatusCode
				latency = ri.Latency
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
		t.Fatal(err)
	}

	if !reqSeen || !respSeen {
		t.Fatalf("hooks: request=%v response=%v", reqSeen, respSeen)
	}
	if gotReqID != "rid-1" {
		t.Fatalf("outbound X-Request-Id = %q", gotReqID)
	}
	if status != http.StatusOK || latency <= 0 {
		t.Fatalf("response info status=%d latency=%v", status, latency)
	}
}

// An explicitly configured header beats the generated id.
func TestExplicitRequestIDWins(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Request-Id")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithProtocol(ProtoOpenAIChat),
		WithHeader("X-Request-Id", "explicit"),
		WithRequestIDFunc(func() string { return "generated" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
		t.Fatal(err)
	}
	if got != "explicit" {
		t.Fatalf("X-Request-Id = %q, want explicit", got)
	}
}
