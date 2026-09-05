// Command webui serves a local chat playground backed by the rosetta SDK.
//
// Endpoints:
//
//	GET    /                          playground UI (embedded)
//	GET    /api/state                 providers + models + usage snapshot
//	POST   /api/providers             add provider
//	DELETE /api/providers/{id}        remove provider
//	POST   /api/providers/test        connectivity test (ListModels)
//	POST   /api/models                register model caps for a provider
//	DELETE /api/models/{provider}/{id}
//	POST   /api/chat                  non-streaming chat
//	POST   /api/chat/stream           streaming chat (SSE)
//	GET    /api/stats                 cumulative usage snapshot
//
// Provider/model configuration persists to a local JSON file (default
// webui/data.json, gitignored — it contains API keys).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
)

//go:embed static/index.html
var indexHTML []byte

const dataFile = "webui/data.json"

// Provider is one configured endpoint+credential pair.
type Provider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Vendor   string `json:"vendor,omitempty"`   // optional preset (fills endpoint/protocol)
	Endpoint string `json:"endpoint,omitempty"` // required unless vendor preset
	APIKey   string `json:"apiKey"`
	Protocol string `json:"protocol,omitempty"` // default openai-chat
}

// ModelDef carries user-declared capability caps for a model id.
type ModelDef struct {
	ProviderID       string `json:"providerId"`
	ID               string `json:"id"`
	ContextWindow    int    `json:"contextWindow"`
	MaxOutputTokens  int    `json:"maxOutputTokens"`
	SupportsThinking bool   `json:"supportsThinking"`
}

type store struct {
	mu        sync.Mutex
	Providers []Provider `json:"providers"`
	Models    []ModelDef `json:"models"`
}

var tracker = rosetta.NewMemoryUsageTracker()

func (s *store) load() {
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, s)
}

func (s *store) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll("webui", 0o755); err == nil {
		_ = os.WriteFile(dataFile, b, 0o600)
	}
}

func (s *store) snapshot() ([]Provider, []ModelDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := make([]Provider, len(s.Providers))
	copy(ps, s.Providers)
	ms := make([]ModelDef, len(s.Models))
	copy(ms, s.Models)
	return ps, ms
}

func (s *store) findProvider(id string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// buildClient assembles a rosetta client for one call. Construction is
// cheap (no network), so per-request clients avoid cache invalidation.
func buildClient(p Provider, m *ModelDef) (*rosetta.Client, error) {
	opts := []rosetta.Option{
		rosetta.WithAPIKey(p.APIKey),
		rosetta.WithUsageTracker(tracker),
	}
	if p.Vendor != "" && p.Endpoint == "" {
		opts = append(opts, rosetta.WithVendor(p.Vendor))
	} else {
		opts = append(opts, rosetta.WithEndpoint(p.Endpoint))
	}
	if p.Protocol != "" {
		opts = append(opts, rosetta.WithProtocol(rosetta.Protocol(p.Protocol)))
	}
	if m != nil {
		opts = append(opts, rosetta.WithModelInfo(rosetta.ModelInfo{
			ID:               m.ID,
			ContextWindow:    m.ContextWindow,
			MaxOutputTokens:  m.MaxOutputTokens,
			SupportsThinking: m.SupportsThinking,
			Known:            true, // user-declared capabilities, enable gating
		}))
		if m.MaxOutputTokens > 0 {
			opts = append(opts, rosetta.WithDefaultMaxOutputTokens(m.MaxOutputTokens))
		}
	}
	return rosetta.NewClient(opts...)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	ProviderID  string        `json:"providerId"`
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Thinking    string        `json:"thinking"` // "", low, medium, high
	MaxTokens   int           `json:"maxTokens"`
	Temperature *float64      `json:"temperature"`
	Stream      bool          `json:"stream"`
}

func (cr *chatRequest) toRosetta() (*rosetta.ChatRequest, error) {
	if cr.Model == "" {
		return nil, errors.New("model is required")
	}
	if len(cr.Messages) == 0 {
		return nil, errors.New("messages are required")
	}
	req := &rosetta.ChatRequest{Model: cr.Model}
	for _, m := range cr.Messages {
		switch m.Role {
		case "system":
			req.Messages = append(req.Messages, rosetta.System(m.Content))
		case "user":
			req.Messages = append(req.Messages, rosetta.User(m.Content))
		case "assistant":
			// Assistant replay is text-only: streamed thinking cannot be
			// replayed safely (Anthropic signatures are unavailable here).
			req.Messages = append(req.Messages, rosetta.Assistant(m.Content))
		default:
			return nil, fmt.Errorf("unsupported role %q", m.Role)
		}
	}
	if cr.Thinking != "" {
		req.Thinking = &rosetta.ThinkingConfig{Effort: rosetta.Effort(cr.Thinking)}
	}
	if cr.MaxTokens > 0 {
		req.MaxOutputTokens = cr.MaxTokens
	}
	req.Temperature = cr.Temperature
	return req, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func handleState(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ps, ms := s.snapshot()
		writeJSON(w, map[string]any{
			"providers": ps,
			"models":    ms,
			"stats":     tracker.Snapshot(),
		})
	}
}

func handleProviders(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p Provider
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if p.Name == "" || p.APIKey == "" {
			fail(w, http.StatusBadRequest, errors.New("name and apiKey are required"))
			return
		}
		if p.Endpoint == "" && p.Vendor == "" {
			fail(w, http.StatusBadRequest, errors.New("endpoint or vendor is required"))
			return
		}
		p.ID = fmt.Sprintf("p%d", time.Now().UnixNano())
		s.mu.Lock()
		s.Providers = append(s.Providers, p)
		s.mu.Unlock()
		s.save()
		writeJSON(w, p)
	}
}

func handleProviderDelete(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		kept := s.Providers[:0]
		for _, p := range s.Providers {
			if p.ID != id {
				kept = append(kept, p)
			}
		}
		s.Providers = kept
		keptM := s.Models[:0]
		for _, m := range s.Models {
			if m.ProviderID != id {
				keptM = append(keptM, m)
			}
		}
		s.Models = keptM
		s.mu.Unlock()
		s.save()
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// handleProviderTest probes the endpoint with ListModels; a failure does
// not necessarily mean chatting fails (some services lack /models).
func handleProviderTest(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p Provider
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		client, err := buildClient(p, nil)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		ctx, cancel := contextWithTimeout(r, 10*time.Second)
		defer cancel()
		models, err := client.ListModels(ctx)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "models": len(models)})
	}
}

func handleModels(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m ModelDef
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if m.ProviderID == "" || m.ID == "" {
			fail(w, http.StatusBadRequest, errors.New("providerId and id are required"))
			return
		}
		if _, ok := s.findProvider(m.ProviderID); !ok {
			fail(w, http.StatusNotFound, errors.New("provider not found"))
			return
		}
		m.ID = strings.TrimSpace(m.ID)
		s.mu.Lock()
		s.Models = append(s.Models, m)
		s.mu.Unlock()
		s.save()
		writeJSON(w, m)
	}
}

func handleModelDelete(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		providerID, id := r.PathValue("provider"), r.PathValue("id")
		s.mu.Lock()
		kept := s.Models[:0]
		for _, m := range s.Models {
			if m.ProviderID != providerID || m.ID != id {
				kept = append(kept, m)
			}
		}
		s.Models = kept
		s.mu.Unlock()
		s.save()
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func resolveChat(w http.ResponseWriter, r *http.Request, s *store) (*rosetta.Client, *rosetta.ChatRequest, bool) {
	var cr chatRequest
	if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
		fail(w, http.StatusBadRequest, err)
		return nil, nil, false
	}
	p, ok := s.findProvider(cr.ProviderID)
	if !ok {
		fail(w, http.StatusNotFound, errors.New("provider not found"))
		return nil, nil, false
	}
	var md *ModelDef
	s.mu.Lock()
	for i := range s.Models {
		if s.Models[i].ProviderID == p.ID && s.Models[i].ID == cr.Model {
			md = &s.Models[i]
			break
		}
	}
	s.mu.Unlock()
	client, err := buildClient(p, md)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return nil, nil, false
	}
	req, err := cr.toRosetta()
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return nil, nil, false
	}
	return client, req, true
}

type sseEvent struct {
	Type    string         `json:"type"` // text | thinking | usage | stop | error | done
	Text    string         `json:"text,omitempty"`
	Usage   *rosetta.Usage `json:"usage,omitempty"`
	Stop    string         `json:"stop,omitempty"`
	Content string         `json:"content,omitempty"` // non-stream payload
	Think   string         `json:"think,omitempty"`   // non-stream payload
	Error   string         `json:"error,omitempty"`
}

func handleChat(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, req, ok := resolveChat(w, r, s)
		if !ok {
			return
		}
		ctx, cancel := contextWithTimeout(r, 5*time.Minute)
		defer cancel()
		resp, err := client.Chat(ctx, req)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{
			"content": resp.Text(),
			"think":   resp.ThinkingText(),
			"stop":    resp.StopReason,
			"usage":   resp.Usage,
		})
	}
}

func handleChatStream(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, req, ok := resolveChat(w, r, s)
		if !ok {
			return
		}
		fl, isFl := w.(http.Flusher)
		if !isFl {
			fail(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		send := func(ev sseEvent) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		stream, err := client.ChatStream(r.Context(), req)
		if err != nil {
			send(sseEvent{Type: "error", Error: err.Error()})
			return
		}
		defer stream.Close()
		for stream.Next() {
			switch ev := stream.Event(); ev.Type {
			case rosetta.EventTextDelta:
				send(sseEvent{Type: "text", Text: ev.Text})
			case rosetta.EventThinkingDelta:
				send(sseEvent{Type: "thinking", Text: ev.Text})
			case rosetta.EventMessageEnd:
				if ev.Usage != nil {
					send(sseEvent{Type: "usage", Usage: ev.Usage})
				}
				send(sseEvent{Type: "stop", Stop: string(ev.StopReason)})
			}
		}
		if err := stream.Err(); err != nil {
			send(sseEvent{Type: "error", Error: err.Error()})
		}
		send(sseEvent{Type: "done"})
	}
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func main() {
	s := &store{}
	s.load()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", handleState(s))
	mux.HandleFunc("POST /api/providers", handleProviders(s))
	mux.HandleFunc("DELETE /api/providers/{id}", handleProviderDelete(s))
	mux.HandleFunc("POST /api/providers/test", handleProviderTest(s))
	mux.HandleFunc("POST /api/models", handleModels(s))
	mux.HandleFunc("DELETE /api/models/{provider}/{id}", handleModelDelete(s))
	mux.HandleFunc("POST /api/chat", handleChat(s))
	mux.HandleFunc("POST /api/chat/stream", handleChatStream(s))
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, tracker.Snapshot())
	})

	addr := "127.0.0.1:8787"
	if p := os.Getenv("ROSETTA_WEBUI_ADDR"); p != "" {
		addr = p
	}
	log.Printf("rosetta playground: http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
