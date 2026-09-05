# Rosetta — 统一 LLM 接入 Go SDK 计划书

| 项目 | 内容 |
|---|---|
| 版本 | v0.1 草案 |
| 日期 | 2026-09-05 |
| 状态 | 待评审 |
| SDK 名称 | Rosetta（module path：`github.com/cn-maul/rosetta`） |

---

## 1. 背景与目标

### 1.1 背景

当前主流 LLM 服务存在三套互不兼容的 API 协议：

1. **OpenAI Chat Completions**（`POST /v1/chat/completions`）——事实上的行业兼容标准，DeepSeek、Moonshot/Kimi、Qwen(DashScope)、智谱 GLM、MiniMax、SiliconFlow、OpenRouter、Groq、Ollama、vLLM 等大量第三方服务均兼容此协议；
2. **OpenAI Responses**（`POST /v1/responses`）——OpenAI 较新的有状态会话协议，事件流格式与 Chat Completions 完全不同；
3. **Anthropic Messages**（`POST /v1/messages`）——Claude 系列原生协议，消息结构、鉴权方式、流式事件均与前两者不同。

调用方如果直接对接，需要为每种协议维护一套请求构造、流式解析、用量统计和错误处理逻辑。Rosetta 的目标是用一个统一的 Go SDK 消除这些差异：**调用方只提供 `endpoint` + `api_key`，其余协议细节由 SDK 承担。**

### 1.2 设计目标

| # | 目标 | 说明 |
|---|------|------|
| G1 | 多协议统一接入 | 一套 `Chat / ChatStream` API 覆盖 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 三种协议 |
| G2 | 极简创建 | `endpoint + api_key` 即可创建客户端，协议自动探测，可显式覆盖 |
| G3 | 模型能力元数据 | 模型列表自动发现 + 手动配置；内置知名模型知识库，提供**上下文窗口长度、最大输出限制、是否支持 thinking** 等元数据 |
| G4 | Thinking 统一配置 | 一份 `ThinkingConfig` 自动映射为 OpenAI 的 `reasoning_effort` / Responses 的 `reasoning.effort` / Anthropic 的 `thinking.budget_tokens` |
| G5 | Token 用量统计 | 每次请求返回统一 `Usage`；客户端内置线程安全累计统计器，可随时查询；留持久化扩展点 |
| G6 | 一等流式支持 | 统一流式事件模型，屏蔽三种协议 SSE 事件差异；支持取消、超时、聚合收集 |
| G7 | 零第三方依赖 | 仅使用 Go 标准库（SSE 解析、JSON 解析自研），保证供应链干净 |
| G8 | 生产可用性 | 统一错误模型、可配置重试、超时控制、goroutine 安全、完整测试覆盖 |

### 1.3 非目标（v1 明确不做）

- **不做 Agent 框架**：不做工具执行循环、内存管理、编排；工具调用仅做定义下发与结果回传（透传层）。
- **不做 Bedrock / Vertex 适配**：AWS Bedrock（SigV4 鉴权）、GCP Vertex（OAuth + 路径差异）协议同源但鉴权/路由差异大，列入后续路线图。
- **不做 Embeddings / 图像生成 / 语音**：v1 聚焦对话（含多模态消息中的图片输入）。
- **不做有状态会话托管**：Responses 的 `previous_response_id` 通过 `Extra` 逃生口可用，但不做会话管理封装。
- **不做精确 tokenizer**：内置 token 估算器为启发式（用于上下文长度预警），提供 `TokenEstimator` 接口供接入精确实现（如 tiktoken binding）。

---

## 2. 总体架构

### 2.1 分层架构

```
                    调用方（业务代码）
                           │
                    rosetta.Client          ← 统一门面
              Chat / ChatStream / ListModels
              ModelInfo / Stats / Close
                           │
        ┌──────────────────┼───────────────────┐
        │                  能力层               │
        │  Registry      模型注册表（发现/内置/手动） │
        │  UsageTracker  用量累计与查询           │
        │  Thinking      统一思考配置映射          │
        │  Detector      协议自动探测            │
        │  Estimator     token 估算与上下文校验     │
        └──────────────────┬───────────────────┘
                           │  Provider 接口（统一契约）
        ┌──────────────────┼────────────────────┐
        │                  │                    │
  openaichat.Provider  openairesp.Provider  anthropic.Provider
  (Chat Completions)   (Responses)          (Messages)
        └──────────────────┼────────────────────┘
                           │
        internal/sse    SSE 解析（三协议共用）
        internal/httpx  HTTP 执行、重试、超时
        internal/jsonx  宽松 JSON 解码（第三方脏数据兜底）
                           │
                  各 AI 服务 endpoint
```

### 2.2 核心抽象

**统一契约 `Provider` 接口**（`provider/provider.go`）——所有协议适配器实现同一接口，`Client` 只面向该接口编程：

```go
type Provider interface {
    Protocol() Protocol // openai_chat | openai_responses | anthropic

    // 非流式对话：入参已是统一模型，出参从协议原始响应映射回统一模型
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

    // 流式对话：返回统一事件流
    StreamChat(ctx context.Context, req *ChatRequest) (StreamReader, error)

    // 模型列表自动发现（协议不支持时返回 ErrModelListUnsupported）
    ListModels(ctx context.Context) ([]ModelInfo, error)
}
```

**关键数据流（以 Anthropic 为例）**：

```
统一 ChatRequest ──请求映射器──> /v1/messages JSON body ──HTTP──> 原始响应
统一 ChatResponse <──响应映射器── 原始 JSON <──────────────────────┘
```

映射是纯函数、双向可测：`mapRequest` 与 `mapResponse` 均为无状态纯函数，用表驱动测试覆盖。

### 2.3 关键设计决策

| 决策 | 选择 | 理由 |
|---|---|---|
| API 风格 | 结构体 + Options 函数（`WithXxx`） | Go 生态主流（openai-go、anthropic-sdk-go 均如此），字段演进不破坏兼容 |
| 流式模型 | 拉取式迭代器 `stream.Next()` | 比回调可控（可暂停/取消/并发消费），EOF 结束符合 Go 惯例 |
| 依赖策略 | 零第三方依赖 | SDK 常被引入多层依赖树，`go.sum` 污染是常见痛点；SSE 解析自研约 200 行 |
| 错误透出 | 统一 `*APIError` + `Raw json.RawMessage` 逃生口 | 统一层管 90% 场景；协议新字段未跟上时调用方可读 Raw 兜底 |
| 第三方脏数据 | 宽松解码（`internal/jsonx`） | 兼容服务常见问题：usage 缺失、字段类型漂移（数字/字符串互换）、流式不含 usage——不因缺字段报错 |
| 模型知识库 | 内嵌 JSON + 三级合并 | 远端 `/models` 不含上下文长度等能力信息，必须内置知识库补齐 |

---

## 3. 公共 API 设计（草案）

> 以下为公开接口形状的定稿前草案，实现阶段允许微调；一旦发布 v0.1.0 按 SemVer 冻结。

### 3.1 客户端创建

```go
// 最简形式：endpoint + api_key，协议自动探测
client, err := rosetta.NewClient(
    rosetta.WithEndpoint("https://api.anthropic.com/v1"),
    rosetta.WithAPIKey("sk-ant-..."),
)

// 完整形式
client, err := rosetta.NewClient(
    rosetta.WithEndpoint("https://api.openai.com/v1"),
    rosetta.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
    rosetta.WithProtocol(rosetta.ProtoOpenAIChat),      // 缺省 ProtoAuto；可强制 ProtoOpenAIResponses 等
    rosetta.WithHTTPClient(&http.Client{}),             // 自定义传输（代理/ tracing）
    rosetta.WithTimeout(2*time.Minute),                 // 单请求超时（流式为 idle 超时语义）
    rosetta.WithMaxRetries(2),                          // 429/5xx 自动重试，默认 2
    rosetta.WithModelsFile("models.json"),              // 手动模型配置（见 5.1）
    rosetta.WithDefaultMaxOutputTokens(4096),           // 注册表查不到时的兜底
    rosetta.WithUsageTracker(rosetta.NewMemoryUsageTracker()), // 默认开启，可传 nil 关闭
    rosetta.WithLogger(slog.Default()),
)
```

`Client` 为 goroutine 安全的长生命周期对象，进程内建议单例复用（内含连接池与统计器）。

### 3.2 统一消息与内容块

```go
// 便捷构造
msgs := []rosetta.Message{
    rosetta.System("你是一个严谨的助手。"),
    rosetta.User("用一句话介绍 Go。"),
    rosetta.Assistant("Go 是 Google 开发的静态类型编译型语言。"),
    rosetta.UserImage("https://example.com/pic.png"), // 多模态图片输入
}

// 完整结构：消息 = 角色 + 内容块序列
type Message struct {
    Role   Role    // system / user / assistant / tool
    Blocks []Block
}

type Block struct {
    Type       BlockType   // text | image | tool_call | tool_result | thinking
    Text       string
    Image      *ImageBlock             // URL 或 base64
    ToolCall   *ToolCall               // assistant 发起：name + arguments(JSON)
    ToolResult *ToolResult             // tool 角色回传：toolCallID + content
    Thinking   *ThinkingBlock          // 思考内容（Anthropic 需 signature 回传）
}
```

适配要点：`system` 消息在 Anthropic 协议下会被提升为顶层 `system` 参数；`tool` 消息映射为 Anthropic 的 `tool_result` 内容块、OpenAI 的 `role:"tool"` 消息。

### 3.3 对话（非流式）

```go
resp, err := client.Chat(ctx, &rosetta.ChatRequest{
    Model: "claude-sonnet-4-5",
    Messages: msgs,
    MaxOutputTokens: 1024,              // 0 = 从注册表取该模型 MaxOutputTokens，再兜底默认值
    Temperature: rosetta.Float(0.7),     // 指针三态：nil=不下发
    Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortHigh},
    Tools: []rosetta.ToolDef{ weatherTool },
    Extra: map[string]any{              // 原生逃生口：直接合并进协议顶层字段
        "top_k": 40,
    },
})
if err != nil {
    var apiErr *rosetta.APIError
    if errors.As(err, &apiErr) && apiErr.Retryable { ... }
}
fmt.Println(resp.Text)       // 便捷字段：拼接所有 text 块
fmt.Println(resp.Usage)      // 统一用量
```

### 3.4 流式对话

```go
stream, err := client.ChatStream(ctx, req)   // req 复用 ChatRequest
defer stream.Close()

for {
    ev, err := stream.Next()                 // 正常结束返回 io.EOF
    if errors.Is(err, io.EOF) { break }
    if err != nil { return err }             // 中途协议/网络错误
    switch ev.Type {
    case rosetta.EventTextDelta:
        fmt.Print(ev.Text)
    case rosetta.EventThinkingDelta:
        log.Printf("[thinking] %s", ev.Text)
    case rosetta.EventToolCall:
        // 增量工具调用（参数 JSON 分片已聚合）
    case rosetta.EventMessageEnd:
        fmt.Printf("\n--- usage: %s, stop=%s\n", ev.Usage, ev.StopReason)
    }
}

// 便捷：一次聚合为完整 ChatResponse（内部即消费上述事件）
resp, err := stream.Collect()
```

统一事件模型：

```go
type EventType int
const (
    EventMessageStart EventType = iota // 流开始（部分协议携带 model/初始 usage）
    EventTextDelta                     // 正文文本增量
    EventThinkingDelta                 // 思考内容增量
    EventToolCall                      // 工具调用（聚合后）
    EventMessageEnd                    // 结束：携带 StopReason + Usage
)
type Event struct {
    Type       EventType
    Text       string          // TextDelta / ThinkingDelta 的增量内容
    ToolCall   *ToolCall
    StopReason StopReason
    Usage      *Usage          // MessageEnd 时必有（协议不吐时尽力补齐）
    Raw        json.RawMessage // 原始 SSE 事件，逃生口
}
```

### 3.5 模型发现与元数据

```go
// 自动发现：GET /v1/models（OpenAI 系）或 GET /v1/models（Anthropic，带 x-api-key）
models, err := client.ListModels(ctx)
// → []rosetta.ModelInfo{ID, DisplayName, ContextWindow, MaxOutputTokens,
//                        SupportsThinking, ...}

// 查询单模型能力（合并 内置知识库 + 远端发现 + 手动配置 后的视图）
info, ok, err := client.ModelInfo(ctx, "deepseek-chat")
fmt.Println(info.ContextWindow, info.MaxOutputTokens, info.SupportsThinking)

// 客户端创建后即拉取一次并缓存；可手动刷新（带 TTL 缓存选项）
err = client.RefreshModels(ctx)
```

手动配置（Go API 与 JSON 文件两种方式，结构相同）：

```json
{
  "models": [
    {
      "id": "deepseek-reasoner",
      "display_name": "DeepSeek R1",
      "context_window": 128000,
      "max_output_tokens": 65536,
      "supports_thinking": true,
      "aliases": ["deepseek-r1"]
    },
    {
      "id": "my-fine-tuned-model",
      "context_window": 32000,
      "max_output_tokens": 4096
    }
  ]
}
```

### 3.6 用量统计与查询

```go
// 单次：resp.Usage / 事件流 EventMessageEnd.Usage
// 累计：客户端内置线程安全统计器
snap := client.Stats().Snapshot()
fmt.Println(snap.TotalRequests, snap.TotalTokens)
fmt.Println(snap.ByModel["gpt-4o"].OutputTokens)
fmt.Println(snap.ByProtocol[rosetta.ProtoAnthropic].InputTokens)

// 自定义持久化：实现接口注入（SQLite / Prometheus / OTel 等由使用方落地）
type UsageTracker interface {
    Record(UsageRecord)             // 每次请求结束时回调（含 model/protocol/时间戳）
    Snapshot() UsageSnapshot
}
```

### 3.7 错误模型

```go
type APIError struct {
    StatusCode int            // HTTP 状态码（网络错误为 0）
    Code       string         // 协议错误码：openai "insufficient_quota" / anthropic "overloaded_error"
    Type       string         // openai error.type / anthropic error.type
    Message    string
    Retryable  bool           // SDK 判定：429、5xx、部分 4xx(overloaded) 为可重试
    Method     string
    URL        string
    Raw        json.RawMessage
}
// 本地校验错误（非服务端）为普通 error：ErrNoAPIKey、ErrNoEndpoint、ErrUnknownModel、ErrContextTooLong ...
```

---

## 4. 协议适配要点

### 4.1 三协议差异对照

| 维度 | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages |
|---|---|---|---|
| 端点 | `POST /chat/completions` | `POST /responses` | `POST /messages` |
| 鉴权 | `Authorization: Bearer` | `Authorization: Bearer` | `x-api-key` + `anthropic-version` |
| system | `role:"system"` 消息 | `instructions` 顶层字段 | 顶层 `system` 参数 |
| max 输出 | `max_completion_tokens`（官方）；兼容服务多用 `max_tokens` | `max_output_tokens` | `max_tokens`（**必填**） |
| thinking | `reasoning_effort` | `reasoning.effort` | `thinking: {type:"enabled", budget_tokens}` |
| 流式协议 | SSE，`data:` 行 + `[DONE]` 结束 | SSE，类型化事件（`response.output_text.delta` 等） | SSE，`event:` + 类型化事件（`content_block_delta` 等） |
| 流式 usage | 需 `stream_options:{include_usage:true}`，最后一个 chunk 携带 | `response.completed` 事件携带 | `message_start`(输入) + `message_delta`(累计输出) |
| 工具定义 | `tools[].function{...}` 嵌套 | `tools[]` 扁平结构 | `tools[].input_schema` |
| 模型列表 | `GET /models` | `GET /models` | `GET /models`（分页） |
| usage 字段 | `prompt_tokens / completion_tokens / total_tokens` | `input_tokens / output_tokens / total_tokens` | `input_tokens / output_tokens` (+cache 字段) |

### 4.2 OpenAI Chat Completions 适配器

- `max_tokens` 映射策略：官方 endpoint（api.openai.com）默认发 `max_completion_tokens`；第三方兼容端点默认发 `max_tokens`（兼容面最广）；`rosetta.WithMaxTokensField(...)` 可强制指定。
- 流式必发 `stream_options: {"include_usage": true}`；部分兼容服务不认识该字段会报错 → 由 quirks 机制（4.5）自动降级重试一次（去掉该字段），并在 usage 缺失时 `Usage` 返回零值 + 记录 warning。
- `reasoning_effort` 仅对支持的模型下发；对未知模型，若注册表无 thinking 能力标记则不下发（避免兼容服务 400）。

### 4.3 OpenAI Responses 适配器

- `input` 统一消息序列映射为 items 数组；system 消息合并进 `instructions`。
- 事件流按 `response.output_text.delta` / `response.output_item.added`(function_call) / `response.completed` 归一；`response.failed` / `response.incomplete` 映射为统一错误或 `StopReason`。
- 该协议仅官方 OpenAI 及少数网关支持，默认不参与自动探测命中（探测到再启用，见 4.6）。

### 4.4 Anthropic Messages 适配器

- `max_tokens` 必填：调用方未指定时自动填充（注册表 `MaxOutputTokens` → `WithDefaultMaxOutputTokens` → 4096），并在响应 `Raw` 之外不额外干扰。
- 开启 thinking 时：自动保证 `budget_tokens ≥ 1024` 且 `budget_tokens < max_tokens`（必要时自动上调 `max_tokens` 并记 warning）；Anthropic 规定思考模式下 `temperature` 必须为 1，SDK 自动忽略/纠正用户温度并记 warning。
- 流式解析 `content_block_delta` 的 `text_delta` / `thinking_delta` / `input_json_delta`，`message_delta` 的累计 `output_tokens` 在 `MessageEnd` 汇总。
- 思考块含 `signature`，多轮回传时 `ThinkingBlock.Signature` 原样透传。

### 4.5 第三方兼容服务与 quirks 机制

目标兼容面（OpenAI 协议族）：DeepSeek、Moonshot/Kimi、Qwen(DashScope compatible-mode)、智谱 GLM、MiniMax、SiliconFlow、OpenRouter、Groq、Together、Fireworks、Ollama、vLLM、LM Studio、One-API/New-API 网关。

已知常见偏差与对策：

| 偏差 | 对策 |
|---|---|
| 不支持 `stream_options` | 自动重试去掉该字段（一次），标记该连接 quirks |
| 流式不返回 usage | Usage 零值 + warning；统计器记 `usage_missing` 计数 |
| 仅认 `max_tokens` | 默认字段名探测策略 + 手动覆盖 |
| 字段类型漂移（数字变字符串等） | `internal/jsonx` 宽松解码 |
| `reasoning_effort` 报 400 | 能力标记缺失时不下发 |

```go
rosetta.WithQuirks(rosetta.Quirks{LegacyMaxTokens: true, NoStreamUsage: true}) // 手动兜底
```

### 4.6 协议自动探测（`ProtoAuto`）

按序执行，命中即止：

1. **显式配置**：`WithProtocol` 直接生效，跳过探测。
2. **知名 Host 表**（内嵌）：`api.openai.com`→OpenAI(默认 chat)；`api.anthropic.com`→Anthropic；`openrouter.ai`、`api.deepseek.com`、`api.moonshot.cn`、`dashscope.aliyuncs.com/compatible-mode`、`api.together.xyz`、`api.groq.com`、`localhost:11434/v1`(Ollama)、`/v1` 结尾的 vLLM/LM Studio 等→OpenAI Chat。
3. **主动探测**（仅未知 Host，一次轻量请求）：`GET {endpoint}/models`，先 Bearer 后 `x-api-key`+`anthropic-version` 两种鉴权；按响应体判别——`data[].object=="model"` → OpenAI 族；`data[].type=="model"` → Anthropic。
4. **兜底**：默认 OpenAI Chat（兼容面最大），记 debug 日志。

探测结果缓存在 Client 生命周期内；探测失败不致命，首次真实请求仍可工作。

---

## 5. 功能模块详细设计

### 5.1 模型注册表（发现 / 手动 / 内置，三级合并）

```go
type ModelInfo struct {
    ID               string
    DisplayName      string
    ContextWindow    int    // 上下文窗口长度（tokens）
    MaxOutputTokens  int    // 最大输出限制（tokens）
    SupportsThinking bool   // 是否支持 thinking/reasoning
    ThinkingStyle    string // "effort"(OpenAI) | "budget"(Anthropic) | ""
    Known            bool   // 是否来自内置知识库/手动配置（区别于远端裸发现）
    Aliases          []string
}
```

- **来源与合并优先级**：手动配置（Go API / JSON 文件）> 远端自动发现 > 内置知识库。同 ID 字段级合并（手动配的 `context_window` 覆盖内置，其余继承）。
- **内置知识库**：`registry_data/models.json` 以 `go:embed` 打包，覆盖主流模型（GPT-4o/4.1/5.x、o3/o4-mini、Claude 4.x 系、DeepSeek V3/R1、Qwen3、GLM 等）的 `context_window / max_output_tokens / thinking` 能力；随版本发布更新。查不到的模型 `Known=false`，用保守默认值（上下文 128k / 输出 4096）。
- **服务点**：
  - `Chat` 时自动补全 `MaxOutputTokens`（Anthropic 必填）；
  - 发送前粗校验 prompt 是否超出 `ContextWindow`（见 5.6）；
  - `Thinking` 请求落到不支持思考的模型时，返回明确的本地错误（可配置为降级忽略）。

### 5.2 Thinking 能力统一

统一抽象（调用方只学一份）：

```go
type ThinkingConfig struct {
    Effort          Effort // EffortNone(关) / EffortLow / EffortMedium / EffortHigh
    BudgetTokens    int    // 精确预算；>0 时优先生效（Anthropic 直接用，OpenAI 映射到最近档位）
    IncludeThoughts bool   // 是否要求返回思考内容（OpenAI: reasoning.summary；Anthropic 默认返回）
}
```

映射规则：

| 统一值 | OpenAI Chat | OpenAI Responses | Anthropic |
|---|---|---|---|
| EffortLow | `reasoning_effort:"low"` | `reasoning.effort:"low"` | `budget_tokens≈2048` |
| EffortMedium | `"medium"` | `"medium"` | `budget_tokens≈8192` |
| EffortHigh | `"high"` | `"high"` | `budget_tokens≈32768`（自动 clamp 到 `< max_tokens` 且 `≥1024`） |
| BudgetTokens=N | 就近映射到档位 | 就近映射到档位 | 原值 |
| EffortNone | 不下发字段 | 不下发字段 | 不下发 `thinking` |

- 模型不支持 thinking 但请求开启 → 默认返回本地错误 `ErrThinkingUnsupported`（携带模型 ID），`WithThinkingFallback(true)` 可改为静默忽略。
- 思考内容统一落在 `Block{Type:"thinking"}` / `EventThinkingDelta`，与正文严格分离。

### 5.3 Token 用量统计与查询

统一 `Usage` 与字段映射：

| 统一字段 | OpenAI Chat | Responses | Anthropic |
|---|---|---|---|
| `InputTokens` | `prompt_tokens` | `input_tokens` | `input_tokens` |
| `OutputTokens` | `completion_tokens` | `output_tokens` | `output_tokens` |
| `TotalTokens` | `total_tokens` | `total_tokens` | 求和 |
| `CachedInputTokens` | `prompt_tokens_details.cached_tokens` | `input_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| `ReasoningTokens` | `completion_tokens_details.reasoning_tokens` | `output_tokens_details.reasoning_tokens` | — |

内存统计器（默认开启）：

- 数据结构：分片互斥（或原子计数）按 `model × protocol × 时间桶` 聚合；`Snapshot()` 返回深拷贝，O(模型数)。
- 查询维度：总量、按模型、按协议、请求次数、错误次数、`usage_missing` 次数。
- `UsageTracker` 为接口：v1 提供 `MemoryUsageTracker`；持久化（SQLite/OTel/Prometheus）由使用方实现接口注入，SDK 不内置存储。
- 记录点：非流式在响应返回时；流式在 `MessageEnd`（或流关闭）时；重试成功只计最终一次，重试失败计 `retries` 字段。

### 5.4 流式引擎（SSE）

`internal/sse` 三协议共用解析器（约 200 行，零依赖）：

- 按行读取 `data:` / `event:` / 注释行，多行 `data` 拼接，空行分发事件；处理 `\r\n`；`[DONE]` 终止（OpenAI 系）。
- `bufio.Reader` 自定义读取（Scanner 默认 64KB 上限不够，长 JSON delta 会截断）——动态增长缓冲，单事件上限可配（默认 8MB）。
- **取消与超时**：`ctx` 贯穿；请求 header 发出后即响应取消；空闲超时（连续 N 秒无字节，默认 60s，可配）判为错误并断开。
- **断流语义**：收到 `error` 事件（Anthropic）或连接中断 → 返回 `*APIError`（已收到的增量事件不丢，调用方可用 `stream.Partial()` 取已聚合的部分文本）。
- `StreamReader` 接口：`Next() (Event, error)` / `Collect() (*ChatResponse, error)` / `Partial() string` / `Usage() Usage` / `Close() error`。

### 5.5 HTTP 层、超时与重试

- 复用 `http.Client`（可注入）；默认拨号/TLS/响应头超时分层设置。
- 重试（默认开，`WithMaxRetries` 控制）：命中 429/500/502/503/504 与网络错误；指数退避 + 抖动；尊重 `Retry-After`；流式仅在**收到首字节前**失败才重试；带 body 请求整体重发安全（每次请求都由统一模型完整重建 payload）。
- 鉴权头由适配器决定（Bearer vs `x-api-key`+`anthropic-version`），`Client` 不感知。

### 5.6 上下文长度校验与 token 估算

- 内置启发式 `TokenEstimator`：CJK ≈ 1 token/字符，ASCII ≈ 1 token/4 字符（保守偏高），叠加每消息固定开销；纯本地、零依赖。
- 默认行为：估算总量 + 预留 `MaxOutputTokens` 超过 `ContextWindow` 时**记 warning 不阻断**；`WithStrictContextCheck(true)` 改为返回 `ErrContextTooLong`。
- 可插拔：`WithTokenEstimator(custom)` 接入精确实现；Anthropic 可选对接 `/v1/messages/count_tokens`（路线图）。

---

## 6. 代码组织

> **实现调整（M0）**：原计划把适配器放 `provider/` 子包，实现时发现子包需要反向依赖根包的统一类型
> （Message/Usage/Event），会造成 import cycle。因此适配器收敛到根包内按文件拆分
> （`provider_openai_chat.go` 等），`protocolProvider` 内部接口定义在 `client.go`。对外 API 不受影响。

```
Rosetta/
├── go.mod                      # module github.com/cn-maul/rosetta
├── PLAN.md
├── README.md                   # 快速上手 + 协议对照说明
├── rosetta.go                   # 包文档、Protocol 常量、默认端点
├── client.go                   # Client 门面 + protocolProvider 内部接口
├── options.go                  # WithXxx 配置、Quirks
├── errors.go                   # APIError、TransportError、哨兵错误
├── message.go                  # Message / Block / 便捷构造
├── request.go                  # ChatRequest / ThinkingConfig / Effort
├── response.go                 # ChatResponse / StopReason
├── stream.go                   # Stream 接口 / Event / streamCore 聚合器
├── usage.go                    # Usage / UsageTracker / MemoryUsageTracker
├── models.go                   # ModelInfo
├── url.go                      # joinEndpoint 等 URL 工具
├── provider_openai_chat.go     # OpenAI Chat 适配器（请求/响应/SSE/降级探测）
├── provider_openai_responses.go# Responses 适配器（M3）
├── provider_anthropic.go       # Anthropic 适配器（M2）
├── provider_stubs.go           # 未落地适配器的占位（返回 ErrNotImplemented）
├── registry.go                 # 模型注册表（合并/查询）—— M4
├── registry_data/models.json   # go:embed 内置知识库 —— M4
├── detector.go                 # 协议探测 —— M4
├── estimator.go                # token 估算 —— M4
├── internal/
│   ├── sse/                    # SSE 解析器（已落地）
│   ├── httpx/                  # 请求执行 / 重试 / 退避（已落地）
│   └── jsonx/                  # 宽松 JSON 解码 —— M6
├── examples/
│   ├── chat/  ├── stream/  └── usage/   #（后续协议示例随里程碑增加）
└── .github/workflows/ci.yml
```

依赖策略：仅标准库；`models.json` 手动配置文件格式仅支持 JSON（不引入 YAML）。

---

## 7. 测试与质量保障

| 层次 | 手段 |
|---|---|
| 映射纯函数 | 表驱动测试：统一模型 ↔ 各协议 JSON 双向用例（含 thinking、tool、多模态、usage） |
| SSE 解析 | 事件序列 fixture 回放（三协议真实抓包样本脱敏）；模糊测试（go-fuzz 原生 fuzzing）防解析器崩溃 |
| 协议集成 | `httptest` 模拟服务：正常/错误码/重试/断流/无 usage 等场景 |
| 第三方脏数据 | jsonx 宽松解码专项用例（类型漂移、字段缺失） |
| 并发 | `-race` 全量开启；统计器并发压测 |
| 真实服务冒烟 | `examples/` 下带 `ROSETTA_SMOKE=1` + 真实 key 才运行的冒烟测试（CI 无 key 自动跳过） |
| 静态检查 | golangci-lint；公开 API 文档齐全（godoc）；测试覆盖率核心包 ≥ 85% |

---

## 8. 里程碑计划

单人全职估算，总计约 5 周；各里程碑均有可运行交付物与验收标准。

> **进度（2026-09-05）**：M0~M6 全部完成，标记 **v0.1.0**（`rosetta.Version`）。
> 已落地：三大协议适配器（M1 Chat / M2 Anthropic / M3 Responses）、模型体系（M4：go:embed
> 知识库 + 三级合并注册表 + DetectClient 探测 + WithVendor 厂商预设 + thinking 门控 +
> 上下文校验）、用量统计（M5：Tracker/Snapshot/usage_missing/流式兜底）、打磨（M6：
> internal/jsonx 宽松解码、CI workflow、6 个示例、README）。剩余事项：真实服务冒烟
> （已通过）。module path `github.com/cn-maul/rosetta`、许可证 MIT 均已定（2026-09-05），§10 待确认事项全部闭环。

| 阶段 | 内容 | 交付物 / 验收 | 预估 |
|---|---|---|---|
| **M0 骨架** | go.mod、目录、CI、统一类型（Message/Usage/错误）、options | `go build` 通过；类型定义冻结评审 | 0.5 周 |
| **M1 OpenAI Chat** | 请求/响应映射、流式、usage、工具透传、重试 | 对官方 + 1 家兼容服务（如 DeepSeek）跑通非流式/流式 | 1 周 |
| **M2 Anthropic** | 消息映射（system 提升）、thinking（含 budget clamp）、流式 delta、cache usage | 对官方 API 跑通；thinking 多轮回传 signature 正确 | 1 周 |
| **M3 Responses** | items 映射、类型化事件流归一、reasoning.effort | 对官方跑通；与 M1 共用同一测试请求集 | 0.5 周 |
| **M4 模型体系** | 内置知识库、ListModels、手动配置合并、上下文校验、探测 | 探测对三类 endpoint 命中正确；合并优先级测试通过 | 1 周 |
| **M5 用量统计** | 统计器、Snapshot、Tracker 接口、流式 usage 兜底 | 并发统计无竞态（-race）；各协议流式均能取到 usage | 0.5 周 |
| **M6 打磨发布** | quirks、examples、README、godoc、语义化版本 v0.1.0 | 5 个示例可运行；文档完整；覆盖率达标 | 0.5 周 |

后续路线图（v0.2+）：Bedrock/Vertex 适配、count_tokens 对接、结构化输出（JSON Schema）统一、prompt caching 统一配置、Embeddings。

---

## 9. 风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| Responses API 仍在演进，字段变动 | 映射失效 | 宽松解码 + `Raw` 逃生口 + fixture 随版本更新 |
| 第三方兼容服务质量参差（无 usage、非法流式） | 统计失真、流式崩溃 | jsonx 宽松解码、quirks 机制、usage_missing 计数、解析器永不 panic |
| 内置知识库过期（新模型上下文参数变化） | 校验不准 | `Known=false` 保守默认；手动配置最高优先级；随版本更新 JSON |
| 思考参数互斥规则（Anthropic temperature=1、budget<max） | 请求 400 | SDK 自动纠正 + warning；文档明示 |
| 兼容服务对 `max_completion_tokens` 支持不一 | 请求 400 | 默认字段探测 + `WithMaxTokensField` 覆盖 |
| SSE 长事件截断/连接抖动 | 流式中断 | 动态大缓冲、空闲超时、`Partial()` 保留已收内容、可重试策略 |

---

## 10. 待确认事项（实现前需拍板）

1. ~~**module path**~~：已定 `github.com/cn-maul/rosetta`（2026-09-05）。
2. **工具调用**：是否按本计划纳入 v1（仅透传，不含执行循环）？若不需要可砍掉约 3 天工作量。
3. **Go 最低版本**：建议 Go 1.22+，是否可接受？
4. **用量持久化**：v1 仅内存 + 接口扩展点，是否符合预期？是否需要内置 SQLite/文件持久化？
5. **多模态范围**：图片输入（URL/base64）v1 纳入，音频/视频是否需要？
6. ~~**SDK 命名**~~：已定名 **Rosetta**（2026-09-05，原暂定名 PolyAI 弃用）。
7. ~~**许可证**~~：已定 MIT（2026-09-05）。

---

## 11. 参考项目研究：cc-switch

> 研究方式：git clone 走 7897 代理持续 TLS 中断，改用 GitHub API 文件树 + raw 文件直读，精读了与 SDK 相关的 5 个源文件。
> 项目定位：Tauri 桌面应用（TS + Rust），管理/切换 Claude Code、Codex 等工具的多供应商配置；其 Rust 侧内置一个本地代理（`src-tauri/src/proxy/`），在上游协议与各供应商之间做协议转换、SSE 转发、thinking 处理与用量统计 —— 与本 SDK 目标高度重叠。

### 11.1 已采纳的借鉴点

| # | 来源 | 借鉴点 | 落地到本计划 |
|---|------|--------|--------------|
| 1 | `proxy/providers/adapter.rs` | Provider Adapter trait 把适配职责拆为：取 base_url、取认证、构造 URL、生成认证头、请求转换、响应转换 | 验证了 §2 的 `Provider` 接口方向；内部按同样职责组织，对外仍是单一接口 |
| 2 | `proxy/sse.rs` | SSE 解析三要点：①块边界取 `\r\n\r\n` 与 `\n\n` 中最先出现者；②字段解析同时兼容 `field:` 与 `field: `（冒号后空格可选）；③跨 chunk 的 UTF-8 多字节字符需缓冲余量（其测试专门把中文/emoji 拆到两个 chunk） | 写入 §7 `internal/sse` 的实现与测试要求；Go 侧按字节缓冲，仅在完整事件上解析 |
| 3 | `proxy/thinking_budget_rectifier.rs` | **响应式 thinking 整流**：上游返回 400 且错误信息含 `budget_tokens`/`thinking`/`1024` 约束时，自动改写 `budget_tokens=32000`、`max_tokens<32001` 则抬到 64000 并重试一次；`type=="adaptive"` 跳过 | 补进 §4 Anthropic 适配：主动 clamp（§5）防错 + 响应式整流兜错，`WithThinkingRectify(true)` 默认开启，仅重试一次 |
| 4 | `docs/pi-thinking-level-map-requirements-zh.md` | 原则「**预设完整可靠，自定义配置不猜测**」：模型知识库只覆盖内置预设，自定义模型不自动推断能力；档位映射用稀疏语义 | 强化 §5 `ModelInfo.Known` 语义：`Known=false` 时不推断 thinking/上下文能力，以上游实际返回为准；per-model 档位覆盖记录为 v0.2 候选 |
| 5 | `src/config/universalProviderPresets.ts` | 供应商预设结构：`{name, providerType, defaultModels, websiteUrl, ...}` + 工厂函数由 (preset, baseUrl, apiKey) 生成实例 | §4 兼容面列表升级为**内置 Vendor 预设表**：`WithVendor("deepseek")` 一次设置默认 endpoint、协议、quirks、模型列表；v0.1 内置 6~8 家主流厂商 |
| 6 | `services/model_fetch.rs` / `model_pricing.rs` | 从 `/models` 拉取列表 + 本地价格表估算成本 | 拉取逻辑已在 §5；成本估算不在本期需求，不采纳 |

### 11.2 不适用的部分

Tauri/GUI、把配置写进 Claude Code `settings.json` 的托管切换、应用侧会话管理 —— 属于桌面产品范畴，与 SDK 无关。

### 11.3 对里程碑的增量影响

M1（OpenAI Chat）不变；M2（Anthropic）增加响应式整流（约 +0.5 天）；M4 增加 Vendor 预设表（约 +1 天）；总体仍约 5 周。
