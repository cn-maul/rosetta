package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
)

func TestEmbedE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)
			if req["model"] != "text-embedding-3-small" || req["dimensions"] != float64(256) {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// The request asks for Dimensions=256, so the reply must carry
			// 256-dim vectors (dimension mismatch is a protocol error now).
			vec := make([]float64, 256)
			vec[0], vec[1] = 0.1, 0.2
			b0, _ := json.Marshal(vec)
			vec2 := make([]float64, 256)
			vec2[0], vec2[1] = 0.3, 0.4
			b1, _ := json.Marshal(vec2)
			io.WriteString(w, `{"object":"list","model":"text-embedding-3-small","data":[
				{"object":"embedding","index":0,"embedding":`+string(b0)+`},
				{"object":"embedding","index":1,"embedding":`+string(b1)+`}],
				"usage":{"prompt_tokens":9,"total_tokens":9}}`)
		}))
		tr := NewMemoryUsageTracker()
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithUsageTracker(tr))
		resp, err := c.Embed(context.Background(), &EmbeddingRequest{
			Model:      "text-embedding-3-small",
			Input:      []string{"你好", "world"},
			Dimensions: 256,
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != "/v1/embeddings" || gotAuth != "Bearer k" {
			t.Fatalf("request = %s %s", gotPath, gotAuth)
		}
		if len(resp.Data) != 2 || resp.Data[1].Embedding[0] != 0.3 {
			t.Fatalf("data = %+v", resp.Data)
		}
		if resp.Usage.InputTokens != 9 || resp.Usage.TotalTokens != 9 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
		snap := tr.Snapshot()
		if snap.TotalRequests != 1 || snap.InputTokens != 9 {
			t.Fatalf("stats = %+v", snap)
		}
	})
}

// WithEmbeddingEndpoint/Key reroute the call and swap the credential.
func TestEmbedAuxEndpointOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer aux" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
		}))
		c, err := NewClient(
			WithEndpoint("https://api.deepseek.test/v1"), // dead host: must not be contacted
			WithAPIKey("k"),
			// srv.URL is empty inside a synctest bubble; the custom
			// transport routes every request to the test server regardless.
			WithEmbeddingEndpoint("https://embed.test/v1"),
			WithEmbeddingAPIKey("aux"),
			WithHTTPClient(srv.Client()),
		)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "bge-m3", Input: []string{"x"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Data) != 1 {
			t.Fatalf("resp = %+v", resp)
		}
	})
}

// Anthropic serves no embeddings API: without an override the call fails
// with ErrNotSupported; with one it goes through as usual.
func TestEmbedAnthropicNeedsOverride(t *testing.T) {
	c, err := NewClient(WithProtocol(ProtoAnthropic), WithEndpoint("https://api.anthropic.test/v1"), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "bge-m3", Input: []string{"x"}}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}

	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		}))
		c, err := NewClient(
			WithProtocol(ProtoAnthropic),
			WithEndpoint("https://api.anthropic.test/v1"),
			WithAPIKey("k"),
			WithEmbeddingEndpoint("https://embed.test/v1"),
			WithHTTPClient(srv.Client()),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "bge-m3", Input: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEmbedErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()))
		if _, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "", Input: []string{"x"}}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("missing model: err = %v", err)
		}
		if _, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "m"}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("empty input: err = %v", err)
		}
		_, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "nope", Input: []string{"x"}})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Message != "model not found" {
			t.Fatalf("api error = %v", err)
		}
	})
}
