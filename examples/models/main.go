// Command models demonstrates the model registry: protocol detection,
// manual capability configuration and remote catalog discovery.
//
// Required environment:
//
//	ROSETTA_API_KEY   your key (local servers accept a placeholder)
//	ROSETTA_ENDPOINT  the API base URL, e.g. https://api.deepseek.com/v1
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cn-maul/rosetta"
)

func main() {
	ctx := context.Background()

	// 1) Protocol auto-detection: probe /models, then classify the
	//    catalog shape (OpenAI vs Anthropic); falls back to OpenAI Chat.
	client, err := rosetta.DetectClient(ctx,
		rosetta.WithEndpoint(os.Getenv("ROSETTA_ENDPOINT")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("endpoint=%s protocol=%s\n", client.Endpoint(), client.Protocol())

	// 2) Manual capability declarations drive gating and validation.
	client, _ = rosetta.NewClient(
		rosetta.WithEndpoint(client.Endpoint()),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithProtocol(client.Protocol()),
		rosetta.WithModelInfo(rosetta.ModelInfo{
			ID:               "my-model",
			ContextWindow:    131072,
			MaxOutputTokens:  8192,
			SupportsThinking: true,
		}),
	)
	if info, err := client.ModelInfo(ctx, "my-model"); err == nil {
		fmt.Printf("- %s: context=%d max_out=%d thinking=%v known=%v\n",
			info.ID, info.ContextWindow, info.MaxOutputTokens, info.SupportsThinking, info.Known)
	}

	// 3) Remote catalog discovery merges into the registry (Known=false).
	models, err := client.ListModels(ctx)
	if err != nil {
		log.Println("list models skipped:", err)
		return
	}
	fmt.Printf("remote catalog: %d models\n", len(models))
	for i, m := range models {
		if i >= 5 {
			break
		}
		fmt.Printf("- %s (known=%v)\n", m.ID, m.Known)
	}
}
