// Command timeout shows the built-in idle watchdog: a stream is aborted
// only when the upstream goes silent, never because generation is long.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(envOr("ROSETTA_ENDPOINT", "https://api.openai.com/v1")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithTimeout(30*time.Second),           // unary calls only
		rosetta.WithStreamIdleTimeout(15*time.Second), // inactivity bound for streams
	)
	if err != nil {
		log.Fatal(err)
	}

	stream, err := client.ChatStream(context.Background(), &rosetta.ChatRequest{
		Model:    envOr("ROSETTA_MODEL", "gpt-4o-mini"),
		Messages: []rosetta.Message{rosetta.User("Write a long story.")},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	for stream.Next() {
		if ev := stream.Event(); ev.Type == rosetta.EventTextDelta {
			_, _ = io.WriteString(os.Stdout, ev.Text)
		}
	}
	switch err := stream.Err(); {
	case err == nil:
		fmt.Println()
	case errors.Is(err, rosetta.ErrStreamIdleTimeout):
		fmt.Println("\n[upstream went silent; partial output kept in Partial()]")
	case errors.Is(err, rosetta.ErrStreamTruncated):
		fmt.Println("\n[connection cut before the terminal event]")
	default:
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
