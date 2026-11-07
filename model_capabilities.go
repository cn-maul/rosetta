package rosetta

import (
	"encoding/json"
	"strings"
)

// rawModelEntry is the loose-match shape for one /models entry. Vendors
// disagree on capability field names, so every candidate name is accepted and
// the first non-zero one wins; unknown fields are ignored and absent fields
// read as zero. This is "read what the vendor reported", not "guess": a field
// the vendor did not send stays unknown rather than being inferred from the
// model family.
type rawModelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`

	ContextLength       int `json:"context_length"`
	ContextWindow       int `json:"context_window"`
	MaxInputTokens      int `json:"max_input_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
	MaxOutputTokens     int `json:"max_output_tokens"`
	TopProvider         struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`

	ReasoningSupportedEfforts []string `json:"reasoning_supported_efforts"`
	SupportedReasoning        []string `json:"supported_reasoning"`
	SupportedEfforts          []string `json:"supported_efforts"`
	EffortLevelsRaw           []string `json:"effort_levels"`

	SupportsReasoning *bool         `json:"supports_reasoning"`
	SupportsThinking  *bool         `json:"supports_thinking"`
	Reasoning         flexReasoning `json:"reasoning"`
}

// flexReasoning decodes a thinking-capability field that vendors report in
// two incompatible shapes: a bare bool (supports / does not support) or an
// object (OpenRouter's {mandatory, supported_efforts, default_effort}).
//
// Unlike the gateway's strict parser, an unrecognized shape (string, number,
// array) is treated as "unknown" instead of failing the whole catalog. The
// SDK's default path must keep working against a vendor that uses the field
// in some way we have not seen, and "unknown" is the safe reading — it means
// "don't strip thinking on incomplete information".
type flexReasoning struct {
	set   bool
	value bool
	isObj bool
	obj   struct {
		Mandatory        bool     `json:"mandatory"`
		DefaultEnabled   bool     `json:"default_enabled"`
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	}
}

func (f *flexReasoning) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	switch trimmed[0] {
	case 't', 'f':
		var v bool
		if err := json.Unmarshal(b, &v); err != nil {
			return nil // lenient: unrecognized shape reads as unknown
		}
		f.set, f.value, f.isObj = true, v, false
		return nil
	case '{':
		if err := json.Unmarshal(b, &f.obj); err != nil {
			return nil
		}
		f.set, f.isObj = true, true
		return nil
	default:
		// Unknown shape: leave unset. See the type comment.
		return nil
	}
}

// boolPtr returns the three-state result: nil means "not reported".
func (f *flexReasoning) boolPtr() *bool {
	if !f.set {
		return nil
	}
	if !f.isObj {
		v := f.value
		return &v
	}
	// Object form: an explicit effort list means "supports"; otherwise the
	// mandatory flag is the signal. Order matters — a model whose reasoning
	// is optional (mandatory=false) but still lists efforts supports
	// thinking, and reading mandatory first would mislabel it unsupported.
	if len(f.obj.SupportedEfforts) > 0 {
		t := true
		return &t
	}
	v := f.obj.Mandatory
	return &v
}

func (f *flexReasoning) effortLevels() []string { return f.obj.SupportedEfforts }

// contextWindow picks the first non-zero candidate across the vendor field
// names, with the nested top_provider form (OpenRouter) as the fallback.
func (e *rawModelEntry) contextWindow() int {
	if e.ContextLength != 0 {
		return e.ContextLength
	}
	if e.ContextWindow != 0 {
		return e.ContextWindow
	}
	if e.MaxInputTokens != 0 {
		return e.MaxInputTokens
	}
	if e.TopProvider.ContextLength != 0 {
		return e.TopProvider.ContextLength
	}
	return 0
}

func (e *rawModelEntry) maxOutputTokens() int {
	if e.MaxCompletionTokens != 0 {
		return e.MaxCompletionTokens
	}
	if e.MaxOutputTokens != 0 {
		return e.MaxOutputTokens
	}
	if e.TopProvider.MaxCompletionTokens != 0 {
		return e.TopProvider.MaxCompletionTokens
	}
	return 0
}

// effortLevelsFor returns the first non-empty effort set. Some vendors emit
// several of these fields (some empty, some not), so the first non-empty one
// wins rather than the first field — which might land on the empty slice.
func (e *rawModelEntry) effortLevelsFor() []string {
	for _, cand := range [][]string{
		e.ReasoningSupportedEfforts,
		e.SupportedReasoning,
		e.SupportedEfforts,
		e.EffortLevelsRaw,
		// OpenRouter nests supported_efforts inside the reasoning object, not
		// at the top level. It is last: top-level fields, when present, are
		// more authoritative.
		e.Reasoning.effortLevels(),
	} {
		if len(cand) > 0 {
			return cand
		}
	}
	return nil
}

// supportsThinkingFor resolves the three-state flag in a fixed precedence
// order. A vendor reporting two contradictory values must not be guessed at,
// so reasoning wins over thinking, and the reasoning object is the fallback.
func (e *rawModelEntry) supportsThinkingFor() *bool {
	if e.SupportsReasoning != nil {
		return e.SupportsReasoning
	}
	if e.SupportsThinking != nil {
		return e.SupportsThinking
	}
	return e.Reasoning.boolPtr()
}

// parseModelCapabilities reads the capability fields a /models entry reports
// and returns them as a ModelInfo. Identity fields (ID, DisplayName) are left
// unset — the caller owns those. Fields the vendor did not report stay
// zero/unknown, never guessed.
//
// Three-state thinking: ThinkingReported marks whether the vendor declared a
// thinking capability at all (an explicit boolean, or an effort list);
// SupportsThinking then reflects that declaration. When ThinkingReported is
// false the vendor said nothing and the field must not be read as "no".
func parseModelCapabilities(raw json.RawMessage) ModelInfo {
	var e rawModelEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return ModelInfo{}
	}
	out := ModelInfo{
		ContextWindow:   e.contextWindow(),
		MaxOutputTokens: e.maxOutputTokens(),
		EffortLevels:    e.effortLevelsFor(),
	}
	if rep := e.supportsThinkingFor(); rep != nil {
		out.ThinkingReported = true
		out.SupportsThinking = *rep
	}
	if len(out.EffortLevels) > 0 {
		// An effort list is itself a declaration of reasoning capability,
		// even when no explicit boolean was reported (and matches the SDK's
		// existing "levels imply thinking" invariant).
		out.SupportsThinking = true
		out.ThinkingReported = true
	}
	return out
}
