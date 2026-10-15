package rosetta_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cn-maul/rosetta"
)

// Live two-turn verification against a real Responses upstream: the first
// turn is streamed and must yield a reasoning item with its sealed payload
// and item id, and the second turn replays that item together with the tool
// result. Opt-in via environment variables, so it never runs in CI:
//
//	ROSETTA_ENDPOINT=https://api.deepseek.com/v1
//	ROSETTA_API_KEY=sk-...
//	ROSETTA_MODEL=deepseek-v4-flash
//	go test -run TestResponsesLiveTwoTurn -v .
func TestResponsesLiveTwoTurn(t *testing.T) {
	endpoint := os.Getenv("ROSETTA_ENDPOINT")
	key := os.Getenv("ROSETTA_API_KEY")
	model := os.Getenv("ROSETTA_MODEL")
	if endpoint == "" || key == "" || model == "" {
		t.Skip("live env not set")
	}

	// Turn 1: real upstream, streamed, must produce reasoning + a tool call.
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(endpoint),
		rosetta.WithAPIKey(key),
		rosetta.WithProtocol(rosetta.ProtoOpenAIResponses),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := client.ChatStream(ctx, &rosetta.ChatRequest{
		Model:    model,
		Messages: []rosetta.Message{rosetta.User("What is the weather in Beijing right now? Use the get_weather tool.")},
		Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortLow},
		Tools: []rosetta.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the current weather in a city",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("turn1 ChatStream: %v", err)
	}
	first, err := st.Collect()
	if err != nil {
		t.Fatalf("turn1 Collect: %v", err)
	}
	for i, b := range first.Content {
		enc := ""
		if b.Sealed != "" {
			enc = " sealed=" + string(b.Sealed[:min(8, len(b.Sealed))]) + "…(len=" + itoa(len(b.Sealed)) + ")"
		}
		t.Logf("turn1 block %d: type=%s thinking=%q text=%q tool=%s id=%q%s", i, b.Type, trunc(b.Thinking), trunc(b.Text), b.ToolName, b.ReasoningID, enc)
	}
	reasoning, tool := findBlocks(first.Content)
	if reasoning == nil || tool == nil {
		t.Fatalf("turn1 must produce a reasoning block and a tool call; got %d blocks", len(first.Content))
	}
	if reasoning.Sealed == "" {
		t.Fatalf("turn1 reasoning block carries no sealed payload: %+v", *reasoning)
	}
	t.Logf("turn1 OK: reasoning sealed len=%d id=%q; tool call=%s", len(reasoning.Sealed), reasoning.ReasoningID, tool.ToolCallID)

	// Turn 2: replay the assistant blocks plus the tool result.
	history := []rosetta.Message{
		rosetta.User("What is the weather in Beijing right now? Use the get_weather tool."),
		{Role: rosetta.RoleAssistant, Blocks: first.Content},
		rosetta.ToolResult(tool.ToolCallID, "", `{"temp_c":21,"sky":"clear"}`),
	}
	second, err := client.Chat(ctx, &rosetta.ChatRequest{
		Model:    model,
		Messages: history,
		Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortLow},
		Extra:    map[string]any{"store": false},
	})
	if err != nil {
		t.Fatalf("turn2 Chat (replay rejected): %v", err)
	}
	for i, b := range second.Content {
		t.Logf("turn2 block %d: type=%s thinking=%q text=%q", i, b.Type, trunc(b.Thinking), trunc(b.Text))
	}
	t.Logf("turn2 OK: stop=%v blocks=%d", second.StopReason, len(second.Content))
}

func findBlocks(bs []rosetta.Block) (*rosetta.Block, *rosetta.Block) {
	var r, c *rosetta.Block
	for i := range bs {
		if bs[i].Type == rosetta.BlockThinking && r == nil {
			r = &bs[i]
		}
		if bs[i].Type == rosetta.BlockToolCall && c == nil {
			c = &bs[i]
		}
	}
	return r, c
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
