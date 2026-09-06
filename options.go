package rosetta

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Quirks adjusts for third-party services whose OpenAI compatibility is
// imperfect. Most services need nothing; set quirks when a vendor is known
// to deviate.
type Quirks struct {
	// LegacyMaxTokens sends "max_tokens" instead of
	// "max_completion_tokens" (for services predating the newer field).
	// When unset, the SDK probes: it starts with max_completion_tokens
	// and falls back once on a matching 400 error (sticky per client).
	LegacyMaxTokens bool
	// NoStreamUsage skips stream_options.include_usage for services that
	// reject the field. Without it, usage still arrives when the vendor
	// sends a usage chunk unprompted; otherwise stream usage is missing.
	NoStreamUsage bool
}

// settings carries all client configuration.
type settings struct {
	endpoint         string
	apiKey           string
	protocol         Protocol
	httpClient       *http.Client
	timeout          time.Duration
	maxRetries       int
	retryBase        time.Duration
	defaultMaxOutput int
	tracker          UsageTracker
	logger           *slog.Logger
	thinkingFallback bool
	thinkingRectify  bool
	maxTokensField   string
	strictContext    bool
	quirks           Quirks
	modelsFile       string
	manualModels     []ModelInfo
}

func defaultSettings() *settings {
	return &settings{
		maxRetries:      2,
		thinkingRectify: true,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Option configures a Client. Options are applied in order at NewClient.
type Option func(*settings)

// WithEndpoint sets the API base URL, e.g. "https://api.openai.com/v1",
// "https://api.deepseek.com/v1", "http://localhost:11434". A version path
// (/v1, /api/paas/v4, ...) is preserved; a bare host gets "/v1" appended.
func WithEndpoint(v string) Option {
	return func(s *settings) { s.endpoint = strings.TrimSpace(v) }
}

// WithAPIKey sets the credential. It is sent as "Authorization: Bearer ..."
// on OpenAI protocols and "x-api-key" on Anthropic.
func WithAPIKey(v string) Option {
	return func(s *settings) { s.apiKey = strings.TrimSpace(v) }
}

// WithProtocol forces a wire protocol. When omitted, it resolves from
// auto-detection (DetectClient), then OpenAI Chat.
func WithProtocol(p Protocol) Option {
	return func(s *settings) { s.protocol = p }
}

// WithModelInfo installs manual model metadata (highest merge priority in
// the registry). Repeat the option or pass several values at once.
func WithModelInfo(infos ...ModelInfo) Option {
	return func(s *settings) { s.manualModels = append(s.manualModels, infos...) }
}

// WithModelsFile loads manual model metadata from a JSON file with the
// same schema as the built-in knowledge base: {"models":[...]}.
func WithModelsFile(path string) Option {
	return func(s *settings) { s.modelsFile = path }
}

// WithStrictContextCheck turns context-window overruns into
// ErrContextTooLong errors instead of warnings. The estimate is a
// heuristic (see EstimateTokens), so the default is warn-only.
func WithStrictContextCheck(v bool) Option {
	return func(s *settings) { s.strictContext = v }
}

// WithHTTPClient replaces the underlying *http.Client (proxy, custom TLS,
// transport tuning). Timeouts on it apply to every request including
// streams — prefer WithTimeout/context deadlines instead.
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithTimeout bounds unary calls (Chat, ListModels). It deliberately does
// not limit streams, whose lifetime is governed by the caller's context.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) { s.timeout = d }
}

// WithMaxRetries sets how many times a failed attempt (transport error or
// 408/429/5xx) is retried with backoff. Streams are retried only before
// the first byte of the response body. Default 2.
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n }
}

// WithRetryBase sets the initial backoff duration (doubled per attempt,
// ±20% jitter, capped at 8s). Default 400ms. Zero values are ignored.
func WithRetryBase(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.retryBase = d
		}
	}
}

// WithDefaultMaxOutputTokens supplies the output cap used when a
// ChatRequest leaves MaxOutputTokens unset. Mainly useful for Anthropic,
// where the field is mandatory.
func WithDefaultMaxOutputTokens(n int) Option {
	return func(s *settings) { s.defaultMaxOutput = n }
}

// WithUsageTracker attaches a usage sink; pass NewMemoryUsageTracker() to
// enable Client.Stats. Without a tracker, no usage bookkeeping happens.
func WithUsageTracker(t UsageTracker) Option {
	return func(s *settings) { s.tracker = t }
}

// WithLogger receives debug-level SDK logs (retries, protocol fallbacks,
// quirks probing). Pass slog.Default() to see them.
func WithLogger(l *slog.Logger) Option {
	return func(s *settings) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithThinkingFallback makes unsupported-thinking requests degrade
// silently instead of failing with ErrThinkingUnsupported (effective once
// model capabilities are known via the registry).
func WithThinkingFallback(v bool) Option {
	return func(s *settings) { s.thinkingFallback = v }
}

// WithThinkingRectify enables the reactive Anthropic budget rectifier: on
// a 400 error citing thinking/budget constraints the SDK rewrites the
// budget once and retries. Default true.
func WithThinkingRectify(v bool) Option {
	return func(s *settings) { s.thinkingRectify = v }
}

// WithMaxTokensField pins the output-cap field name for OpenAI Chat
// ("max_completion_tokens" or "max_tokens"), bypassing probe/fallback.
func WithMaxTokensField(v string) Option {
	return func(s *settings) { s.maxTokensField = v }
}

// WithQuirks applies explicit compatibility adjustments for the endpoint.
func WithQuirks(q Quirks) Option {
	return func(s *settings) { s.quirks = q }
}
