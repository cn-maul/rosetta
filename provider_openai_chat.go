package rosetta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// sanitizing retries. Probed downgrades are remembered per model for the
// client lifetime.
type openaiChatProvider struct {
	c *Client

	mu sync.Mutex
	// Sticky downgrades are remembered per model: one model's field support
	// says nothing about another's behind a multi-model gateway, so a
	// legacy model rejecting max_completion_tokens must not downgrade the
	// field for a modern model served at the same endpoint.
	stickyTokens      map[string]string // model -> resolved max-tokens field name
	stickyNoStreamOpt map[string]bool   // upstream rejected stream_options
	stickyNoReasoning map[string]bool   // upstream rejected reasoning_effort
}

// oaSendState is the per-request mutable send strategy.
type oaSendState struct {
	tokensField   string // "max_completion_tokens" | "max_tokens"
	streamOptions bool
	reasoning     bool
}

func (p *openaiChatProvider) initialState(model string) *oaSendState {
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
	case p.stickyTokens[model] != "":
		st.tokensField = p.stickyTokens[model]
	case p.c.settings.quirks.LegacyMaxTokens:
		st.tokensField = "max_tokens"
	}
	if p.stickyNoStreamOpt[model] {
		st.streamOptions = false
	}
	if p.stickyNoReasoning[model] {
		st.reasoning = false
	}
	return st
}

func (p *openaiChatProvider) headers(accept string) http.Header {
	return openAIHeaders(p.c, accept)
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
	if err := mergeExtra(pl, req.Extra, p.c.settings.extraOverrides, openaiChatReservedPayloadKeys); err != nil {
		return nil, err
	}
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
			mm, err := encodeOpenAIUser(m)
			if err != nil {
				return nil, err
			}
			out = append(out, mm)
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

func encodeOpenAIUser(m Message) (map[string]any, error) {
	mm := map[string]any{"role": "user"}
	var multimodal bool
	for _, b := range m.Blocks {
		switch b.Type {
		case BlockImage, BlockAudio, BlockFile:
			multimodal = true
		}
		if multimodal {
			break
		}
	}
	if !multimodal {
		mm["content"] = m.text()
		return mm, nil
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
		case BlockAudio:
			if b.AudioData == "" {
				return nil, fmt.Errorf("%w: audio block has no AudioData", ErrInvalidRequest)
			}
			if b.AudioFormat != "wav" && b.AudioFormat != "mp3" {
				return nil, fmt.Errorf("%w: audio format %q unsupported (wav or mp3)", ErrInvalidRequest, b.AudioFormat)
			}
			parts = append(parts, map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": b.AudioData, "format": b.AudioFormat},
			})
		case BlockFile:
			f := map[string]any{}
			if b.FileID != "" {
				f["file_id"] = b.FileID
			} else {
				du, err := fileDataURL(b)
				if err != nil {
					return nil, err
				}
				f["file_data"] = du
			}
			if b.FileName != "" {
				f["filename"] = b.FileName
			}
			parts = append(parts, map[string]any{"type": "file", "file": f})
		}
	}
	mm["content"] = parts
	return mm, nil
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

// sanitize inspects a 400 error and downgrades one optional field. It
// returns true when the payload changed and the request should be retried.
// Downgrades are remembered per model for the client lifetime. A
// provider-supplied error.param is authoritative — it names the rejected
// field directly — so it is matched first; only when it is absent does the
// sanitizer fall back to scanning the message for the field name plus a
// rejection hint, which keeps unrelated 400s from misfiring. A rejection of
// a *value* inside a field (e.g. an unsupported reasoning_effort level) is
// a request configuration error, not evidence the field is unsupported, so
// it is surfaced as-is; and a field pinned via WithMaxTokensField is never
// flipped.
func (p *openaiChatProvider) sanitize(st *oaSendState, apiErr *APIError, model string, stream bool) bool {
	if isValueRejection(apiErr) {
		return false
	}
	if apiErr.Type != "" && !strings.Contains(strings.ToLower(apiErr.Type), "invalid_request") {
		return false
	}
	// hit reports whether apiErr rejects one of the named fields, using the
	// shared param-aware matcher.
	hit := func(names ...string) bool { return rejectionHit(apiErr, names...) }
	remember := func(fn func()) {
		p.mu.Lock()
		defer p.mu.Unlock()
		fn()
	}
	pinned := p.c.settings.maxTokensField != ""
	switch {
	case st.reasoning && hit("reasoning_effort"):
		st.reasoning = false
		remember(func() {
			if p.stickyNoReasoning == nil {
				p.stickyNoReasoning = map[string]bool{}
			}
			p.stickyNoReasoning[model] = true
		})
		p.c.settings.logger.Debug("openai-chat: upstream rejected reasoning_effort; dropping it", "model", model)
		return true
	case stream && st.streamOptions && hit("stream_options"):
		st.streamOptions = false
		remember(func() {
			if p.stickyNoStreamOpt == nil {
				p.stickyNoStreamOpt = map[string]bool{}
			}
			p.stickyNoStreamOpt[model] = true
		})
		p.c.settings.logger.Debug("openai-chat: upstream rejected stream_options; dropping include_usage", "model", model)
		return true
	case !pinned && st.tokensField == "max_completion_tokens" && hit("max_completion_tokens", "max_tokens"):
		st.tokensField = "max_tokens"
		remember(func() {
			if p.stickyTokens == nil {
				p.stickyTokens = map[string]string{}
			}
			p.stickyTokens[model] = "max_tokens"
		})
		p.c.settings.logger.Debug("openai-chat: falling back to legacy max_tokens field", "model", model)
		return true
	case !pinned && st.tokensField == "max_tokens" && hit("max_completion_tokens", "max_tokens"):
		st.tokensField = "max_completion_tokens"
		remember(func() {
			if p.stickyTokens == nil {
				p.stickyTokens = map[string]string{}
			}
			p.stickyTokens[model] = "max_completion_tokens"
		})
		p.c.settings.logger.Debug("openai-chat: upstream requires max_completion_tokens", "model", model)
		return true
	}
	return false
}

// Chat performs a non-streaming completion.
func (p *openaiChatProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/chat/completions")
	st := p.initialState(req.Model)
	sanitizes := 0
	for {
		payload, err := p.buildPayload(req, false, st)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: p.headers("application/json"),
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
			apiErr := parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model, false) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		return decodeOpenAIChatResponse(body, method, url, resp.Header.Get("X-Request-Id"))
	}
}

// StreamChat starts a streaming completion.
func (p *openaiChatProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/chat/completions")
	st := p.initialState(req.Model)
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
			Header: p.headers("text/event-stream"),
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
		apiErr := parseOpenAIError(r.StatusCode, body, method, url, r.Header.Get("X-Request-Id"))
		if r.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model, true) {
			sanitizes++
			continue
		}
		return nil, apiErr
	}
	reqID := resp.Header.Get("X-Request-Id")
	if bufferedJSONResponse(resp.Header.Get("Content-Type")) {
		// A 200 that is not event-stream carries a buffered JSON body —
		// either a complete completion or an error the SSE scanner would
		// silently drop (audit B3). Decode it and replay as one stream.
		body, rerr := readBody(resp, method, url, bodyLimit)
		if rerr != nil {
			return nil, rerr
		}
		cr, derr := decodeOpenAIChatResponse(body, method, url, reqID)
		if derr != nil {
			return nil, derr
		}
		return bufferedStream(cr), nil
	}
	s := newStream(p.streamEvents(resp.Body, method, url, reqID), nil)
	s.attachCloser(resp.Body)
	return s, nil
}

// streamEvents maps the OpenAI chunk SSE stream onto unified events.
// Usage and stop reason are captured from their chunks and delivered in a
// single synthesized EventMessageEnd, emitted at [DONE]. An EOF before
// [DONE] still yields the end event (with StopOther) but the stream then
// fails with ErrStreamTruncated, so callers never mistake a cut-off
// response for a clean finish.
func (p *openaiChatProvider) streamEvents(body io.Reader, method, url, requestID string) func() (*Event, error) {
	sc := sse.New(body)
	var (
		pending   []*Event
		startSent bool
		stop      StopReason
		usage     *Usage
		ended     bool
		truncated bool
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
			// Once the terminal signal ([DONE] or a truncated EOF) was
			// seen, never emit further events even if the server keeps
			// sending.
			if ended {
				if truncated {
					return nil, fmt.Errorf("rosetta: %w: openai-chat stream ended without [DONE] (partial response kept in Stream.Partial)", ErrStreamTruncated)
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
					// A clean close that already carried a semantic terminal
					// (finish_reason) is a complete response even without the
					// [DONE] sentinel (audit B2); only an un-terminated stream
					// counts as truncated.
					truncated = stop == ""
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
				// A malformed chunk may carry content the caller will
				// otherwise never see; dropping it silently would corrupt
				// text or tool-call arguments, so fail the stream instead.
				return nil, fmt.Errorf("rosetta: openai-chat stream (%s %s): malformed event: %w", method, url, err)
			}
			if ch.Error != nil {
				apiErr := ch.Error.apiError(200)
				apiErr.Method, apiErr.URL, apiErr.RequestID = method, url, requestID
				return nil, apiErr
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
				if choice.Index != 0 {
					// The unified event stream models a single assistant
					// turn; extra choices (n>1) would interleave their text
					// and clobber the stop reason, so only choice 0 maps.
					continue
				}
				d := &choice.Delta
				if d.Content.Set && d.Content.Value != "" {
					events = append(events, &Event{Type: EventTextDelta, Text: d.Content.Value})
				}
				if d.Refusal.Set && d.Refusal.Value != "" {
					events = append(events, &Event{Type: EventTextDelta, Text: d.Refusal.Value})
				}
				if d.ReasoningContent.Set && d.ReasoningContent.Value != "" {
					events = append(events, &Event{Type: EventThinkingDelta, Text: d.ReasoningContent.Value})
				}
				for _, tc := range d.ToolCalls {
					events = append(events, &Event{
						Type:           EventToolCall,
						ToolIndex:      tc.Index,
						ToolID:         tc.ID.Value,
						ToolName:       tc.Function.Name,
						ArgumentsDelta: tc.Function.Arguments.Value,
					})
				}
				if fc := d.FunctionCall; fc != nil && (fc.Name != "" || fc.Arguments.Value != "") {
					events = append(events, &Event{
						Type:           EventToolCall,
						ToolIndex:      0,
						ToolName:       fc.Name,
						ArgumentsDelta: fc.Arguments.Value,
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
	Param   string          `json:"param"`
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
		Param:      e.Param,
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
	Index    int              `json:"index"`
	ID       jsonx.FlexString `json:"id"`
	Type     string           `json:"type"`
	Function struct {
		Name      string               `json:"name"`
		Arguments jsonx.FlexJSONString `json:"arguments"`
	} `json:"function"`
}

// oaFunctionCall is the legacy (pre tool_calls) OpenAI shape, still returned
// by some gateways and Anthropic→OpenAI translators as
// {"name","arguments"} with finish_reason "function_call".
type oaFunctionCall struct {
	Name      string               `json:"name"`
	Arguments jsonx.FlexJSONString `json:"arguments"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string              `json:"role"`
			Content          jsonx.ContentString `json:"content"`
			ReasoningContent jsonx.ContentString `json:"reasoning_content"`
			Refusal          jsonx.FlexString    `json:"refusal"`
			ToolCalls        []oaToolCall        `json:"tool_calls"`
			FunctionCall     *oaFunctionCall     `json:"function_call"`
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
			ReasoningContent jsonx.ContentString `json:"reasoning_content"`
			Refusal          jsonx.FlexString    `json:"refusal"`
			ToolCalls        []oaToolCall        `json:"tool_calls"`
			FunctionCall     *oaFunctionCall     `json:"function_call"`
		} `json:"message"`
		FinishReason jsonx.FlexString `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage     `json:"usage"`
	Error *oaErrorBody `json:"error"`
}

func decodeOpenAIChatResponse(body []byte, rc ...string) (*ChatResponse, error) {
	var r oaResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rosetta: decoding openai-chat response: %w", err)
	}
	if r.Error != nil {
		apiErr := r.Error.apiError(200)
		attachRequest(apiErr, rc)
		return nil, apiErr
	}
	out := &ChatResponse{
		ID:    r.ID,
		Model: r.Model,
		Raw:   truncateBody(body),
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toUsage()
	}
	if len(r.Choices) == 0 {
		// A 200 with no choices is unusual but not an error: some gateways
		// return it for filtered or empty completions. Hand back a valid
		// (empty) response that keeps the billing usage instead of failing
		// the whole call with an untyped error.
		out.StopReason = StopEnd
		return out, nil
	}
	choice := &r.Choices[0]
	msg := &choice.Message
	if msg.ReasoningContent.Set && msg.ReasoningContent.Value != "" {
		out.Content = append(out.Content, Block{Type: BlockThinking, Thinking: msg.ReasoningContent.Value})
	}
	if msg.Content.Set && msg.Content.Value != "" {
		out.Content = append(out.Content, Block{Type: BlockText, Text: msg.Content.Value})
	}
	// A refusal arrives with content null; surface its text so the answer
	// does not silently vanish.
	if !msg.Content.Set && msg.Refusal.Set && msg.Refusal.Value != "" {
		out.Content = append(out.Content, Block{Type: BlockText, Text: msg.Refusal.Value})
	}
	for _, tc := range msg.ToolCalls {
		out.Content = append(out.Content, Block{
			Type:       BlockToolCall,
			ToolCallID: tc.ID.Value,
			ToolName:   tc.Function.Name,
			Arguments:  tc.Function.Arguments.Value,
		})
	}
	// Legacy function_call folds into the unified tool-call block (no id:
	// the shape predates tool call ids).
	if fc := msg.FunctionCall; fc != nil && (fc.Name != "" || fc.Arguments.Value != "") {
		out.Content = append(out.Content, Block{
			Type:      BlockToolCall,
			ToolName:  fc.Name,
			Arguments: fc.Arguments.Value,
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
		apiErr.Param = obj.Param
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
