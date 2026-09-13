// Command rerank demonstrates relevance scoring of documents against a
// query (Cohere-compatible /rerank endpoint).
//
// Required environment:
//
//	ROSETTA_ENDPOINT    e.g. https://api.siliconflow.cn/v1
//	ROSETTA_API_KEY     your key
//	ROSETTA_MODEL       e.g. BAAI/bge-reranker-v2-m3
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

	docs := []string{
		"SSE 流式响应通过事件流逐步返回生成内容。",
		"嵌入向量用于语义检索和相似度计算。",
		"重排序模型对检索结果按相关度重新打分。",
	}
	resp, err := client.Rerank(context.Background(), &rosetta.RerankRequest{
		Model:     os.Getenv("ROSETTA_MODEL"),
		Query:     "RAG 为什么要加 rerank",
		Documents: docs,
		TopN:      2,
	})
	if err != nil {
		log.Fatal(err)
	}

	for i, r := range resp.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			log.Fatalf("provider returned out-of-range result index %d", r.Index)
		}
		fmt.Printf("#%d score=%.4f doc=%q\n", i, r.RelevanceScore, docs[r.Index])
	}
	if resp.Usage.TotalTokens > 0 {
		fmt.Printf("usage: %d tokens\n", resp.Usage.TotalTokens)
	}
}
