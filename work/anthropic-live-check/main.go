// Command anthropic-live-check exercises the v1.3.2 Anthropic payload against a
// real endpoint: stop_sequences / service_tier / speed must reach the wire and
// must not be rejected locally, while an OpenAI-vocabulary service_tier must
// fail locally instead of turning into an upstream error.
//
// Key comes from TIANTOKEN_KEY; never hardcode it here.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cn-maul/rosetta"
)

func main() {
	key := os.Getenv("TIANTOKEN_KEY")
	if key == "" {
		fmt.Println("TIANTOKEN_KEY is not set")
		os.Exit(2)
	}
	c, err := rosetta.NewClient(
		rosetta.WithEndpoint("https://api.tiantoken.com"),
		rosetta.WithAPIKey(key),
		rosetta.WithProtocol(rosetta.ProtoAnthropic),
		rosetta.WithTimeout(60*time.Second),
	)
	if err != nil {
		fmt.Println("NewClient:", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// 1) Valid vocabulary: must go out and come back clean.
	resp, err := c.Chat(ctx, &rosetta.ChatRequest{
		Model:           "claude-sonnet-5-5",
		MaxOutputTokens: 32,
		Messages:        []rosetta.Message{rosetta.User("Reply with the single word: OK")},
		StopSequences:   []string{"NEVERMATCH"},
		ServiceTier:     "auto",
		Speed:           "standard",
	})
	if err != nil {
		fmt.Println("valid request failed:", err)
		os.Exit(1)
	}
	fmt.Printf("valid  -> ok: text=%q stop=%s model=%s tier_echo=%q\n",
		resp.Text(), resp.StopReason, resp.Model, resp.ServiceTier)

	// 2) OpenAI-vocabulary tier on the Anthropic protocol: local reject.
	_, err = c.Chat(ctx, &rosetta.ChatRequest{
		Model:       "claude-sonnet-5-5",
		Messages:    []rosetta.Message{rosetta.User("hi")},
		ServiceTier: "flex",
	})
	switch {
	case err == nil:
		fmt.Println("invalid-> FAILED: service_tier=flex was accepted")
		os.Exit(1)
	case errors.Is(err, rosetta.ErrInvalidRequest):
		fmt.Println("invalid-> rejected locally as expected:", err)
	default:
		fmt.Println("invalid-> unexpected error kind:", err)
		os.Exit(1)
	}

	// 3) Invalid speed: local reject too.
	_, err = c.Chat(ctx, &rosetta.ChatRequest{
		Model:    "claude-sonnet-5-5",
		Messages: []rosetta.Message{rosetta.User("hi")},
		Speed:    "turbo",
	})
	if !errors.Is(err, rosetta.ErrInvalidRequest) {
		fmt.Println("speed   -> FAILED, expected ErrInvalidRequest, got:", err)
		os.Exit(1)
	}
	fmt.Println("speed   -> rejected locally as expected")
}
