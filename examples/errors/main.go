// Command errors shows how to classify a failure instead of string-matching
// the message: Category answers "what failed", AffectsModel says whether the
// credential is still usable for other models.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint(envOr("ROSETTA_ENDPOINT", "https://api.openai.com/v1")),
		rosetta.WithAPIKey(os.Getenv("ROSETTA_API_KEY")),
		rosetta.WithMaxRetries(3),
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    envOr("ROSETTA_MODEL", "gpt-4o-mini"),
		Messages: []rosetta.Message{rosetta.User("hello")},
	})
	if err == nil {
		fmt.Println("ok")
		return
	}

	var apiErr *rosetta.APIError
	switch {
	case errors.As(err, &apiErr):
		fmt.Printf("status=%d category=%s retryable=%v request_id=%s\n",
			apiErr.StatusCode, categoryName(apiErr.Category), apiErr.Retryable, apiErr.RequestID)
		if apiErr.AffectsModel != "" {
			fmt.Printf("model %s is unavailable on this credential; other models are unaffected\n", apiErr.AffectsModel)
		}
		if apiErr.RetryAfter > 0 {
			fmt.Printf("provider asked to wait %s\n", apiErr.RetryAfter)
		}
	case errors.Is(err, rosetta.ErrUpstreamMalformed):
		fmt.Println("upstream returned an unreadable body; fail over or retry")
	case errors.Is(err, rosetta.ErrContextTooLong):
		fmt.Println("prompt exceeds the model context window; trim it")
	case errors.Is(err, rosetta.ErrInvalidRequest):
		fmt.Println("the request itself is malformed; retrying unchanged will not help")
	default:
		var te *rosetta.TransportError
		if errors.As(err, &te) {
			fmt.Printf("network failure during %s %s: %v\n", te.Method, te.URL, te.Err)
			return
		}
		fmt.Println("unclassified:", err)
	}
}

func categoryName(c rosetta.ErrorCategory) string {
	switch c {
	case rosetta.CatOutOfCredit:
		return "out_of_credit"
	case rosetta.CatRateLimited:
		return "rate_limited"
	case rosetta.CatQuotaExhausted:
		return "quota_exhausted"
	case rosetta.CatModelUnavailable:
		return "model_unavailable"
	case rosetta.CatContentRefused:
		return "content_refused"
	case rosetta.CatAuthFailed:
		return "auth_failed"
	default:
		return "unclassified"
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
