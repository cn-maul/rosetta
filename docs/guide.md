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
- endpoint 规则：带版本路径（`/v1`、`/api/paas/v4`）原样保留；裸主机自动补 `/v1`；**不允许携带 query 串**（query 可能藏有凭据，会泄入错误信息与重试日志），携带时构建即报错。
- 凭证发送方式由协议决定：OpenAI 系 `Authorization: Bearer`，Anthropic 默认只发 `x-api-key`（需要 Bearer 的兼容网关用 `WithAnthropicBearerAuth(true)` 显式开启）。
- `Client` 并发安全，可多 goroutine 共享；`Stream` 的 `Next` 须单 goroutine 驱动，`Err` / `Usage` / `Partial` / `Close` 可并发调用。

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
| `WithInterleavedThinking(v)` | 自动 | 强制开关 Anthropic 的 `interleaved-thinking-2025-05-14` beta 头。默认按 payload 是否**同时**含 thinking 与 tools 自动判定；端点经 Bedrock / Vertex 转发时设 `false`（这两家会拒绝白名单外模型的该头），见[协议与兼容](protocols.md) |
| `WithMaxTokensField(f)` | 自动探测 | 强制 OpenAI Chat 的输出上限字段（`max_completion_tokens` / `max_tokens`） |
| `WithStrictContextCheck(v)` | false | 上下文超限从告警变为报错 |
| `WithModelInfo(...)` | 无 | 手动注入模型元数据（可多次调用，与文件配置合并，id 冲突时优先），见[模型体系](models.md) |
| `WithModelsFile(path)` | 无 | 从 JSON 文件加载模型元数据（与 `WithModelInfo` 合并进同一手动层） |
| `WithEmbeddingEndpoint(url)` | 主 endpoint | Embed / Rerank 的独立基地址（chat 走主地址、embedding 走另一家时使用），见[嵌入与重排](#嵌入与重排) |
| `WithEmbeddingAPIKey(key)` | 主 key | 嵌入端点的独立凭证 |
| `WithAnthropicBearerAuth(v)` | false | 额外发送 `Authorization: Bearer`（默认只发 `x-api-key`；仅认证方式只有 Bearer 的兼容网关需要） |
| `WithExtraOverrides(v)` | false | 允许 `Extra` 覆盖 SDK 管理的负载字段（默认冲突即报 `ErrInvalidRequest`）。保留键按 API 族划分：chat（`model`/`messages`/`stream`/输出上限/采样/工具/thinking 等）、embeddings（`model`/`input`/`dimensions`/`user`/`encoding_format`）、rerank（`model`/`query`/`documents`/`top_n`/`return_documents`）——因此 `ChatRequest.Extra["user"]` 合法（chat 没有 user 字段），而 `EmbeddingRequest.Extra["user"]` 会被拒 |
| `WithMultimediaTokenEstimates(e)` | 1500/500/3000 | 上下文检查中图片/音频/文档块的平估 token 数 |
| `WithQuirks(q)` | 无 | 声明第三方兼容性偏差，见[协议与兼容](protocols.md) |

## 消息与内容块

一条 `Message` 由角色 + 若干 `Block` 组成，七种块类型覆盖三协议的全部内容形态：

| BlockType | 有效字段 | 用途 |
|---|---|---|
| `BlockText` | `Text` | 普通文本 |
| `BlockImage` | `ImageURL`（http(s) 或 `data:` URL） | 图片输入 |
| `BlockAudio` | `AudioData`（原始 base64）/ `AudioFormat`（`wav`/`mp3`） | 音频输入（仅 OpenAI 系，见下） |
| `BlockFile` | `FileData`（base64、`data:` URL 或 http(s) URL）或 `FileID`，配 `FileName` / `MimeType` | 文档输入（PDF 等），见下 |
| `BlockToolCall` | `ToolCallID` / `ToolName` / `Arguments`（JSON 字符串） | 模型请求调用工具 |
| `BlockToolResult` | `ToolCallID` / `Content` / `IsError` | 工具结果回传 |
| `BlockThinking` | `Thinking` / `Signature` | 思考内容（Signature 为 Anthropic 签名，回传时必须原样携带） |

便捷构造函数覆盖常见场景：

```go
rosetta.System("系统提示")                 // system 消息
rosetta.User("用户输入")                   // user 文本
rosetta.UserImage("看这张图", imageURL)    // 图文混合
rosetta.UserAudio("听一下", b64, "wav")    // 音频输入
rosetta.UserFile("总结这份 PDF", "报告.pdf", "application/pdf", b64) // 文档输入
rosetta.Assistant("上一轮回答")            // assistant 文本
rosetta.AssistantBlocks(                  // assistant 含工具调用/思考回放
	rosetta.ToolCall("call_1", "get_weather", `{"city":"SF"}`),
	rosetta.Thinking("推理过程", "sig..."),
)
rosetta.ToolResult("call_1", "get_weather", "sunny 22C") // 工具结果（RoleTool）
```

`ChatRequest.System` 是顶层系统提示的快捷方式，与消息列表中的 system 消息按顺序合并。

### 音频与文件输入的协议差异

音频与文件在各协议下的映射由适配器处理，但**能力子集不同**，无效组合在请求构建时即报 `ErrInvalidRequest`。角色与块类型也有矩阵约束（例如 system / assistant 消息不能携带媒体块，`RoleTool` 只能携带工具结果）——**不支持的角色×块组合会显式报错，绝不静默丢弃**：

| 统一输入 | OpenAI Chat | OpenAI Responses | Anthropic |
|---|---|---|---|
| 音频（`wav`/`mp3`） | `input_audio` part | ❌ 不支持，直接报错（官方输入类型仅 text/image/file） | ❌ 不支持，直接报错 |
| 内联文件（base64 / `data:` URL） | `file` part（`file_data`） | `input_file` part（`file_data`） | `document` block（base64 source，仅 PDF）或 text source（`text/plain`） |
| 已上传文件（`FileRef`/`FileID`） | `file` part（`file_id`） | `input_file` part（`file_id`） | `document` block（file source） |
| http(s) URL 文件 | ❌ 报错 | `input_file` part（`file_url`） | `document` block（url source，仅 PDF） |

- 内联 base64 未给 `MimeType` 时按 `application/pdf` 处理；Anthropic 的 base64 document source 官方仅定义 PDF，`text/plain` 转成官方 text source（base64 解码后原文直传），其他 MIME 直接报错。
- 音频格式只接受 `wav` / `mp3`；媒体载荷做本地 base64 / `data:` URL 语法校验，空载荷或非法 base64 构建请求时报错。
- 文件来源必须唯一：同时设置 `FileData` 与 `FileID` 报 `ErrInvalidRequest`。
- 媒体块旁的空文本块视为占位符自动忽略（`UserImage("", url)` 与手工拼装 `Blocks` 均适用），但整条消息不能没有任何有效内容。

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
	Extra           map[string]any     // 逃生口：逐键合并进协议负载；与当前 API 的 SDK 管理字段冲突时报错（WithExtraOverrides 可放行）
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

**工具调用之间的推理（交错思考）**：请求同时带 `Thinking` 与 `Tools` 时，SDK 自动附上 `anthropic-beta: interleaved-thinking-2025-05-14`，让模型在收到每个工具结果后继续推理，而不是在轮次开头思考一次就不再思考。该头对 Claude API 无副作用（不支持的模型忽略它），但 **Bedrock / Vertex 会对白名单外的模型报错**——端点经这类网关转发时用 `WithInterleavedThinking(false)` 关闭自动发送。模型差异与完整规则见[协议与兼容](protocols.md)。

**能力门控**：对已知的非思考模型（手动配置的 `ModelInfo.Known && !SupportsThinking`），请求 thinking 默认报 `ErrThinkingUnsupported`；`WithThinkingFallback(true)` 改为静默去掉 thinking 配置。未知模型不做猜测、原样透传。

## 上下文校验

请求发出前，若模型在注册表中有 `ContextWindow`，SDK 会用启发式估算（中文≈1 token/字、英文≈4 字符/token、每图 1500、每段音频 500、每份文档 3000、每条消息 +4、每个工具定义 +24 与 schema 文本、`Extra` 按其 JSON 长度）比较 `估算输入 + 输出上限` 与窗口：

- 超限默认**仅告警**（进日志），请求照发；
- `WithStrictContextCheck(true)` 改为返回 `ErrContextTooLong`。

输出上限的取值顺序是 请求的 `MaxOutputTokens` → 注册表 `ModelInfo.MaxOutputTokens` → 客户端默认；Anthropic 协议在都未设置时按它自己的 4096 下限参与判断（请求了 thinking 时该值更高），OpenAI 协议则视为"由 provider 决定"、输出侧不参与。

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

哨兵错误（`errors.Is` 匹配）：`ErrNoEndpoint`、`ErrNoAPIKey`、`ErrUnknownModel`、`ErrContextTooLong`、`ErrThinkingUnsupported`、`ErrInvalidRequest`、`ErrNotSupported`、`ErrStreamTruncated`。

`APIError` 携带 `StatusCode / Code / Type / Message / RequestID / Method / URL / Retryable / Raw`，由三协议的错误体归一而来。`Raw` 存储前做保守脱敏（**先脱敏后截断**，超 4KB 的大错误体同样生效）：敏感键的值替换为 `[redacted]`，`sk-…` 密钥材料统一掩码；但错误信息与 Raw 仍可能包含 provider 回显的内容，请避免把完整错误对象直接写入公开日志。注意 `Retryable` 是给调用方的重试提示（408/429/5xx/529 为 true），**SDK 自身只按重试策略自动重试**：GET 类请求默认重试，chat 等非幂等 POST 默认不自动重试（embedding/rerank 幂等，按策略重试）。流内错误（HTTP 200 但 SSE 事件报错）与流截断（`ErrStreamTruncated`）不看 HTTP 状态码判断。另外：第三方网关以 400 拒绝某个可选字段**取值**（如 `reasoning.effort: low`）时按配置错误原样报错，不会触发"删除整个字段"的降级；字段级拒绝的降级记忆按模型隔离。

## 嵌入与重排

Embedding 和 Rerank 是独立的 API 族，**不走 chat 的三协议**：embedding 走 OpenAI `/embeddings` 事实标准（OpenAI、Ollama、vLLM、Qwen、GLM、Moonshot、SiliconFlow 等兼容）；rerank 走 **Cohere `/rerank` 线格式**（Cohere、Jina、SiliconFlow、vLLM 等兼容）。注意 Voyage（`top_k` / `data[]`）与 DashScope（独立路径与嵌套负载/响应）的接口与 Cohere 格式**并不兼容**，当前实现未适配，请勿按本节用法直连这两家。embedding/rerank 与 `WithProtocol` 无关——`openai-chat` 和 `openai-responses` 客户端行为一致；**Anthropic 协议客户端没有这两个 API**（Anthropic 官方不提供），不配置 `WithEmbeddingEndpoint` 时调用返回 `ErrNotSupported`。

```go
resp, err := client.Embed(ctx, &rosetta.EmbeddingRequest{
	Model:      "bge-m3",
	Input:      []string{"第一段文本", "第二段文本"},
	Dimensions: 0, // 可选：OpenAI text-embedding-3 系的降维
})
// resp.Data[i].Index / resp.Data[i].Embedding ([]float32)；Usage 只含输入侧 token

rr, err := client.Rerank(ctx, &rosetta.RerankRequest{
	Model:           "bge-reranker-v2-m3",
	Query:           "什么是流式响应",
	Documents:       []string{"doc1", "doc2", "doc3"},
	TopN:            3,
	ReturnDocuments: true, // 结果中回带文档文本；否则按 Index 回查自己的切片
})
// rr.Results 按相关度降序：Index（原切片下标）、RelevanceScore、Document
```

- 调用走 `WithEmbeddingEndpoint` / `WithEmbeddingAPIKey`（未设置则用主 endpoint/key）。典型场景：chat 在 DeepSeek，embedding/rerank 在本地 Ollama、SiliconFlow 或其他向量服务商——这也是 Anthropic 客户端使用这两个 API 的唯一途径。
- 均为幂等 POST，失败按重试策略重试；无流式、无 thinking 门控；usage 记入与 chat 同一个 tracker（rerank 各家返回口径不一，Cohere 计费单位、Jina 总 token 等尽力映射，取不到时 UsageMissing 计数）。
- 上下文校验、thinking 门控等 chat 专属检查不适用。
- **响应严格校验**：embedding 的 index 必须存在、在范围内且不重复，向量元素必须全为数字，整批维度必须一致（显式传了 `Dimensions` 时必须精确匹配），返回顺序按 Index 恢复为输入顺序；rerank 的 index/score 必须存在且合法，结果按分数降序重排。负数 token 用量视为协议错误。空结果、数量不匹配、缺字段/`null` 一律报错而不是按零值成功。

## 附注

- **默认继承进程的环境代理。** 不传 `WithHTTPClient` 时底层用的是 `http.DefaultTransport`，它会读 `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`。Go 对 `localhost` 与回环地址自动跳过代理，所以本地 Ollama/vLLM 安全；但**内网域名和 `http://` 端点会被送进代理**——`http://` 还会让 API key 以明文经过代理。需要直连时显式给一个不装代理的客户端：
  ```go
  rosetta.WithHTTPClient(&http.Client{Transport: &http.Transport{}})
  ```
  同理，写单测时若断言"请求到不了服务器"，别依赖某个不存在的域名解析失败——设了代理的机器上代理会代答（一条 `http_proxy` 就能让这类断言红掉），用恒失败的 `RoundTripper` 更可靠。
- 默认的跨主机重定向守卫：SDK 会拒绝跳到别的主机的 3xx（net/http 只剥离 `Authorization`/`Cookie`，不会剥离 `x-api-key`），自己传 `http.Client` 时若已设 `CheckRedirect` 则以你的为准。
- Windows 本地跑 `go test -race` 需要 CGO（gcc）；无 gcc 环境用 `go test ./...` 即可，CI（Linux）会跑 race。
- **MinGW 装在含空格的路径下（如 `C:\Program Files\mingw64`）会导致所有 cgo 链接失败**（gcc 的 `*endfile` spec 引用 `default-manifest.o` 时路径未加引号）。把 MinGW 移到无空格路径是根治方案；临时绕过：导出并打补丁 specs 后在 `-ldflags` 中引用：
  ```bash
  gcc -dumpspecs > C:/Users/<you>/mingw64-specs.txt
  sed -i 's/%{!shared:%:if-exists(default-manifest\.o%s)}//' C:/Users/<you>/mingw64-specs.txt
  go build -ldflags "-extldflags=-specs=C:/Users/<you>/mingw64-specs.txt" ./...
  ```
- Windows Insider 构建（本机 build 29648）上 `-race` 可编译链接，但 TSan 运行时在固定地址分配 shadow memory 会报 `error code: 87` 而无法启动——属 OS 层限制，本地以 `go test ./...` 为准，race 由 CI（Linux）执行。
- MinGW 在含空格路径下跑 `-race` 的另一条绕过：给链接器显式传短路径的库搜索目录，`CC='C:/PROGRA~1/mingw64/bin/gcc.exe' CGO_LDFLAGS='-B C:/PROGRA~1/mingw64/x86_64-w64-mingw32/lib/' go test -race ./...`（`PROGRA~1` 是 `Program Files` 的 DOS 短名，规避 ld 对未加引号路径的拆分）。**2026-09-20 复测：这条绕过在当前工具链上已失效**（`ld.exe: cannot find C:/Program`）——gcc 内部仍按编译期前缀展开成长路径。可靠做法是把 MinGW 装到无空格路径（如 `C:\mingw64`）并设 `CC=C:/mingw64/bin/gcc.exe`，或改在 WSL / Linux 上跑本地 race 检查。
- 示例程序读 `ROSETTA_ENDPOINT` / `ROSETTA_API_KEY` / `ROSETTA_MODEL` 环境变量：`go run ./examples/chat`。
