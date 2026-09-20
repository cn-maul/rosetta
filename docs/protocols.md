# 协议与兼容

三个适配器如何把统一请求/响应/事件映射到具体协议，以及面向不完美第三方服务的容错机制。

## 三协议对照

| | OpenAI Chat | OpenAI Responses | Anthropic |
|---|---|---|---|
| 端点 | `/v1/chat/completions` | `/v1/responses` | `/v1/messages` |
| 认证 | `Authorization: Bearer` | 同左 | `x-api-key` + `anthropic-version`（另附 Bearer 兼容网关） |
| system | `messages` 里的 system 角色 | `input` 里的 message item | 顶层 `system` 字段（与消息内 system 合并，`\n\n` 连接） |
| 输出上限 | `max_completion_tokens`（探测）或 `max_tokens` | `max_output_tokens` | `max_tokens`（必填；兜底链：请求值 → 模型元数据 → WithDefaultMaxOutputTokens → 4096） |
| thinking | `reasoning_effort` | `reasoning.effort` | `thinking.budget_tokens` |
| stop | `stop` | 不支持（丢弃并记日志） | `stop_sequences` |
| 工具定义 | `{type:"function", function:{...}}` | 扁平 `{type:"function", name, parameters}` | `{name, description, input_schema}` |
| 消息形态 | `messages` | `input` items（message / function_call / function_call_output） | 角色严格交替；tool_result 归位 user 轮；连续同角色自动合并 |
| 流式终止 | `data: [DONE]` | `response.completed` 事件 | `message_stop` 事件 |
| 特殊状态码 | — | — | 529 overloaded 参与重试 |

## StopReason 映射

| 统一值 | Chat `finish_reason` | Responses `status` | Anthropic `stop_reason` |
|---|---|---|---|
| `StopEnd` | stop | completed | end_turn / stop_sequence |
| `StopLength` | length | incomplete(max_output_tokens) | max_tokens |
| `StopToolUse` | tool_calls / function_call | — | tool_use |
| `StopContentFilter` | content_filter | incomplete(content_filter) | — |
| `StopRefusal` | — | — | refusal |
| `StopOther` | 其余值 | 其余状态 | 其余值 |

补充语义：非流式 200 响应省略终止原因时视为 `StopEnd`（一次性响应必然完整）；流式在未收到任何终止信号就 EOF 时合成 `StopOther`（截断信号，见[流式响应](streaming.md#中断与收尾语义)）。

## 流式事件映射

| 统一事件 | Chat | Responses | Anthropic |
|---|---|---|---|
| MessageStart | 首个 chunk（id/model） | `response.created` | `message_start` |
| TextDelta | `delta.content` | `response.output_text.delta` | `content_block_delta(text_delta)` |
| ThinkingDelta | `delta.reasoning_content`（DeepSeek 系） | `response.reasoning_summary_text.delta` / `reasoning_text.delta` | `thinking_delta` / `signature_delta` |
| ToolCall | `delta.tool_calls[]` 按 index 分组 | `output_item.added`(function_call) + `function_call_arguments.delta` | `content_block_start`(tool_use) + `input_json_delta` |
| MessageEnd | finish_reason chunk + usage chunk + [DONE] 合成 | `response.completed` / `response.incomplete`（usage 与 status 在此） | `message_delta`（stop_reason/output）+ `message_stop` 合成 |

usage 组装：Chat 在 usage chunk；Responses 在 completed 事件；Anthropic 的 input 来自 `message_start`、output 来自 `message_delta`——服务端缺哪部分就缺哪部分（如实呈现零值，不伪造）。

## 输出上限字段探测（OpenAI Chat）

官方 API 要求 `max_completion_tokens`，老服务只认 `max_tokens`。策略：

1. `WithMaxTokensField` 显式指定 → 直接使用；
2. `Quirks.LegacyMaxTokens` → 直接用 `max_tokens`；
3. 默认发 `max_completion_tokens`；收到 400 且报错信息命中（`max_*tokens` + unrecognized/unknown/unsupported 等）→ 本次换 `max_tokens` 重试，并**粘性记住**（同 client 后续请求直达，不再探测）。

同机制覆盖另外两个字段：`stream_options.include_usage` 与 `reasoning_effort` 被拒时同样降级一次并记住。每次请求最多触发 4 次此类消毒重试。

## Prompt 缓存（Anthropic）

Anthropic 的上下文缓存要**显式打断点**才会写入，命中的 token 按约 1/10 计价。OpenAI 系（含 DeepSeek）是**自动前缀缓存**，无需也无法传 `cache_control`——那边命中与否取决于调用方能否保持 prompt 前缀稳定。

rosetta 用统一字段 `CacheControl` 承载断点，仅 Anthropic 适配器会渲染它，OpenAI 系静默忽略：

```go
req := &rosetta.ChatRequest{
	Model:  "claude-sonnet-4-5",
	System: "一大段稳定的系统提示词……",
	Tools: []rosetta.ToolDefinition{
		{Name: "search", Description: "...", Parameters: schema},
		// 断点打在最后一个工具上 → 缓存整个工具集及其之前的内容
		{Name: "fetch", Description: "...", Parameters: schema2, CacheControl: rosetta.EphemeralCache()},
	},
	Messages: []rosetta.Message{
		rosetta.User("……"),
		{Role: rosetta.RoleSystem, Blocks: []rosetta.Block{
			{Type: rosetta.BlockText, Text: "一大段稳定上下文……", CacheControl: rosetta.ExtendedCache()},
		}},
	},
}
```

- `EphemeralCache()` = 默认 5 分钟（每次命中续期）；`ExtendedCache()` = 1 小时扩展缓存。非法 TTL 在本地 `ErrInvalidRequest` 拒绝。
- 断点可打在 text / image / document / tool_result / tool_use 块与工具定义上；打在 Anthropic 不支持的位置（如 thinking 块）会本地报错，而非静默丢弃。
- system 断点用一条带 `CacheControl` 的 `RoleSystem` 消息表达（`ChatRequest.System` 是纯字符串、无法携带断点）。有断点时适配器把 `system` 渲染成 Anthropic 的 text-block 数组；无断点时仍是原来的字符串，**wire 字节不变**，不影响既有调用的命中。
- Anthropic 限制：每请求最多 4 个断点，被缓存前缀需 ≥1024 token（Haiku 类 2048），过短的前缀上游不会缓存。
- 用量回报：命中量见 `Usage.CachedInputTokens`（`cache_read_input_tokens`），本次写入量见 `Usage.CachedCreationTokens`（`cache_creation_input_tokens`），两者都计入 `Stats()`。**缓存量已经折进 `Usage.InputTokens` 与 `TotalTokens`**（与 OpenAI 的 `prompt_tokens` 口径一致）：`CachedInputTokens` 是 `InputTokens` 的子集，`CachedCreationTokens` 也已包含在 `InputTokens` 内，这两个字段只用于展示缓存明细，**不要再加进输入量**。线格式上 Anthropic 的 `input_tokens` 只计未缓存部分，折算是适配器做的（见 `docs/usage-stats.md`）。

## Anthropic beta 头

Anthropic 把部分能力放在 `anthropic-beta` 请求头后面。SDK 按**已构建的 wire payload** 判定当前请求需要哪些 beta，再把它们**逗号连接成一个头**——多个 beta 同时成立时逐个 `Set` 会互相覆盖，导致靠后的那个静默丢失并被上游 400 拒绝。

| beta | 触发条件 | 缺失的后果 |
|---|---|---|
| `extended-cache-ttl-2025-04-11` | payload 中任一断点带 `ttl:"1h"` | 上游对该请求 400 |
| `interleaved-thinking-2025-05-14` | payload **同时**含 `thinking` 与 `tools` | 模型不在工具调用之间推理（能力降级，不报错） |

判定基于 payload 而非类型化请求，因此经 `Extra`（`WithExtraOverrides(true)`）注入的字段同样生效。

**交错思考的模型差异**（依据 Anthropic 官方 extended-thinking 文档）：Claude Opus 4.5 / Sonnet 4.5 及更早的 Claude 4 模型**需要**该头才启用；Opus 4.6+ / Sonnet 5 走自适应思考、该头已弃用且被安全忽略；Haiku 4.5 不支持，头被接受但忽略。

**Claude API 与 AWS Claude Platform 对任何模型都接受该头**，不支持时忽略，所以默认自动发送是安全的。例外是 **Amazon Bedrock 与 Google Cloud Vertex AI：它们会拒绝**发给白名单外模型的该头。

```go
// 端点经 Bedrock / Vertex 转发时关掉自动发送
rosetta.WithInterleavedThinking(false)

// 或强制发送（默认由 payload 自动判定）
rosetta.WithInterleavedThinking(true)
```

## Quirks

已知偏差直接声明，跳过探测：

```go
rosetta.WithQuirks(rosetta.Quirks{
	LegacyMaxTokens: true, // 只认 max_tokens
	NoStreamUsage:   true, // 会拒绝 stream_options.include_usage
})
```

## 脏数据处理（internal/jsonx）

第三方服务的常见畸形由宽松解码兜住，单字段异常不拖垮整个响应：

- 数字被序列化成字符串（`"prompt_tokens": "120"`）、浮点 token（`120.0`）；
- `content` 是分块数组而非字符串（自动拼接 text 部分）；
- `null` 出现在标量字段上（finish_reason、usage 细节等）；
- 流里无法解析的 chunk 跳过并记 debug 日志，解析器永不 panic。

## 重试策略

- 触发条件：网络层错误（DNS/连接/TLS/读失败）与状态码 408 / 429 / 500 / 502 / 503 / 504 / 529。
- 退避：`400ms × 2^n`（±20% 抖动，上限 8s）；`Retry-After` 头（秒数或 HTTP 日期）优先且只增不减。
- 每次重试重建请求体（Body 函数重发），Header 逐次克隆。
- 流式请求仅在拿到 200 之前重试（状态码阶段）；已经开始消费响应体后的一切失败都通过 `Stream.Err()` / `Partial()` 交给调用方。
- 4xx（除 408/429）不重试——那是请求本身的问题；Anthropic thinking 预算类 400 是唯一例外，走[响应式整流](guide.md#thinking-统一配置)。
