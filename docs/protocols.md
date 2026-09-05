# 协议与兼容

三个适配器如何把统一请求/响应/事件映射到具体协议，以及面向不完美第三方服务的容错机制。

## 三协议对照

| | OpenAI Chat | OpenAI Responses | Anthropic |
|---|---|---|---|
| 端点 | `/v1/chat/completions` | `/v1/responses` | `/v1/messages` |
| 认证 | `Authorization: Bearer` | 同左 | `x-api-key` + `anthropic-version`（另附 Bearer 兼容网关） |
| system | `messages` 里的 system 角色 | `input` 里的 message item | 顶层 `system` 字段（与消息内 system 合并，`\n\n` 连接） |
| 输出上限 | `max_completion_tokens`（探测）或 `max_tokens` | `max_output_tokens` | `max_tokens`（必填；兜底链：请求值 → WithDefaultMaxOutputTokens → 4096） |
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
