// Command stream demonstrates streaming with unified events.
//
// Required environment:
//
//	ROSETTA_ENDPOINT  e.g. https://api.deepseek.com/v1
//	ROSETTA_API_KEY   your key
//	ROSETTA_MODEL     e.g. deepseek-reasoner (or any chat model)
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
		rosetta.WithEndpoint(os.Getenv("ROSETTA_ENDPOINT")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithUsageTracker(rosetta.NewMemoryUsageTracker()),
	)
	if err != nil {
		log.Fatal(err)
	}

	stream, err := client.ChatStream(context.Background(), &rosetta.ChatRequest{
		Model:    os.Getenv("ROSETTA_MODEL"),
		Messages: []rosetta.Message{rosetta.User("写一首关于秋天代码评审的七绝。")},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	inThinking := false
	for stream.Next() {
		ev := stream.Event()
		switch ev.Type {
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
	fmt.Println()
	if err := stream.Err(); err != nil {
		fmt.Println("stream interrupted:", err)
		fmt.Println("partial answer:", stream.Partial().Text())
		return
	}
	resp, _ := stream.Collect()
	fmt.Printf("\nstop=%s usage: in=%d out=%d\n", resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
