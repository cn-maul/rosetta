// Command chat demonstrates a basic non-streaming chat with thinking.
//
// Required environment:
//
//	ROSETTA_ENDPOINT  e.g. https://api.deepseek.com/v1
//	ROSETTA_API_KEY   your key
//	ROSETTA_MODEL     e.g. deepseek-chat
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
	)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:           os.Getenv("ROSETTA_MODEL"),
		System:          "你是一个简洁的助手。",
		Messages:        []rosetta.Message{rosetta.User("用一句话解释什么是 SSE 流式响应。")},
		MaxOutputTokens: 200,
		Temperature:     rosetta.Float(0.7),
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("model:", resp.Model)
	fmt.Println("answer:", resp.Text())
	if t := resp.ThinkingText(); t != "" {
		fmt.Println("thinking:", t)
	}
	fmt.Printf("usage: in=%d out=%d total=%d (cached=%d reasoning=%d)\n",
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.TotalTokens,
		resp.Usage.CachedInputTokens, resp.Usage.ReasoningTokens)
}
