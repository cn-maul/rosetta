package rosetta

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// protocolProvider is the internal adapter contract. One implementation
// per wire protocol translates unified requests/responses/events.
type protocolProvider interface {
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	StreamChat(ctx context.Context, req *ChatRequest) (Stream, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// Client is a unified client for one endpoint+credential pair. It is safe
// for concurrent use by multiple goroutines.
type Client struct {
	settings         *settings
	http             *httpx.Client
	provider         protocolProvider
	registry         *Registry
	modelsMu         sync.Mutex
	modelsRefreshing bool
	modelsDone       chan struct{}
	modelsErr        error
	// unknownMu guards the short-TTL negative cache of model ids that were
	// not found even after a remote refresh, so a serial batch of lookups
	// for the same unknown id does not hammer /models 1:1 (G7).
	unknownMu sync.Mutex
	unknown   map[string]time.Time
}

// buildSettings applies options and resolves protocol/endpoint defaults.
func buildSettings(opts []Option) (*settings, error) {
	st := defaultSettings()
	for _, o := range opts {
		if o != nil {
			o(st)
		}
	}
	if st.protocol == "" {
		st.protocol = ProtoOpenAIChat
	}
	switch st.protocol {
	case ProtoOpenAIChat, ProtoOpenAIResponses, ProtoAnthropic:
	default:
		return nil, fmt.Errorf("rosetta: unknown protocol %q", st.protocol)
	}
	// The output-cap field name is pinned for OpenAI Chat only; a typo here
	// would otherwise silently drop the cap (unbounded cost/latency), so it
	// is constrained to the two real spellings (audit C5).
	switch st.maxTokensField {
	case "", "max_tokens", "max_completion_tokens":
	default:
		return nil, fmt.Errorf("rosetta: invalid WithMaxTokensField %q (use \"max_tokens\" or \"max_completion_tokens\")", st.maxTokensField)
	}
	if st.endpoint == "" {
		st.endpoint = defaultEndpoint(st.protocol)
	}
	if err := validateEndpoint(st.endpoint); err != nil {
		return nil, fmt.Errorf("rosetta: invalid endpoint %q: %w", displayEndpoint(st.endpoint), err)
	}
	if st.embedEndpoint != "" {
		if err := validateEndpoint(st.embedEndpoint); err != nil {
			return nil, fmt.Errorf("rosetta: invalid embedding endpoint %q: %w", displayEndpoint(st.embedEndpoint), err)
		}
	}
	return st, nil
}

// NewClient builds a Client from options. Endpoint and API key are
// required — explicitly or through the per-protocol official default.
func NewClient(opts ...Option) (*Client, error) {
	st, err := buildSettings(opts)
	if err != nil {
		return nil, err
	}
	return newClientFromSettings(st)
}

// newClientFromSettings builds a Client from already-resolved settings. It
// is shared by NewClient and DetectClient so protocol detection does not
// rebuild settings a second time (M9).
func newClientFromSettings(st *settings) (*Client, error) {
	// Defensive assertion: buildSettings fills a default endpoint for every
	// known protocol and rejects unknown ones, so this is unreachable today.
	// Kept so a future protocol that forgets a default fails loudly rather
	// than sending requests to an empty URL (G10).
	if st.endpoint == "" {
		return nil, ErrNoEndpoint
	}
	if st.apiKey == "" {
		return nil, ErrNoAPIKey
	}

	hx := httpx.New()
	if st.httpClient != nil {
		// Copy so the cross-host redirect guard (A1) does not mutate the
		// caller's client, and honor any CheckRedirect they already set.
		guarded := *st.httpClient
		if guarded.CheckRedirect == nil {
			guarded.CheckRedirect = httpx.CrossHostSafeRedirect
		}
		hx.HTTP = &guarded
	}
	hx.MaxRetries = st.maxRetries
	if st.retryBase > 0 {
		hx.Base = st.retryBase
	}
	hx.Logger = st.logger

	c := &Client{settings: st, http: hx, registry: NewRegistry()}
	if st.modelsFile != "" {
		infos, err := parseModelsFile(st.modelsFile)
		if err != nil {
			return nil, err
		}
		// File entries and explicit WithModelInfo values merge into one
		// manual layer; explicit entries win on duplicate ids.
		st.manualModels = append(infos, st.manualModels...)
	}
	if len(st.manualModels) > 0 {
		if err := c.registry.SetManual(st.manualModels); err != nil {
			return nil, err
		}
	}
	switch st.protocol {
	case ProtoOpenAIChat:
		c.provider = &openaiChatProvider{c: c}
	case ProtoOpenAIResponses:
		c.provider = &openaiResponsesProvider{c: c}
	case ProtoAnthropic:
		c.provider = &anthropicProvider{c: c}
	}
	return c, nil
}

// Protocol returns the wire protocol this client speaks.
func (c *Client) Protocol() Protocol { return c.settings.protocol }

// Endpoint returns the configured API base URL.
func (c *Client) Endpoint() string { return c.settings.endpoint }

// validated returns a client-owned copy of req that has passed validation,
// leaving the caller's struct untouched. validate() caches the serialized
// Extra in an unexported field; writing that into the caller's struct would
// mutate a request the docs let callers share across goroutines (a data race
// on concurrent reuse) and surprise anyone comparing the request before and
// after a call. The shallow copy is sufficient: the cache field is the only
// thing validate() writes, and the copy carries sliced Values by reference
// exactly as the original does.
//
// Only ChatRequest carries such a field, so the copy is applied here rather
// than in the auxiliary families.
func (c *Client) validated(req *ChatRequest) (*ChatRequest, error) {
	cp := new(ChatRequest)
	if req != nil {
		*cp = *req
	}
	if err := cp.validate(); err != nil {
		return nil, err
	}
	return cp, nil
}

// Chat performs a non-streaming completion. Usage, when returned by the
// provider, is recorded into the configured tracker. Before dispatch the
// request passes through the model registry: thinking configs are gated
// on the model's declared capability, and prompt size is checked against
// the context window (warning by default, error in strict mode).
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	req, err := c.validated(req)
	if err != nil {
		return nil, err
	}
	req, err = c.prepare(req)
	if err != nil {
		return nil, err
	}
	if c.settings.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
		defer cancel()
	}
	resp, err := c.provider.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	c.record(resp.Model, resp.Usage, resp.Usage.IsZero())
	return resp, nil
}

// ChatStream starts a streaming completion. The returned Stream must be
// driven (or Closed) by the caller; usage is recorded when the stream
// terminates. Note that the caller's ctx bounds the whole stream, while
// WithTimeout applies only to unary calls.
func (c *Client) ChatStream(ctx context.Context, req *ChatRequest) (Stream, error) {
	req, err := c.validated(req)
	if err != nil {
		return nil, err
	}
	req, err = c.prepare(req)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	stream, err := c.provider.StreamChat(sctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	sc, ok := stream.(*streamCore)
	if !ok {
		// Unreachable today: every provider returns a *streamCore. A future
		// provider that did not would leak its HTTP context and body here —
		// cancel() kills the context and the caller has no closer — so fail
		// loudly instead (audit C11).
		_ = stream.Close()
		cancel()
		return nil, fmt.Errorf("%w: provider returned an unmanaged stream", ErrNotSupported)
	}
	sc.attachCancel(cancel)
	sc.attachOnEnd(func(u Usage, err error) {
		// sc.model() reads under the stream mutex: the callback fires
		// outside the lock, so touching sc.partial directly would be an
		// unsynchronized read whose safety rests only on an implicit
		// ordering argument (the model is what identifies the record). A
		// caller-abandoned stream (Close before usage) is not a
		// missing-usage response, so it is not counted as UsageMissing (G9).
		c.record(sc.model(), u, err == nil && u.IsZero() && !sc.aborted())
	})
	return stream, nil
}

// ListModels fetches the endpoint's model catalog and merges it into the
// registry's remote layer (below manual configuration). The merged catalog
// is returned.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.settings.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
		defer cancel()
	}
	infos, err := c.provider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.registry.SetRemote(infos); err != nil {
		return nil, err
	}
	return c.registry.List(), nil
}

// RefreshModels forces a re-fetch of the remote catalog. It is an alias
// of ListModels kept for API clarity (ListModels always fetches fresh).
func (c *Client) RefreshModels(ctx context.Context) ([]ModelInfo, error) {
	return c.ListModels(ctx)
}

func (c *Client) refreshModels(ctx context.Context) error {
	c.modelsMu.Lock()
	if c.modelsRefreshing {
		done := c.modelsDone
		c.modelsMu.Unlock()
		select {
		case <-done:
			// Waiters share the refresh outcome: a failed refresh must
			// not read as success to whoever waited on it.
			c.modelsMu.Lock()
			err := c.modelsErr
			c.modelsMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.modelsRefreshing = true
	c.modelsDone = make(chan struct{})
	done := c.modelsDone
	c.modelsMu.Unlock()

	infos, err := c.provider.ListModels(ctx)
	if err == nil {
		err = c.registry.SetRemote(infos)
	}
	c.modelsMu.Lock()
	c.modelsRefreshing = false
	c.modelsErr = err
	close(done)
	c.modelsMu.Unlock()
	return err
}

// unknownModelCacheTTL bounds how long a "not found after refresh" model id
// is remembered before the next lookup retries remote discovery. Short
// enough that a model added upstream is picked up promptly, long enough to
// absorb a serial batch of lookups for the same unknown id (G7).
const unknownModelCacheTTL = 30 * time.Second

// ModelInfo returns merged metadata for one model id (aliases accepted).
// If the model is unknown to the manual layer, a best-effort remote
// discovery is attempted before failing with ErrUnknownModel. A short-TTL
// negative cache skips the /models round trip for ids that were already
// confirmed unknown, so a serial batch of lookups does not amplify traffic
// 1:1 (G7).
func (c *Client) ModelInfo(ctx context.Context, id string) (ModelInfo, error) {
	if _, ok := c.registry.Lookup(id); !ok && !c.unknownCached(id) {
		if c.settings.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
			defer cancel()
		}
		// Route through refreshModels so concurrent unknown-model lookups
		// share one /models request instead of stampeding the endpoint.
		if err := c.refreshModels(ctx); err != nil {
			c.settings.logger.Debug("rosetta: remote model discovery failed",
				"model", id, "err", err.Error())
		}
		// After refresh, if the id is still unknown, remember it so the next
		// lookup within the TTL skips the refresh.
		if _, ok := c.registry.Lookup(id); !ok {
			c.rememberUnknown(id)
		}
	}
	mi, ok := c.registry.Lookup(id)
	if !ok {
		return ModelInfo{}, fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}
	return mi, nil
}

// unknownCached reports whether id has a live negative-cache entry.
func (c *Client) unknownCached(id string) bool {
	c.unknownMu.Lock()
	defer c.unknownMu.Unlock()
	exp, ok := c.unknown[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(c.unknown, id)
		return false
	}
	return true
}

// rememberUnknown records a negative-cache entry for id, expiring after
// unknownModelCacheTTL.
func (c *Client) rememberUnknown(id string) {
	c.unknownMu.Lock()
	defer c.unknownMu.Unlock()
	if c.unknown == nil {
		c.unknown = make(map[string]time.Time)
	}
	c.unknown[id] = time.Now().Add(unknownModelCacheTTL)
}

// prepare runs registry-backed request gating before dispatch. It first
// normalizes an alias to its canonical model id so the wire request always
// carries the canonical name — an alias and its full name would otherwise
// each hit the upstream cache separately (upstreams key prefix caches by
// model), guaranteeing a miss for one of them. Unknown models and models
// already canonical pass through unchanged, so no /models probe is
// triggered (G7).
func (c *Client) prepare(req *ChatRequest) (*ChatRequest, error) {
	if mi, ok := c.registry.Lookup(req.Model); ok && mi.ID != "" && mi.ID != req.Model {
		cp := *req
		cp.Model = mi.ID
		req = &cp
	}
	req, err := c.gateThinking(req)
	if err != nil {
		return nil, err
	}
	if err := c.checkContext(req); err != nil {
		return nil, err
	}
	return req, nil
}

// gateThinking enforces the "presets are reliable, custom models are not
// guessed" rule: only Known models with SupportsThinking=false are
// judged. Unsupported thinking either fails (default) or degrades
// silently (WithThinkingFallback).
func (c *Client) gateThinking(req *ChatRequest) (*ChatRequest, error) {
	if req.Thinking == nil {
		return req, nil
	}
	mi, ok := c.registry.Lookup(req.Model)
	if !ok || !mi.Known || mi.SupportsThinking {
		return req, nil
	}
	if c.settings.thinkingFallback {
		c.settings.logger.Warn("rosetta: model does not support thinking; dropping thinking config",
			"model", req.Model)
		cp := *req
		cp.Thinking = nil
		return &cp, nil
	}
	return nil, fmt.Errorf("%w: model %s (pass WithThinkingFallback(true) to degrade silently)",
		ErrThinkingUnsupported, req.Model)
}

// outputResolver is implemented by providers that substitute an output cap of
// their own when the caller leaves MaxOutputTokens unset. Only Anthropic does
// (it requires max_tokens and falls back to 4096, grown further in thinking
// mode), but the hook is generic so the check stays protocol-agnostic.
type outputResolver interface {
	resolvedMaxOutput(req *ChatRequest) int
}

// checkContext compares the heuristic prompt estimate against the model's
// context window. Default behavior is a warning; strict mode errors.
func (c *Client) checkContext(req *ChatRequest) error {
	mi, ok := c.registry.Lookup(req.Model)
	if !ok || mi.ContextWindow <= 0 {
		return nil
	}
	in := req.estimateInputTokens(c.settings.estimates)
	out := c.effectiveMaxOutput(req)
	if out <= 0 {
		// Zero is "let the provider decide", but on Anthropic the provider
		// decides a concrete 4096 (or more), so the output side would
		// otherwise never participate in the check on that protocol.
		if r, ok := c.provider.(outputResolver); ok {
			out = r.resolvedMaxOutput(req)
		}
	}
	// Wrap-free overrun test: `in+out <= window` overflows to a negative when
	// out is near MaxInt, wrongly reading a huge request as "fits" (audit B14).
	if in > mi.ContextWindow || (out > 0 && out > mi.ContextWindow-in) {
		if c.settings.strictContext {
			return contextTooLongError(req.Model, in, out, mi.ContextWindow)
		}
		c.settings.logger.Warn("rosetta: prompt may exceed model context window",
			"model", req.Model, "estimated_input", in, "max_output", out, "context_window", mi.ContextWindow)
	}
	return nil
}

func contextTooLongError(model string, in, out, window int) error {
	return fmt.Errorf("%w: model %s estimated input %d + output %d > context %d",
		ErrContextTooLong, model, in, out, window)
}

// Stats returns a snapshot of accumulated usage. It reports zeros when no
// tracker is configured.
func (c *Client) Stats() UsageSnapshot {
	if c.settings.tracker == nil {
		return UsageSnapshot{}
	}
	return c.settings.tracker.Snapshot()
}

// record feeds one usage observation to the tracker, if any. The model id
// is normalized to its canonical form first so an alias request and its
// full-name twin are accounted under one key instead of being split across
// two (M2).
func (c *Client) record(model string, u Usage, missing bool) {
	if c.settings.tracker == nil {
		return
	}
	if mi, ok := c.registry.Lookup(model); ok && mi.ID != "" {
		model = mi.ID
	}
	c.settings.tracker.Record(context.Background(), UsageRecord{
		Time:         time.Now(),
		Protocol:     c.settings.protocol,
		Model:        model,
		Usage:        u,
		UsageMissing: missing,
	})
}

// effectiveMaxOutput resolves the output cap: request value, then the
// model's declared metadata, then the client default. Zero means "let the
// provider decide" (only safe on OpenAI protocols; Anthropic substitutes
// its own floor of 4096).
func (c *Client) effectiveMaxOutput(req *ChatRequest) int {
	if req.MaxOutputTokens > 0 {
		return req.MaxOutputTokens
	}
	if mi, ok := c.registry.Lookup(req.Model); ok && mi.MaxOutputTokens > 0 {
		return mi.MaxOutputTokens
	}
	return c.settings.defaultMaxOutput
}

// newIdempotencyKey returns a fresh random key for an Idempotency-Key
// header, unique per logical request so a transport-layer retry of the same
// request can be deduplicated by the provider (M1). It is not used for
// security, only for uniqueness, so math/rand is sufficient.
func newIdempotencyKey() string {
	return fmt.Sprintf("rosetta-%016x-%016x", rand.Uint64(), rand.Uint64())
}
