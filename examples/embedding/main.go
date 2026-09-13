// Command embedding demonstrates computing text embedding vectors.
//
// Required environment:
//
//	ROSETTA_ENDPOINT    e.g. https://api.siliconflow.cn/v1
//	ROSETTA_API_KEY     your key
//	ROSETTA_MODEL       e.g. BAAI/bge-m3
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

	resp, err := client.Embed(context.Background(), &rosetta.EmbeddingRequest{
		Model: os.Getenv("ROSETTA_MODEL"),
		Input: []string{"什么是流式响应", "统一接入多家 LLM 服务商"},
	})
	if err != nil {
		log.Fatal(err)
	}

	for _, e := range resp.Data {
		head := e.Embedding
		if len(head) > 3 {
			head = head[:3]
		}
		fmt.Printf("vector %d (dim %d): %v...\n", e.Index, len(e.Embedding), head)
	}
	fmt.Printf("usage: %d tokens\n", resp.Usage.TotalTokens)
}
