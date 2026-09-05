// Command responses demonstrates the OpenAI Responses protocol.
//
// Required environment:
//
//	ROSETTA_ENDPOINT  e.g. https://api.openai.com/v1 (default)
//	ROSETTA_API_KEY   your key
//	ROSETTA_MODEL     e.g. gpt-5
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(os.Getenv("ROSETTA_ENDPOINT")), // 留空则用官方地址
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithProtocol(rosetta.ProtoOpenAIResponses), // 切到 Responses 协议
	)
	if err != nil {
		log.Fatal(err)
	}

	stream, err := client.ChatStream(context.Background(), &rosetta.ChatRequest{
		Model:           os.Getenv("ROSETTA_MODEL"),
		System:          "你是一个严谨的助手。",
		Messages:        []rosetta.Message{rosetta.User("用两句话介绍 Responses API 和 Chat Completions 的区别。")},
		MaxOutputTokens: 512,
		Thinking:        &rosetta.ThinkingConfig{Effort: rosetta.EffortLow}, // 映射为 reasoning.effort
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	for stream.Next() {
		switch ev := stream.Event(); ev.Type {
		case rosetta.EventThinkingDelta:
			fmt.Print(ev.Text)
		case rosetta.EventTextDelta:
			fmt.Print(ev.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		log.Fatal("stream error:", err)
	}
	resp, _ := stream.Collect()
	fmt.Printf("stop=%s usage: in=%d (cached %d) out=%d (reasoning %d)\n",
		resp.StopReason, resp.Usage.InputTokens, resp.Usage.CachedInputTokens,
		resp.Usage.OutputTokens, resp.Usage.ReasoningTokens)
}
