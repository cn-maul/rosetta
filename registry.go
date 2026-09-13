package rosetta

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
//
// Installed and returned ModelInfo values are snapshots: Aliases slices
// are copied on the way in and on the way out, so callers can never
// mutate registry state behind its lock.
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
	r.resolved, r.alias, _ = buildView(r.remote, r.manual)
	return r
}

// LoadFile loads manual model configuration from a JSON file:
// {"models":[...]} with the same fields as ModelInfo. It replaces the
// manual configuration layer. Alias conflicts are rejected.
func (r *Registry) LoadFile(path string) error {
	infos, err := parseModelsFile(path)
	if err != nil {
		return err
	}
	return r.SetManual(infos)
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

// SetManual installs (replacing) the manual configuration layer. It
// rejects states in which one alias resolves to several canonical ids or
// shadows another model id, leaving the previous configuration intact.
func (r *Registry) SetManual(infos []ModelInfo) error {
	layer := map[string]ModelInfo{}
	for _, m := range infos {
		if m.ID == "" {
			continue
		}
		m.Known = true
		normalizeInfo(&m)
		layer[m.ID] = cloneModelInfo(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved, alias, err := buildView(r.remote, layer)
	if err != nil {
		return err
	}
	r.manual = layer
	r.resolved = resolved
	r.alias = alias
	return nil
}

// SetRemote installs (replacing) the discovery layer. It rejects alias
// conflicts for the same reason as SetManual.
func (r *Registry) SetRemote(infos []ModelInfo) error {
	layer := map[string]ModelInfo{}
	for _, m := range infos {
		if m.ID == "" {
			continue
		}
		m.Known = false
		normalizeInfo(&m)
		layer[m.ID] = cloneModelInfo(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved, alias, err := buildView(layer, r.manual)
	if err != nil {
		return err
	}
	r.remote = layer
	r.resolved = resolved
	r.alias = alias
	return nil
}

// normalizeInfo applies field invariants to one installed entry:
// DisableThinking is the explicit revocation switch and must hold even
// when no cross-layer merge happens for the entry.
func normalizeInfo(m *ModelInfo) {
	if m.DisableThinking {
		m.SupportsThinking = false
	}
}

// buildView merges the remote and manual layers into the resolved catalog
// and alias index. It is deterministic: the same inputs always produce the
// same view, and any alias claimed by two different canonical ids — within
// a layer, across layers, or shadowing another model id — is an error
// rather than a silent map-order-dependent overwrite.
func buildView(remote, manual map[string]ModelInfo) (map[string]ModelInfo, map[string]string, error) {
	resolved := make(map[string]ModelInfo, len(remote)+len(manual))
	alias := make(map[string]string)
	for _, layer := range []map[string]ModelInfo{remote, manual} {
		for id, m := range layer {
			if prev, ok := resolved[id]; ok {
				resolved[id] = mergeInfo(m, prev)
			} else {
				resolved[id] = m
			}
			for _, a := range m.Aliases {
				if a == "" {
					continue
				}
				if prev, ok := alias[a]; ok && prev != id {
					return nil, nil, fmt.Errorf("rosetta: alias %q maps to both %q and %q", a, prev, id)
				}
				alias[a] = id
			}
		}
	}
	for a, id := range alias {
		if other, ok := resolved[a]; ok && other.ID != id {
			return nil, nil, fmt.Errorf("rosetta: alias %q shadows model id %q", a, other.ID)
		}
	}
	return resolved, alias, nil
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
	// Boolean OR merge cannot express "not supported"; DisableThinking is
	// the explicit revocation knob — a sparse manual entry can thus veto
	// the remote catalog's SupportsThinking=true.
	out.DisableThinking = high.DisableThinking || low.DisableThinking
	if out.DisableThinking {
		out.SupportsThinking = false
	}
	if out.Protocol == "" {
		out.Protocol = low.Protocol
	}
	if out.Type == "" {
		out.Type = low.Type
	}
	if len(out.Aliases) == 0 {
		out.Aliases = low.Aliases
	}
	out.Known = high.Known || low.Known
	return out
}

// Validate checks that aliases resolve deterministically without shadowing
// another canonical model id.
func (r *Registry) Validate() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, _, err := buildView(r.remote, r.manual)
	return err
}

// Lookup resolves a model id (or alias) against the merged view. The
// returned ModelInfo is a snapshot; mutating it never affects the registry.
func (r *Registry) Lookup(id string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if canon, ok := r.alias[id]; ok {
		id = canon
	}
	m, ok := r.resolved[id]
	if !ok {
		return ModelInfo{}, false
	}
	return cloneModelInfo(m), true
}

// List returns the merged catalog sorted by id. Entries are snapshots.
func (r *Registry) List() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.resolved))
	for _, m := range r.resolved {
		out = append(out, cloneModelInfo(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// cloneModelInfo copies m deeply enough that no caller-visible slice or map
// aliases registry state.
func cloneModelInfo(m ModelInfo) ModelInfo {
	m.Aliases = slices.Clone(m.Aliases)
	return m
}
