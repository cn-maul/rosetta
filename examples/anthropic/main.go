// Command anthropic demonstrates the Anthropic Messages protocol with
// thinking and streaming.
//
// Required environment:
//
//	ROSETTA_ENDPOINT  e.g. https://api.anthropic.com/v1 (default)
//	ROSETTA_API_KEY   your Anthropic key
//	ROSETTA_MODEL     e.g. claude-sonnet-4-5
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
		rosetta.WithProtocol(rosetta.ProtoAnthropic), // 关键：切到 Messages 协议
	)
	if err != nil {
		log.Fatal(err)
	}

	// 思考档位被统一映射：EffortMedium -> budget_tokens 8192，
	// 并自动满足 Anthropic 的约束（budget >= 1024 且 < max_tokens）。
	stream, err := client.ChatStream(context.Background(), &rosetta.ChatRequest{
		Model:    os.Getenv("ROSETTA_MODEL"),
		System:   "你是一个谨慎的助手。",
		Messages: []rosetta.Message{rosetta.User("9.11 和 9.9 哪个大？先思考再回答。")},
		Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortMedium},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	inThinking := false
	for stream.Next() {
		switch ev := stream.Event(); ev.Type {
		case rosetta.EventThinkingDelta:
			if !inThinking {
				fmt.Print("[思考] ")
				inThinking = true
			}
			fmt.Print(ev.Text)
		case rosetta.EventTextDelta:
			if inThinking {
				fmt.Println("\n[正文]")
				inThinking = false
			}
			fmt.Print(ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		log.Fatal("stream error:", err)
	}
	fmt.Println()
}
