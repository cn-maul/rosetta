# 更新日志

## v0.5.1 (2026-09-20)

修复 v0.5.0 引入的错误消息文案回归。只影响人读的字符串，`errors.Is` 匹配一直正常；升级无需改代码。

### 修复

- **流错误不再双写 `rosetta:` 前缀。** v0.5.0 把 `ErrStreamTruncated` 的文案由 `"stream truncated"` 改成 `"rosetta: stream truncated"`（这项变更当时未记入日志），但四处包装点仍沿用了**包外部错误**的写法 `fmt.Errorf("rosetta: %w: ...")`，于是错误串变成 `rosetta: rosetta: stream truncated: ...`。
  现在这四处包装点改用包**哨兵**的既定写法 `fmt.Errorf("%w: ...")`（哨兵自身已带前缀），与项目里其余 40 余处哨兵包装一致：`provider_anthropic.go`、`provider_openai_chat.go`、`provider_openai_responses.go` 的断流分支，以及 `stream.go` 的累计量溢出分支（后者包的是 v0.5.0 新增的 `ErrStreamOverflow`，同样双前缀）。
  修复后对外错误串与 v0.4.0 **逐字节相同**：

  ```
  rosetta: stream truncated: openai-chat stream ended without [DONE] (partial response kept in Stream.Partial)
  rosetta: stream truncated: responses stream ended without response.completed (partial response kept in Stream.Partial)
  rosetta: stream truncated: anthropic stream ended without message_stop (partial response kept in Stream.Partial)
  rosetta: stream accumulation exceeded safety limits: stream accumulation exceeded N bytes / M blocks (partial response kept in Stream.Partial)
  ```

  `errors.Is(err, ErrStreamTruncated)` / `errors.Is(err, ErrStreamOverflow)` 行为不变。新增 `stream_sentinel_test.go` 把三个协议的真实断流错误串钉住，并新增一条哨兵前缀约定测试（每个哨兵恰好一次 `rosetta: ` 前缀）。

## v0.5.0 (2026-09-20)

第二、三、四轮审计的修复与语义对齐批次。**含调用方可见的行为变更**（见下节）——按 semver 视为 minor。

### 行为变更（升级前必读）

- **Anthropic 缓存量并入 `InputTokens`/`TotalTokens`**（第三轮 B9）。Anthropic 线格式的 `input_tokens` 只计未缓存部分，适配器现在把 `cache_read_input_tokens` 与 `cache_creation_input_tokens` 折进 `InputTokens`/`TotalTokens`，使三协议的 `Input`/`Total` 口径一致（`CachedInputTokens` ⊆ `InputTokens`）。**这会改变既有 `Stats()` 数字**：Anthropic 用户的 `InputTokens`/`TotalTokens` 会比 v0.4.0 高（多出缓存读+写），但更接近真实用量；原先按文档自行 `Input + Cached + Creation` 相加的调用方现在会**重复计数**，请只读 `InputTokens`。文档已同步（`docs/protocols.md`、`docs/usage-stats.md`）。
- **`Usage.IsZero()` 现在把 `CachedInputTokens`/`CachedCreationTokens`/`ReasoningTokens` 计入判定**。只回报缓存（或只回报思考）token 的响应不再被当作"无用量"，`Stats().UsageMissing` 对这些 provider 会下降。
- **Anthropic 一元响应缺 `stop_reason` 但含 tool call 时返回 `StopToolUse`** 而不是 `StopEnd`，与 Responses 适配器对齐。依赖 `StopReason` 决定"要不要执行工具"的 agent 循环行为会变（这正是修正点）。
- **空 text 块携带 `CacheControl` 现在报 `ErrInvalidRequest`**。该块在渲染时被丢弃，断点从来上不了 wire，此前是静默失效；现在显式报错。
- **`Extra` 计入上下文估算**（按其 JSON 长度）。用 `Extra` 注入大块内容的请求可能开始出现上下文告警，`WithStrictContextCheck(true)` 下可能被拒。
- **Anthropic 的隐式输出上限参与上下文校验**。省略 `MaxOutputTokens` 时该协议实际会发 4096（thinking 时更高），此前输出侧不参与 `估算输入 + 输出上限 ≤ 窗口` 的判断，现在参与；同样的窗口设置下可能开始告警或报 `ErrContextTooLong`。
- **失败/错误响应体的读取上限由 1 MiB 提到 8 MiB**（与 `/models` 一致），网关的大体积错误页不再被截断成解码错误。

### 新增

- **哨兵错误 `ErrStreamTruncated` 与 `ErrStreamOverflow`**：前者区分"provider 终止事件之前 EOF"与干净结束（部分结果仍保留在 `Stream.Partial()`），后者在单流累计内容超过 64 MiB 或 10000 个内容块时终止流。
- **`Anthropic` 1 小时缓存的 beta 头判定改为基于已构建的 payload**，因此经 `Extra`（`WithExtraOverrides(true)`）注入的 `ttl:"1h"` 断点也能正确附带 `anthropic-beta: extended-cache-ttl-2025-04-11`（此前只扫类型化请求，这类断点上得了 wire 却拿不到 beta 头，被上游 400 拒绝）。
- **Anthropic 交错思考的 beta 头自动附带**（第四轮 C22）。payload **同时**含 `thinking` 与 `tools` 时（即官方限定的 Messages API 工具用法），SDK 发送 `anthropic-beta: interleaved-thinking-2025-05-14`，让模型在收到每个工具结果后继续推理，而不是一轮只在开头思考一次。模型差异按 Anthropic 官方文档落实：Opus 4.5 / Sonnet 4.5 及更早的 Claude 4 需要该头；Opus 4.6+ / Sonnet 5 走自适应思考、该头已弃用并被安全忽略；Haiku 4.5 不支持。**两个 beta 现在合并成一个逗号分隔的头**——此前逐个 `Set` 会互相覆盖，同时用扩展缓存与交错思考时必有一个丢失。新选项 `WithInterleavedThinking(v)` 可强制开关：经 **Amazon Bedrock / Google Cloud Vertex AI** 转发时设 `false`（这两家会拒绝白名单外模型的该头），Claude API 本身对任何模型都接受并忽略不支持者。
- **Anthropic 错误路径的端到端测试**（第四轮收尾）：错误信封解析、`request_id` 缺头时从响应体恢复、thinking 预算类 400 的整流重试（一元与流式各一条，断言确实只重试一次且第二次 payload 携带改写后的 budget/max_tokens）。此前 `parseAnthropicError` 覆盖率为 0、整流重试循环完全没有测试——三协议里唯独 Anthropic 的错误分支是空白。
- 缓存机制的回归测试：断点 TTL/Type 三态、无断点时 `system` 保持字符串（wire 字节不变）、有断点时降级为 text-block 数组、断点在 image/document/tool_use/tool_result/工具定义上的落地、beta 头四条路径、`ExtendedCache` 等此前零覆盖的函数。

### 修复 · 正确性与安全

- **跨主机重定向守卫装到默认与用户传入的 `http.Client`**：net/http 只剥离 `Authorization`/`Cookie`/`Proxy-*`，不会剥离 `x-api-key`；此前守卫只装在 `DetectClient` 探测路径，Anthropic 凭据可在一次 3xx 后泄露到第三方主机（307/308 还会重放含 prompt 的请求体）。
- **OpenAI Chat/Responses 解码接住 `refusal` 与旧版 `function_call`**：refusal 折成文本块，`function_call` 折成 `BlockToolCall`；只有 refusal 的回复不再表现为"成功但内容为空"。
- **Responses 流式 `error` 事件读嵌套信封**（`error.message`/`error.type`/`error.code`），不再产生信息全空、`Type` 被伪造成 `api_error` 的错误。
- **Responses 流式输出按 `item_id`/`output_index` 归并**，交错到达的增量不再丢失整项。
- **非 SSE 的 200 响应**（网关把 `stream:true` 回成普通 JSON）按完整回复处理，不再被当成截断的空流。
- **流语义终止后的干净 EOF 不再误报截断**：已见 `message_delta` / `[DONE]` / `response.completed` 后，即使 `message_stop` 缺席也算正常结束。
- **`Body.Close` 的错误不再冒充流失败**（改由 `Stream.Close()` 的返回值携带）。
- **错误体脱敏加固**：`redactJSON` 改用 `UseNumber`，越界浮点无法再绕过；非 JSON（网关 HTML 等）与大体积错误体同样掩码，且 `Raw` 保持合法 JSON。
- **`httpx` 预算耗尽时保留响应体**（不 drain），使调用方能读到最终 provider 错误而非空的 body。

### 修复 · 防护与健壮性

- **Anthropic 4 断点上限此前是死代码**：`countCacheControl` 未穿透构建器使用的 `[]map[string]any`，计数恒为 0，超限请求会直接打到上游吃 400。
- **上下文门不可回绕**：token 字段上限（`maxWireInt`）加不回绕的越界判定，巨大的输出上限不再让超限请求读成"放得下"。
- **`ChatRequest.validate` 加固**：拒绝 NaN/Inf 的采样参数、非 JSON 对象的工具参数、空工具名与重名工具、无法序列化的 `Extra`、空 stop sequence、越界的缓存 Type/TTL。
- **流累加改摊还拼接**（`strings.Builder`）并加字节/块数上限，恶意或异常 provider 无法驱动无界的 O(n²) 内存增长。
- **`MemoryUsageTracker` 零值可直接使用**（map 惰性创建）；聚合值对单次观测做饱和钳制，`MaxInt64` 量级不再把累计值加成负数。
- **`ListModels` 响应缺 `data` 时报协议错误**而不是当空列表，不再清空已学到的远端层。
- **`DetectClient` 尊重显式 `WithProtocol`**，不再被探测结果覆盖。
- **估算器计入 thinking 签名与 redacted 数据**（多轮扩展思考的真实 prompt 占用）。
- 注册表手动层同 id 条目做字段级合并，不再整条覆盖。
- 流式 `message_delta` 的 usage 字段改为按**是否上报**（而非是否非零）覆盖 `message_start` 基线，显式上报的 0（缓存完全未命中）不再被忽略。
- 用量记账的模型名改在流锁内读取，去掉 `onEnd` 回调中一处依赖时序假设的无锁读。

### 工程

- `docs/audit-2026-09-13*.md` / `docs/audit-2026-09-19*.md` / `docs/audit-2026-09-20.md` 四轮审计报告与逐条修复记录入库存档。
- `TestExtraReservedKeys` 改为注入恒失败的 `RoundTripper`，不再依赖"某域名必须解析失败"——设了 `http_proxy` 的环境（CI、企业网、本机）此前必然误报。
- 覆盖率 82.2% → 83.5%（CI 门禁 75%）；`ExtendedCache`、`CacheControl.validate`、`renderAnthropicSystem`、`payloadNeedsExtendedCacheTTL` 由 0%/40%/52.6%/未引用 提升到满覆盖。

## v0.4.0 (2026-09-13)

新能力批次：检索配套 API（Embeddings / Rerank）与多模态输入扩展。

### 新增

- **`Client.Embed`：统一的文本向量接口**。走 OpenAI `/embeddings` 事实标准（OpenAI、Ollama、vLLM、Qwen、GLM、Moonshot、SiliconFlow 等兼容），`EmbeddingRequest{Model, Input, Dimensions, User, Extra}` → `EmbeddingResponse{Model, Data[]{Index, Embedding []float32}, Usage}`。无流式、无 thinking 门控，usage（仅输入侧 token）记入与 chat 同一个 tracker；幂等 POST，按重试策略重试。
- **`Client.Rerank`：统一的重排序接口**。走 **Cohere `/rerank` 线格式**（Cohere、Jina、SiliconFlow、vLLM 等兼容；注意 Voyage 的 `top_k`/`data[]` 与 DashScope 的专用路径/嵌套结构和 Cohere 格式**不兼容**，当前未适配，请勿直连），`RerankRequest{Model, Query, Documents, TopN, ReturnDocuments, Extra}` → `RerankResponse{Results[]{Index, RelevanceScore, Document}}` 按相关度降序。各家 usage 口径不一（Cohere `meta.billed_units`/`meta.tokens`、Jina 平铺 `usage.total_tokens`）尽力映射，取不到时按 UsageMissing 记账。
- **`WithEmbeddingEndpoint` / `WithEmbeddingAPIKey`**：Embed / Rerank 的独立基地址与凭证。典型场景 chat 与 embedding 不同服务商（如 chat 在 DeepSeek、embedding 在本地 Ollama）；未设置时用主 endpoint/key。
- **Anthropic 客户端可用 Embed / Rerank 的唯一途径**：Anthropic 官方没有这两个 API，未配置 `WithEmbeddingEndpoint` 时调用返回新哨兵错误 `ErrNotSupported`，而不是发出注定失败的请求。
- **`ModelInfo.Type` 模型分类字段**：`chat` / `embedding` / `rerank`（`ModelType*` 常量），手动配置层与合并逻辑生效；OpenAI 系 `/models` 不声明类型，SDK 不猜测，需要时手动声明。
- **多模态输入扩展：`BlockAudio` / `BlockFile`**。音频（`AudioContent`/`UserAudio`，base64 + `wav`/`mp3`）与文档（`FileContent`/`UserFile`，base64、`data:` URL、http(s) URL；`FileRef` 引用已上传文件）作为新内容块进入统一消息模型。三协议映射（能力子集差异在请求构建期即报 `ErrInvalidRequest`，不支持一律显式报错、绝不静默丢弃）：OpenAI Chat → `input_audio` part / `file` part（`file_data`/`file_id`）；Responses → `input_file` part（`file_data`/`file_url`/`file_id`，**不支持音频**——官方输入类型联合仅 text/image/file）；Anthropic → `document` block（PDF 用 base64 source、`text/plain` 用官方 text source、http(s) PDF URL 用 url source、`FileRef` 用 file source，其他 MIME 拒绝；**不支持音频**）。媒体载荷做本地 base64/`data:` URL 语法校验；`FileData` 与 `FileID` 双来源报错。
- 上下文估算计入新块类型（每段音频 +500、每份文档 +3000 的保守平估）。

### 工程

- 辅助 API 族（embedding/rerank）的公共管线收敛在 `auxiliary.go`：endpoint/key 解析、Bearer 头、`RetryIdempotent` 的 JSON POST 助手；64 MiB 响应读取上限（批量向量远大于 chat 响应）。
- 新增测试：Embedding / Rerank 端到端（httptest + synctest，覆盖负载形状、aux 路由、凭证切换、usage 记账、错误路径、Anthropic 无 override 拦截）与三协议新块映射（含非法组合的构建期报错、data URL 直通、MimeType 缺省推导）。
- 新增示例 `examples/embedding`、`examples/rerank`；文档：基础指南新增「音频与文件输入的协议差异」「嵌入与重排」，模型体系新增 `Type` 说明。

### 修复（2026-09-13 审计）

- **流完整性**：SSE 事件 JSON 解析失败不再静默跳过（三协议一致）——畸形事件可能携带文本或工具调用增量，丢弃会静默损坏回答，现在直接以结构化错误终止流。
- **流截断检测**：流在收到 provider 终止事件（`message_stop` / `[DONE]` / `response.completed`）之前 EOF 时，仍会交付结束事件（StopOther、部分 usage），但随后 `Next()` 返回错误并以新哨兵 `ErrStreamTruncated` 匹配；部分结果保留在 `Stream.Partial()`。调用方从此能区分"干净结束"与"连接被掐断"。
- **流内错误补齐上下文**：SSE `error` / `response.failed` 事件构造的 `APIError` 现在携带 `Method` / `URL` / `RequestID`。
- **Registry 快照隔离**：`Aliases` 切片在安装与返回（`Lookup`/`List`）时均深拷贝，调用方无法绕过锁篡改注册表状态或制造数据竞争。
- **alias 冲突确定性处理**：同一 alias 映射到多个 canonical id（同层、跨层或遮蔽其他模型 id）一律报错，不再依赖 Go map 随机遍历顺序决定归属。
- **`Registry.SetManual` / `SetRemote` / `LoadFile` 返回 `error`**：拒绝非法状态而不是静默安装（见下方破坏性变更）。
- **`refreshModels` 错误传播**：并发刷新的等待者现在读取同一轮刷新的真实结果；`ModelInfo` 的未知模型远程发现改走 `refreshModels`，并发查询共享一次 `/models` 请求，不再各自触发。
- **响应体超限检测**：`httpx.ReadBody` 以 limit+1 字节探测溢出，超限报 `httpx.ErrBodyTooLarge` 而不是让截断的 JSON 冒充解码错误；chat（1 MiB）、models（8 MiB）、embedding/rerank（64 MiB）读取点全部接入，模型列表响应此前被忽略的读取错误现在也会上报。
- **Embedding 响应校验**：`data` 非空、向量数与输入数一致、index 在 `[0, len(Input))` 内且不重复、向量非空，违规一律报协议错误；usage 兼容 `input_tokens` 变体（此前只认 `prompt_tokens`）。
- **Rerank 响应校验**：`results` 非空、index 在 `[0, len(Documents))` 内且不重复、score 非有限值报错（防止调用方按 index 取文档越界 panic）。
- **Rerank usage 口径**：Cohere `meta.billed_units.search_units` 是计费单位不是 token，不再回填 `Usage.InputTokens`/`TotalTokens` 污染统计。
- **Embedding/Rerank 负值参数**：`Dimensions < 0`、`TopN < 0` 显式报 `ErrInvalidRequest`（此前被静默当作未设置）。
- **endpoint 拒绝 query**：`WithEndpoint` / `WithEmbeddingEndpoint` 含查询串时构建报错——query 可能藏有凭据，会原样泄入错误信息与重试日志。
- **`APIError.Raw` 敏感内容脱敏**：错误体存入 `Raw` 前做保守脱敏——敏感键（api_key/token/password/authorization/secret/credential 等）的字符串值替换为 `[redacted]`，OpenAI 风格的 `sk-…` 密钥材料在 JSON 与非 JSON 错误体（网关 HTML 等）中统一掩码；Raw 保持合法 JSON 且保留非敏感字段用于诊断。
- **Responses 协议兼容降级重试**：对齐 Chat 的 sticky probe——第三方网关以 400 + 关键词拒绝 `reasoning` 或 `max_output_tokens` 时，去掉该可选字段重发一次并按客户端记忆降级；非 invalid_request 错误与无关 400 不会触发降级（此前 Responses 请求遇可选字段被拒直接失败）。
- **Anthropic 认证头收敛**：默认只发送 `x-api-key`；需要 Bearer 的兼容网关用新选项 `WithAnthropicBearerAuth(true)` 显式开启，凭证不再默认复制到第二个认证通道。
- 文档同步：`PLAN.md` 待确认事项的 Go 最低版本定为 1.27+；`APIError.Retryable` 语义在指南中明确。

### 加固（2026-09-13 审计·第三阶段）

- **统一请求结构校验**：`ChatRequest.validate` 现在拒绝未知角色、无内容块的消息、块缺必填字段（图片 URL、音频数据/格式、文件数据、工具调用 ID/参数 JSON、工具结果 ID）、负的 `MaxOutputTokens`、越界的 `Temperature`/`TopP`、未知的 `Thinking.Effort` 与负的 `BudgetTokens`——明显非法的请求在本地即报 `ErrInvalidRequest`，不再等到 provider 侧以各不相同的 400 表现。
- **`Extra` 保留字段保护**：`Extra` 中与 SDK 管理的负载字段（`model`/`messages`/`stream`/输出上限/采样参数等，含 Embed/Rerank 负载）冲突的键默认报 `ErrInvalidRequest`，防止意外覆盖绕过校验；确有需要时用新选项 `WithExtraOverrides(true)` 显式放行。
- **Stream 并发加固**：`streamCore` 状态迁移与资源释放改为互斥保护——`Close` / `Err` / `Usage` / `Partial` 可安全地与阻塞中的 `Next` 并发（例如看门狗超时关闭流），release 恰好执行一次，不会双重释放；`Next` 仍须单 goroutine 驱动。
- **多媒体 token 估算可配置**：新选项 `WithMultimediaTokenEstimates`（Image/Audio/File，默认 1500/500/3000），strict 上下文校验在多媒体场景下的误判可通过调参缓解。
- **`ModelInfo.DisableThinking`**：布尔合并是 OR 语义，稀疏的手动条目此前无法撤销远端目录错误的 `SupportsThinking=true`；新字段是显式撤销开关，强制合并结果为不支持 thinking（models 文件同名 `disable_thinking`）。

### 破坏性变更（同上批修复）

- `Registry.SetManual` / `SetRemote` 签名由无返回值改为 `error`；直接调用这两个公开方法（或 `LoadFile`）的代码需要接住错误。
- endpoint（含 embedding endpoint）不允许携带 query 串；v0.3.1 的 `joinEndpoint` query 兼容随构建期拒绝一并失效。
- Anthropic 请求默认不再发送 `Authorization: Bearer`；受影响的兼容网关请设置 `WithAnthropicBearerAuth(true)`。
- 流式响应在 EOF 缺终止事件时 `Err()` 返回匹配 `ErrStreamTruncated` 的错误（此前返回 nil）。
- Embedding / Rerank 对空结果、数量不匹配、非法 index/score 的响应改为报错（此前按原样返回）。

### 修复（2026-09-13 第二轮审计，报告：docs/audit-2026-09-13.md）

针对审计报告的 13 项主要发现与边界项的逐条修复，新增 `audit_report_fixes_test.go` 回归测试（16 例）。

**流式与并发**

- **`Stream.Partial()` 数据竞争（P1）**：快照的内容克隆移入锁内——此前克隆发生在解锁后，与 `Next()` 对既有块的就地修改构成 Go 数据竞争。`Err` / `Usage` / `Partial` / `Close` 与 `Next` 并发现在是真实承诺（race 检测通过）。
- **用量回调死锁**：流结束回调（含用户 `UsageTracker.Record`）改在释放流互斥锁之后执行——tracker 同步读取 `Partial()` / `Err()` / `Usage()` 或调用 `Close()` 不再死锁；回调仍恰好触发一次。
- **真实 HTTP 断流识别**：三协议流结束判断除 `io.EOF` 外纳入 `io.ErrUnexpectedEOF`——chunked 响应在终止零块前被掐断（网关超时、连接中断）现在同样先交付带已知 usage 的结束事件、再以 `ErrStreamTruncated` 失败，此前直接返回裸 `unexpected EOF` 且丢失已收到的 usage。

**多模态协议正确性**

- **Responses 拒绝音频输入**：官方 Responses 输入类型联合只有 text/image/file，`input_audio` 是 Chat Completions 的形状；此前错误编码并发送。音频请改用 Chat Completions 客户端。
- **Responses 支持 `file_url`**：http(s) 文件 URL 现在映射为官方 `input_file.file_url`（此前误报为协议限制）。Chat Completions 仍拒绝 URL 文件（其官方形态确实只有内联数据/file_id）。
- **Anthropic 文本文件 source**：`text/plain` 内联文档按官方契约转成 `text` source（base64 解码后原文直传）；base64 source 保留给 PDF；其他 MIME 显式报错而非伪装成 PDF。
- **角色×块矩阵**：system/assistant 等角色不允许的块类型（如 system 里的文件/音频、assistant 里的图片）在请求校验期报 `ErrInvalidRequest`，不再被编码器静默丢弃——此前一条 system 消息携带的附件会无声消失。
- **文件来源唯一**：`FileData` 与 `FileID` 同时设置报错（此前静默取旧 `FileID`，更新内容不生效）。
- **媒体载荷本地校验**：音频/文件 base64 语法、`data:` URL 结构与空载荷在构建期报错。
- **空文本占位恢复**：媒体块旁的空文本块视为占位符跳过，修复"空文本+有效图片"请求被新校验误拒的回归。

**嵌入与重排校验**

- **缺失/`null` 字段拒绝**：embedding 的 index、向量元素与 rerank 的 index/score 用指针/原始 JSON 解码，字段缺失或为 `null` 报协议错误，不再静默变零值。
- **维度校验**：embedding 批内向量维度必须一致；显式传 `Dimensions` 时必须精确匹配（含 `WithExtraOverrides` 覆盖后的有效值）。返回数据按 Index 恢复输入顺序；rerank 结果按分数降序重排。
- **负数用量拒绝**：embedding/rerank 响应的负 token 计数报协议错误，不再污染用量统计。
- **rerank 兼容声明收敛**：文档明确 Voyage（`top_k`/`data[]`）与 DashScope（独立路径与嵌套结构）与 Cohere 线格式不兼容、当前未适配（官方文档核对结论）。
- **`WithExtraOverrides(true)` 下的响应校验**改按覆盖后的有效 `input` / `documents` 数量执行。

**其他**

- **`Extra` 保留键按 API 族划分**：chat 不再全局保留 `user` —— `ChatRequest.Extra["user"]`（终端用户标识）恢复可用；embeddings/rerank 各自保留自己的负载字段。
- **大错误体脱敏**：`APIError.Raw` 先脱敏后截断——超过 4KiB 的 JSON 错误体此前被先截断成字符串、结构化脱敏失效，敏感键值可能泄露进日志。
- **reasoning 值拒绝不再降级**：上游 400 拒绝的是 `reasoning` 字段的**取值**（如某模型不支持 `low`）时原样报错；只有字段级拒绝才触发降级，且 Responses 的降级记忆按模型隔离（不同模型互不污染）。
- **`ModelInfo.DisableThinking` 单条目生效**：仅手动层一条记录同时声明二者时也强制 `SupportsThinking=false`，不再依赖跨层合并才生效。
- **端点错误不回显 query**：携带 query 的非法端点在构建错误中剥离 query 后输出，误贴进 URL 的凭据不再回显到日志。

## v0.3.1 (2026-09-08)

2026-09-08 审计报告的逐条修复。

### 破坏性变更

- **删除 `ThinkingConfig.IncludeThoughts`**：该字段自引入起从未被任何协议适配器读取（三个协议均无对应 wire 字段），属于无实现死 API。请求思考内容仍由各协议默认返回；需要显式控制时通过 `Extra` 透传厂商字段。

### 修复

- **OpenAI Chat 不再丢弃同一消息中的多个工具结果**：一条 `RoleTool` 消息携带多个 `BlockToolResult`（并行工具调用）时，此前只有第一个结果被发送，其余静默丢失，模型会基于不完整上下文作答。现在每个结果各发一条 `role:"tool"` 消息。
- **`WithModelsFile` 与 `WithModelInfo` 混用不再互相覆盖**：此前文件配置被后续的 `SetManual` 整体替换，文件中的模型全部丢失。现在两者合并进同一手动层，显式 `WithModelInfo` 条目在 id 冲突时优先。
- **`ChatResponse.Raw` / `APIError.Raw` 保证是合法 JSON**：截断到 4KB 的响应体与非 JSON 错误体（HTML 网关页等）此前会破坏 `json.Marshal`（日志/遥测路径报错），现在降级为 JSON 字符串存储。
- **`Stream.Partial()` / `Collect()` 返回时点快照**：此前浅拷贝与流内部共享 `Content` 切片，保存的中间快照会被后续事件改写；现在深拷贝。
- **`ModelInfo.MaxOutputTokens` 声明生效**：输出上限回退链改为 请求值 → 模型元数据 → 客户端默认，注册表中声明的上限不再被忽略。
- **`joinEndpoint` 正确处理带 query 的 base URL**（此前 `/v1` 插入与路径拼接会落到 query 之后，拼出畸形 URL）。
- **`DetectClient` 的探测尊重 `WithHTTPClient` 与 `WithTimeout`**：此前探测用自带 10s 超时的裸 client，自定义传输与超时对探测无效。

### 工程

- 修复文档与注释中残留的"内置知识库"描述（知识库已于 v0.2.0 移除，现为手动 + 远程两级）。
- 重试状态码判定收敛到 `httpx.RetryableStatus` 单点，错误类型与重试决策不会再漂移。
- `estimateInputTokens` 计入工具定义（每工具 +24 与 name/description/schema 文本），上下文告警不再低估工具型请求。
- 新增 Responses 适配器端到端测试（`Chat`/`StreamChat`/`ListModels` 此前为零覆盖）；主包语句覆盖提升至 86%，jsonx 达 86.6%（75% 目标达成）。
- `.gitattributes` 固定 `*.go` 为 LF 行尾，Windows 检出不再破坏 `gofmt -l`；CI 新增 Format 检查步骤与覆盖率门禁。
- **jsonx 契约澄清**：删除从未被调用的 v1 `UnmarshalJSON` 兼容入口（v2 后端直接调用 `UnmarshalJSONFrom`），包注释明确依赖默认 jsonv2 构建模式、不支持 `GOEXPERIMENT=nojsonv2`（此前注释承诺的 v1-only 兼容并不成立，该模式下无法编译）。
- **decode 热路径移除冗余校验**：`truncateBody` 拆分为已知合法 JSON 的 `truncateBody`（零额外开销）与错误体专用的 `safeTruncateBody`（含有效性检查）。v0.3.1 初版为修复 `Raw` 序列化在每条成功响应解码上多跑一次 `json.Valid` 全量扫描，实测使响应解码慢约 15%；拆分后恢复至 v0.3.0 水平（本机 4430 vs 4488 ns/op）。
- 补齐改造计划要求的回归测试：Responses 终止信号纪律、`truncateBody` UTF-8 边界、`DetectClient` 调用方切片不被污染、`streamCore` closer 恰好调用一次、SSE 超长行（1 MiB）。
- 基准对比（本机 go1.27.0 / Windows amd64 / 12 核，`go test -bench . -benchmem`，同一份 bench 检出 v0.2.0 上运行；v0.2.0 为 go 1.22 时代，无测试文件，仅新增 bench）：

  | Benchmark | v0.2.0 | v0.3.1 | 差异 |
  |---|---|---|---|
  | OAChunkDecode | 1810 ns/op · 196 B · 3 allocs | 1390 ns/op · 208 B · 3 allocs | **-23% 耗时** |
  | OAChunkDecodeFlexFields | 2915 ns/op · 336 B · 7 allocs | 2210 ns/op · 320 B · 6 allocs | **-24% 耗时 · -1 alloc** |
  | OpenAIResponseDecode | 5570 ns/op · 1667 B · 15 allocs | 4440 ns/op · 1665 B · 16 allocs | **-20% 耗时** |

## v0.3.0 (2026-09-06)

Go 1.27 现代化 + 正确性加固。详见 [REFACTOR_PLAN.md](REFACTOR_PLAN.md)。

### 破坏性变更

- **go.mod 升至 `go 1.27`**：消费者需要 Go 1.27+ 工具链（`GOTOOLCHAIN=auto` 默认会自动拉取）。

### 修复

- **Anthropic 流式 error 事件缺 error 字段时不再 panic**（第三方网关可触发）：返回通用 `APIError`；`streamCore` 对 `(nil, nil)` 事件做防御，视为流错误。
- **`jsonx.FlexInt64` 兑现包契约**：非数字字符串（如 `"N/A"`）降级为零值，不再使整个响应解码失败或丢弃整个流 chunk。
- **`Retry-After` 设 60s 硬上限**：异常网关无法再让重试挂起数小时。
- **Anthropic `ListModels` 处理分页**（`has_more`/`after_id`），此前只返回第一页（默认 20 条）。
- **`Client.ModelInfo` 的远端发现应用 `WithTimeout`**，与 `ListModels` 一致。
- **`EstimateTokens` 改为向上取整**：3 个 ASCII 字符计 1 token，不再低估上下文占用。
- **流终止信号后不再产出后续事件**：三个协议的 `streamEvents` 在 `[DONE]`/`message_stop`/`response.completed` 之后直接结束。
- **`APIError.Raw` 截断保证合法 UTF-8**。
- **Anthropic 首条消息校验后移**：blocks 全空的 user 消息被过滤后再检查"首条必须为 user"，避免必然 400 的 payload。
- **`WithMaxTokensField` 显式 pin 现在优先于探测学到的 sticky 状态**，且不会被 sanitize 翻转（符合"bypassing probe/fallback"的文档语义）。
- **sanitize 降级判定收紧**：error `type` 存在且不是 invalid-request 类时不降级，减少关键词误匹配导致的永久粘性翻转。
- **`DetectClient` 不再向调用方切片底层数组写入**。
- **`httpx` 退避 Cap 成为硬上限**（此前 jitter 可超出 20%）。
- **SSE 孤儿事件字段不泄漏**：无 data 的被丢弃事件不再把 `id`/`event` 带给下一个事件。
- **流式响应 body 在流终止时显式关闭**（提前 `Close` 与自然结束均生效），连接可正常复用。

### 性能

- Go 1.27 的 `encoding/json`（v1 API）已由 json/v2 后端实现：SSE chunk 解码与响应解码显著提速、分配减少（基准见 `bench_test.go`）。
- `io.ReadAll` 提速、HTTP/1 响应体自动 drain、Green Tea GC 等工具链收益自动生效。

### 测试与工程

- **补齐全套单元测试**（此前为零）：SSE 解析器、jsonx 容错解码、三个协议适配器（payload 编码/错误解析/流事件）、`streamCore`、`httpx` 重试与退避、registry、estimator、usage tracker，全部通过 `testing/synctest` + `httptest.NewTestServer`（Go 1.27 内存假网络）实现零真实等待的确定性测试。
- **CI 增加 `go test ./... -race` 与覆盖率输出**。
- 内部代码现代化：`go fix` modernizers（`maps.Copy`、`bytes/strings.Cut`、`new(expr)` + `//go:fix inline`）、`math/rand/v2`。

### 文档

- README/guide 更新 Go 1.27 要求、`new(expr)` 与 `errors.AsType` 用法。
