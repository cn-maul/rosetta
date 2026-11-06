# Rosetta

[![CI](https://github.com/cn-maul/rosetta/actions/workflows/ci.yml/badge.svg)](https://github.com/cn-maul/rosetta/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cn-maul/rosetta.svg)](https://pkg.go.dev/github.com/cn-maul/rosetta)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A zero-dependency Go SDK that speaks three LLM wire protocols behind one API:
**OpenAI Chat Completions**, **OpenAI Responses**, and **Anthropic Messages** —
plus the many compatible services (DeepSeek, Moonshot, Qwen, GLM, OpenRouter,
vLLM, Ollama). Embeddings (OpenAI format) and rerank (Cohere format) ride on
the same client. Auth headers, system-prompt placement, thinking parameters,
stream events and usage field names are all absorbed by the SDK.

## Install

```bash
go get github.com/cn-maul/rosetta
```

Requires Go 1.27+. No third-party dependencies.

## Quick start

```go
client, err := rosetta.NewClient(
	rosetta.WithEndpoint("https://api.deepseek.com/v1"),
	rosetta.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
)
if err != nil {
	log.Fatal(err)
}

resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
	Model:    "deepseek-chat",
	Messages: []rosetta.Message{rosetta.User("hello")},
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(resp.Text())
```

Switching providers means changing the endpoint, key and model id. Add
`rosetta.WithProtocol(...)` when the protocol cannot be inferred, or use
`rosetta.DetectClient` to probe it.

## What it handles

- **Protocol translation** — one request/response/stream model across all
  three wire formats, including tool calls and thinking replay.
- **Thinking** — a unified `low/medium/high` dial plus verbatim levels,
  Anthropic budget mapping, and sealed reasoning replay (OpenAI
  `encrypted_content`, Anthropic signed/redacted blocks).
- **Structured output and tools** — `ResponseFormat`, `ToolChoice`,
  parallel tool calls, per-tool `strict`, and provider server-side tools
  (`web_search`, `file_search`, `code_interpreter`, `computer_use`, `mcp`).
- **Streaming** — a pull-based `Stream`, incremental tool arguments, idle
  watchdog, truncation/overflow semantics, and an opt-in raw-event escape
  hatch for events newer than the SDK.
- **Errors** — typed `APIError` with `Category`, `AffectsModel`,
  `RetryAfter`, `InBand`, credential redaction, and classified sentinels.
- **Observability** — token usage (including cache and audio splits),
  latency, attempt count and request id per call.
- **Gateway extension points** — custom headers, Anthropic betas, query
  parameters, path prefixes, and organization/project ids (Azure OpenAI and
  OpenRouter work without a fork).

## Scope and non-goals

This is a **protocol adapter layer**, not an application framework. It owns
protocol translation for one endpoint and one key, streaming, retries, error
attribution, and precise token-usage accounting (including cache-hit splits).

**Deliberately out of scope** — build these on top, or use another library:

- multi-provider routing, load balancing, account pools, automatic failover
- agent loops, tool orchestration, parallel tool execution
- response caching, conversation history, context trimming/summarization
- a local MCP client, Realtime voice
- vector stores, Assistants, fine-tuning and other standalone API surfaces
- cost accounting and price tables (pricing is a business concern; the SDK
  measures tokens precisely and does not convert them into cost)

These are products built *with* the SDK, not the SDK itself. Keeping them out
keeps the core dependency small and the upgrade path fast.

The SDK also does not make business decisions for you: budget caps, multi-key
rotation and degradation policy belong in the caller, driven by `UsageRecord`
and `APIError.Category`.

## Documentation

The detailed guides are currently written in Chinese:

| Doc | Contents |
|---|---|
| [guide.md](docs/guide.md) | client options, messages and tools, thinking, context checks, errors |
| [streaming.md](docs/streaming.md) | event model, iteration, recovery, tool-call streams |
| [models.md](docs/models.md) | registry, protocol detection, capability gating |
| [protocols.md](docs/protocols.md) | per-protocol mapping and compatibility |
| [usage-stats.md](docs/usage-stats.md) | usage tracking and aggregation |
| [stability.md](docs/stability.md) | SemVer and deprecation policy |

Godoc examples are runnable: see `example_test.go`.

## Security

See [SECURITY.md](SECURITY.md) for the reporting process and credential
handling guarantees.

## License

MIT
