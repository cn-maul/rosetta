package rosetta

// Model type constants for ModelInfo.Type. Declare a model's type through
// WithModelInfo or the models file; OpenAI-family /models listings carry
// no type data, so the SDK never guesses it.
const (
	// ModelTypeChat marks conversational models (the default assumption).
	ModelTypeChat = "chat"
	// ModelTypeEmbedding marks text-embedding models (served via
	// Client.Embed).
	ModelTypeEmbedding = "embedding"
	// ModelTypeRerank marks reranking models (served via Client.Rerank).
	ModelTypeRerank = "rerank"
)

// ModelInfo describes a model's identity and capability metadata. It comes
// from two sources in priority order: manual configuration (WithModelInfo
// / WithModelsFile) and remote discovery (ListModels). Manual entries have
// Known=true; entries learned from /models have Known=false and carry no
// inferred capability data (no guessing for custom models).
type ModelInfo struct {
	// ID is the provider model id used in requests.
	ID string `json:"id"`
	// DisplayName is a human-friendly name when known.
	DisplayName string `json:"display_name,omitempty"`
	// Type classifies the model: "chat", "embedding" or "rerank"
	// (ModelType* constants). Empty means undeclared.
	Type string `json:"type,omitempty"`
	// ContextWindow is the maximum total prompt+output tokens.
	ContextWindow int `json:"context_window,omitempty"`
	// MaxOutputTokens is the per-request output cap.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// SupportsThinking reports whether the model can reason. Meaningful
	// only when Known is true.
	SupportsThinking bool `json:"supports_thinking,omitempty"`
	// DisableThinking explicitly revokes a thinking claim inherited from a
	// lower-priority layer: a sparse manual entry cannot unset a remote
	// entry's SupportsThinking=true (merge uses OR semantics), so set this
	// on the manual entry to force SupportsThinking=false. Without it, a
	// wrong remote claim would let thinking configs through to a model
	// that cannot reason.
	DisableThinking bool `json:"disable_thinking,omitempty"`
	// Known marks entries backed by manual configuration (entries learned
	// from /models are false).
	Known bool `json:"known,omitempty"`
	// Protocol is the protocol this model was listed under.
	Protocol Protocol `json:"protocol,omitempty"`
	// Aliases are alternate ids resolving to this model.
	Aliases []string `json:"aliases,omitempty"`
}
