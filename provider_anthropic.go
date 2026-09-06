package rosetta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/cn-maul/rosetta/internal/httpx"
	"github.com/cn-maul/rosetta/internal/sse"
)

// anthropicProvider adapts the Anthropic Messages protocol
// (POST /v1/messages), including thinking budgets and their constraints.
type anthropicProvider struct{ c *Client }

const anthropicVersion = "2023-06-01"

func (p *anthropicProvider) headers(stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	if key := p.c.settings.apiKey; key != "" {
		// x-api-key is the Anthropic-native auth header; Authorization
		// helps Anthropic-compatible gateways that expect Bearer.
		h.Set("x-api-key", key)
		h.Set("Authorization", "Bearer "+key)
	}
	h.Set("anthropic-version", anthropicVersion)
	return h
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
	if system != "" {
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
		}
		payload["tools"] = tools
	}
	if stream {
		payload["stream"] = true
	}
	maps.Copy(payload, req.Extra)
	return payload, nil
}

// encodeMessages maps unified messages onto Anthropic's shape: system
// content is lifted to the top-level system field, tool results ride
// inside user turns, and consecutive same-role messages are merged
// (Anthropic enforces strict role alternation).
func (p *anthropicProvider) encodeMessages(req *ChatRequest) (string, []map[string]any, error) {
	var sysParts []string
	if req.System != "" {
		sysParts = append(sysParts, req.System)
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
				sysParts = append(sysParts, t)
			}
		case RoleUser:
			add("user", m.Blocks)
		case RoleTool:
			add("user", m.Blocks) // tool_result blocks belong to a user turn
		case RoleAssistant:
			add("assistant", m.Blocks)
		default:
			return "", nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidRequest, m.Role)
		}
	}
	if len(msgs) == 0 {
		return "", nil, fmt.Errorf("%w: anthropic requires at least one non-system message", ErrInvalidRequest)
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]map[string]any, 0, len(m.blocks))
		for _, b := range m.blocks {
			switch b.Type {
			case BlockText:
				if b.Text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				}
			case BlockImage:
				src, err := encodeAnthropicImage(b.ImageURL)
				if err != nil {
					return "", nil, err
				}
				blocks = append(blocks, map[string]any{"type": "image", "source": src})
			case BlockToolResult:
				tr := map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ToolCallID,
					"content":     b.Content,
				}
				if b.IsError {
					tr["is_error"] = true
				}
				blocks = append(blocks, tr)
			case BlockThinking:
				if m.role == "assistant" {
					blocks = append(blocks, map[string]any{
						"type":      "thinking",
						"thinking":  b.Thinking,
						"signature": b.Signature,
					})
				}
			case BlockToolCall:
				if m.role == "assistant" {
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    b.ToolCallID,
						"name":  b.ToolName,
						"input": parseToolInput(b.Arguments),
					})
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
		return "", nil, fmt.Errorf("%w: anthropic requires at least one non-system message", ErrInvalidRequest)
	}
	if out[0]["role"] != "user" {
		return "", nil, fmt.Errorf("%w: anthropic requires the first message to be user role", ErrInvalidRequest)
	}
	return strings.Join(sysParts, "\n\n"), out, nil
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
	for {
		payload, err := p.buildPayload(req, false, pl)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: p.headers(false),
			Body:   func() ([]byte, error) { return json.Marshal(payload) },
		}
		resp, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, url)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, transport(rerr, method, url)
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
		return decodeAnthropicResponse(body)
	}
}

// StreamChat starts a streaming completion.
func (p *anthropicProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/messages")
	pl := p.plan(req)
	var resp *http.Response
	for {
		payload, err := p.buildPayload(req, true, pl)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: p.headers(true),
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
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		apiErr := parseAnthropicError(r.StatusCode, body, method, url, requestID(r.Header))
		if r.StatusCode == http.StatusBadRequest && p.c.settings.thinkingRectify && pl.rectify(apiErr.Message) {
			continue
		}
		return nil, apiErr
	}
	s := newStream(p.streamEvents(resp.Body), nil)
	s.attachCloser(resp.Body)
	return s, nil
}

// streamEvents maps Anthropic's typed SSE stream onto unified events.
// Input tokens arrive with message_start, output tokens with
// message_delta; both are folded into a single EventMessageEnd emitted at
// message_stop (or EOF, for truncated streams).
func (p *anthropicProvider) streamEvents(body io.Reader) func() (*Event, error) {
	sc := sse.New(body)
	var (
		stop     StopReason
		usage    anthroUsage
		hasUsage bool
		ended    bool
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
	return func() (*Event, error) {
		for {
			// Once message_stop (or EOF) was seen, never emit further
			// events even if the server keeps sending.
			if ended {
				return nil, io.EOF
			}
			ssev, err := sc.Next()
			if err != nil {
				if errors.Is(err, io.EOF) && !ended {
					ended = true
					return endEvent(), nil
				}
				return nil, err
			}
			data := bytes.TrimSpace(ssev.Data)
			if len(data) == 0 {
				continue
			}
			var ch anthroChunk
			if err := json.Unmarshal(data, &ch); err != nil {
				p.c.settings.logger.Debug("anthropic: skipping malformed stream chunk", "err", err.Error())
				continue
			}
			switch ch.Type {
			case "ping":
				continue
			case "error":
				// Tolerate a malformed error event with no error body
				// (seen on third-party Anthropic-compatible gateways):
				// surface a generic APIError instead of a nil event.
				if ch.Error != nil {
					return nil, ch.Error.apiError(200)
				}
				return nil, &APIError{StatusCode: 200, Type: "api_error", Message: "stream error event without details"}
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
						return &Event{Type: EventTextDelta, Text: ch.Delta.Text}, nil
					}
				case "thinking_delta":
					if ch.Delta.Thinking != "" {
						return &Event{Type: EventThinkingDelta, Text: ch.Delta.Thinking}, nil
					}
				case "signature_delta":
					if ch.Delta.Signature != "" {
						return &Event{Type: EventThinkingDelta, Signature: ch.Delta.Signature}, nil
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
					usage.OutputTokens = ch.Usage.OutputTokens
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
	var models []ModelInfo
	after := ""
	for page := 0; ; page++ {
		pageURL := base + "?limit=100"
		if after != "" {
			pageURL += "&after_id=" + url.QueryEscape(after)
		}
		call := &httpx.Call{Method: method, URL: pageURL, Header: p.headers(false)}
		resp, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, pageURL)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, parseAnthropicError(resp.StatusCode, body, method, pageURL, requestID(resp.Header))
		}
		var list struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("rosetta: decoding model list: %w", err)
		}
		for _, m := range list.Data {
			if m.ID == "" {
				continue
			}
			models = append(models, ModelInfo{ID: m.ID, DisplayName: m.DisplayName, Protocol: ProtoAnthropic})
		}
		if !list.HasMore || page > 100 {
			return models, nil
		}
		if next := list.LastID; next != "" && len(list.Data) > 0 {
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
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u *anthroUsage) toUsage() Usage {
	return Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		TotalTokens:       u.InputTokens + u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
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
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *anthroUsage     `json:"usage"`
	Error      *anthroErrorBody `json:"error"`
}

func decodeAnthropicResponse(body []byte) (*ChatResponse, error) {
	var r anthroResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rosetta: decoding anthropic response: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error.apiError(200)
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
		case "tool_use":
			out.Content = append(out.Content, Block{
				Type:       BlockToolCall,
				ToolCallID: blk.ID,
				ToolName:   blk.Name,
				Arguments:  string(blk.Input),
			})
		default:
			// redacted_thinking and future types are skipped
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
// {"error":{...}} parser covers it, so it is reused.
func parseAnthropicError(status int, body []byte, method, url, requestID string) *APIError {
	return parseOpenAIError(status, body, method, url, requestID)
}

func requestID(h http.Header) string {
	if v := h.Get("request-id"); v != "" {
		return v
	}
	return h.Get("x-request-id")
}
