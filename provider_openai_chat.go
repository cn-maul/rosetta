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
	"strings"
	"sync"

	"github.com/cn-maul/rosetta/internal/httpx"
	"github.com/cn-maul/rosetta/internal/jsonx"
	"github.com/cn-maul/rosetta/internal/sse"
)

// openaiChatProvider adapts the OpenAI Chat Completions protocol, spoken
// by api.openai.com and most compatible third-party services.
//
// Third-party compatibility is handled in three layers, in escalation
// order: explicit quirks (WithQuirks), a sticky probe — start with modern
// fields and downgrade once on a matching 400 error — and per-request
// sanitizing retries. Probed downgrades stick for the client lifetime.
type openaiChatProvider struct {
	c *Client

	mu                sync.Mutex
	stickyLegacy      *bool // true: use max_tokens; false: use max_completion_tokens
	stickyNoStreamOpt *bool // upstream rejected stream_options
	stickyNoReasoning *bool // upstream rejected reasoning_effort
}

// oaSendState is the per-request mutable send strategy.
type oaSendState struct {
	tokensField   string // "max_completion_tokens" | "max_tokens"
	streamOptions bool
	reasoning     bool
}

func (p *openaiChatProvider) initialState() *oaSendState {
	st := &oaSendState{
		tokensField:   "max_completion_tokens",
		streamOptions: !p.c.settings.quirks.NoStreamUsage,
		reasoning:     true,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Precedence: explicit WithMaxTokensField pin > probed sticky state >
	// LegacyMaxTokens quirk > modern default.
	switch {
	case p.c.settings.maxTokensField != "":
		st.tokensField = p.c.settings.maxTokensField
	case p.stickyLegacy != nil:
		if *p.stickyLegacy {
			st.tokensField = "max_tokens"
		} else {
			st.tokensField = "max_completion_tokens"
		}
	case p.c.settings.quirks.LegacyMaxTokens:
		st.tokensField = "max_tokens"
	}
	if p.stickyNoStreamOpt != nil && *p.stickyNoStreamOpt {
		st.streamOptions = false
	}
	if p.stickyNoReasoning != nil && *p.stickyNoReasoning {
		st.reasoning = false
	}
	return st
}

func (p *openaiChatProvider) headers() http.Header {
	return openAIHeaders(p.c, "application/json")
}

// buildPayload renders the unified request into the wire payload.
func (p *openaiChatProvider) buildPayload(req *ChatRequest, stream bool, st *oaSendState) (map[string]any, error) {
	msgs, err := p.encodeMessages(req)
	if err != nil {
		return nil, err
	}
	pl := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if max := p.c.effectiveMaxOutput(req); max > 0 {
		pl[st.tokensField] = max
	}
	if req.Temperature != nil {
		pl["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		pl["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		pl["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		pl["tools"] = tools
	}
	if req.Thinking != nil && st.reasoning {
		if eff := req.effort(); eff != EffortUnset {
			pl["reasoning_effort"] = string(eff)
		}
	}
	if stream {
		pl["stream"] = true
		if st.streamOptions {
			pl["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	maps.Copy(pl, req.Extra)
	return pl, nil
}

func (p *openaiChatProvider) encodeMessages(req *ChatRequest) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if t := m.text(); t != "" {
				out = append(out, map[string]any{"role": "system", "content": t})
			}
		case RoleUser:
			out = append(out, encodeOpenAIUser(m))
		case RoleAssistant:
			mm := map[string]any{"role": "assistant"}
			if t := m.text(); t != "" {
				mm["content"] = t
			}
			var tcs []map[string]any
			for _, b := range m.Blocks {
				if b.Type != BlockToolCall {
					continue // thinking blocks are not replayable here
				}
				tcs = append(tcs, map[string]any{
					"id":   b.ToolCallID,
					"type": "function",
					"function": map[string]any{
						"name":      b.ToolName,
						"arguments": b.Arguments,
					},
				})
			}
			if len(tcs) > 0 {
				mm["tool_calls"] = tcs
			}
			if len(mm) == 1 {
				mm["content"] = ""
			}
			out = append(out, mm)
		case RoleTool:
			// Each tool result is its own message on this protocol; a
			// message may carry several results (parallel tool calls) and
			// every one of them must reach the model.
			for _, b := range m.Blocks {
				if b.Type != BlockToolResult {
					continue
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolCallID,
					"content":      b.Content,
				})
			}
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidRequest, m.Role)
		}
	}
	return out, nil
}

func encodeOpenAIUser(m Message) map[string]any {
	mm := map[string]any{"role": "user"}
	var hasImage bool
	for _, b := range m.Blocks {
		if b.Type == BlockImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		mm["content"] = m.text()
		return mm
	}
	parts := make([]map[string]any, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Type {
		case BlockText:
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case BlockImage:
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": b.ImageURL},
			})
		}
	}
	mm["content"] = parts
	return mm
}

// hintWords match 400-error phrasing that identifies an optional-field
// rejection worth a sanitizing retry.
var hintWords = []string{
	"unrecognized", "unknown", "unexpected", "not supported",
	"unsupported", "invalid parameter", "unexpected keyword",
}

func containsAny(low string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// sanitize inspects a 400 error message and downgrades one optional field.
// It returns true when the payload changed and the request should be
// retried. Downgrades are remembered for the client lifetime. To keep the
// keyword matching from misfiring on unrelated 400s, a non-empty error
// type must look like an invalid-request error; and a field pinned via
// WithMaxTokensField is never flipped.
func (p *openaiChatProvider) sanitize(st *oaSendState, msg, errType string, stream bool) bool {
	low := strings.ToLower(msg)
	if errType != "" && !strings.Contains(strings.ToLower(errType), "invalid_request") {
		return false
	}
	note := func(slot **bool, v bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if *slot == nil {
			b := v
			*slot = &b
		}
	}
	pinned := p.c.settings.maxTokensField != ""
	switch {
	case st.reasoning && containsAny(low, "reasoning_effort") && containsAny(low, hintWords...):
		st.reasoning = false
		note(&p.stickyNoReasoning, true)
		p.c.settings.logger.Debug("openai-chat: upstream rejected reasoning_effort; dropping it")
		return true
	case stream && st.streamOptions && containsAny(low, "stream_options") && containsAny(low, hintWords...):
		st.streamOptions = false
		note(&p.stickyNoStreamOpt, true)
		p.c.settings.logger.Debug("openai-chat: upstream rejected stream_options; dropping include_usage")
		return true
	case !pinned && st.tokensField == "max_completion_tokens" &&
		containsAny(low, "max_tokens", "max_completion_tokens") && containsAny(low, hintWords...):
		st.tokensField = "max_tokens"
		note(&p.stickyLegacy, true)
		p.c.settings.logger.Debug("openai-chat: falling back to legacy max_tokens field")
		return true
	case !pinned && st.tokensField == "max_tokens" &&
		containsAny(low, "max_tokens", "max_completion_tokens") && containsAny(low, hintWords...):
		st.tokensField = "max_completion_tokens"
		note(&p.stickyLegacy, false)
		p.c.settings.logger.Debug("openai-chat: upstream requires max_completion_tokens")
		return true
	}
	return false
}

// Chat performs a non-streaming completion.
func (p *openaiChatProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/chat/completions")
	st := p.initialState()
	sanitizes := 0
	for {
		payload, err := p.buildPayload(req, false, st)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: p.headers(),
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
			apiErr := parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr.Message, apiErr.Type, false) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		return decodeOpenAIChatResponse(body)
	}
}

// StreamChat starts a streaming completion.
func (p *openaiChatProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/chat/completions")
	st := p.initialState()
	sanitizes := 0
	var resp *http.Response
	for {
		payload, err := p.buildPayload(req, true, st)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: p.headers(),
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
		apiErr := parseOpenAIError(r.StatusCode, body, method, url, r.Header.Get("X-Request-Id"))
		if r.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr.Message, apiErr.Type, true) {
			sanitizes++
			continue
		}
		return nil, apiErr
	}
	s := newStream(p.streamEvents(resp.Body), nil)
	s.attachCloser(resp.Body)
	return s, nil
}

// streamEvents maps the OpenAI chunk SSE stream onto unified events.
// Usage and stop reason are captured from their chunks and delivered in a
// single synthesized EventMessageEnd, emitted at [DONE] or EOF whichever
// comes first (usage sometimes trails the finish_reason chunk).
func (p *openaiChatProvider) streamEvents(body io.Reader) func() (*Event, error) {
	sc := sse.New(body)
	var (
		pending   []*Event
		startSent bool
		stop      StopReason
		usage     *Usage
		ended     bool
	)
	endEvent := func() *Event {
		// No finish_reason seen: stream ended without the provider's
		// terminal signal; surface as StopOther rather than StopEnd.
		if stop == "" {
			stop = StopOther
		}
		ev := &Event{Type: EventMessageEnd, StopReason: stop}
		if usage != nil {
			ev.Usage = usage
		}
		return ev
	}
	return func() (*Event, error) {
		if len(pending) > 0 {
			ev := pending[0]
			pending = pending[1:]
			return ev, nil
		}
		for {
			// Once the terminal signal ([DONE] or EOF) was seen, never
			// emit further events even if the server keeps sending.
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
			if bytes.Equal(data, []byte("[DONE]")) {
				if !ended {
					ended = true
					return endEvent(), nil
				}
				continue
			}
			if len(data) == 0 {
				continue
			}
			var ch oaChunk
			if err := json.Unmarshal(data, &ch); err != nil {
				p.c.settings.logger.Debug("openai-chat: skipping malformed stream chunk",
					"err", err.Error())
				continue
			}
			if ch.Error != nil {
				return nil, ch.Error.apiError(200)
			}
			var events []*Event
			if !startSent {
				startSent = true
				events = append(events, &Event{Type: EventMessageStart, ID: ch.ID, Model: ch.Model})
			}
			if ch.Usage != nil {
				u := ch.Usage.toUsage()
				usage = &u
			}
			for _, choice := range ch.Choices {
				d := &choice.Delta
				if d.Content.Set && d.Content.Value != "" {
					events = append(events, &Event{Type: EventTextDelta, Text: d.Content.Value})
				}
				if d.ReasoningContent != nil && *d.ReasoningContent != "" {
					events = append(events, &Event{Type: EventThinkingDelta, Text: *d.ReasoningContent})
				}
				for _, tc := range d.ToolCalls {
					events = append(events, &Event{
						Type:           EventToolCall,
						ToolIndex:      tc.Index,
						ToolID:         tc.ID,
						ToolName:       tc.Function.Name,
						ArgumentsDelta: tc.Function.Arguments,
					})
				}
				if choice.FinishReason.Set && choice.FinishReason.Value != "" {
					stop = mapOpenAIStop(choice.FinishReason.Value)
				}
			}
			if len(events) > 0 {
				pending = append(pending, events...)
				ev := pending[0]
				pending = pending[1:]
				return ev, nil
			}
		}
	}
}

// ListModels queries GET /models via the shared OpenAI-family helper.
func (p *openaiChatProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	return listOpenAIModels(ctx, p.c)
}

func mapOpenAIStop(s string) StopReason {
	switch s {
	case "stop":
		return StopEnd
	case "length":
		return StopLength
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopContentFilter
	default:
		return StopOther
	}
}

// ---- wire types ----

type oaErrorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

func (e *oaErrorBody) apiError(status int) *APIError {
	if e == nil {
		return nil
	}
	return &APIError{
		StatusCode: status,
		Code:       decodeJSONString(e.Code),
		Type:       e.Type,
		Message:    e.Message,
	}
}

func decodeJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

type oaUsage struct {
	PromptTokens        jsonx.FlexInt64 `json:"prompt_tokens"`
	CompletionTokens    jsonx.FlexInt64 `json:"completion_tokens"`
	TotalTokens         jsonx.FlexInt64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens jsonx.FlexInt64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens jsonx.FlexInt64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *oaUsage) toUsage() Usage {
	total := u.TotalTokens.Value
	if total == 0 {
		total = u.PromptTokens.Value + u.CompletionTokens.Value
	}
	out := Usage{
		InputTokens:  u.PromptTokens.Value,
		OutputTokens: u.CompletionTokens.Value,
		TotalTokens:  total,
	}
	if u.PromptTokensDetails != nil {
		out.CachedInputTokens = u.PromptTokensDetails.CachedTokens.Value
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens.Value
	}
	return out
}

type oaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string              `json:"role"`
			Content          jsonx.ContentString `json:"content"`
			ReasoningContent *string             `json:"reasoning_content"`
			ToolCalls        []oaToolCall        `json:"tool_calls"`
		} `json:"delta"`
		FinishReason jsonx.FlexString `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage     `json:"usage"`
	Error *oaErrorBody `json:"error"`
}

type oaResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          jsonx.ContentString `json:"content"`
			ReasoningContent *string             `json:"reasoning_content"`
			ToolCalls        []oaToolCall        `json:"tool_calls"`
		} `json:"message"`
		FinishReason jsonx.FlexString `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage     `json:"usage"`
	Error *oaErrorBody `json:"error"`
}

func decodeOpenAIChatResponse(body []byte) (*ChatResponse, error) {
	var r oaResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rosetta: decoding openai-chat response: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error.apiError(200)
	}
	if len(r.Choices) == 0 {
		return nil, errors.New("rosetta: openai-chat response contains no choices")
	}
	choice := &r.Choices[0]
	out := &ChatResponse{
		ID:    r.ID,
		Model: r.Model,
		Raw:   truncateBody(body),
	}
	msg := &choice.Message
	if msg.ReasoningContent != nil && *msg.ReasoningContent != "" {
		out.Content = append(out.Content, Block{Type: BlockThinking, Thinking: *msg.ReasoningContent})
	}
	if msg.Content.Set && msg.Content.Value != "" {
		out.Content = append(out.Content, Block{Type: BlockText, Text: msg.Content.Value})
	}
	for _, tc := range msg.ToolCalls {
		out.Content = append(out.Content, Block{
			Type:       BlockToolCall,
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Arguments:  tc.Function.Arguments,
		})
	}
	if choice.FinishReason.Set && choice.FinishReason.Value != "" {
		out.StopReason = mapOpenAIStop(choice.FinishReason.Value)
	}
	// A unary 200 response is complete by definition, even when a sloppy
	// serializer omits finish_reason (streams keep the StopOther signal
	// for truncation detection instead).
	if out.StopReason == "" {
		out.StopReason = StopEnd
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toUsage()
	}
	return out, nil
}

// parseOpenAIError converts a non-2xx body into an APIError. OpenAI's
// shape is {"error":{"message","type","code"}} but "error" is sometimes a
// bare string on third-party services; both are handled.
func parseOpenAIError(status int, body []byte, method, url, requestID string) *APIError {
	apiErr := &APIError{
		StatusCode: status,
		Method:     method,
		URL:        url,
		RequestID:  requestID,
		Retryable:  retryableStatus(status),
		Raw:        safeTruncateBody(body),
	}
	var top struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &top); err != nil || len(top.Error) == 0 {
		apiErr.Message = shortMessage(body)
		return apiErr
	}
	var obj oaErrorBody
	if err := json.Unmarshal(top.Error, &obj); err == nil && (obj.Message != "" || obj.Type != "") {
		apiErr.Message = obj.Message
		apiErr.Type = obj.Type
		apiErr.Code = decodeJSONString(obj.Code)
		return apiErr
	}
	var s string
	if err := json.Unmarshal(top.Error, &s); err == nil {
		apiErr.Message = s
		return apiErr
	}
	apiErr.Message = shortMessage(body)
	return apiErr
}

// shortMessage bounds a human-facing message taken from an unparseable
// error body, rune-safe.
func shortMessage(body []byte) string {
	s := strings.TrimSpace(string(body))
	if runes := []rune(s); len(runes) > 256 {
		s = string(runes[:256]) + "…"
	}
	return s
}
