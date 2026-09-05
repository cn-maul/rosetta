package rosetta

// ModelInfo describes a model's identity and capability metadata. It comes
// from one of three sources (priority order): manual configuration, remote
// discovery (ListModels), or the built-in knowledge base. Knowledge-base
// entries have Known=true; entries learned from /models have Known=false
// and carry no inferred capability data (no guessing for custom models).
type ModelInfo struct {
	// ID is the provider model id used in requests.
	ID string `json:"id"`
	// DisplayName is a human-friendly name when known.
	DisplayName string `json:"display_name,omitempty"`
	// ContextWindow is the maximum total prompt+output tokens.
	ContextWindow int `json:"context_window,omitempty"`
	// MaxOutputTokens is the per-request output cap.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// SupportsThinking reports whether the model can reason. Meaningful
	// only when Known is true.
	SupportsThinking bool `json:"supports_thinking,omitempty"`
	// Known marks entries backed by the built-in knowledge base or by
	// manual configuration (entries learned from /models are false).
	Known bool `json:"known,omitempty"`
	// Protocol is the protocol this model was listed under.
	Protocol Protocol `json:"protocol,omitempty"`
	// Aliases are alternate ids resolving to this model.
	Aliases []string `json:"aliases,omitempty"`
}
