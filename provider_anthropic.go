package rosetta

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cn-maul/rosetta/internal/httpx"
	"github.com/cn-maul/rosetta/internal/jsonx"
	"github.com/cn-maul/rosetta/internal/sse"
)

// anthropicProvider adapts the Anthropic Messages protocol
// (POST /v1/messages), including thinking budgets and their constraints.
type anthropicProvider struct{ c *Client }

const anthropicVersion = "2023-06-01"

// extendedCacheTTLBeta gates Anthropic's 1-hour prompt-cache TTL: a request
// carrying cache_control with ttl "1h" is rejected unless this beta header is
// present.
const extendedCacheTTLBeta = "extended-cache-ttl-2025-04-11"

// maxCacheBreakpoints is Anthropic's hard limit on cache_control blocks per
// request; exceeding it is a 400 upstream, so it is caught locally first.
const maxCacheBreakpoints = 4

func (p *anthropicProvider) headers(stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	if key := p.c.settings.apiKey; key != "" {
		// x-api-key is the Anthropic-native auth header and the default.
		// WithAnthropicBearerAuth additionally sends Bearer for
		// gateways that authenticate exclusively that way.
		h.Set("x-api-key", key)
		if p.c.settings.anthropicBearer {
			h.Set("Authorization", "Bearer "+key)
		}
	}
	h.Set("anthropic-version", anthropicVersion)
	return h
}

// needsExtendedCacheTTL reports whether any cache breakpoint in the request
// asks for the 1-hour TTL, which Anthropic gates behind a beta header.
func needsExtendedCacheTTL(req *ChatRequest) bool {
	hourly := func(c *CacheControl) bool { return c != nil && c.TTL == "1h" }
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if hourly(b.CacheControl) {
				return true
			}
		}
	}
	for _, t := range req.Tools {
		if hourly(t.CacheControl) {
			return true
		}
	}
	return false
}

// countCacheControl walks a built payload counting cache_control breakpoints
// so the wire request can be checked against Anthropic's four-breakpoint
// ceiling — which is enforced on what actually reaches the API, so an
// empty-text block whose breakpoint was dropped is not counted.
func countCacheControl(v any) int {
	switch t := v.(type) {
	case map[string]any:
		n := 0
		for k, val := range t {
			if k == "cache_control" {
				n++
				continue
			}
			n += countCacheControl(val)
		}
		return n
	case []any:
		n := 0
		for _, item := range t {
			n += countCacheControl(item)
		}
		return n
	case []map[string]any:
		// The Anthropic builder types messages, tools and content blocks as
		// []map[string]any, not []any; without this case the walk stopped at
		// the payload root and the guard always counted 0.
		n := 0
		for _, item := range t {
			n += countCacheControl(item)
		}
		return n
	default:
		return 0
	}
}

// anthroPlan is the per-request resolution of max_tokens and thinking
// budget. It is computed once (proactive clamping) and may be rewritten
// once by the reactive rectifier when the upstream rejects the budget.
type anthroPlan struct {
	budget    int // 0 = thinking not requested
	maxTokens int
	rectified bool
}

func (p *anthropicProvider) plan(req *ChatRequest) *anthroPlan {
	pl := &anthroPlan{maxTokens: p.c.effectiveMaxOutput(req)}
	if pl.maxTokens <= 0 {
		pl.maxTokens = 4096
	}
	if req.Thinking == nil {
		return pl
	}
	b := req.Thinking.BudgetTokens
	if b <= 0 {
		b = anthropicBudget(req.effort())
	}
	if b < 1024 {
		b = 1024 // Anthropic floor
	}
	if b >= pl.maxTokens {
		// Thinking budget must be strictly less than max_tokens; grow
		// the output cap rather than silently shrinking the budget.
		pl.maxTokens = b + 4096
	}
	pl.budget = b
	return pl
}

// rectify rewrites the budget once on a 400 error that cites thinking
// budget constraints (borrowed from cc-switch's thinking_budget_rectifier).
func (pl *anthroPlan) rectify(msg string) bool {
	if pl.budget == 0 || pl.rectified {
		return false
	}
	low := strings.ToLower(msg)
	if !containsAny(low, "budget_tokens") || !containsAny(low, "thinking") {
		return false
	}
	pl.budget = 32000
	if pl.maxTokens < 32001 {
		pl.maxTokens = 64000
	}
	pl.rectified = true
	return true
}

// buildPayload renders the unified request into the Anthropic wire payload.
func (p *anthropicProvider) buildPayload(req *ChatRequest, stream bool, pl *anthroPlan) (map[string]any, error) {
	system, msgs, err := p.encodeMessages(req)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":      req.Model,
		"messages":   msgs,
		"max_tokens": pl.maxTokens,
	}
	if system != nil {
		payload["system"] = system
	}
	if req.Thinking != nil {
		payload["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": pl.budget,
		}
		// Thinking mode forbids sampling overrides; drop them quietly
		// (the API would otherwise 400 on temperature != 1).
		if req.Temperature != nil && *req.Temperature != 1 {
			p.c.settings.logger.Debug("anthropic: dropping temperature in thinking mode")
		}
		if req.TopP != nil {
			p.c.settings.logger.Debug("anthropic: dropping top_p in thinking mode")
		}
	} else {
		if req.Temperature != nil {
			payload["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			payload["top_p"] = *req.TopP
		}
	}
	if len(req.StopSequences) > 0 {
		payload["stop_sequences"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": schema,
			})
			if cc := t.CacheControl.toWire(); cc != nil {
				tools[len(tools)-1]["cache_control"] = cc
			}
		}
		payload["tools"] = tools
	}
	if stream {
		payload["stream"] = true
	}
	if err := mergeExtra(payload, req.Extra, p.c.settings.extraOverrides, anthropicReservedPayloadKeys); err != nil {
		return nil, err
	}
	if n := countCacheControl(payload); n > maxCacheBreakpoints {
		return nil, fmt.Errorf("%w: anthropic allows at most %d cache breakpoints, request has %d", ErrInvalidRequest, maxCacheBreakpoints, n)
	}
	return payload, nil
}

// encodeMessages maps unified messages onto Anthropic's shape: system
// content is lifted to the top-level system field, tool results ride
// inside user turns, and consecutive same-role messages are merged
// (Anthropic enforces strict role alternation).
//
// The system value is returned as a plain string unless a caller marked a
// cache breakpoint on a system segment, in which case it becomes Anthropic's
// array-of-text-blocks form so the cache_control can ride along. It is nil
// when there is no system content.
func (p *anthropicProvider) encodeMessages(req *ChatRequest) (any, []map[string]any, error) {
	var sysParts []anthroSysPart
	if req.System != "" {
		sysParts = append(sysParts, anthroSysPart{text: req.System})
	}
	type mapped struct {
		role   string
		blocks []Block
	}
	var msgs []mapped
	add := func(role string, blocks []Block) {
		if n := len(msgs); n > 0 && msgs[n-1].role == role {
			msgs[n-1].blocks = append(msgs[n-1].blocks, blocks...)
			return
		}
		msgs = append(msgs, mapped{role: role, blocks: blocks})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if t := m.text(); t != "" {
				sysParts = append(sysParts, anthroSysPart{text: t, cc: firstCacheControl(m.Blocks)})
			}
		case RoleUser:
			add("user", m.Blocks)
		case RoleTool:
			add("user", m.Blocks) // tool_result blocks belong to a user turn
		case RoleAssistant:
			add("assistant", m.Blocks)
		default:
			return nil, nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidRequest, m.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, nil, fmt.Errorf("%w: anthropic requires at least one non-system message", ErrInvalidRequest)
	}
	putCC := func(m map[string]any, b Block) {
		if cc := b.CacheControl.toWire(); cc != nil {
			m["cache_control"] = cc
		}
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]map[string]any, 0, len(m.blocks))
		for _, b := range m.blocks {
			switch b.Type {
			case BlockText:
				if b.Text != "" {
					blk := map[string]any{"type": "text", "text": b.Text}
					putCC(blk, b)
					blocks = append(blocks, blk)
				}
			case BlockImage:
				src, err := encodeAnthropicImage(b.ImageURL)
				if err != nil {
					return nil, nil, err
				}
				blk := map[string]any{"type": "image", "source": src}
				putCC(blk, b)
				blocks = append(blocks, blk)
			case BlockAudio:
				return nil, nil, fmt.Errorf("%w: anthropic does not support audio input", ErrInvalidRequest)
			case BlockFile:
				doc, err := encodeAnthropicDocument(b)
				if err != nil {
					return nil, nil, err
				}
				putCC(doc, b)
				blocks = append(blocks, doc)
			case BlockToolResult:
				tr := map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ToolCallID,
					"content":     b.Content,
				}
				if b.IsError {
					tr["is_error"] = true
				}
				putCC(tr, b)
				blocks = append(blocks, tr)
			case BlockThinking:
				if m.role != "assistant" {
					continue
				}
				// Anthropic rejects replayed thinking blocks that lack a
				// valid signature, so an unsigned block (e.g. one the caller
				// hand-authored) is dropped rather than poisoning the turn.
				if b.Signature == "" {
					p.c.settings.logger.Debug("anthropic: dropping unsigned thinking block on replay")
					continue
				}
				blocks = append(blocks, map[string]any{
					"type":      "thinking",
					"thinking":  b.Thinking,
					"signature": b.Signature,
				})
			case BlockRedactedThinking:
				// Opaque redacted reasoning must be replayed verbatim to keep
				// the assistant turn's block sequence valid.
				if m.role == "assistant" && b.Thinking != "" {
					blocks = append(blocks, map[string]any{
						"type": "redacted_thinking",
						"data": b.Thinking,
					})
				}
			case BlockToolCall:
				if m.role == "assistant" {
					tu := map[string]any{
						"type":  "tool_use",
						"id":    b.ToolCallID,
						"name":  b.ToolName,
						"input": parseToolInput(b.Arguments),
					}
					putCC(tu, b)
					blocks = append(blocks, tu)
				}
			}
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, map[string]any{"role": m.role, "content": blocks})
	}
	// Checked after empty messages are dropped: a user message with no
	// renderable blocks must not leave an assistant turn first (Anthropic
	// requires the conversation to open with a user turn).
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%w: anthropic requires at least one non-system message", ErrInvalidRequest)
	}
	if out[0]["role"] != "user" {
		return nil, nil, fmt.Errorf("%w: anthropic requires the first message to be user role", ErrInvalidRequest)
	}
	return renderAnthropicSystem(sysParts), out, nil
}

// firstCacheControl returns the first non-nil breakpoint among the blocks,
// so a caller can mark a whole system segment by tagging any of its blocks.
func firstCacheControl(blocks []Block) *CacheControl {
	for _, b := range blocks {
		if b.CacheControl != nil {
			return b.CacheControl
		}
	}
	return nil
}

// anthroSysPart is one system segment (from req.System or a RoleSystem
// message) with an optional cache breakpoint lifted from its blocks.
type anthroSysPart struct {
	text string
	cc   *CacheControl
}

// renderAnthropicSystem collapses system segments into Anthropic's system
// field: a joined string when no segment carries a cache breakpoint (the
// wire-identical default), otherwise an array of text blocks so the
// breakpoints survive. Returns nil when there is no system content.
func renderAnthropicSystem(parts []anthroSysPart) any {
	if len(parts) == 0 {
		return nil
	}
	hasCC := false
	for _, s := range parts {
		if s.cc != nil {
			hasCC = true
			break
		}
	}
	if !hasCC {
		texts := make([]string, len(parts))
		for i, s := range parts {
			texts[i] = s.text
		}
		return strings.Join(texts, "\n\n")
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, s := range parts {
		blk := map[string]any{"type": "text", "text": s.text}
		if cc := s.cc.toWire(); cc != nil {
			blk["cache_control"] = cc
		}
		blocks = append(blocks, blk)
	}
	return blocks
}

// encodeAnthropicImage converts an image reference into Anthropic's
// source object: data: URLs become base64 sources, http(s) URLs pass
// through as url sources.
func encodeAnthropicImage(imageURL string) (map[string]any, error) {
	if media, data, ok := splitDataURL(imageURL); ok {
		return map[string]any{
			"type":       "base64",
			"media_type": media,
			"data":       data,
		}, nil
	}
	if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		return map[string]any{"type": "url", "url": imageURL}, nil
	}
	return nil, fmt.Errorf("%w: unsupported image reference %q", ErrInvalidRequest, imageURL)
}

// encodeAnthropicDocument converts a file reference into Anthropic's
// document block: uploaded ids become file sources, http(s) URLs url
// sources, inline base64/data-URL content base64 sources. FileName rides
// along as the optional title. Plain-text files use Anthropic's text
// source (with the decoded text); the official base64 source is defined
// for PDFs only, so other media types are rejected locally instead of
// being mis-encoded and failing upstream.
func encodeAnthropicDocument(b Block) (map[string]any, error) {
	var src map[string]any
	switch {
	case b.FileID != "":
		src = map[string]any{"type": "file", "file_id": b.FileID}
	case strings.HasPrefix(b.FileData, "http://") || strings.HasPrefix(b.FileData, "https://"):
		src = map[string]any{"type": "url", "url": b.FileData}
	case b.FileData != "":
		media, data := b.MimeType, b.FileData
		if m, d, ok := splitDataURL(b.FileData); ok {
			media, data = m, d
		}
		if media == "" {
			media = "application/pdf"
		}
		switch media {
		case "application/pdf":
			src = map[string]any{"type": "base64", "media_type": media, "data": data}
		case "text/plain":
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return nil, fmt.Errorf("%w: text/plain file data must be valid base64: %v", ErrInvalidRequest, err)
			}
			src = map[string]any{"type": "text", "media_type": media, "data": string(decoded)}
		default:
			return nil, fmt.Errorf("%w: anthropic document sources support application/pdf (base64), text/plain (text source) and http(s) PDF URLs, not %q", ErrInvalidRequest, media)
		}
	default:
		return nil, fmt.Errorf("%w: file block needs FileData or FileID", ErrInvalidRequest)
	}
	doc := map[string]any{"type": "document", "source": src}
	if b.FileName != "" {
		doc["title"] = b.FileName
	}
	return doc, nil
}

func splitDataURL(u string) (media, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := u[len(prefix):]
	before, after, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	head := before
	if !strings.HasSuffix(head, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(head, ";base64"), after, true
}

// parseToolInput converts a tool-call arguments JSON string into an object
// (Anthropic expects input as a JSON value, not a string).
func parseToolInput(args string) map[string]any {
	var m map[string]any
	if args != "" && json.Unmarshal([]byte(args), &m) == nil && m != nil {
		return m
	}
	return map[string]any{}
}

// Chat performs a non-streaming completion.
func (p *anthropicProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/messages")
	pl := p.plan(req)
	hdr := p.headers(false)
	if needsExtendedCacheTTL(req) {
		hdr.Set("anthropic-beta", extendedCacheTTLBeta)
	}
	for {
		payload, err := p.buildPayload(req, false, pl)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: hdr,
			Body:   func() ([]byte, error) { return json.Marshal(payload) },
		}
		resp, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, url)
		}
		body, rerr := readBody(resp, method, url, bodyLimit)
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode != http.StatusOK {
			apiErr := parseAnthropicError(resp.StatusCode, body, method, url, requestID(resp.Header))
			if resp.StatusCode == http.StatusBadRequest && p.c.settings.thinkingRectify && pl.rectify(apiErr.Message) {
				p.c.settings.logger.Debug("anthropic: rectifying thinking budget and retrying",
					"budget", pl.budget, "max_tokens", pl.maxTokens)
				continue
			}
			return nil, apiErr
		}
		return decodeAnthropicResponse(body, method, url, requestID(resp.Header))
	}
}

// StreamChat starts a streaming completion.
func (p *anthropicProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/messages")
	pl := p.plan(req)
	hdr := p.headers(true)
	if needsExtendedCacheTTL(req) {
		hdr.Set("anthropic-beta", extendedCacheTTLBeta)
	}
	var resp *http.Response
	for {
		payload, err := p.buildPayload(req, true, pl)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: hdr,
			Body:   func() ([]byte, error) { return json.Marshal(payload) },
		}
		r, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, url)
		}
		if r.StatusCode == http.StatusOK {
			resp = r
			break
		}
		body, rerr := readBody(r, method, url, bodyLimit)
		if rerr != nil {
			return nil, rerr
		}
		apiErr := parseAnthropicError(r.StatusCode, body, method, url, requestID(r.Header))
		if r.StatusCode == http.StatusBadRequest && p.c.settings.thinkingRectify && pl.rectify(apiErr.Message) {
			continue
		}
		return nil, apiErr
	}
	reqID := requestID(resp.Header)
	if bufferedJSONResponse(resp.Header.Get("Content-Type")) {
		body, rerr := readBody(resp, method, url, bodyLimit)
		if rerr != nil {
			return nil, rerr
		}
		cr, derr := decodeAnthropicResponse(body, method, url, reqID)
		if derr != nil {
			return nil, derr
		}
		return bufferedStream(cr), nil
	}
	s := newStream(p.streamEvents(resp.Body, method, url, reqID), nil)
	s.attachCloser(resp.Body)
	return s, nil
}

// streamEvents maps Anthropic's typed SSE stream onto unified events.
// Input tokens arrive with message_start, output tokens with
// message_delta; both are folded into a single EventMessageEnd emitted at
// message_stop. An EOF before message_stop still yields the end event
// (with StopOther) but the stream then fails with ErrStreamTruncated, so
// callers never mistake a cut-off response for a clean finish.
func (p *anthropicProvider) streamEvents(body io.Reader, method, url, requestID string) func() (*Event, error) {
	sc := sse.New(body)
	var (
		stop      StopReason
		usage     anthroUsage
		hasUsage  bool
		ended     bool
		truncated bool
	)
	endEvent := func() *Event {
		// No stop reason seen: the stream ended without the provider's
		// terminal event (message_delta/message_stop). Surface that as
		// StopOther instead of pretending it ended cleanly.
		if stop == "" {
			stop = StopOther
		}
		ev := &Event{Type: EventMessageEnd, StopReason: stop}
		if hasUsage {
			u := usage.toUsage()
			ev.Usage = &u
		}
		return ev
	}
	malformed := func(err error) error {
		return fmt.Errorf("rosetta: anthropic stream (%s %s): malformed event: %w", method, url, err)
	}
	return func() (*Event, error) {
		for {
			// Once message_stop (or a truncated EOF) was seen, never emit
			// further events even if the server keeps sending.
			if ended {
				if truncated {
					return nil, fmt.Errorf("rosetta: %w: anthropic stream ended without message_stop (partial response kept in Stream.Partial)", ErrStreamTruncated)
				}
				return nil, io.EOF
			}
			ssev, err := sc.Next()
			if err != nil {
				// io.EOF covers a clean server-side close; io.ErrUnexpectedEOF
				// covers a real HTTP truncation (chunked stream cut before the
				// final zero chunk, gateway timeout).
				if errors.Is(err, io.ErrUnexpectedEOF) && !ended {
					ended = true
					truncated = true
					return endEvent(), nil
				}
				if errors.Is(err, io.EOF) && !ended {
					ended = true
					// message_delta (which carries the authoritative
					// stop_reason) is the semantic terminal; a clean close
					// after it is complete even without message_stop
					// (audit B2).
					truncated = stop == ""
					return endEvent(), nil
				}
				return nil, err
			}
			data := bytes.TrimSpace(ssev.Data)
			if len(data) == 0 {
				continue
			}
			// Anthropic never emits the OpenAI "[DONE]" sentinel, but a
			// compatibility proxy fronting it may append one as a terminal
			// marker; treat it as a clean end rather than a malformed event.
			if bytes.Equal(data, []byte("[DONE]")) {
				if !ended {
					ended = true
					return endEvent(), nil
				}
				continue
			}
			var ch anthroChunk
			if err := json.Unmarshal(data, &ch); err != nil {
				// A malformed chunk may carry content the caller will
				// otherwise never see; dropping it silently would corrupt
				// text or tool-call arguments, so fail the stream instead.
				return nil, malformed(err)
			}
			switch ch.Type {
			case "ping":
				continue
			case "error":
				// Tolerate a malformed error event with no error body
				// (seen on third-party Anthropic-compatible gateways):
				// surface a generic APIError instead of a nil event.
				var apiErr *APIError
				if ch.Error != nil {
					apiErr = ch.Error.apiError(200)
				} else {
					apiErr = &APIError{StatusCode: 200, Type: "api_error", Message: "stream error event without details"}
				}
				apiErr.Method, apiErr.URL, apiErr.RequestID = method, url, requestID
				return nil, apiErr
			case "message_start":
				if ch.Message != nil {
					if ch.Message.Usage != nil {
						usage = *ch.Message.Usage
						hasUsage = true
					}
					if !ended {
						return &Event{Type: EventMessageStart, ID: ch.Message.ID, Model: ch.Message.Model}, nil
					}
				}
			case "content_block_start":
				if ch.ContentBlock != nil && ch.ContentBlock.Type == "tool_use" {
					return &Event{
						Type:      EventToolCall,
						ToolIndex: ch.Index,
						ToolID:    ch.ContentBlock.ID,
						ToolName:  ch.ContentBlock.Name,
					}, nil
				}
			case "content_block_delta":
				if ch.Delta == nil {
					continue
				}
				switch ch.Delta.Type {
				case "text_delta":
					if ch.Delta.Text != "" {
						return &Event{Type: EventTextDelta, Text: ch.Delta.Text, BlockIndex: ch.Index}, nil
					}
				case "thinking_delta":
					if ch.Delta.Thinking != "" {
						return &Event{Type: EventThinkingDelta, Text: ch.Delta.Thinking, BlockIndex: ch.Index}, nil
					}
				case "signature_delta":
					if ch.Delta.Signature != "" {
						return &Event{Type: EventThinkingDelta, Signature: ch.Delta.Signature, BlockIndex: ch.Index}, nil
					}
				case "input_json_delta":
					if ch.Delta.PartialJSON != "" {
						return &Event{
							Type:           EventToolCall,
							ToolIndex:      ch.Index,
							ArgumentsDelta: ch.Delta.PartialJSON,
						}, nil
					}
				}
			case "message_delta":
				if ch.Delta != nil && ch.Delta.StopReason != nil && *ch.Delta.StopReason != "" {
					stop = mapAnthropicStop(*ch.Delta.StopReason)
				}
				if ch.Usage != nil {
					// message_delta carries the authoritative final usage.
					// Fold every field it reports over the message_start
					// baseline, but only when it is non-zero: an omitted
					// field must not clobber the baseline with 0.
					if ch.Usage.OutputTokens.Value != 0 {
						usage.OutputTokens = ch.Usage.OutputTokens
					}
					if ch.Usage.InputTokens.Value != 0 {
						usage.InputTokens = ch.Usage.InputTokens
					}
					if ch.Usage.CacheReadInputTokens.Value != 0 {
						usage.CacheReadInputTokens = ch.Usage.CacheReadInputTokens
					}
					if ch.Usage.CacheCreationInputTokens.Value != 0 {
						usage.CacheCreationInputTokens = ch.Usage.CacheCreationInputTokens
					}
					hasUsage = true
				}
			case "message_stop":
				if !ended {
					ended = true
					return endEvent(), nil
				}
			}
		}
	}
}

// ListModels queries GET /v1/models, following the has_more/after_id
// pagination (the catalog is served in pages of ~20 by default). Entries
// carry no capability metadata.
func (p *anthropicProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	const method = http.MethodGet
	base := joinEndpoint(p.c.settings.endpoint, "/models")
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("rosetta: invalid endpoint %q: %w", displayEndpoint(base), err)
	}
	var models []ModelInfo
	after := ""
	for page := 0; ; page++ {
		q := parsed.Query()
		q.Set("limit", "100")
		if after != "" {
			q.Set("after_id", after)
		} else {
			q.Del("after_id")
		}
		parsed.RawQuery = q.Encode()
		pageURL := parsed.String()
		call := &httpx.Call{Method: method, URL: pageURL, Header: p.headers(false)}
		resp, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, pageURL)
		}
		body, rerr := readBody(resp, method, pageURL, bodyLimit)
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, parseAnthropicError(resp.StatusCode, body, method, pageURL, requestID(resp.Header))
		}
		var list struct {
			Data *[]struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("rosetta: decoding model list: %w", err)
		}
		// Missing/null "data" is a malformed catalog, not an empty one:
		// treating it as empty would wipe the previously learned remote
		// layer (audit B11).
		if list.Data == nil {
			return nil, fmt.Errorf("rosetta: model list response is missing the required \"data\" field")
		}
		for _, m := range *list.Data {
			if m.ID == "" {
				continue
			}
			models = append(models, ModelInfo{ID: m.ID, DisplayName: m.DisplayName, Protocol: ProtoAnthropic})
		}
		if !list.HasMore || page > 100 {
			return models, nil
		}
		if next := list.LastID; next != "" && len(*list.Data) > 0 {
			after = next
			continue
		}
		// has_more without a usable cursor: stop rather than loop forever.
		return models, nil
	}
}

func mapAnthropicStop(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return StopEnd
	case "max_tokens":
		return StopLength
	case "tool_use":
		return StopToolUse
	case "refusal":
		return StopRefusal
	default:
		return StopOther
	}
}

// ---- wire types ----

type anthroUsage struct {
	InputTokens              jsonx.FlexInt64 `json:"input_tokens"`
	OutputTokens             jsonx.FlexInt64 `json:"output_tokens"`
	CacheReadInputTokens     jsonx.FlexInt64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens jsonx.FlexInt64 `json:"cache_creation_input_tokens"`
}

func (u *anthroUsage) toUsage() Usage {
	// Anthropic's input_tokens counts only *uncached* prompt tokens; the
	// cache read/write counts are disjoint. The unified Usage follows OpenAI
	// semantics where CachedInputTokens ⊆ InputTokens, so fold the cache
	// counts into the input (and therefore total) baseline.
	cached := u.CacheReadInputTokens.Value
	creation := u.CacheCreationInputTokens.Value
	input := u.InputTokens.Value + cached + creation
	return Usage{
		InputTokens:          input,
		OutputTokens:         u.OutputTokens.Value,
		TotalTokens:          input + u.OutputTokens.Value,
		CachedInputTokens:    cached,
		CachedCreationTokens: creation,
	}
}

type anthroErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (e *anthroErrorBody) apiError(status int) *APIError {
	if e == nil {
		return nil
	}
	return &APIError{StatusCode: status, Type: e.Type, Message: e.Message}
}

type anthroChunk struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *anthroUsage `json:"usage"`
	} `json:"message"`
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string  `json:"type"`
		Text        string  `json:"text"`
		Thinking    string  `json:"thinking"`
		Signature   string  `json:"signature"`
		PartialJSON string  `json:"partial_json"`
		StopReason  *string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthroUsage     `json:"usage"`
	Error *anthroErrorBody `json:"error"`
}

type anthroResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *anthroUsage     `json:"usage"`
	Error      *anthroErrorBody `json:"error"`
}

func decodeAnthropicResponse(body []byte, rc ...string) (*ChatResponse, error) {
	var r anthroResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rosetta: decoding anthropic response: %w", err)
	}
	if r.Error != nil {
		// An in-band 200 error still carries request context, so callers can
		// log and retry it like a non-2xx failure.
		apiErr := r.Error.apiError(200)
		attachRequest(apiErr, rc)
		return nil, apiErr
	}
	out := &ChatResponse{ID: r.ID, Model: r.Model, Raw: truncateBody(body)}
	for _, blk := range r.Content {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				out.Content = append(out.Content, Block{Type: BlockText, Text: blk.Text})
			}
		case "thinking":
			out.Content = append(out.Content, Block{Type: BlockThinking, Thinking: blk.Thinking, Signature: blk.Signature})
		case "redacted_thinking":
			// Keep the opaque payload so a later turn can replay the block
			// unchanged; the Thinking field carries the redacted data.
			out.Content = append(out.Content, Block{Type: BlockRedactedThinking, Thinking: blk.Data})
		case "tool_use":
			out.Content = append(out.Content, Block{
				Type:       BlockToolCall,
				ToolCallID: blk.ID,
				ToolName:   blk.Name,
				Arguments:  string(blk.Input),
			})
		default:
			// unknown content types are skipped
		}
	}
	if r.StopReason != "" {
		out.StopReason = mapAnthropicStop(r.StopReason)
	} else {
		out.StopReason = StopEnd // unary 200 is complete by definition
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toUsage()
	}
	return out, nil
}

// parseAnthropicError converts a non-2xx body into an APIError. Anthropic's
// shape is {"type":"error","error":{"type","message"}}; the generic
// {"error":{...}} parser covers it, so it is reused. When the request id is
// absent from the headers (some gateways omit request-id), it is recovered
// from the error body's top-level request_id field.
func parseAnthropicError(status int, body []byte, method, url, requestID string) *APIError {
	if requestID == "" {
		var doc struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(body, &doc) == nil {
			requestID = doc.RequestID
		}
	}
	return parseOpenAIError(status, body, method, url, requestID)
}

func requestID(h http.Header) string {
	if v := h.Get("request-id"); v != "" {
		return v
	}
	return h.Get("x-request-id")
}
