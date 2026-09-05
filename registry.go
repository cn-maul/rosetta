package rosetta

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

//go:embed registry_data/models.json
var builtinModelsJSON []byte

// Registry merges model metadata from three sources, in descending
// priority: manual configuration, remote discovery (GET /models), and the
// built-in knowledge base. Higher layers override lower ones field by
// field; empty fields inherit from the layer below.
//
// Following the "presets are reliable, custom models are not guessed"
// principle: entries learned purely from /models carry Known=false and no
// inferred capability data.
type Registry struct {
	mu       sync.RWMutex
	builtin  map[string]ModelInfo
	remote   map[string]ModelInfo
	manual   map[string]ModelInfo
	alias    map[string]string
	resolved map[string]ModelInfo
}

func newRegistry() *Registry {
	r := &Registry{
		builtin: map[string]ModelInfo{},
		remote:  map[string]ModelInfo{},
		manual:  map[string]ModelInfo{},
		alias:   map[string]string{},
	}
	var doc struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(builtinModelsJSON, &doc); err == nil {
		for _, m := range doc.Models {
			if m.ID == "" {
				continue
			}
			m.Known = true
			r.builtin[m.ID] = m
		}
	}
	r.rebuild()
	return r
}

// LoadFile loads manual model configuration from a JSON file with the
// same schema as the built-in knowledge base ({"models":[...]}).
func (r *Registry) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("rosetta: reading models file: %w", err)
	}
	var doc struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("rosetta: parsing models file %s: %w", path, err)
	}
	infos := make([]ModelInfo, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.ID != "" {
			m.Known = true
			infos = append(infos, m)
		}
	}
	r.SetManual(infos)
	return nil
}

// SetManual installs (replacing) the manual configuration layer.
func (r *Registry) SetManual(infos []ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manual = map[string]ModelInfo{}
	for _, m := range infos {
		if m.ID == "" {
			continue
		}
		m.Known = true
		r.manual[m.ID] = m
	}
	r.rebuild()
}

// SetRemote installs (replacing) the discovery layer.
func (r *Registry) SetRemote(infos []ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remote = map[string]ModelInfo{}
	for _, m := range infos {
		if m.ID == "" {
			continue
		}
		m.Known = false
		r.remote[m.ID] = m
	}
	r.rebuild()
}

// rebuild recomputes the merged view and alias index. Callers hold mu.
func (r *Registry) rebuild() {
	resolved := make(map[string]ModelInfo, len(r.builtin)+len(r.remote)+len(r.manual))
	alias := make(map[string]string)
	add := func(layer string, src map[string]ModelInfo) {
		for id, m := range src {
			if prev, ok := resolved[id]; ok {
				resolved[id] = mergeInfo(m, prev)
			} else {
				resolved[id] = m
			}
			for _, a := range m.Aliases {
				if a != "" {
					alias[a] = id
				}
			}
		}
	}
	add("builtin", r.builtin)
	add("remote", r.remote)
	add("manual", r.manual)
	r.resolved = resolved
	r.alias = alias
}

// mergeInfo overlays high's non-zero fields onto low. Boolean fields
// follow OR semantics: a lower layer can declare thinking support that a
// sparse higher-layer entry does not mention, but a higher layer cannot
// revoke it (use a dedicated field if that is ever needed).
func mergeInfo(high, low ModelInfo) ModelInfo {
	out := high
	if out.DisplayName == "" {
		out.DisplayName = low.DisplayName
	}
	if out.ContextWindow == 0 {
		out.ContextWindow = low.ContextWindow
	}
	if out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = low.MaxOutputTokens
	}
	if !out.SupportsThinking {
		out.SupportsThinking = low.SupportsThinking
	}
	if out.Protocol == "" {
		out.Protocol = low.Protocol
	}
	if len(out.Aliases) == 0 {
		out.Aliases = low.Aliases
	}
	out.Known = high.Known || low.Known
	return out
}

// Lookup resolves a model id (or alias) against the merged view.
func (r *Registry) Lookup(id string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if canon, ok := r.alias[id]; ok {
		id = canon
	}
	m, ok := r.resolved[id]
	return m, ok
}

// List returns the merged catalog sorted by id.
func (r *Registry) List() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.resolved))
	for _, m := range r.resolved {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
