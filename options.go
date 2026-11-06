package rosetta

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RequestInfo describes an outgoing HTTP call to a hook.
type RequestInfo struct {
	Method string
	URL    string
	Header http.Header
}

// ResponseInfo describes the outcome of an outgoing call. Latency is the
// round-trip time through the transport; for streams it stops at the
// response headers, not at the end of the body.
type ResponseInfo struct {
	Method     string
	URL        string
	StatusCode int
	Latency    time.Duration
	RequestID  string
	Err        error
}

// Hooks observe every HTTP call. Both callbacks run on the request path and
// must not block: use them for metrics, tracing and logging, never for
// retries. RequestInfo.Header carries the credentials the request will
// send, so redact before logging.
type Hooks struct {
	OnRequest  func(RequestInfo)
	OnResponse func(ResponseInfo)
}

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
	// NoIdempotencyKey disables the Idempotency-Key header and the
	// transport-level retry of chat POSTs (429/503) that this SDK enables by
	// default. Set it for OpenAI-compatible gateways that reject or
	// mishandle the header, or that must never receive a replayed request.
	// Chat then reverts to the pre-v0.6.0 behavior: a 429/503 surfaces to
	// the caller immediately.
	NoIdempotencyKey bool
}

// settings carries all client configuration.
type settings struct {
	endpoint         string
	apiKey           string
	embedEndpoint    string
	embedAPIKey      string
	protocol         Protocol
	protocolSet      bool // WithProtocol supplied explicitly (B12)
	httpClient       *http.Client
	timeout          time.Duration
	streamIdle       time.Duration
	maxRetries       int
	retryBase        time.Duration
	defaultMaxOutput int
	tracker          UsageTracker
	logger           *slog.Logger
	thinkingFallback bool
	thinkingRectify  bool
	// interleavedThinking is retained for API compatibility only: the
	// interleaved-thinking beta went GA in Anthropic API 2026-10-21, so the
	// value is no longer read by the adapter. WithInterleavedThinking still
	// compiles and is a no-op.
	interleavedThinking *bool
	maxTokensField      string
	anthropicBearer     bool
	strictContext       bool
	extraOverrides      bool
	unknownEvents       bool
	headers             http.Header
	betas               []string
	queryParams         url.Values
	pathPrefix          string
	organization        string
	project             string
	estimates           MultimediaTokenEstimates
	tokenCounter        func(string) int
	hooks               Hooks
	requestIDFunc       func() string
	quirks              Quirks
	modelsFile          string
	manualModels        []ModelInfo
	modelCapability     ModelCapabilityParser
}

// defaultUnaryTimeout bounds unary calls (Chat, ListModels, Embed, Rerank)
// when the caller does not set WithTimeout. A server that accepts the
// connection but never responds would otherwise hang the call forever. It
// is generous enough to leave room for the default retry backoff budget.
// WithTimeout(0) still disables the bound.
const defaultUnaryTimeout = 60 * time.Second

func defaultSettings() *settings {
	return &settings{
		maxRetries:      2,
		thinkingRectify: true,
		timeout:         defaultUnaryTimeout,
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

// WithHeader sets a request header on every call. Headers set here win over
// the SDK's defaults, which is what makes gateway-specific auth (Azure's
// api-key, OpenRouter's HTTP-Referer/X-Title) and trace propagation
// (traceparent) reachable.
func WithHeader(k, v string) Option {
	return func(s *settings) {
		if s.headers == nil {
			s.headers = http.Header{}
		}
		s.headers.Set(k, v)
	}
}

// WithHeaders merges a header set into the same layer as WithHeader. Later
// options win over earlier ones.
func WithHeaders(h http.Header) Option {
	return func(s *settings) {
		if s.headers == nil {
			s.headers = http.Header{}
		}
		for k, vs := range h {
			s.headers[k] = append([]string(nil), vs...)
		}
	}
}

// WithBeta appends Anthropic beta flags to the ones the SDK derives from the
// payload. Values are comma-joined into anthropic-beta; duplicates are left
// alone because the header is idempotent upstream.
func WithBeta(flags ...string) Option {
	return func(s *settings) {
		for _, f := range flags {
			if f = strings.TrimSpace(f); f != "" {
				s.betas = append(s.betas, f)
			}
		}
	}
}

// WithQueryParam appends a query parameter to every request URL. This is how
// Azure OpenAI's required ?api-version= is supplied; endpoints themselves
// still reject a query string, so credentials cannot hide there.
func WithQueryParam(k, v string) Option {
	return func(s *settings) {
		if s.queryParams == nil {
			s.queryParams = url.Values{}
		}
		s.queryParams.Add(k, v)
	}
}

// WithPathPrefix prepends a path segment to every request URL, for gateways
// that mount the API under a sub-path (Azure's /openai, a reverse proxy's
// /llm, ...).
func WithPathPrefix(p string) Option {
	return func(s *settings) {
		p = strings.TrimSpace(p)
		if p == "" || p == "/" {
			s.pathPrefix = ""
			return
		}
		s.pathPrefix = "/" + strings.Trim(p, "/")
	}
}

// WithOrganization sends OpenAI-Organization on OpenAI-family requests.
func WithOrganization(id string) Option {
	return func(s *settings) { s.organization = strings.TrimSpace(id) }
}

// WithProject sends OpenAI-Project on OpenAI-family requests.
func WithProject(id string) Option {
	return func(s *settings) { s.project = strings.TrimSpace(id) }
}

// WithEmbeddingEndpoint overrides the base URL used for Embed and Rerank
// calls, for setups where the chat provider does not serve embeddings —
// e.g. chat on DeepSeek with embeddings on a local Ollama or SiliconFlow.
// When unset, both ride on the main endpoint. Note that Anthropic-protocol
// clients cannot serve embeddings at all; this option is what unlocks
// Embed/Rerank there.
func WithEmbeddingEndpoint(v string) Option {
	return func(s *settings) { s.embedEndpoint = strings.TrimSpace(v) }
}

// WithEmbeddingAPIKey sets the credential used against the embedding
// endpoint when it differs from the chat key.
func WithEmbeddingAPIKey(v string) Option {
	return func(s *settings) { s.embedAPIKey = strings.TrimSpace(v) }
}

// WithProtocol forces a wire protocol. When omitted, it resolves from
// auto-detection (DetectClient), then OpenAI Chat.
func WithProtocol(p Protocol) Option {
	return func(s *settings) { s.protocol = p; s.protocolSet = true }
}

// WithModelInfo installs manual model metadata (highest merge priority in
// the registry). Repeat the option or pass several values at once.
func WithModelInfo(infos ...ModelInfo) Option {
	return func(s *settings) { s.manualModels = append(s.manualModels, infos...) }
}

// WithModelsFile loads manual model metadata from a JSON file with the
// same schema as WithModelInfo: {"models":[...]}. Its entries merge with
// WithModelInfo values; explicit values win on duplicate ids.
func WithModelsFile(path string) Option {
	return func(s *settings) { s.modelsFile = path }
}

// WithModelCapabilityParser installs a hook that fills ModelInfo capability
// fields from raw /models entries during ListModels. Without it ListModels
// parses only id (and display name where the protocol has one), keeping the
// no-guessing default for custom models.
//
// The parser runs on every remote entry for every protocol. Anthropic's
// /v1/models returns no capability fields, so the parser is the only way to
// populate them there (typically by mapping the model id to a known profile).
func WithModelCapabilityParser(fn ModelCapabilityParser) Option {
	return func(s *settings) { s.modelCapability = fn }
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

// WithTimeout bounds unary calls (Chat, ListModels, Embed, Rerank). It
// deliberately does not limit streams, whose lifetime is governed by the
// caller's context. When unset, a default of 60s applies; pass WithTimeout(0)
// to disable the bound entirely.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) { s.timeout = d }
}

// WithStreamIdleTimeout bounds how long a stream may go without delivering
// an event before it is aborted with ErrStreamIdleTimeout. It is the missing
// counterpart to WithTimeout, which deliberately leaves streams alone: a
// total stream budget would punish a legitimately long generation, while
// this only fires when the upstream has gone *silent*.
//
// Unset (or a non-positive value) disables the watchdog — the default,
// since a reasonable ceiling varies by provider and by what the caller is
// streaming. The window is per-Read, not per-stream, so a slow-but-alive
// stream is never cut. Err() then matches ErrStreamIdleTimeout and Partial()
// keeps whatever arrived, so the caller can tell a timeout apart from a
// truncation or a clean end.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d < 0 {
			d = 0
		}
		s.streamIdle = d
	}
}

// WithMaxRetries sets how many times an eligible failed attempt (transport
// error or 408/429/5xx) is retried with backoff. GET-like methods retry by
// default; POST requests require an explicit internal idempotency policy.
// Default 2.
func WithMaxRetries(n int) Option {
	return func(s *settings) {
		if n >= 0 {
			s.maxRetries = n
		}
	}
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

// WithInterleavedThinking is a no-op as of Anthropic API version 2026-10-21:
// interleaved thinking (reasoning between tool calls) went GA and no longer
// needs the "interleaved-thinking-2025-05-14" beta header. It is kept so
// existing call sites keep compiling; the value is ignored.
//
// Deprecated: the SDK now always targets 2026-10-21, where this toggle has
// no effect.
func WithInterleavedThinking(v bool) Option {
	return func(s *settings) { s.interleavedThinking = &v }
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

// WithAnthropicBearerAuth additionally sends "Authorization: Bearer <key>"
// on Anthropic requests. By default only the Anthropic-native "x-api-key"
// header is sent, so the credential is not copied onto a second auth
// channel that proxies and gateways may log differently. Enable this for
// Anthropic-compatible gateways that authenticate exclusively via Bearer.
func WithAnthropicBearerAuth(v bool) Option {
	return func(s *settings) { s.anthropicBearer = v }
}

// WithHooks installs per-call observation callbacks. They fire for every
// HTTP call, including retries and auxiliary APIs.
func WithHooks(h Hooks) Option {
	return func(s *settings) { s.hooks = h }
}

// WithRequestIDFunc injects an X-Request-Id header on every outgoing call,
// so one logical request can be correlated across a gateway and the
// provider. An explicitly configured X-Request-Id (WithHeader) wins; an
// empty return leaves the header unset.
func WithRequestIDFunc(fn func() string) Option {
	return func(s *settings) { s.requestIDFunc = fn }
}

// WithUnknownEvents makes streams emit EventUnknown for provider events the
// SDK does not model, with the original SSE payload in Event.Raw. It is off
// by default because enabling it changes the event sequence a consumer
// sees; turn it on in a gateway that must forward new upstream events
// without waiting for an SDK release.
func WithUnknownEvents(v bool) Option {
	return func(s *settings) { s.unknownEvents = v }
}

// WithExtraOverrides lets ChatRequest/EmbeddingRequest/RerankRequest.Extra
// keys override SDK-managed payload fields ("model", "messages", "stream",
// output caps, ...). By default such collisions are rejected with
// ErrInvalidRequest, because an accidental override silently bypasses
// validation. Enable only when you really mean to rewrite SDK-computed
// fields for a provider quirk.
func WithExtraOverrides(v bool) Option {
	return func(s *settings) { s.extraOverrides = v }
}

// WithTokenCounter plugs a real tokenizer into the local estimator. It is
// used by the context-window check and Client.EstimateTokens, replacing the
// built-in heuristic (CJK ≈1 token, ASCII ≈4 chars). nil restores the
// heuristic. The SDK deliberately ships no tokenizer: tiktoken and friends
// are large, model-specific, and would break the zero-dependency guarantee.
func WithTokenCounter(fn func(string) int) Option {
	return func(s *settings) { s.tokenCounter = fn }
}

// WithMultimediaTokenEstimates overrides the flat per-block token
// estimates (image/audio/file) used by the context-window check. The
// defaults are coarse approximations — tune them to your provider when
// strict context checking rejects valid requests or lets oversized ones
// through. Zero fields keep the defaults (1500 / 500 / 3000).
func WithMultimediaTokenEstimates(e MultimediaTokenEstimates) Option {
	return func(s *settings) { s.estimates = e }
}
