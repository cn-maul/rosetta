// Command tools demonstrates a bounded tool-calling loop: ask, execute,
// feed the result back, repeat until the model answers in text.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(envOr("ROSETTA_ENDPOINT", "https://api.openai.com/v1")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
	)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	model := envOr("ROSETTA_MODEL", "gpt-4o-mini")

	tools := []rosetta.ToolDefinition{{
		Name:        "weather",
		Description: "Current weather for a city",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		Strict:      rosetta.Bool(true),
	}}
	messages := []rosetta.Message{rosetta.User("What's the weather in San Francisco?")}

	for step := 0; step < 4; step++ {
		resp, err := client.Chat(ctx, &rosetta.ChatRequest{
			Model:    model,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			log.Fatal(err)
		}
		if resp.StopReason != rosetta.StopToolUse {
			fmt.Println(resp.Text())
			return
		}
		calls := resp.ToolCalls()
		messages = append(messages, rosetta.AssistantBlocks(calls...))
		for _, call := range calls {
			// Replace this with the real tool. A failed tool should be fed
			// back as an error result, not returned to the caller: the model
			// can often recover.
			messages = append(messages, rosetta.ToolResult(call.ToolCallID, call.ToolName, `{"temp_c":22,"sky":"clear"}`))
		}
	}
	log.Fatal("tool loop did not converge in 4 steps")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
