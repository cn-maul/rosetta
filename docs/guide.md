# 基础指南

客户端的创建、配置与一次性调用（Chat）。流式见[流式响应](streaming.md)，模型元数据见[模型体系](models.md)，底层协议差异见[协议与兼容](protocols.md)。

## 创建客户端

```go
client, err := rosetta.NewClient(
	rosetta.WithEndpoint("https://api.deepseek.com/v1"),
	rosetta.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
)
```

- endpoint 与 API key 必须可用：显式给出，或使用所选协议的官方默认端点。
- endpoint 规则：带版本路径（`/v1`、`/api/paas/v4`）原样保留；裸主机自动补 `/v1`；base 上的 query 参数保留在拼接后 URL 的末尾。
- 凭证发送方式由协议决定：OpenAI 系 `Authorization: Bearer`，Anthropic `x-api-key`（同时附带 Bearer 以兼容网关）。
- `Client` 并发安全，可多 goroutine 共享；`Stream` 不并发。

## 配置选项

| Option | 默认 | 说明 |
|---|---|---|
| `WithEndpoint(url)` | 协议官方地址 | API 基地址，见上方规则 |
| `WithAPIKey(key)` | 无（必填） | 凭证 |
| `WithProtocol(p)` | `ProtoOpenAIChat` | `ProtoOpenAIChat` / `ProtoOpenAIResponses` / `ProtoAnthropic`；也可由 `DetectClient` 探测决定 |
| `WithHTTPClient(c)` | `&http.Client{}` | 自定义底层 HTTP 客户端（代理、TLS）；不要在它上面设总超时，会杀死流式；也用于 `DetectClient` 的协议探测 |
| `WithTimeout(d)` | 无 | 仅约束非流式调用（Chat / ListModels / ModelInfo），以及 `DetectClient` 的协议探测 |
| `WithMaxRetries(n)` | 2 | 可重试失败（网络错误、408/429/5xx/529）的重试次数，见[重试策略](protocols.md#重试策略) |
| `WithRetryBase(d)` | 400ms | 退避基数（逐次翻倍 ±20%，上限 8s） |
| `WithDefaultMaxOutputTokens(n)` | 无 | 请求未指定输出上限时的默认值 |
| `WithUsageTracker(t)` | 无（不记账） | 传 `rosetta.NewMemoryUsageTracker()` 启用，见[用量统计](usage-stats.md) |
| `WithLogger(l)` | 丢弃 | 接收 debug/warn 日志（重试、降级、告警） |
| `WithThinkingFallback(v)` | false | 见 [thinking](#thinking-统一配置) |
| `WithThinkingRectify(v)` | true | Anthropic 预算错误的响应式整流开关 |
| `WithMaxTokensField(f)` | 自动探测 | 强制 OpenAI Chat 的输出上限字段（`max_completion_tokens` / `max_tokens`） |
| `WithStrictContextCheck(v)` | false | 上下文超限从告警变为报错 |
| `WithModelInfo(...)` | 无 | 手动注入模型元数据（可多次调用，与文件配置合并，id 冲突时优先），见[模型体系](models.md) |
| `WithModelsFile(path)` | 无 | 从 JSON 文件加载模型元数据（与 `WithModelInfo` 合并进同一手动层） |
| `WithQuirks(q)` | 无 | 声明第三方兼容性偏差，见[协议与兼容](protocols.md) |

## 消息与内容块

一条 `Message` 由角色 + 若干 `Block` 组成，五种块类型覆盖三协议的全部内容形态：

| BlockType | 有效字段 | 用途 |
|---|---|---|
| `BlockText` | `Text` | 普通文本 |
| `BlockImage` | `ImageURL`（http(s) 或 `data:` URL） | 图片输入 |
| `BlockToolCall` | `ToolCallID` / `ToolName` / `Arguments`（JSON 字符串） | 模型请求调用工具 |
| `BlockToolResult` | `ToolCallID` / `Content` / `IsError` | 工具结果回传 |
| `BlockThinking` | `Thinking` / `Signature` | 思考内容（Signature 为 Anthropic 签名，回传时必须原样携带） |

便捷构造函数覆盖常见场景：

```go
rosetta.System("系统提示")                 // system 消息
rosetta.User("用户输入")                   // user 文本
rosetta.UserImage("看这张图", imageURL)    // 图文混合
rosetta.Assistant("上一轮回答")            // assistant 文本
rosetta.AssistantBlocks(                  // assistant 含工具调用/思考回放
	rosetta.ToolCall("call_1", "get_weather", `{"city":"SF"}`),
	rosetta.Thinking("推理过程", "sig..."),
)
rosetta.ToolResult("call_1", "get_weather", "sunny 22C") // 工具结果（RoleTool）
```

`ChatRequest.System` 是顶层系统提示的快捷方式，与消息列表中的 system 消息按顺序合并。

## 请求字段

```go
type ChatRequest struct {
	Model           string             // 必填
	Messages        []Message          // 与 System 至少有一项
	System          string             // 顶层系统提示
	MaxOutputTokens int                // 0 = 依次回退：注册表元数据 → WithDefaultMaxOutputTokens；Anthropic 兜底 4096
	Temperature     *float64           // 指针，nil = 不发送；用 rosetta.Float(0.7) 构造
	TopP            *float64
	StopSequences   []string           // Responses 协议不支持，会被丢弃并记日志
	Tools           []ToolDefinition   // 仅透传，执行工具是你的事
	Thinking        *ThinkingConfig    // nil = 协议默认（不思考）
	Extra           map[string]any     // 逃生口：逐键合并进协议负载，最后生效
}
```

`ToolDefinition{Name, Description, Parameters json.RawMessage}` 的 Parameters 是 JSON Schema；省略时 SDK 补空对象。

## 工具调用往返

```go
resp, _ := client.Chat(ctx, req) // req.Tools 定义了 get_weather

for _, call := range resp.ToolCalls() {
	result := executeTool(call.ToolName, call.Arguments) // 你的执行逻辑
	messages = append(messages,
		rosetta.AssistantBlocks(call),
		rosetta.ToolResult(call.ToolCallID, call.ToolName, result),
	)
}
```

协议差异（Anthropic 的 `input` 对象、`tool_result` 归位 user 轮等）由适配器处理，见[协议与兼容](protocols.md)。

## Thinking 统一配置

```go
Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortMedium} // 低/中/高三档
// 或显式预算：&rosetta.ThinkingConfig{BudgetTokens: 8192}
```

| 协议 | 发送形式 | 映射规则 |
|---|---|---|
| OpenAI Chat | `reasoning_effort` | Effort 直传；显式 BudgetTokens 就近映射档位（≤4096 low，≤16384 medium，其余 high） |
| OpenAI Responses | `reasoning.effort` | 同上 |
| Anthropic | `thinking.budget_tokens` | Effort ≈ 2048/8192/32768；显式预算原样；两者都没给默认 medium |

Anthropic 约束由 SDK 主动满足：budget ≥ 1024；budget ≥ max_tokens 时抬高 max_tokens（预算优先）；thinking 模式下丢弃 temperature/top_p；上游仍报预算约束错误时自动改写重试一次（整流，`WithThinkingRectify(false)` 关闭）。

**能力门控**：对已知的非思考模型（手动配置的 `ModelInfo.Known && !SupportsThinking`），请求 thinking 默认报 `ErrThinkingUnsupported`；`WithThinkingFallback(true)` 改为静默去掉 thinking 配置。未知模型不做猜测、原样透传。

## 上下文校验

请求发出前，若模型在注册表中有 `ContextWindow`，SDK 会用启发式估算（中文≈1 token/字、英文≈4 字符/token、每图 1500、每条消息 +4、每个工具定义 +24 与 schema 文本）比较 `估算输入 + 输出上限` 与窗口：

- 超限默认**仅告警**（进日志），请求照发；
- `WithStrictContextCheck(true)` 改为返回 `ErrContextTooLong`。

估算是保守近似，不是 tokenizer；需要精确计数请自行接真实 tokenizer 后自行校验。

## 响应

```go
type ChatResponse struct {
	ID, Model  string
	Content    []Block    // 生成内容：text / thinking / tool_call，按协议原始顺序
	StopReason StopReason
	Usage      Usage
	Raw        json.RawMessage // 原始响应体（截断至 4KB；本身不是合法 JSON 时降级为 JSON 字符串，保证可再序列化）
}
```

便捷方法：`Text()` 拼接文本块、`ThinkingText()` 拼接思考块、`ToolCalls()` 取工具调用。

`StopReason` 统一取值：`StopEnd` / `StopLength` / `StopToolUse` / `StopContentFilter` / `StopRefusal` / `StopOther`。非流式 200 响应省略终止原因时一律视为 `StopEnd`；流式的对应语义见[流式响应](streaming.md)。

## 错误处理

```go
resp, err := client.Chat(ctx, req)
var apiErr *rosetta.APIError
switch {
case errors.As(err, &apiErr):
	fmt.Println(apiErr.StatusCode, apiErr.Type, apiErr.Code, apiErr.Message, apiErr.Retryable)
case errors.Is(err, rosetta.ErrContextTooLong):
	// 本地拦截，请求未发出
default:
	// 网络层失败：*rosetta.TransportError（可 errors.As 取 URL）
}
```

Go 1.26+ 可以用泛型的 `errors.AsType` 省去变量声明：

```go
if apiErr, ok := errors.AsType[*rosetta.APIError](err); ok {
	fmt.Println(apiErr.StatusCode, apiErr.Retryable)
}
```

哨兵错误（`errors.Is` 匹配）：`ErrNoEndpoint`、`ErrNoAPIKey`、`ErrUnknownModel`、`ErrContextTooLong`、`ErrThinkingUnsupported`、`ErrInvalidRequest`。

`APIError` 携带 `StatusCode / Code / Type / Message / RequestID / Method / URL / Retryable / Raw`，由三协议的错误体归一而来。

## 附注

- Windows 本地跑 `go test -race` 需要 CGO（gcc）；无 gcc 环境用 `go test ./...` 即可，CI（Linux）会跑 race。
- **MinGW 装在含空格的路径下（如 `C:\Program Files\mingw64`）会导致所有 cgo 链接失败**（gcc 的 `*endfile` spec 引用 `default-manifest.o` 时路径未加引号）。把 MinGW 移到无空格路径是根治方案；临时绕过：导出并打补丁 specs 后在 `-ldflags` 中引用：
  ```bash
  gcc -dumpspecs > C:/Users/<you>/mingw64-specs.txt
  sed -i 's/%{!shared:%:if-exists(default-manifest\.o%s)}//' C:/Users/<you>/mingw64-specs.txt
  go build -ldflags "-extldflags=-specs=C:/Users/<you>/mingw64-specs.txt" ./...
  ```
- Windows Insider 构建（本机 build 29648）上 `-race` 可编译链接，但 TSan 运行时在固定地址分配 shadow memory 会报 `error code: 87` 而无法启动——属 OS 层限制，本地以 `go test ./...` 为准，race 由 CI（Linux）执行。
- 示例程序读 `ROSETTA_ENDPOINT` / `ROSETTA_API_KEY` / `ROSETTA_MODEL` 环境变量：`go run ./examples/chat`。
