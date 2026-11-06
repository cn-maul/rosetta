package rosetta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RG-06: gateway and tracing headers, URL query and path prefix must be
// reachable without forking the adapter.
func TestRequestExtensionPoints(t *testing.T) {
	var gotHeader http.Header
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithProtocol(ProtoOpenAIChat),
		WithHeader("X-Trace", "trace-1"),
		WithQueryParam("api-version", "2024-10-21"),
		WithPathPrefix("/openai"),
		WithOrganization("org-1"),
		WithProject("proj-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
		t.Fatal(err)
	}

	if gotHeader.Get("X-Trace") != "trace-1" {
		t.Fatalf("X-Trace = %q", gotHeader.Get("X-Trace"))
	}
	if gotHeader.Get("OpenAI-Organization") != "org-1" || gotHeader.Get("OpenAI-Project") != "proj-1" {
		t.Fatalf("org/project headers = %v", gotHeader)
	}
	if !strings.HasPrefix(gotPath, "/openai/v1/") {
		t.Fatalf("path = %q, want /openai/v1/...", gotPath)
	}
	if !strings.Contains(gotQuery, "api-version=2024-10-21") {
		t.Fatalf("query = %q", gotQuery)
	}
}

// A caller header replaces the SDK default of the same name (Azure's
// api-key auth, a gateway that wants its own x-api-key).
func TestHeaderOverridesDefault(t *testing.T) {
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithProtocol(ProtoAnthropic),
		WithHeader("x-api-key", "gateway-key"),
		WithBeta("files-api-2025-04-14"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
		t.Fatal(err)
	}
	if gotHeader.Get("x-api-key") != "gateway-key" {
		t.Fatalf("x-api-key = %q", gotHeader.Get("x-api-key"))
	}
	if beta := gotHeader.Get("anthropic-beta"); !strings.Contains(beta, "files-api-2025-04-14") {
		t.Fatalf("anthropic-beta = %q", beta)
	}
}
