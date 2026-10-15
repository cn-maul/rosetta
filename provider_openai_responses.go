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

	mu sync.RWMutex
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
	p.mu.RLock()
	defer p.mu.RUnlock()
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
	case st.reasoning && rejectionHit(apiErr, "reasoning"):
		st.reasoning = false
		remember("reasoning", true)
		p.c.settings.logger.Debug("responses: upstream rejected the reasoning field; dropping it", "model", model)
		return true
	case st.maxOutput && rejectionHit(apiErr, "max_output_tokens"):
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
		hdr := openAIHeaders(p.c, "application/json")
		retry := httpx.RetryIdempotent
		if p.c.settings.quirks.NoIdempotencyKey {
			retry = httpx.RetryDefault
		} else {
			hdr.Set("Idempotency-Key", newIdempotencyKey())
		}
		call := &httpx.Call{
			Method:      method,
			URL:         url,
			Header:      hdr,
			Body:        func() ([]byte, error) { return json.Marshal(payload) },
			RetryPolicy: retry,
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
			apiErr := withRetryAfter(parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id")), resp.Header)
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		return decodeResponsesResponse(body, method, url, resp.Header.Get("X-Request-Id"))
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
		hdr := openAIHeaders(p.c, "text/event-stream")
		retry := httpx.RetryIdempotent
		if p.c.settings.quirks.NoIdempotencyKey {
			retry = httpx.RetryDefault
		} else {
			hdr.Set("Idempotency-Key", newIdempotencyKey())
		}
		call := &httpx.Call{
			Method:      method,
			URL:         url,
			Header:      hdr,
			Body:        func() ([]byte, error) { return json.Marshal(payload) },
			RetryPolicy: retry,
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
			apiErr := withRetryAfter(parseOpenAIError(resp.StatusCode, body, method, url, resp.Header.Get("X-Request-Id")), resp.Header)
			if resp.StatusCode == http.StatusBadRequest && sanitizes < 4 && p.sanitize(st, apiErr, req.Model) {
				sanitizes++
				continue
			}
			return nil, apiErr
		}
		reqID := resp.Header.Get("X-Request-Id")
		if bufferedJSONResponse(resp.Header.Get("Content-Type")) {
			body, rerr := readBody(resp, method, url, bodyLimit)
			if rerr != nil {
				return nil, rerr
			}
			cr, derr := decodeResponsesResponse(body, method, url, reqID)
			if derr != nil {
				return nil, derr
			}
			return bufferedStream(cr), nil
		}
		s := newStream(p.streamEvents(resp.Body, method, url, reqID), nil)
		s.setIdle(p.c.settings.streamIdle)
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
	// Sampling params (temperature/top_p) are only rejected when a
	// reasoning config actually rides the wire; gate the drop on that,
	// not merely on the caller having set Thinking. Two cases send no
	// reasoning: a sticky drop (st.reasoning == false) and Thinking == nil.
	// A Thinking with no level named is not one of them — effort() defaults
	// it to medium, so it emits and does drop these.
	emitReasoning := false
	if req.Thinking != nil && st.reasoning {
		// effortWire honors EffortRaw (see ThinkingConfig.EffortRaw) and is
		// otherwise identical to effort(), so this is behavior-preserving.
		// The guard is defensive: non-nil Thinking never yields an empty
		// value thanks to the medium default.
		if eff := req.effortWire(); eff != "" {
			payload["reasoning"] = map[string]any{"effort": eff}
			emitReasoning = true
		}
	}
	if emitReasoning {
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
	if err := mergeExtra(payload, req.Extra, p.c.settings.extraOverrides, openaiResponsesReservedPayloadKeys); err != nil {
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
			// Blocks are emitted in their original order. Collecting all
			// text up front and appending the rest afterwards would move a
			// text block that came after a reasoning item or a tool call to
			// before it, which is exactly the reordering that breaks a
			// multi-turn tool conversation.
			var textGroup []string
			flushText := func() {
				if len(textGroup) == 0 {
					return
				}
				content := make([]map[string]any, 0, len(textGroup))
				for _, t := range textGroup {
					content = append(content, map[string]any{"type": "output_text", "text": t})
				}
				items = append(items, map[string]any{
					"type": "message", "role": "assistant", "content": content,
				})
				textGroup = nil
			}
			for _, b := range m.Blocks {
				switch b.Type {
				case BlockText:
					if b.Text != "" {
						textGroup = append(textGroup, b.Text)
					}
					continue
				default:
					flushText()
				}
				switch b.Type {
				case BlockToolCall:
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   b.ToolCallID,
						"name":      b.ToolName,
						"arguments": b.Arguments,
					})
				case BlockThinking:
					// A sealed payload only rides this API family's own
					// reasoning item — it was sealed by OpenAI and is
					// meaningless to another upstream. SealedBy gates it so a
					// conversation routed elsewhere drops the seal instead
					// of sending foreign opaque state to a provider that
					// never issued it.
					if b.Sealed == "" || b.SealedBy != "openai-responses" {
						continue
					}
					item := map[string]any{
						"type":              "reasoning",
						"encrypted_content": b.Sealed,
					}
					// The summary is what the model sees. It is sent as an
					// explicit array even when empty: the reasoning item
					// shape requires one, and omitting it makes the item
					// invalid for an upstream that validates strictly.
					summary := []map[string]any{}
					if b.Thinking != "" {
						summary = append(summary, map[string]any{"type": "summary_text", "text": b.Thinking})
					}
					item["summary"] = summary
					// The item id is what makes the item addressable on the
					// next turn, so it is carried back verbatim. A caller
					// that built the block by hand without one keeps the
					// previous behavior of omitting it.
					if b.ReasoningID != "" {
						item["id"] = b.ReasoningID
					}
					items = append(items, item)
				}
			}
			flushText()
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
//
// Reasoning items carry an opaque sealed payload that must be replayed
// verbatim to keep a multi-turn conversation intact, and the payload only
// arrives on the terminating events. The item registry below therefore
// tracks reasoning items by identity (item id, then output index) so their
// text, sealed payload and source position all land in the same block, and
// keeps a bounded queue of content that a terminating event contributes
// before the stream emits its single EventMessageEnd.
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
		announced     = map[int]bool{}
		argDeltaSeen  = map[int]bool{}
		textDeltaSeen = map[int]bool{}
		// toolIdxByID keys function-call events by item_id so argument
		// deltas coalesce even when output_index is absent or reused.
		toolIdxByID map[string]int
		// nextToolIdx allocates a fresh unified index when an emitter omits
		// output_index; lastToolIdx tracks the most recent call for orphan
		// argument deltas.
		nextToolIdx int
		lastToolIdx = -1
		items       = newItemRegistry()
	)
	finalizeItem := func(item *oaRespOutputItem, outIdx *int) ([]*Event, error) {
		return items.finalize(item, outIdx)
	}
	finalizeFromEvent := func(ev oaRespEvent) ([]*Event, error) {
		if ev.Item == nil {
			return nil, nil
		}
		cp := *ev.Item
		cp.eventID = ev.ItemID
		return items.finalize(&cp, ev.OutputIndex)
	}
	malformed := func(what string) error {
		return fmt.Errorf("rosetta: responses stream (%s %s): %s: %w", method, url, what, ErrUpstreamMalformed)
	}
	// queue holds events a terminating event contributed but that have not
	// been delivered yet. It is drained before the single end event, so
	// content always precedes EventMessageEnd.
	queue := []*Event{}
	enqueue := func(evs ...*Event) {
		queue = append(queue, evs...)
	}
	nextQueued := func() (*Event, bool) {
		if len(queue) == 0 {
			return nil, false
		}
		ev := queue[0]
		queue = queue[1:]
		queue = queue[:len(queue):len(queue)] // keep the backing array bounded
		return ev, true
	}
	// resolveToolIdx returns the unified ToolIndex for a function-call item
	// announcement, preferring a registered item_id, then an explicit
	// output_index, and finally allocating the next slot when the emitter
	// omits both.
	resolveToolIdx := func(itemID string, outIdx *int) int {
		if itemID != "" {
			if i, ok := toolIdxByID[itemID]; ok {
				return i
			}
		}
		idx := -1
		if outIdx != nil {
			idx = *outIdx
		}
		if idx == -1 {
			idx = nextToolIdx
			nextToolIdx++
		} else if idx >= nextToolIdx {
			nextToolIdx = idx + 1
		}
		if itemID != "" {
			if toolIdxByID == nil {
				toolIdxByID = map[string]int{}
			}
			toolIdxByID[itemID] = idx
		}
		return idx
	}
	// deltaToolIdx maps an argument delta to its call. Deltas must attach to
	// an already-announced call, so it never allocates: item_id first, then
	// output_index, then the most recent call.
	deltaToolIdx := func(itemID string, outIdx *int) int {
		if itemID != "" {
			if i, ok := toolIdxByID[itemID]; ok {
				return i
			}
		}
		if outIdx != nil {
			if itemID != "" {
				if toolIdxByID == nil {
					toolIdxByID = map[string]int{}
				}
				toolIdxByID[itemID] = *outIdx
			}
			return *outIdx
		}
		if lastToolIdx >= 0 {
			return lastToolIdx
		}
		return 0
	}
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
					return nil, fmt.Errorf("%w: responses stream ended without response.completed (partial response kept in Stream.Partial)", ErrStreamTruncated)
				}
				// Content a terminating event contributed is delivered
				// before the stream reports its end.
				if ev, ok := nextQueued(); ok {
					return ev, nil
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
				//
				// NOT ErrUpstreamMalformed: same reasoning as the openai-chat
				// stream decoder — a stream error may arrive after the caller
				// already received events, so "fail over and retry" would
				// duplicate delivered output. See ErrUpstreamMalformed's doc
				// and the comment on the openai-chat site.
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
				// A reasoning announcement registers identity and order only.
				// Its encrypted_content is explicitly not final yet, so it is
				// never surfaced as a sealed payload, and an item with no
				// readable text yet must not create an empty content block.
				if ev.Item != nil && ev.Item.Type == "reasoning" {
					id, err := idFrom(ev.ItemID, ev.Item.ID)
					if err != nil {
						return nil, malformed(errIDConflict.Error())
					}
					if _, err := items.resolve(id, ev.OutputIndex); err != nil {
						return nil, mapItemErr(err, method, url)
					}
					continue
				}
				if ev.Item != nil && ev.Item.Type == "function_call" {
					sawToolCall = true
					idx := resolveToolIdx(ev.ItemID, ev.OutputIndex)
					announced[idx] = true
					lastToolIdx = idx
					return &Event{
						Type:      EventToolCall,
						ToolIndex: idx,
						ToolID:    ev.Item.CallID,
						ToolName:  ev.Item.Name,
					}, nil
				}
			case "response.output_item.done":
				if ev.Item == nil {
					break
				}
				switch ev.Item.Type {
				case "reasoning":
					// The only place the final sealed payload is guaranteed:
					// an id-less event finds its item through the output index,
					// so the payload lands in the block the text deltas built.
					evs, err := finalizeFromEvent(ev)
					if err != nil {
						return nil, mapItemErr(err, method, url)
					}
					if len(evs) == 0 {
						continue
					}
					enqueue(evs...)
					if ev, ok := nextQueued(); ok {
						return ev, nil
					}
					continue
				case "function_call":
					sawToolCall = true
					idx := resolveToolIdx(ev.ItemID, ev.OutputIndex)
					if !announced[idx] {
						announced[idx] = true
						lastToolIdx = idx
						return &Event{
							Type:           EventToolCall,
							ToolIndex:      idx,
							ToolID:         ev.Item.CallID,
							ToolName:       ev.Item.Name,
							ArgumentsDelta: ev.Item.Arguments.Value,
						}, nil
					}
					// Announced earlier but no argument delta ever arrived:
					// fill in the finalized arguments (the block's args are
					// still empty, so this completes rather than corrupts).
					if !argDeltaSeen[idx] && ev.Item.Arguments.Value != "" {
						return &Event{
							Type:           EventToolCall,
							ToolIndex:      idx,
							ToolID:         ev.Item.CallID,
							ToolName:       ev.Item.Name,
							ArgumentsDelta: ev.Item.Arguments.Value,
						}, nil
					}
				case "message":
					// A minimal emitter may finalize a message only here,
					// with no output_text.delta events; surface its text so
					// the reply is never lost.
					ti := 0
					if ev.OutputIndex != nil {
						ti = *ev.OutputIndex
					}
					if !textDeltaSeen[ti] {
						var sb strings.Builder
						for _, part := range ev.Item.Content {
							if part.Type == "output_text" && part.Text != "" {
								if sb.Len() > 0 {
									sb.WriteString("\n")
								}
								sb.WriteString(part.Text)
							}
						}
						if sb.Len() > 0 {
							return &Event{Type: EventTextDelta, Text: sb.String()}, nil
						}
					}
				}
			case "response.output_text.delta", "response.text.delta":
				if ev.Delta != "" {
					// Each message item is its own block: without an index a
					//// second message item's text would fold into the first.
					ti := 0
					if ev.OutputIndex != nil {
						ti = *ev.OutputIndex
					}
					textDeltaSeen[ti] = true
					out := &Event{Type: EventTextDelta, Text: ev.Delta, BlockIndex: ti}
					if ev.OutputIndex != nil {
						idx := *ev.OutputIndex
						out.OutputIndex = &idx
					}
					return out, nil
				}
			case "response.refusal.delta":
				if ev.Delta != "" {
					return &Event{Type: EventTextDelta, Text: ev.Delta}, nil
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if ev.Delta != "" {
					id, err := idFrom(ev.ItemID, "")
					if err != nil {
						return nil, malformed(errIDConflict.Error())
					}
					it, err := items.resolve(id, ev.OutputIndex)
					if err != nil {
						return nil, mapItemErr(err, method, url)
					}
					// The part this delta belongs to, so the terminating event
					// does not append the same text again.
					if ev.SummaryIndex != nil {
						it.sawPart("summary", *ev.SummaryIndex)
					} else {
						it.sawPart("summary", 0)
					}
					if ev.ContentIndex != nil {
						it.sawPart("content", *ev.ContentIndex)
					}
					out := &Event{
						Type: EventThinkingDelta, BlockIndex: it.slot,
						Text: ev.Delta, ReasoningID: it.id,
					}
					if ev.OutputIndex != nil {
						idx := *ev.OutputIndex
						out.OutputIndex = &idx
					}
					return out, nil
				}
			case "response.function_call_arguments.delta":
				if ev.Delta != "" {
					sawToolCall = true
					idx := deltaToolIdx(ev.ItemID, ev.OutputIndex)
					argDeltaSeen[idx] = true
					return &Event{
						Type:           EventToolCall,
						ToolIndex:      idx,
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
					// The terminating output may be the only place an item's
					// final text and sealed payload appear, so it is folded
					// before the end event rather than dropped.
					for i := range ev.Response.Output {
						item := &ev.Response.Output[i]
						if item.Type != "reasoning" {
							continue
						}
						evs, ferr := finalizeItem(item, itemOutputIndex(&ev.Response.Output, i))
						if ferr != nil {
							return nil, mapItemErr(ferr, method, url)
						}
						enqueue(evs...)
					}
				}
				if !ended {
					ended = true
					// The end event goes on the same queue so every event a
					// terminating response contributed is delivered first, and
					// the stream still reports its terminal state exactly once.
					enqueue(endEvent())
					if ev, ok := nextQueued(); ok {
						return ev, nil
					}
				}
			case "response.incomplete":
				if ev.Response != nil {
					if ev.Response.Usage != nil {
						u := ev.Response.Usage.toUsage()
						usage = &u
					}
					stop = mapResponsesStop(ev.Response.Status, ev.Response.IncompleteReason())
					for i := range ev.Response.Output {
						item := &ev.Response.Output[i]
						if item.Type != "reasoning" {
							continue
						}
						evs, ferr := finalizeItem(item, itemOutputIndex(&ev.Response.Output, i))
						if ferr != nil {
							return nil, mapItemErr(ferr, method, url)
						}
						enqueue(evs...)
					}
				}
				if !ended {
					ended = true
					enqueue(endEvent())
					if ev, ok := nextQueued(); ok {
						return ev, nil
					}
				}
			case "response.failed":
				if ev.Response != nil && ev.Response.Error != nil {
					apiErr := ev.Response.Error.apiError(200)
					apiErr.InBand = true
					apiErr.Method, apiErr.URL, apiErr.RequestID = method, url, requestID
					return nil, apiErr
				}
				return nil, &APIError{StatusCode: 200, InBand: true, Message: "response.failed", Type: "api_error", Method: method, URL: url, RequestID: requestID}
			case "error":
				// The Responses error envelope nests details under "error"
				// ({"type":"error","error":{...}}); read that first and fall
				// back to the flat shape only for non-conforming emitters.
				var apiErr *APIError
				if ev.Error != nil {
					apiErr = ev.Error.apiError(200)
				} else {
					apiErr = (&oaErrorBody{Message: ev.Message, Type: "api_error", Code: json.RawMessage(maybeQuote(ev.Code))}).apiError(200)
				}
				apiErr.InBand = true
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
		case "refusal":
			return StopRefusal
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
	Type string `json:"type"`
	ID   string `json:"id"` // reasoning items; replayed verbatim
	// eventID is the enclosing event's item_id when it carried one. It is
	// not part of the wire shape; the stream sets it before finalizing so a
	// terminating event that omits the nested id can still be matched.
	eventID   string
	Role      string               `json:"role"`
	CallID    string               `json:"call_id"`
	Name      string               `json:"name"`
	Arguments jsonx.FlexJSONString `json:"arguments"`
	// EncryptedContent is the opaque reasoning payload of a reasoning item.
	// OpenAI requires it back, unchanged, for the conversation to continue
	// across turns; it is never interpreted.
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
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
	ItemID      string `json:"item_id"`
	OutputIndex *int   `json:"output_index"`
	// SummaryIndex / ContentIndex locate a reasoning part inside its item.
	// Pointers so a genuine 0 is distinguishable from an absent field.
	SummaryIndex *int              `json:"summary_index"`
	ContentIndex *int              `json:"content_index"`
	Delta        string            `json:"delta"`
	Code         string            `json:"code"`
	Message      string            `json:"message"`
	Error        *oaErrorBody      `json:"error"`
	Item         *oaRespOutputItem `json:"item"`
	Response     *oaRespResponse   `json:"response"`
}

func decodeResponsesResponse(body []byte, rc ...string) (*ChatResponse, error) {
	var r oaRespResponse
	if err := json.Unmarshal(body, &r); err != nil {
		// Unary response body decode failure: upstream fault, not the
		// caller's — see ErrUpstreamMalformed. Same %w/%w shape and safety
		// argument as decodeOpenAIChatResponse.
		return nil, fmt.Errorf("%w: responses payload: %w", ErrUpstreamMalformed, err)
	}
	if r.Error != nil {
		apiErr := r.Error.apiError(200)
		apiErr.InBand = true
		attachRequest(apiErr, rc)
		return nil, apiErr
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
			// The item id is what makes the reasoning addressable on the
			// next turn, so it travels with the block.
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
			// A reasoning item with an encrypted payload but no readable
			// text is still a real turn: it carries the state OpenAI needs
			// to continue, and dropping it because "there is nothing to
			// show" would break the next replay. The seal goes in Sealed,
			// never mixed into the displayable Thinking text.
			if th.Len() > 0 || item.EncryptedContent != "" {
				out.Content = append(out.Content, Block{
					Type:        BlockThinking,
					Thinking:    th.String(),
					Sealed:      item.EncryptedContent,
					SealedBy:    "openai-responses",
					ReasoningID: item.ID,
				})
			}
		case "function_call":
			sawToolCall = true
			out.Content = append(out.Content, Block{
				Type:       BlockToolCall,
				ToolCallID: item.CallID,
				ToolName:   item.Name,
				Arguments:  item.Arguments.Value,
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

// itemRegistry tracks reasoning items across the Responses SSE stream.
//
// A reasoning item's sealed payload arrives only on its terminating event,
// which may carry no item id at all, so the item must be re-identified
// from whatever the event does carry. Identity is resolved from the item
// id first and the output index second, and each item owns one internal
// block slot for the whole stream: text deltas, the final text backfill and
// the sealed payload all fold into that same slot.
type itemRegistry struct {
	byID    map[string]*respItem
	byIndex map[int]*respItem
	slots   []*respItem
	// anonymous is the single id-less reasoning item a minimal emitter may
	// produce. Nil once an item with an identity has been seen, so a
	// mislabeled later event cannot be folded into it.
	anonymous *respItem
	idsBytes  int
	nextSlot  int
}

// respItem is one tracked output item.
type respItem struct {
	slot      int
	id        string
	order     int
	orderSet  bool
	indexBind map[int]bool // output indices claimed for this item

	// partOut[channel][index] records that a part's text has already been
	// delivered, so a terminating event does not append it a second time.
	partOut map[string]map[int]bool

	sealed     string
	sealedBy   string
	sealedDone bool
	finalized  bool
}

// maxTrackedItemIDs bounds the retained item ids. A metadata-only emitter
// could otherwise grow the registry without bound, bypassing the stream
// block cap that guards accumulated content.
const maxTrackedItemIDs = maxStreamBlocks

func newItemRegistry() *itemRegistry {
	return &itemRegistry{
		byID:    map[string]*respItem{},
		byIndex: map[int]*respItem{},
	}
}

// idFrom derives the item id from an event, preferring the nested item id
// and falling back to the top-level item_id. Both present and different is
// an upstream inconsistency rather than something to silently pick.
func idFrom(eventID, nestedID string) (string, error) {
	if nestedID == "" {
		return eventID, nil
	}
	if eventID != "" && eventID != nestedID {
		return "", errIDConflict
	}
	return nestedID, nil
}

var errIDConflict = errors.New("conflicting item ids")

// resolve returns the item an event refers to, registering it when needed.
//
// id is the event's item id (may be empty), outIdx its output index (nil
// when absent). The returned item is never nil unless the stream has run
// out of trackable items, which is reported through errTooManyItems.
func (r *itemRegistry) resolve(id string, outIdx *int) (*respItem, error) {
	if id != "" {
		if it := r.byID[id]; it != nil {
			if outIdx != nil {
				it.bindIndex(*outIdx)
			}
			return it, nil
		}
	}
	// No known id: an unambiguous output index may already name the item,
	// which is how a terminating event without an id finds its item.
	if outIdx != nil {
		if it := r.byIndex[*outIdx]; it != nil {
			if it.id == "" || it.id == id {
				if id != "" {
					r.registerID(it, id)
				}
				return it, nil
			}
			// The index belongs to a different, already-identified item.
			// Folding this event into it would put one item's text or
			// sealed payload into another item's block, so the event is
			// rejected instead of guessed at.
			return nil, errIndexTaken
		}
	}
	// A brand-new item. An anonymous one is only allowed while nothing has
	// been identified, and there may only be one of them.
	if id == "" {
		if r.anonymous != nil {
			return r.anonymous, nil
		}
		if r.hasIdentified() {
			return nil, errUnidentified
		}
	}
	if len(r.slots) >= maxStreamBlocks {
		return nil, errTooManyItems
	}
	it := &respItem{slot: r.nextSlot, id: id, indexBind: map[int]bool{}}
	r.nextSlot++
	r.slots = append(r.slots, it)
	if id != "" {
		r.registerID(it, id)
	} else {
		r.anonymous = it
	}
	if outIdx != nil {
		it.bindIndex(*outIdx)
	}
	return it, nil
}

func (r *itemRegistry) registerID(it *respItem, id string) {
	if r.byID[id] == it {
		return
	}
	if r.idsBytes+len(id) > maxTrackedItemIDs*maxItemIDBytes {
		return
	}
	r.idsBytes += len(id)
	r.byID[id] = it
	it.id = id
}

// maxItemIDBytes bounds the retained id bytes; ids are short opaque
// strings, so this only guards against a hostile emitter.
const maxItemIDBytes = 1 << 10

func (r *itemRegistry) hasIdentified() bool {
	for _, it := range r.slots {
		if it.id != "" {
			return true
		}
	}
	return false
}

func (it *respItem) bindIndex(idx int) {
	it.indexBind[idx] = true
}

// sawPart records that a part's text was delivered.
func (it *respItem) sawPart(channel string, index int) {
	if it.partOut == nil {
		it.partOut = map[string]map[int]bool{}
	}
	if it.partOut[channel] == nil {
		it.partOut[channel] = map[int]bool{}
	}
	it.partOut[channel][index] = true
}

func (it *respItem) partSeen(channel string, index int) bool {
	if it.partOut == nil {
		return false
	}
	return it.partOut[channel][index]
}

var (
	errIndexTaken    = errors.New("output index already bound to another item")
	errUnidentified  = errors.New("cannot identify the item an event belongs to")
	errTooManyItems  = errors.New("too many output items")
	errSealConflict  = errors.New("conflicting sealed payloads for one item")
	errIndexNegative = errors.New("negative part index")
)

// finalize accepts the terminating description of an item and returns the
// events it still owes the consumer. It is used for both
// response.output_item.done and the response.output of a terminating
// response, so an item described by either source is folded exactly once.
//
// The events it returns are:
//   - one EventThinkingDelta per part whose text never arrived as a delta
//   - one EventThinkingDelta carrying the sealed payload and item id, when
//     the item has a sealed payload (whether or not it has readable text)
//
// A terminating event that repeats state already delivered yields nothing,
// which is what keeps done-then-completed from duplicating content.
func (r *itemRegistry) finalize(item *oaRespOutputItem, outIdx *int) ([]*Event, error) {
	if item.Type != "reasoning" {
		return nil, nil
	}
	if outIdx != nil && *outIdx < 0 {
		return nil, errIndexNegative
	}
	id, err := idFrom(item.eventID, item.ID)
	if err != nil {
		return nil, err
	}
	it, err := r.resolve(id, outIdx)
	if err != nil {
		return nil, err
	}
	if item.EncryptedContent != "" {
		if it.sealedDone && it.sealed != item.EncryptedContent {
			return nil, errSealConflict
		}
	}
	var evs []*Event
	text, parts := itemReadableText(item)
	// A part whose text already arrived as a delta must not be appended
	// again; only parts that never streamed are backfilled here.
	if text != "" && !it.allPartsSeen(item, parts) {
		evs = append(evs, &Event{
			Type: EventThinkingDelta, BlockIndex: it.slot, Text: text,
			ReasoningID: it.id,
		})
		_ = parts
	}
	if item.EncryptedContent != "" {
		it.sealed = item.EncryptedContent
		it.sealedBy = string(ProtoOpenAIResponses)
		it.sealedDone = true
		evs = append(evs, &Event{
			Type: EventThinkingDelta, BlockIndex: it.slot,
			Sealed: item.EncryptedContent, SealedBy: string(ProtoOpenAIResponses),
			ReasoningID: it.id,
		})
	}
	it.finalized = true
	return evs, nil
}

// allPartsSeen reports whether every part of the item already streamed its
// text, so a terminating event has nothing to backfill. A delta carries no
// part index on a minimal emitter, which is treated as part 0.
func (it *respItem) allPartsSeen(item *oaRespOutputItem, parts int) bool {
	if parts == 0 {
		return true
	}
	if it.partOut == nil {
		return false
	}
	for channel, n := range map[string]int{"summary": len(item.Summary), "content": len(item.Content)} {
		seen := 0
		for i := 0; i < n; i++ {
			if it.partSeen(channel, i) {
				seen++
			}
		}
		if seen < n {
			return false
		}
	}
	return true
}

// itemOutputIndex returns the position an item occupies in the terminating
// output array. It is used as the fallback when no event named the index.
func itemOutputIndex(output *[]oaRespOutputItem, i int) *int {
	if output == nil {
		return nil
	}
	return &[]int{i}[0]
}

// itemReadableText renders an item's readable summary text, joining parts
// with a newline. The second return value reports how many parts carried
// text, so callers can tell "no parts" from "parts, all empty".
func itemReadableText(item *oaRespOutputItem) (string, int) {
	var b strings.Builder
	n := 0
	writePart := func(text string) {
		if text == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
		n++
	}
	for _, p := range item.Summary {
		writePart(p.Text)
	}
	for _, p := range item.Content {
		if p.Type == "reasoning_text" {
			writePart(p.Text)
		}
	}
	return b.String(), n
}

// mapItemErr reports a registry failure as a stream error. Identity
// conflicts are upstream inconsistencies, so they fail the stream with
// ErrUpstreamMalformed and the request context, leaving whatever has
// already been accumulated in Partial.
func mapItemErr(err error, method, url string) error {
	switch {
	case errors.Is(err, errIDConflict), errors.Is(err, errIndexTaken),
		errors.Is(err, errUnidentified), errors.Is(err, errSealConflict),
		errors.Is(err, errIndexNegative):
		return fmt.Errorf("rosetta: responses stream (%s %s): %s: %w", method, url, err, ErrUpstreamMalformed)
	case errors.Is(err, errTooManyItems):
		return fmt.Errorf("rosetta: responses stream (%s %s): %s: %w", method, url, err, ErrStreamOverflow)
	}
	return err
}
