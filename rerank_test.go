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

func TestRerankE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gotPath string
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)
			if req["query"] != "capital" || req["top_n"] != float64(2) || req["return_documents"] != true {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"r1","results":[
				{"index":2,"relevance_score":0.99,"document":{"text":"Washington, D.C."}},
				{"index":0,"relevance_score":0.31}],
				"meta":{"billed_units":{"input_tokens":42,"search_units":1}}}`)
		}))
		tr := NewMemoryUsageTracker()
		c := newTestClient(t, WithHTTPClient(srv.Client()), WithUsageTracker(tr))
		resp, err := c.Rerank(context.Background(), &RerankRequest{
			Model:           "rerank-v3.5",
			Query:           "capital",
			Documents:       []string{"Paris", "A cat", "Washington, D.C."},
			TopN:            2,
			ReturnDocuments: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != "/v1/rerank" {
			t.Fatalf("path = %s", gotPath)
		}
		if len(resp.Results) != 2 || resp.Results[0].Index != 2 || resp.Results[0].RelevanceScore != 0.99 {
			t.Fatalf("results = %+v", resp.Results)
		}
		if resp.Results[0].Document != "Washington, D.C." || resp.Results[1].Document != "" {
			t.Fatalf("document echo = %+v", resp.Results)
		}
		if resp.Usage.InputTokens != 42 || resp.Usage.TotalTokens != 42 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
		snap := tr.Snapshot()
		if snap.TotalRequests != 1 || snap.InputTokens != 42 {
			t.Fatalf("stats = %+v", snap)
		}
	})
}

// top_n/return_documents stay off the wire when unset.
func TestRerankMinimalPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)
			if _, has := req["top_n"]; has {
				w.WriteHeader(400)
				return
			}
			if _, has := req["return_documents"]; has {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":7}}`)
		}))
		c := newTestClient(t, WithHTTPClient(srv.Client()))
		resp, err := c.Rerank(context.Background(), &RerankRequest{
			Model: "bge-reranker-v2-m3", Query: "q", Documents: []string{"d"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Jina-style flat usage falls back to a token count.
		if resp.Usage.InputTokens != 7 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
	})
}

func TestRerankValidationAndUnsupported(t *testing.T) {
	c, err := NewClient(WithProtocol(ProtoAnthropic), WithEndpoint("https://api.anthropic.test/v1"), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "q", Documents: []string{"d"}}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
	openai := newTestClient(t)
	if _, err := openai.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "", Documents: []string{"d"}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty query: err = %v", err)
	}
	if _, err := openai.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "q"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no documents: err = %v", err)
	}
}
