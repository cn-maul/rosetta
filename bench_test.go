package rosetta

import (
	"encoding/json"
	"testing"
)

// Baselines for the JSON decode paths that run per SSE chunk and per
// unary response. Encoding/json (v1) has been v2-backed since Go 1.27;
// re-run these when touching wire types or the jsonx decoders.

var benchChunkBody = []byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello world"},"finish_reason":null}],"usage":null}`)

var benchResponseBody = []byte(`{
	"id": "chatcmpl-1", "model": "gpt-4o",
	"choices": [{"message": {"content": "hi", "reasoning_content": "thinking",
		"tool_calls": [{"id": "t1", "type": "function", "function": {"name": "fn", "arguments": "{\"x\":1}"}}]},
		"finish_reason": "tool_calls"}],
	"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
		"prompt_tokens_details": {"cached_tokens": 4},
		"completion_tokens_details": {"reasoning_tokens": 3}}
}`)

func BenchmarkOAChunkDecode(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var ch oaChunk
		if err := json.Unmarshal(benchChunkBody, &ch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOAChunkDecodeFlexFields(b *testing.B) {
	// The lenient-decoder path (FlexInt64 / ContentString) in the chunk.
	body := []byte(`{"id":"c1","choices":[{"delta":{"content":[{"type":"text","text":"a"}],"finish_reason":null}}],"usage":{"prompt_tokens":"12","completion_tokens":3,"total_tokens":15}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var ch oaChunk
		if err := json.Unmarshal(body, &ch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenAIResponseDecode(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeOpenAIChatResponse(benchResponseBody); err != nil {
			b.Fatal(err)
		}
	}
}
