// Command usage demonstrates token usage accounting with Stats.
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
		rosetta.WithUsageTracker(rosetta.NewMemoryUsageTracker()),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	prompts := []string{"1+1=?", "天空为什么是蓝色？用一句话回答。", "Go 的 interface 是什么？"}
	for _, p := range prompts {
		resp, err := client.Chat(ctx, &rosetta.ChatRequest{
			Model:    os.Getenv("ROSETTA_MODEL"),
			Messages: []rosetta.Message{rosetta.User(p)},
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("- %s -> %s\n", p, resp.Text())
	}

	stats := client.Stats()
	fmt.Printf("\n总计: %d 次请求, 输入 %d + 输出 %d = %d tokens\n",
		stats.TotalRequests, stats.InputTokens, stats.OutputTokens, stats.TotalTokens)
	if missing := stats.UsageMissing; missing > 0 {
		fmt.Printf("警告: %d 次响应未返回用量数据\n", missing)
	}
	for model, mu := range stats.ByModel {
		fmt.Printf("按模型 %s: %d 次 / %d tokens\n", model, mu.Requests, mu.TotalTokens)
	}
	for proto, pu := range stats.ByProtocol {
		fmt.Printf("按协议 %s: %d 次 / %d tokens\n", proto, pu.Requests, pu.TotalTokens)
	}
}
