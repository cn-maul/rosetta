package rosetta_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/cn-maul/rosetta"
)

// A minimal unary completion against a stub endpoint.
func ExampleClient_Chat() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"demo","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("test"),
	)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "demo",
		Messages: []rosetta.Message{rosetta.User("hi")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Text())
	// Output: hello
}

// Streaming is pull-based: Next until it reports false, then check Err.
func ExampleClient_ChatStream() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n")
	}))
	defer srv.Close()

	client, err := rosetta.NewClient(rosetta.WithEndpoint(srv.URL), rosetta.WithAPIKey("test"))
	if err != nil {
		log.Fatal(err)
	}
	stream, err := client.ChatStream(context.Background(), &rosetta.ChatRequest{
		Model:    "demo",
		Messages: []rosetta.Message{rosetta.User("hi")},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	for stream.Next() {
		if ev := stream.Event(); ev.Type == rosetta.EventTextDelta {
			fmt.Print(ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
	// Output: hello world
}

// A two-turn tool call: the model asks, the caller executes, the result goes
// back as a RoleTool message.
func ExampleClient_Chat_tools() {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			io.WriteString(w, `{"id":"1","model":"demo","choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		io.WriteString(w, `{"id":"2","model":"demo","choices":[{"message":{"content":"sunny 22C"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client, err := rosetta.NewClient(rosetta.WithEndpoint(srv.URL), rosetta.WithAPIKey("test"))
	if err != nil {
		log.Fatal(err)
	}
	tools := []rosetta.ToolDefinition{{
		Name:       "weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	first, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "demo",
		Messages: []rosetta.Message{rosetta.User("weather in SF?")},
		Tools:    tools,
	})
	if err != nil {
		log.Fatal(err)
	}
	call := first.ToolCalls()[0]

	second, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model: "demo",
		Messages: []rosetta.Message{
			rosetta.User("weather in SF?"),
			rosetta.AssistantBlocks(call),
			rosetta.ToolResult(call.ToolCallID, call.ToolName, "sunny 22C"),
		},
		Tools: tools,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(second.Text())
	// Output: sunny 22C
}

// Structured output: one ResponseFormat maps onto all three protocols.
func ExampleResponseFormat() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"demo","choices":[{"message":{"content":"{\"name\":\"Ada\"}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client, err := rosetta.NewClient(rosetta.WithEndpoint(srv.URL), rosetta.WithAPIKey("test"))
	if err != nil {
		log.Fatal(err)
	}
	strict := true
	resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "demo",
		Messages: []rosetta.Message{rosetta.User("extract the name")},
		ResponseFormat: &rosetta.ResponseFormat{
			Type:   "json_schema",
			Name:   "person",
			Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			Strict: &strict,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(resp.Text()), &out); err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Name)
	// Output: Ada
}
