// Command concurrent shows that one Client is safe to share: a bounded
// worker pool issues several requests in parallel.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(envOr("ROSETTA_ENDPOINT", "https://api.openai.com/v1")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithUsageTracker(rosetta.NewMemoryUsageTracker()),
	)
	if err != nil {
		log.Fatal(err)
	}

	prompts := []string{"Say hi", "Say bye", "Count to three", "Name a color"}
	sem := make(chan struct{}, 2) // at most two in flight
	var wg sync.WaitGroup
	for i, p := range prompts {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
				Model:    envOr("ROSETTA_MODEL", "gpt-4o-mini"),
				Messages: []rosetta.Message{rosetta.User(p)},
			})
			if err != nil {
				fmt.Printf("[%d] error: %v\n", i, err)
				return
			}
			fmt.Printf("[%d] %s\n", i, resp.Text())
		}(i, p)
	}
	wg.Wait()

	fmt.Printf("requests=%d output_tokens=%d\n",
		client.Stats().TotalRequests, client.Stats().OutputTokens)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
