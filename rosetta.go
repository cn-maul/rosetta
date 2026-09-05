// Package rosetta provides a unified Go client for large-model chat APIs.
// It speaks three wire protocols — OpenAI Chat Completions, OpenAI
// Responses and Anthropic Messages — plus the many third-party services
// compatible with them, behind one small surface.
//
// A client is created with just an endpoint and an API key; protocol
// differences (auth headers, system prompt placement, thinking budgets,
// streaming events, usage field names) are absorbed by the SDK:
//
//	client, err := rosetta.NewClient(
//		rosetta.WithEndpoint("https://api.deepseek.com/v1"),
//		rosetta.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
//	)
//
//	resp, err := client.Chat(ctx, &rosetta.ChatRequest{
//		Model:    "deepseek-chat",
//		Messages: []rosetta.Message{rosetta.User("你好")},
//	})
//
// Streaming follows a pull model: ChatStream returns a Stream whose Next
// method yields events until it reports false, with Err reporting io.EOF
// only through a clean finish. Usage accounting is opt-in via
// WithUsageTracker and queryable through Client.Stats.
//
// The package has zero third-party dependencies.
package rosetta

// Version is the semantic version of this SDK release.
const Version = "0.2.0"

// Protocol identifies one of the supported wire protocols.
type Protocol string

const (
	// ProtoOpenAIChat is POST /v1/chat/completions (OpenAI and most
	// compatible services: DeepSeek, Moonshot, Qwen, GLM, vLLM, ...).
	ProtoOpenAIChat Protocol = "openai-chat"
	// ProtoOpenAIResponses is POST /v1/responses (OpenAI Responses API).
	ProtoOpenAIResponses Protocol = "openai-responses"
	// ProtoAnthropic is POST /v1/messages (Anthropic Messages API).
	ProtoAnthropic Protocol = "anthropic"
)

// String returns the canonical wire name of the protocol.
func (p Protocol) String() string { return string(p) }

// defaultEndpoint returns the official endpoint for a protocol, or "" when
// unknown (in which case the caller must supply one).
func defaultEndpoint(p Protocol) string {
	switch p {
	case ProtoOpenAIChat, ProtoOpenAIResponses:
		return "https://api.openai.com/v1"
	case ProtoAnthropic:
		return "https://api.anthropic.com/v1"
	}
	return ""
}
