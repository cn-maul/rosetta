// Command models demonstrates the model registry: vendor presets,
// protocol detection and merged model metadata.
//
// Optional environment:
//
//	ROSETTA_API_KEY   your key (local servers accept a placeholder)
//	ROSETTA_ENDPOINT  overrides the vendor preset endpoint
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

	// 1) Vendor preset: endpoint + protocol in one line.
	endpoint := os.Getenv("ROSETTA_ENDPOINT")
	opts := []rosetta.Option{rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY"))}
	if endpoint == "" {
		opts = append(opts, rosetta.WithVendor("deepseek"))
	} else {
		opts = append(opts, rosetta.WithEndpoint(endpoint))
	}

	// 2) Protocol auto-detection: known-host table -> /models probe ->
	//    OpenAI Chat fallback.
	client, err := rosetta.DetectClient(ctx, opts...)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("endpoint=%s protocol=%s\n", client.Endpoint(), client.Protocol())

	// 3) Merged model metadata: manual > remote > builtin knowledge base.
	for _, id := range []string{"deepseek-reasoner", "gpt-4o", "claude-sonnet-4-5"} {
		info, err := client.ModelInfo(ctx, id)
		if err != nil {
			fmt.Printf("- %s: unknown (%v)\n", id, err)
			continue
		}
		fmt.Printf("- %s: context=%d max_out=%d thinking=%v known=%v\n",
			info.ID, info.ContextWindow, info.MaxOutputTokens, info.SupportsThinking, info.Known)
	}

	// 4) Remote catalog discovery merges into the registry.
	models, err := client.ListModels(ctx)
	if err != nil {
		log.Println("list models skipped:", err)
		return
	}
	fmt.Printf("remote catalog: %d models (merged view)\n", len(models))
}
