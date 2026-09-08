package rosetta

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Registry merges model metadata from two sources, in descending
// priority: manual configuration (WithModelInfo / WithModelsFile) and
// remote discovery (GET /models via ListModels). Higher layers override
// lower ones field by field; empty fields inherit from the layer below.
//
// Following the "custom models are not guessed" principle: entries
// learned purely from /models carry Known=false and no inferred
// capability data, so thinking gating and context validation only apply
// to what was explicitly declared.
type Registry struct {
	mu       sync.RWMutex
	remote   map[string]ModelInfo
	manual   map[string]ModelInfo
	alias    map[string]string
	resolved map[string]ModelInfo
}

func newRegistry() *Registry {
	r := &Registry{
		remote: map[string]ModelInfo{},
		manual: map[string]ModelInfo{},
		alias:  map[string]string{},
	}
	r.rebuild()
	return r
}

// LoadFile loads manual model configuration from a JSON file:
// {"models":[...]} with the same fields as ModelInfo. It replaces the
// manual configuration layer.
func (r *Registry) LoadFile(path string) error {
	infos, err := parseModelsFile(path)
	if err != nil {
		return err
	}
	r.SetManual(infos)
	return nil
}

// parseModelsFile reads and validates a manual model configuration file,
// marking every entry Known. It does not touch the registry, so callers
// can merge the result with other manual sources before installing it.
func parseModelsFile(path string) ([]ModelInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rosetta: reading models file: %w", err)
	}
	var doc struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("rosetta: parsing models file %s: %w", path, err)
	}
	infos := make([]ModelInfo, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.ID != "" {
			m.Known = true
			infos = append(infos, m)
		}
	}
	return infos, nil
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
	resolved := make(map[string]ModelInfo, len(r.remote)+len(r.manual))
	alias := make(map[string]string)
	for _, layer := range []map[string]ModelInfo{r.remote, r.manual} {
		for id, m := range layer {
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
