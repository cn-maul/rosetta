package rosetta

import (
	"context"
	"fmt"
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
	if st.endpoint == "" {
		st.endpoint = defaultEndpoint(st.protocol)
	}
	if err := validateEndpoint(st.endpoint); err != nil {
		return nil, fmt.Errorf("rosetta: invalid endpoint %q: %w", st.endpoint, err)
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
	if st.endpoint == "" {
		return nil, ErrNoEndpoint
	}
	if st.apiKey == "" {
		return nil, ErrNoAPIKey
	}

	hx := httpx.New()
	if st.httpClient != nil {
		hx.HTTP = st.httpClient
	}
	hx.MaxRetries = st.maxRetries
	if st.retryBase > 0 {
		hx.Base = st.retryBase
	}
	hx.Logger = st.logger

	c := &Client{settings: st, http: hx, registry: newRegistry()}
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
		c.registry.SetManual(st.manualModels)
		if err := c.registry.Validate(); err != nil {
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

// Chat performs a non-streaming completion. Usage, when returned by the
// provider, is recorded into the configured tracker. Before dispatch the
// request passes through the model registry: thinking configs are gated
// on the model's declared capability, and prompt size is checked against
// the context window (warning by default, error in strict mode).
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	req, err := c.prepare(req)
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
	if err := req.validate(); err != nil {
		return nil, err
	}
	req, err := c.prepare(req)
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
		cancel()
		return stream, nil
	}
	sc.attachCancel(cancel)
	sc.onEnd = func(u Usage, err error) {
		c.record(sc.partial.Model, u, err == nil && u.IsZero())
	}
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
	c.registry.SetRemote(infos)
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
			return nil
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
		c.registry.SetRemote(infos)
	}
	c.modelsMu.Lock()
	c.modelsRefreshing = false
	close(done)
	c.modelsMu.Unlock()
	return err
}

// ModelInfo returns merged metadata for one model id (aliases accepted).
// If the model is unknown to the manual layer, a best-effort remote
// discovery is attempted before failing with ErrUnknownModel.
func (c *Client) ModelInfo(ctx context.Context, id string) (ModelInfo, error) {
	if _, ok := c.registry.Lookup(id); !ok {
		if c.settings.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.settings.timeout)
			defer cancel()
		}
		if infos, err := c.provider.ListModels(ctx); err == nil {
			c.registry.SetRemote(infos)
		}
	}
	mi, ok := c.registry.Lookup(id)
	if !ok {
		return ModelInfo{}, fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}
	return mi, nil
}

// prepare runs registry-backed request gating before dispatch.
func (c *Client) prepare(req *ChatRequest) (*ChatRequest, error) {
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

// checkContext compares the heuristic prompt estimate against the model's
// context window. Default behavior is a warning; strict mode errors.
func (c *Client) checkContext(req *ChatRequest) error {
	mi, ok := c.registry.Lookup(req.Model)
	if !ok || mi.ContextWindow <= 0 {
		return nil
	}
	in := req.estimateInputTokens()
	out := c.effectiveMaxOutput(req)
	if in <= mi.ContextWindow && (out <= 0 || in+out <= mi.ContextWindow) {
		return nil
	}
	if c.settings.strictContext {
		return contextTooLongError(req.Model, in, out, mi.ContextWindow)
	}
	c.settings.logger.Warn("rosetta: prompt may exceed model context window",
		"model", req.Model, "estimated_input", in, "max_output", out, "context_window", mi.ContextWindow)
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

// record feeds one usage observation to the tracker, if any.
func (c *Client) record(model string, u Usage, missing bool) {
	if c.settings.tracker == nil {
		return
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
