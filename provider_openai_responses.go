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

	"github.com/cn-maul/rosetta/internal/httpx"
	"github.com/cn-maul/rosetta/internal/sse"
)

// openaiResponsesProvider adapts the OpenAI Responses protocol
// (POST /v1/responses). It shares Bearer auth and the /models catalog
// with the Chat Completions adapter but uses its own wire shapes:
// items instead of messages, max_output_tokens, reasoning.effort and
// typed stream events.
type openaiResponsesProvider struct{ c *Client }

// Chat performs a non-streaming completion.
func (p *openaiResponsesProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/responses")
	payload, err := p.buildPayload(req, false)
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
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if rerr != nil {
		return nil, transport(rerr, method, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
	}
	return decodeResponsesResponse(body)
}

// StreamChat starts a streaming completion.
func (p *openaiResponsesProvider) StreamChat(ctx context.Context, req *ChatRequest) (Stream, error) {
	const method = http.MethodPost
	url := joinEndpoint(p.c.settings.endpoint, "/responses")
	payload, err := p.buildPayload(req, true)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id"))
	}
	s := newStream(p.streamEvents(resp.Body), nil)
	s.attachCloser(resp.Body)
	return s, nil
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
func (p *openaiResponsesProvider) buildPayload(req *ChatRequest, stream bool) (map[string]any, error) {
	input, err := p.encodeInput(req)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model": req.Model,
		"input": input,
	}
	if max := p.c.effectiveMaxOutput(req); max > 0 {
		payload["max_output_tokens"] = max
	}
	if req.Thinking != nil {
		if eff := req.effort(); eff != EffortUnset {
			payload["reasoning"] = map[string]any{"effort": string(eff)}
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
	maps.Copy(payload, req.Extra)
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
			items = append(items, encodeResponsesUser(m))
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

func encodeResponsesUser(m Message) map[string]any {
	item := map[string]any{"type": "message", "role": "user"}
	var hasImage bool
	for _, b := range m.Blocks {
		if b.Type == BlockImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		item["content"] = m.text()
		return item
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
		}
	}
	item["content"] = parts
	return item
}

// streamEvents maps Responses' typed SSE stream onto unified events.
// Usage and stop status arrive with response.completed / response.incomplete
// and are folded into a single EventMessageEnd; the stream ends there
// (the Responses API does not send a [DONE] sentinel).
func (p *openaiResponsesProvider) streamEvents(body io.Reader) func() (*Event, error) {
	sc := sse.New(body)
	var (
		stop  StopReason
		usage *Usage
		ended bool
	)
	endEvent := func() *Event {
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
		for {
			// Once response.completed/incomplete (or EOF) was seen, never
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
			if len(data) == 0 {
				continue
			}
			var ev oaRespEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				p.c.settings.logger.Debug("responses: skipping malformed stream event", "err", err.Error())
				continue
			}
			switch ev.Type {
			case "response.created":
				if ev.Response != nil {
					return &Event{Type: EventMessageStart, ID: ev.Response.ID, Model: ev.Response.Model}, nil
				}
			case "response.output_item.added":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					return &Event{
						Type:      EventToolCall,
						ToolIndex: ev.OutputIndex,
						ToolID:    ev.Item.CallID,
						ToolName:  ev.Item.Name,
					}, nil
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
					return nil, ev.Response.Error.apiError(200)
				}
				return nil, &APIError{StatusCode: 200, Message: "response.failed", Type: "api_error"}
			case "error":
				return nil, (&oaErrorBody{Message: ev.Message, Type: "api_error", Code: json.RawMessage(maybeQuote(ev.Code))}).apiError(200)
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
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *oaRespUsage) toUsage() Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	out := Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  total,
	}
	if u.InputTokensDetails != nil {
		out.CachedInputTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
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
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
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
			out.Content = append(out.Content, Block{
				Type:       BlockToolCall,
				ToolCallID: item.CallID,
				ToolName:   item.Name,
				Arguments:  item.Arguments,
			})
		}
	}
	out.StopReason = mapResponsesStop(r.Status, r.IncompleteReason())
	if out.StopReason == StopOther && r.Status == "" {
		out.StopReason = StopEnd // unary 200 is complete by definition
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toUsage()
	}
	return out, nil
}
