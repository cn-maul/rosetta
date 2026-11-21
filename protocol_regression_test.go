package rosetta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponsesTerminalFillsPartsAndCalls(t *testing.T) {
	client := newTestClient(t, WithProtocol(ProtoOpenAIResponses), WithUnknownEvents(true))
	stream := client.provider.(*openaiResponsesProvider)
	terminal := `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":"second"}]},{"type":"function_call","id":"fc","call_id":"call","name":"lookup","arguments":"{}"},{"type":"web_search_call","action":{"query":"cats"}}]}`
	feed := `data: {"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"second"}` + "\n\n" +
		`data: {"type":"response.completed","response":` + terminal + "}\n\n"
	sawUnknown := false
	next := stream.streamEvents(strings.NewReader(feed), http.MethodPost, "http://api.test", "")
	result, err := newStream(func() (*Event, error) {
		event, err := next()
		if event != nil && event.Type == EventUnknown {
			sawUnknown = bytes.Contains(event.Raw, []byte(`"query":"cats"`))
		}
		return event, err
	}, nil).Collect()
	calls := result.ToolCalls()
	if err != nil || result.Text() != "first\nsecond" || len(calls) != 1 || calls[0].ToolCallID != "call" || calls[0].Arguments != "{}" || result.StopReason != StopToolUse || !sawUnknown {
		t.Fatalf("stream result = %+v, err = %v, unknown = %v", result, err, sawUnknown)
	}
	unary, err := decodeResponsesResponse([]byte(terminal))
	if err != nil || len(unary.Unknown) != 1 || !bytes.Contains(unary.Unknown[0].Raw, []byte(`"query":"cats"`)) || unary.Text() != result.Text() {
		t.Fatalf("unary result = %+v, err = %v", unary, err)
	}
}

func TestAnthropicServerToolDoesNotBecomeClientCall(t *testing.T) {
	client := newTestClient(t, WithProtocol(ProtoAnthropic), WithUnknownEvents(true))
	provider := client.provider.(*anthropicProvider)
	block := `{"type":"server_tool_use", "id":"srv", "name":"web_search", "input":{}, "extension":{"value":1}}`
	data := `data: {"type":"content_block_start","index":0,"content_block":` + block + "}\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	next := provider.streamEvents(strings.NewReader(data), "POST", "http://api.test", "")
	for range 2 {
		event, err := next()
		if err != nil || event.Type != EventUnknown {
			t.Fatalf("event = %+v, err = %v", event, err)
		}
	}
	result, err := decodeAnthropicResponse([]byte(`{"content":[` + block + `]}`))
	if err != nil || len(result.Unknown) != 1 || string(result.Unknown[0].Raw) != block {
		t.Fatalf("unary result = %+v, err = %v", result, err)
	}
}

type protocolTestTransport func(*http.Request) (*http.Response, error)

func (fn protocolTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type protocolTestTracker struct {
	*MemoryUsageTracker
	records []UsageRecord
}

func (tracker *protocolTestTracker) Record(ctx context.Context, record UsageRecord) {
	tracker.records = append(tracker.records, record)
	tracker.MemoryUsageTracker.Record(ctx, record)
}

func TestChatAmbiguousTransportRetryOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprint(optIn), func(t *testing.T) {
			var attempts int
			var key string
			tracker := &protocolTestTracker{MemoryUsageTracker: NewMemoryUsageTracker()}
			client := newTestClient(t, WithChatIdempotentRetries(optIn), WithRetryBase(time.Nanosecond), WithUsageTracker(tracker),
				WithHTTPClient(&http.Client{Transport: protocolTestTransport(func(req *http.Request) (*http.Response, error) {
					defer req.Body.Close()
					attempts++
					if attempts == 1 {
						key = req.Header.Get("Idempotency-Key")
						return nil, io.ErrUnexpectedEOF
					}
					if key == "" || req.Header.Get("Idempotency-Key") != key {
						t.Error("retry changed the idempotency key")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"req-test"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
				})}))
			_, err := client.Chat(context.Background(), &ChatRequest{Model: "demo", Messages: []Message{User("hi")}})
			if optIn && (err != nil || attempts != 2) || !optIn && (!errors.Is(err, ErrRequestOutcomeUnknown) || attempts != 1) {
				t.Fatalf("attempts = %d, err = %v", attempts, err)
			}
			if client.Stats().ByModel["demo"].Requests != 1 {
				t.Fatalf("stats = %+v", client.Stats())
			}
			record := tracker.records[0]
			if record.Attempts != attempts || record.Latency < 0 || optIn && (record.StatusCode != 200 || record.RequestID != "req-test") || !optIn && !errors.Is(record.Err, ErrRequestOutcomeUnknown) {
				t.Fatalf("usage record = %+v", record)
			}
		})
	}
}

func TestResponsesToolIDsSeparateReusedOutputIndex(t *testing.T) {
	client := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	provider := client.provider.(*openaiResponsesProvider)
	feed := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"first","call_id":"a","name":"lookup"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"second","call_id":"b","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"first","delta":"[1]"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"second","delta":"[2]"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n\n")
	result, err := newStream(provider.streamEvents(strings.NewReader(feed), http.MethodPost, "http://api.test", ""), nil).Collect()
	calls := result.ToolCalls()
	if err != nil || len(calls) != 2 || calls[0].ToolCallID != "a" || calls[0].Arguments != "[1]" || calls[1].ToolCallID != "b" || calls[1].Arguments != "[2]" {
		t.Fatalf("tool calls = %+v, err = %v", calls, err)
	}
}

func TestFilesPaginationWireShapes(t *testing.T) {
	for _, protocol := range []Protocol{ProtoAnthropic, ProtoOpenAIChat} {
		t.Run(string(protocol), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				query := req.URL.Query()
				if protocol == ProtoAnthropic {
					if query.Get("page") == "page_2" {
						io.WriteString(w, `{"data":[{"id":"two"}],"next_page":null}`)
						return
					}
					io.WriteString(w, `{"data":[{"id":"one","created_at":"2025-01-02T03:04:05Z"}],"next_page":"page_2"}`)
				} else if query.Get("after") == "one" {
					io.WriteString(w, `{"data":[{"id":"two"}],"has_more":false}`)
				} else {
					io.WriteString(w, `{"data":[{"id":"one","created_at":1735787045}],"has_more":true}`)
				}
			}))
			defer server.Close()
			client := newTestClient(t, WithProtocol(protocol), WithEndpoint(server.URL))
			files, err := client.ListFiles(context.Background())
			if err != nil || len(files) != 2 || files[0].ID != "one" || files[0].CreatedAt.IsZero() || files[1].ID != "two" {
				t.Fatalf("files = %+v, err = %v", files, err)
			}
		})
	}
}

func TestCountTokensUsesCountFieldsAndCanonicalModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if req.URL.Path != "/v1/messages/count_tokens" || body["model"] != "canonical" || body["max_tokens"] != nil || body["stop_sequences"] != nil || body["service_tier"] != nil {
			t.Errorf("unexpected count request %s: %+v", req.URL.Path, body)
		}
		io.WriteString(w, `{"input_tokens":9}`)
	}))
	defer server.Close()
	client := newTestClient(t, WithProtocol(ProtoAnthropic), WithEndpoint(server.URL), WithModelInfo(ModelInfo{ID: "canonical", Aliases: []string{"alias"}}))
	count, err := client.CountTokens(context.Background(), &ChatRequest{Model: "alias", Messages: []Message{User("hi")}, StopSequences: []string{"stop"}, ServiceTier: "auto"})
	if err != nil || count != 9 {
		t.Fatalf("count = %d, err = %v", count, err)
	}
}

func TestAuxInvalidOverridesDoNotSend(t *testing.T) {
	client := newTestClient(t, WithExtraOverrides(true), WithHTTPClient(&http.Client{Transport: protocolTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("invalid payload reached the transport")
		return nil, io.ErrUnexpectedEOF
	})}))
	for _, extra := range []map[string]any{{"input": []any{nil}}, {"input": []string{}}, {"dimensions": -1}, {"model": 9}} {
		if _, err := client.Embed(context.Background(), &EmbeddingRequest{Model: "m", Input: []string{"x"}, Extra: extra}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("embed error = %v", err)
		}
	}
	for _, extra := range []map[string]any{{"documents": []string{}}, {"query": false}, {"top_n": -1}} {
		if _, err := client.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "q", Documents: []string{"x"}, Extra: extra}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("rerank error = %v", err)
		}
	}
}

func TestDetectClientUsesDecoratedCatalogFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/gateway/models" || request.URL.Query().Get("tenant") != "one" || request.Header.Get("X-Gateway-Key") != "test" {
			http.NotFound(writer, request)
			return
		}
		io.WriteString(writer, `{"data":[{}, {"id":"claude","type":"model"}]}`)
	}))
	defer server.Close()
	client, err := DetectClient(context.Background(), WithEndpoint(server.URL), WithAPIKey("test"), WithPathPrefix("/gateway"), WithQueryParam("tenant", "one"), WithHeader("X-Gateway-Key", "test"))
	if err != nil || client.Protocol() != ProtoAnthropic {
		t.Fatalf("client = %v, error = %v", client, err)
	}
}

func TestAnthropicThinkingUsesFinalOutputBudget(t *testing.T) {
	client := newTestClient(t, WithProtocol(ProtoAnthropic), WithStrictContextCheck(true), WithModelInfo(ModelInfo{ID: "legacy", ContextWindow: 6000, SupportsThinking: true}))
	_, err := client.Chat(context.Background(), &ChatRequest{
		Model: "legacy", Messages: []Message{User("hi")}, MaxOutputTokens: 512,
		Thinking: &ThinkingConfig{BudgetTokens: 8192},
	})
	if !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("error = %v, want final budget error", err)
	}
	provider := client.provider.(*anthropicProvider)
	request := &ChatRequest{Model: "claude-opus-4-6", Messages: []Message{User("hi")}, Thinking: &ThinkingConfig{Effort: EffortHigh}}
	payload, _, err := provider.buildPayload(request, false, provider.plan(request))
	if err != nil || payload["thinking"].(map[string]any)["type"] != "adaptive" || payload["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("payload = %+v, error = %v", payload, err)
	}
}
