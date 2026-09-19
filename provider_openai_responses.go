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

// openaiResponsesProvider adapts the OpenAI Responses protocol
// (POST /v1/responses). It shares Bearer auth and the /models catalog
// with the Chat Completions adapter but uses its own wire shapes:
// items instead of messages, max_output_tokens, reasoning.effort and
// typed stream events.
//
// Compatibility with third-party gateways follows the same escalation as
// Chat: a sticky probe — send the modern optional fields, drop one once
// on a matching 400 — remembering downgrades per model for the client
// lifetime. A rejection of a *value* inside a field (e.g. an unsupported
// reasoning.effort) is surfaced as-is: it is a request configuration
// error, not evidence that the field is unsupported.
type openaiResponsesProvider struct {
	c *Client

	mu sync.Mutex
	// Sticky downgrades are remembered per model: one model rejecting a
	// field says nothing about another model's capabilities.
	stickyNoReasoning map[string]bool // upstream rejected the reasoning object
	stickyNoMaxOutput map[string]bool // upstream rejected max_output_tokens
}

// respSendState is the per-request mutable send strategy.
type respSendState struct {
	reasoning bool
	maxOutput bool
}

func (p *openaiResponsesProvider) initialState(model string) *respSendState {
	st := &respSendState{reasoning: true, maxOutput: true}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stickyNoReasoning[model] {
		st.reasoning = false
	}
	if p.stickyNoMaxOutput[model] {
		st.maxOutput = false
	}
	return st
}

// isValueRejection reports whether a 400 rejects a *value* inside an
// optional field (e.g. reasoning.effort "low" unsupported by the model)
// rather than the field itself. Dropping the whole field would silently
// mask a configuration error and pollute later requests, so such errors
// are surfaced as-is.
func isValueRejection(apiErr *APIError) bool {
	if apiErr == nil {
		return false
	}
	if apiErr.Code == "unsupported_value" {
		return true
	}
	return containsAny(strings.ToLower(apiErr.Message), "unsupported value")
}

// sanitize inspects a 400 error and drops one optional field. It returns
// true when the payload changed and the request should be retried. The
// same hint-word guarding as Chat's sanitizer keeps unrelated 400s from
// triggering downgrades.
func (p *openaiResponsesProvider) sanitize(st *respSendState, apiErr *APIError, model string) bool {
	if isValueRejection(apiErr) {
		return false
	}
	low := strings.ToLower(apiErr.Message)
	if apiErr.Type != "" && !strings.Contains(strings.ToLower(apiErr.Type), "invalid_request") {
		return false
	}
	remember := func(field string, v bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch field {
		case "reasoning":
			if p.stickyNoReasoning == nil {
				p.stickyNoReasoning = map[string]bool{}
			}
			p.stickyNoReasoning[model] = v
		case "max_output":
			if p.stickyNoMaxOutput == nil {
				p.stickyNoMaxOutput = map[string]bool{}
			}
			p.stickyNoMaxOutput[model] = v
		}
	}
	switch {
	case st.reasoning && containsAny(low, "reasoning") && containsAny(low, hintWords...):
		st.reasoning = false
		remember("reasoning", true)
		p.c.settings.logger.Debug("responses: upstream rejected the reasoning field; dropping it", "model", model)
		return true
	case st.maxOutput && containsAny(low, "max_output_tokens") && containsAny(low, hintWords...):
		st.maxOutput = false
		remember("max_output", true)
		p.c.settings.logger.Debug("responses: upstream rejected max_output_tokens; letting the provider decide the cap", "model", model)
		return true
	}
	return false
}

// Chat performs a non-streaming completion.
func (p *openaiResponsesProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/responses")
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
			Header: openAIHeaders(p.c, "application/json"),
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
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		return decodeResponsesResponse(body)
	}
}

// StreamChat starts a streaming completion.
func (p *openaiResponsesProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/responses")
	st := p.initialState(req.Model)
	sanitizes := 0
	for {
		payload, err := p.buildPayload(req, true, st)
		if err != nil {
			return nil, err
		}
		call := &httpx.Call{
			Method: method,
			URL:    url,
			Header: openAIHeaders(p.c, "text/event-stream"),
			Body:   func() ([]byte, error) { return json.Marshal(payload) },
		}
		resp, err := p.c.http.Do(ctx, call)
		if err != nil {
			return nil, transport(err, method, url)
		}
		if resp.StatusCode != http.StatusOK {
			body, rerr := readBody(resp, method, url, bodyLimit)
			if rerr != nil {
				return nil, rerr
			}
			apiErr := parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		s := newStream(p.streamEvents(resp.Body, method, url, resp.Header.Get("X-Request-Id")), nil)
		s.attachCloser(resp.Body)
		return s, nil
	}
}

// ListModels queries GET /models via the shared OpenAI-family helper.
func (p *openaiResponsesProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	return listOpenAIModels(ctx, p.c)
}

// buildPayload renders the unified request into the Responses payload.
// Notes on protocol differences from Chat Completions: the output cap is
// max_output_tokens, tools are flat objects, stop sequences are not
// supported (dropped with a debug log), and sampling params are dropped
// alongside reasoning (reasoning-capable models reject them).
func (p *openaiResponsesProvider) buildPayload(req *ChatRequest, stream bool, st *respSendState) (map[string]any, error) {
	input, err := p.encodeInput(req)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model": req.Model,
		"input": input,
	}
	if st.maxOutput {
		if max := p.c.effectiveMaxOutput(req); max > 0 {
			payload["max_output_tokens"] = max
		}
	}
	if req.Thinking != nil {
		if st.reasoning {
			if eff := req.effort(); eff != EffortUnset {
				payload["reasoning"] = map[string]any{"effort": string(eff)}
			}
		}
		if req.Temperature != nil {
			p.c.settings.logger.Debug("responses: dropping temperature for a reasoning request")
		}
		if req.TopP != nil {
			p.c.settings.logger.Debug("responses: dropping top_p for a reasoning request")
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
		p.c.settings.logger.Debug("responses: stop sequences are not supported by this protocol; dropping them")
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			})
		}
		payload["tools"] = tools
	}
	if stream {
		payload["stream"] = true
	}
	if err := mergeExtra(payload, req.Extra, p.c.settings.extraOverrides, chatReservedPayloadKeys); err != nil {
		return nil, err
	}
	return payload, nil
}

// encodeInput maps unified messages onto Responses input items.
func (p *openaiResponsesProvider) encodeInput(req *ChatRequest) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(req.Messages)+1)
	addSystem := func(text string) {
		items = append(items, map[string]any{"type": "message", "role": "system", "content": text})
	}
	if req.System != "" {
		addSystem(req.System)
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if t := m.text(); t != "" {
				addSystem(t)
			}
		case RoleUser:
			item, err := encodeResponsesUser(m)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		case RoleAssistant:
			if t := m.text(); t != "" {
				items = append(items, map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": t},
					},
				})
			}
			for _, b := range m.Blocks {
				if b.Type == BlockToolCall {
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   b.ToolCallID,
						"name":      b.ToolName,
						"arguments": b.Arguments,
					})
				}
			}
		case RoleTool:
			for _, b := range m.Blocks {
				if b.Type == BlockToolResult {
					items = append(items, map[string]any{
						"type":    "function_call_output",
						"call_id": b.ToolCallID,
						"output":  b.Content,
					})
				}
			}
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidRequest, m.Role)
		}
	}
	return items, nil
}

func encodeResponsesUser(m Message) (map[string]any, error) {
	item := map[string]any{"type": "message", "role": "user"}
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
		item["content"] = m.text()
		return item, nil
	}
	parts := make([]map[string]any, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Type {
		case BlockText:
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
			}
		case BlockImage:
			parts = append(parts, map[string]any{"type": "input_image", "image_url": b.ImageURL})
		case BlockAudio:
			// The official Responses input content union carries text,
			// image and file parts only; input_audio is a Chat
			// Completions shape. Encoding it anyway would produce requests
			// the official API does not define.
			return nil, fmt.Errorf("%w: responses protocol does not support audio input; use Chat Completions (ProtoOpenAIChat) for audio", ErrInvalidRequest)
		case BlockFile:
			if b.FileID != "" {
				parts = append(parts, map[string]any{"type": "input_file", "file_id": b.FileID})
				continue
			}
			// The official Responses file input carries inline data via
			// file_data and remote files via file_url; only Chat
			// Completions lacks the URL form.
			if strings.HasPrefix(b.FileData, "http://") || strings.HasPrefix(b.FileData, "https://") {
				parts = append(parts, map[string]any{"type": "input_file", "file_url": b.FileData})
				continue
			}
			du, err := fileDataURL(b)
			if err != nil {
				return nil, err
			}
			part := map[string]any{"type": "input_file", "file_data": du}
			if b.FileName != "" {
				part["filename"] = b.FileName
			}
			parts = append(parts, part)
		}
	}
	item["content"] = parts
	return item, nil
}

// streamEvents maps Responses' typed SSE stream onto unified events.
// Usage and stop status arrive with response.completed / response.incomplete
// and are folded into a single EventMessageEnd; the stream ends there
// (the Responses API does not send a [DONE] sentinel). An EOF before
// either event still yields the end event (with StopOther) but the stream
// then fails with ErrStreamTruncated, so callers never mistake a cut-off
// response for a clean finish.
func (p *openaiResponsesProvider) streamEvents(body io.Reader, method, url, requestID string) func() (*Event, error) {
	sc := sse.New(body)
	var (
		stop        StopReason
		usage       *Usage
		ended       bool
		truncated   bool
		sawToolCall bool
		// announced guards output_item.added vs output_item.done so a
		// minimal emitter that skips the incremental events still has its
		// finalized function_call delivered once.
		announced map[int]bool
	)
	endEvent := func() *Event {
		// A completed turn that requested a tool call ends as tool_use, not
		// end_turn: the Responses API reports status "completed" even when
		// the model emitted a function_call, so the presence of a tool call
		// is the authoritative signal.
		if stop == "" {
			if sawToolCall {
				stop = StopToolUse
			} else {
				stop = StopOther
			}
		} else if stop == StopEnd && sawToolCall {
			stop = StopToolUse
		}
		ev := &Event{Type: EventMessageEnd, StopReason: stop}
		if usage != nil {
			ev.Usage = usage
		}
		return ev
	}
	return func() (*Event, error) {
		for {
			// Once response.completed/incomplete (or a truncated EOF) was
			// seen, never emit further events even if the server keeps
			// sending.
			if ended {
				if truncated {
					return nil, fmt.Errorf("rosetta: %w: responses stream ended without response.completed (partial response kept in Stream.Partial)", ErrStreamTruncated)
				}
				return nil, io.EOF
			}
			ssev, err := sc.Next()
			if err != nil {
				// io.EOF covers a clean server-side close; io.ErrUnexpectedEOF
				// covers a real HTTP truncation (chunked stream cut before the
				// final zero chunk, gateway timeout). Both mean the terminal
				// event never arrived.
				if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && !ended {
					ended = true
					truncated = true
					return endEvent(), nil
				}
				return nil, err
			}
			data := bytes.TrimSpace(ssev.Data)
			if len(data) == 0 {
				continue
			}
			var ev oaRespEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				// A malformed event may carry content the caller will
				// otherwise never see; dropping it silently would corrupt
				// text or tool-call arguments, so fail the stream instead.
				return nil, fmt.Errorf("rosetta: responses stream (%s %s): malformed event: %w", method, url, err)
			}
			// Dispatch on the JSON type field, but fall back to the SSE
			// `event:` name: a proxy that forwards the envelope unchanged may
			// drop or fail to populate the inner type.
			dtype := ev.Type
			if dtype == "" {
				dtype = ssev.Name
			}
			switch dtype {
			case "response.created":
				if ev.Response != nil {
					return &Event{Type: EventMessageStart, ID: ev.Response.ID, Model: ev.Response.Model}, nil
				}
			case "response.output_item.added":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					sawToolCall = true
					if announced == nil {
						announced = map[int]bool{}
					}
					announced[ev.OutputIndex] = true
					return &Event{
						Type:      EventToolCall,
						ToolIndex: ev.OutputIndex,
						ToolID:    ev.Item.CallID,
						ToolName:  ev.Item.Name,
					}, nil
				}
			case "response.output_item.done":
				// The finalized item is normally assembled from the earlier
				// added + argument-delta events. Only when a minimal emitter
				// skipped those do we surface the complete function_call here
				// so its id/name/arguments are never lost.
				if ev.Item != nil && ev.Item.Type == "function_call" {
					sawToolCall = true
					if !announced[ev.OutputIndex] {
						if announced == nil {
							announced = map[int]bool{}
						}
						announced[ev.OutputIndex] = true
						return &Event{
							Type:           EventToolCall,
							ToolIndex:      ev.OutputIndex,
							ToolID:         ev.Item.CallID,
							ToolName:       ev.Item.Name,
							ArgumentsDelta: ev.Item.Arguments,
						}, nil
					}
				}
			case "response.output_text.delta":
				if ev.Delta != "" {
					return &Event{Type: EventTextDelta, Text: ev.Delta}, nil
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if ev.Delta != "" {
					return &Event{Type: EventThinkingDelta, Text: ev.Delta}, nil
				}
			case "response.function_call_arguments.delta":
				if ev.Delta != "" {
					sawToolCall = true
					return &Event{
						Type:           EventToolCall,
						ToolIndex:      ev.OutputIndex,
						ArgumentsDelta: ev.Delta,
					}, nil
				}
			case "response.completed":
				if ev.Response != nil {
					if ev.Response.Usage != nil {
						u := ev.Response.Usage.toUsage()
						usage = &u
					}
					stop = mapResponsesStop(ev.Response.Status, ev.Response.IncompleteReason())
				}
				if !ended {
					ended = true
					return endEvent(), nil
				}
			case "response.incomplete":
				if ev.Response != nil {
					if ev.Response.Usage != nil {
						u := ev.Response.Usage.toUsage()
						usage = &u
					}
					stop = mapResponsesStop(ev.Response.Status, ev.Response.IncompleteReason())
				}
				if !ended {
					ended = true
					return endEvent(), nil
				}
			case "response.failed":
				if ev.Response != nil && ev.Response.Error != nil {
					apiErr := ev.Response.Error.apiError(200)
					apiErr.Method, apiErr.URL, apiErr.RequestID = method, url, requestID
					return nil, apiErr
				}
				return nil, &APIError{StatusCode: 200, Message: "response.failed", Type: "api_error", Method: method, URL: url, RequestID: requestID}
			case "error":
				apiErr := (&oaErrorBody{Message: ev.Message, Type: "api_error", Code: json.RawMessage(maybeQuote(ev.Code))}).apiError(200)
				apiErr.Method, apiErr.URL, apiErr.RequestID = method, url, requestID
				return nil, apiErr
			default:
				// in_progress, content_part.*, *_done, output_item.done, ... are ignored
			}
		}
	}
}

func mapResponsesStop(status, reason string) StopReason {
	switch status {
	case "completed":
		return StopEnd
	case "incomplete":
		switch reason {
		case "max_output_tokens":
			return StopLength
		case "content_filter":
			return StopContentFilter
		default:
			return StopOther
		}
	default:
		return StopOther
	}
}

// maybeQuote wraps a bare string in quotes so it can serve as
// json.RawMessage (error codes arrive as plain strings on error events).
func maybeQuote(s string) string {
	if s == "" {
		return ""
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// ---- wire types ----

type oaRespUsage struct {
	InputTokens        jsonx.FlexInt64 `json:"input_tokens"`
	OutputTokens       jsonx.FlexInt64 `json:"output_tokens"`
	TotalTokens        jsonx.FlexInt64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens jsonx.FlexInt64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens jsonx.FlexInt64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *oaRespUsage) toUsage() Usage {
	total := u.TotalTokens.Value
	if total == 0 {
		total = u.InputTokens.Value + u.OutputTokens.Value
	}
	out := Usage{
		InputTokens:  u.InputTokens.Value,
		OutputTokens: u.OutputTokens.Value,
		TotalTokens:  total,
	}
	if u.InputTokensDetails != nil {
		out.CachedInputTokens = u.InputTokensDetails.CachedTokens.Value
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens.Value
	}
	return out
}

type oaRespOutputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"` // output_text | reasoning_text
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"` // summary_text
		Text string `json:"text"`
	} `json:"summary"`
}

type oaRespResponse struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error  *oaErrorBody       `json:"error"`
	Output []oaRespOutputItem `json:"output"`
	Usage  *oaRespUsage       `json:"usage"`
}

func (r *oaRespResponse) IncompleteReason() string {
	if r.IncompleteDetails != nil {
		return r.IncompleteDetails.Reason
	}
	return ""
}

type oaRespEvent struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Item        *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *oaRespResponse `json:"response"`
}

func decodeResponsesResponse(body []byte) (*ChatResponse, error) {
	var r oaRespResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rosetta: decoding responses payload: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error.apiError(200)
	}
	out := &ChatResponse{ID: r.ID, Model: r.Model, Raw: truncateBody(body)}
	var sawToolCall bool
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					out.Content = append(out.Content, Block{Type: BlockText, Text: part.Text})
				}
			}
		case "reasoning":
			var th strings.Builder
			for _, part := range item.Summary {
				if part.Text != "" {
					if th.Len() > 0 {
						th.WriteString("\n")
					}
					th.WriteString(part.Text)
				}
			}
			for _, part := range item.Content {
				if part.Type == "reasoning_text" && part.Text != "" {
					if th.Len() > 0 {
						th.WriteString("\n")
					}
					th.WriteString(part.Text)
				}
			}
			if th.Len() > 0 {
				out.Content = append(out.Content, Block{Type: BlockThinking, Thinking: th.String()})
			}
		case "function_call":
			sawToolCall = true
			out.Content = append(out.Content, Block{
				Type:       BlockToolCall,
				ToolCallID: item.CallID,
				ToolName:   item.Name,
				Arguments:  item.Arguments,
			})
		}
	}
	out.StopReason = mapResponsesStop(r.Status, r.IncompleteReason())
	// A completed response that requested a tool call is tool_use, not a
	// plain end_turn: status alone does not distinguish them.
	if out.StopReason == StopEnd && sawToolCall {
		out.StopReason = StopToolUse
	}
	if out.StopReason == StopOther && r.Status == "" {
		out.StopReason = StopEnd // unary 200 is complete by definition
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toUsage()
	}
	return out, nil
}
