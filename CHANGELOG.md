# 更新日志

## 未发布

面向网关/多凭据调用方的三项错误可观测性增强。均为**新增字段与方法**，不改 wire 行为；
按 v1.0.0 冻结契约，`APIError`/`Stream` 成员扩充属 minor。

### 新增

- **`APIError.RetryAfter`**：透传上游 `Retry-After` 响应头（秒数或 HTTP-date，
  与传输层退避同一套 `httpx.retryAfter` 解析，同样受 60s 上限；无该头时为 0）。

  **为什么**：该头此前只被 SDK 自身消费——完整解析与上限都实现了，却只写进 debug
  日志，调用方拿到的 429 上**看不到上游要求的等待时长**。这对网关是真实缺口：凭据
  冷却只能拍一个固定值，上游说 5 秒会白扔容量，说 300 秒则会提前叫醒它继续吃 429；
  向下游透传 `Retry-After` 也无从谈起。

  ```go
  var apiErr *rosetta.APIError
  if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
      // 按上游给的时长安排重试/冷却，而非猜一个值
  }
  ```

  覆盖三协议 chat（流式与非流式）、embeddings/rerank 与 `/models` 目录的全部非 2xx
  出口。`InBand`（HTTP 200 信内错误）不携带——那类错误的 HTTP 交换已成功，没有等待
  语义。

- **`Stream.Abort(cause)` 与 `ErrStreamIdleTimeout` / `ErrStreamAborted`**：让流的
  中断原因可留痕。

  **为什么**：`Close()` 的语义是「调用方主动走开」——`Err()` 保持 `nil`、不计入
  `UsageMissing`，这是对的；但看门狗因**上游静默**掐断流时，调用方只能调 `Close()`，
  于是「我掐的」与「上游断了」在结果里**无法区分**，被迫各自发明留痕（如网关侧用
  `atomic.Bool` 旁路记录）。`Abort` 补上另一半：释放同样的资源，但把 `cause` 记进
  `Err()`，并照常触发用量回调。

  与 `Close` 一样并发安全、可与阻塞中的 `Next` 竞争、恰好一次收尾；若流已正常结束或
  已因自身错误结束，**迟到的 `Abort` 是空操作**——不会把干净结束误报成超时。
  `Abort(nil)` 退化为 `ErrStreamAborted`。

  SDK 仍**不内建**空闲计时器（`WithTimeout` 依旧只约束非流式调用）：不同上游合理静默
  上限差异很大，计时策略属于调用方。官方范式见 `docs/streaming.md` 的「看门狗」一节。

- **`APIError.Category`（`ErrorCategory`）与 `AffectsModel`**：错误归因提示，免去每个
  调用方重新解析 provider 错误体。

  `CatUnclassified`（零值）/ `CatOutOfCredit` / `CatRateLimited` / `CatQuotaExhausted` /
  `CatModelUnavailable` / `CatContentRefused` / `CatAuthFailed`，外加厂商点名模型时的
  `AffectsModel`。三处对网关尤其有用：

  - `CatContentRefused`——内容被安全策略拒绝，**凭据无辜**。换一把 key 重发同样的内容
    照样被拒；误当限流会导致无意义的全凭据轮询。
  - `CatOutOfCredit` 与 `CatQuotaExhausted` 分开——「没钱」要充值、「窗口耗尽」等重置，
    处置完全不同。
  - `CatModelUnavailable` + `AffectsModel`——可只下架**该模型**而不连坐整把 key。

  **刻意不匹配错误消息里的自由文本关键词**：那是没有边界、随厂商改写而漂移的表面。
  判定只用**状态码与结构化的 `code`/`type`**，外加 Anthropic 官方文档化的重置时间措辞
  （用于区分其 429 的「太快」与「窗口耗尽」）。需要更细的判断请自行基于 `Raw` 叠加。

  它是**提示而非判据**：`CatUnclassified` 意为「无已知信号命中」，不是「以上皆非」，
  必须与 `StatusCode` 叠加使用。

### 测试

`error_affordance_test.go` 新增。除正向用例外，**每项都配了反向断言**（延续
`upstream_malformed_test.go` 的纪律：过宽的提示比没有提示更糟，因为调用方要拿它做
真实决策）——已结束的流不得被迟到的 `Abort` 改写、prose 里的 "quota" 字样不得变成
欠费信号、未点名模型的 404 不得变成模型级信号、每个错误出口（含无法解析的 body）
都必须拿到分类。

### 质量基线

`go test ./... -race` 全绿；`go vet`、`gofmt` 无告警；新增字段后既有测试无需修改即通过。

## v1.0.1 (2026-10-08)

修复批次：让「上游响应体解不开」成为**可分类**的错误。纯修复，不含任何 wire 行为变更。

### 新增

- **哨兵错误 `ErrUpstreamMalformed`**：上游回了成功（或 200 等价）状态，但响应体**无法被解码成协议要求的形状**时返回——截断的响应体、中间代理返回的 HTML 错误页、半截 JSON 文档。

  **为什么它必须是独立的哨兵**：归因方向决定调用方的动作，而这里的两个方向相反：

  - `ErrInvalidRequest` = **调用方**构造的东西被拒 → 原样重试必然同样失败，归咎上游会掩盖调用方的 bug。
  - `ErrUpstreamMalformed` = **上游**给了一份没法用的答案，而请求本身没问题 → 调用方唯一有用的动作是换上游（故障转移）、退避、或上报。

  修复前这些失败被裸 `fmt.Errorf` 包装，既不是哨兵也不是 `*APIError` / `*TransportError`。真实网关的分类器三段全不命中，落到兜底的 "internal gateway error"：回 **500** 且**不可转移**——一个「接受请求、返回垃圾」的上游永远换不掉，而排障方向被指向网关自己。已在真实进程上复现确认。

  ```go
  _, err := client.Chat(ctx, req)
  if errors.Is(err, rosetta.ErrUpstreamMalformed) {
      // 上游给了读不懂的响应 → 换目标/退避；不是"你的请求有问题"
  }
  ```

  **覆盖范围**（一元响应体解码，三协议 + 两个辅助 API + `/models` 目录）：
  `decodeOpenAIChatResponse` / `decodeAnthropicResponse` / `decodeResponsesResponse` /
  `embedding` / `rerank` / `/models` 目录解码（OpenAI 兼容与 Anthropic 两条路径），
  以及目录缺 `data` 字段这一**结构性**缺陷（与"解不开"同类——必需字段缺失与解析失败
  对调用方而言没有区别）。

  **刻意不含流式事件解析。** 这是本节最容易"顺手统一"的地方，而那会引入正确性 bug：
  一元解不开时调用方还没有任何内容，换上游重试是安全且正确的；流式事件解不开时
  **先前的事件可能已交付**，此时消费者最自然的映射
  `errors.Is(ErrUpstreamMalformed) → 换上游重试` 会**重放已交付的输出并可能重复计费**。
  一个哨兵无法同时表达"可安全重试"与"不可重试"——安全性取决于调用方走到了哪一步，
  而错误本身不知道。所以流式保持无类型，由调用方按自己的 committed 状态决定
  （rosetta-gateway 的 `TestE2E_C3` 正是这样钉住"首片之后掐断不得触碰健康次目标"）。
  三处流式解码点（`provider_openai_chat.go` / `provider_anthropic.go` /
  `provider_openai_responses.go`）均已就地注明该决定。

  **不泄漏响应体**：错误消息只报"解不开"，不报"解不开的是什么"。畸形体可能是
  含内部主机名的 HTML 错误页，也可能是把调用方 key 回显进去的半截 JSON，
  而错误消息会进日志与遥测。原始 `encoding/json` 错误以 `%w` 保留，
  `errors.As` 仍可拿到 `*json.SyntaxError`（含字节 Offset，单条最有用的诊断）。
  实测确认 json 错误只携带偏移量、字段名与**类型名**，从不携带出错的值本身。

### 修复

- 一元响应体解码失败现在**可用 `errors.Is(err, ErrUpstreamMalformed)` 分类**（见上）。
- 解开之后仍不合要求的消息不再被误报成响应体无法解码：embeddings 的"向量元素不是数字"、
  "维度不符"，rerank 的"结果为空"属于**内容**问题而非**结构**问题，保持无类型——
  否则 `errors.Is(ErrUpstreamMalformed)` 作为故障转移信号会因语义拉伸而失真。

### 测试

- 新增 `upstream_malformed_test.go`。**此前这一档错误类型零覆盖**——已有错误测试
  全部从 `*APIError` / `*TransportError` 出发，正好绕过了那个兜底分支，
  这正是它没被测出来的直接原因。新测试覆盖：
  - 每个改动点命中哨兵（三协议 × HTML/半截JSON/空body 分档，逐一对应独立解码函数，
    漏改任一处即红）；辅助 API 与 `/models` 目录（含 `data:null` 结构性缺陷）；
  - **反向**：`500`/`429`/`401`/`InBand(200)`/`*TransportError`/`ErrInvalidRequest`/
    两个流哨兵/普通错误**均不得**命中（哨兵过宽会把整个错误分类体系压平，比不加更糟）；
    并有一条走真实 HTTP 状态码的端到端版，证明解出来的 `*APIError` 未被套上哨兵；
  - `errors.Is` 穿透 0~2 层 `%w` 包装与 `errors.Join`，并钉住 `%v` 会断链这一语义；
  - 流式路径**必须不命中**（三协议分档），外加一元 vs 流式的交叉验证，
    防止有人"统一一下"把两边改成一样；
  - 错误消息点明"上游"而非指向调用方；畸形响应体的内容不泄漏进消息。

  两处断言都验证过"有牙"：把 anthropic 一元解码改回未打哨兵 → 对应子测试红、
  其余仍绿；给流式解码加上哨兵 → 流式反向子测试红、其余仍绿。

### 质量基线

主包语句覆盖率 89.7%（v1.0.0 为 89.3%）；`go test ./... -race` 全绿；
`go vet` / `staticcheck v0.8.1` / `gofmt` 无告警。

## v1.0.0 (2026-10-05)

首个稳定版本，公共 API 自本版起冻结。**相对 v0.6.0 无任何代码或行为变更**——`rosetta.Version` 由 `"0.6.0"` 改为 `"1.0.0"`，变更日志仅新增本条目；本版的意义是宣告 semver 稳定契约生效。

自 v0.1.0 以来，本 SDK 历经六轮审计与加固（前缀、幂等、流量、并发）、五轮审计报告逐条修复、多个官方特性对齐（交错思考、扩展缓存、Responses 归并、refusal 映射）、多模态输入扩展与 Embeddings / Rerank 辅助 API 的落地，最终收敛到稳定接口。本版起：

- **1.0.0 之后**：破坏性变更（移除/改名公开符号、改变 wire 行为）必须走新 major 版本；新增能力走 minor，纯修复走 patch。0.x 时期"minor 也允许破坏"的宽松不再适用。
- **既有调用方**：从任意 `v0.6.0` 升级到 `v1.0.0` 无需改动代码。
- **语义说明**：`go get` 会解析到本版；`pkg.go.dev` 徽章展示的即本版。

### 冻结时点记录（供后续 major 参考）

公开 API 的契约集中体现在：`Client`（`NewClient` / `DetectClient` 与 `Chat` / `StreamChat` / `ListModels` / `ModelInfo` / `Embed` / `Rerank` / `Stats`）、`protocols` 常量、`ChatRequest` / `EmbeddingRequest` / `RerankRequest` 与消息块类型、`Options`（`With*`）、哨兵错误集（`Err*`）与 `APIError` / `TransportError` 字段、`Quirks` 逃生开关、`Usage` 与 `UsageTracker` 口径、`EstimateTokens` 语义。后续若改动上述任一项，按 semver 一律视为破坏性。

### 质量基线（冻结时点）

- 主包语句覆盖率 89.3%，`internal/*` 各包 80%+；CI 门禁 75%。
- `go test ./... -race` 全绿；`go vet` / `staticcheck` 无告警。
- 三协议（OpenAI Chat / Responses / Anthropic）端到端测试均为确定性实现（`synctest` + `httptest`），无真实网络等待。
- 零第三方依赖；要求 Go 1.27+。

## v0.6.0 (2026-10-01)

第五轮审计（正确性）+ 性能优化调研的落地批次。**含调用方可见的行为变更**（见下节）——按 semver 视为 minor。

### 行为变更（升级前必读）

- **一元调用默认超时 60s**。`Chat` / `ListModels` / `Embed` / `Rerank` 在未显式设置 `WithTimeout` 时，现在有 60s 的默认总超时；底层传输层另加了 30s 的 `ResponseHeaderTimeout`（服务端"已建连但不回响应头"不再无限挂起）。`WithTimeout(0)` 关闭该默认上界。流式仍由调用方 ctx 管辖、不受影响。
- **chat 现在自动发送 `Idempotency-Key` 并对 429/503 做传输层退避重试**。此前 chat POST 的 429/503 直接透传给调用方；现在会带上幂等键并按退避重试（默认最多 2 次）。对官方 OpenAI/Anthropic 端点更健壮；对不识别该头或绝不能重放的第三方网关，用 `WithQuirks(Quirks{NoIdempotencyKey: true})` 恢复旧行为（不发该头、429/503 立即报错）。
- **OpenAI Chat 在 reasoning 请求上不再发送 `temperature`/`top_p`**（修复 G1）。官方 reasoning 模型（o 系列、gpt-5 系）会因这两个参数返回 400；现在只要 `reasoning_effort` 上 wire，采样参数即被丢弃（与 Responses/Anthropic 适配器对齐）。
- **上下文 token 估算改为整体一次取整**（修复 M8）。此前逐块向上取整后累加，大量短文本块时系统性高估；现在按全部文本的 ASCII/非 ASCII 计数一次取整，估算整体略降、更接近真实。依赖严格上下文告警阈值的调用方可能看到告警减少。
- **模型别名上 wire 前规范化。** 请求发出前把注册表别名解析为规范 ID 再发送（未知模型原样透传，不触发 `/models` 探测）；用量记账的 `UsageRecord.Model` / `Stats().ByModel` 同步按规范 ID 归并。直接检查 wire 或日志的调用方会看到 `model` 字段变为规范名；别名与全名混用的调用方，上游前缀缓存与用量统计不再把同一模型拆成两个 key。
- **默认 Transport 不再继承环境代理。** 自建的 `*http.Transport` 未设置 `Proxy`，而此前的 `http.DefaultTransport` 会读 `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`。默认现在直连——内网域名与 `http://` 端点的 API key 不再流经代理；需要环境代理的调用方显式传 `WithHTTPClient(&http.Client{Transport: http.DefaultTransport})`（或自带 `Proxy: http.ProxyFromEnvironment` 的 Transport）。

### 新增

- **`Quirks.NoIdempotencyKey`**：见上。
- **自建默认 `*http.Transport`**（`MaxIdleConnsPerHost=64`、显式 h2），替代进程级共享的 `http.DefaultTransport`（其 `MaxIdleConnsPerHost=2`）。高并发下连接复用更好、尾延迟更低；`WithHTTPClient` 仍可完全覆盖。

### 修复

- **G2**：Anthropic system 段的多断点不再被静默折叠成一个，每个带 `CacheControl` 的 text 块各自成段。
- **G3 / N1**：重试日志与协议探测失败日志不再裸打 URL，路径内嵌凭据会被脱敏；凭据正则收敛到 `internal/httpx` 单一出处，避免两处漂移。
- **重试睡眠与 deadline 交互**：退避期间调用方 deadline 到期时，返回的最后一个响应 body 不再是被关闭的（修复 M4 引入的闭包 body 回归）。
- **sticky 降级锁**改为 RWMutex；**DetectClient** 不再重复构建 settings；**Anthropic payload 单趟扫描**替代三次整树遍历；SSE 读行改 `ReadSlice` 并复用事件缓冲；`Partial()` 内容缓存；Embed/Rerank 的 wire 计数仅在 `Extra` 实际覆盖字段时执行；base64 校验改字符集+长度判定（大载荷不再整份解码）；`Extra` 序列化在 `validate()` 与估算间复用；被重试丢弃的响应 drain 上限 8KiB → 64KiB。均为内部性能项，对外行为不变。
- **跨主机重定向拒绝改为永久错误**：不再先 sleep 完退避序列（约 1.2s）才失败，`Do` 立即返回。
- **G4**：`data:` URL 校验层与编码层一致——校验层现在也只接受 `;base64` 形态，URL 形式的图片/文件在 `validate()` 阶段即以一致口径报错，不再等 Anthropic 编码时给出误导性错误。
- **G5**：一元响应里 refusal 与 content 并存时不再丢弃 refusal（与流式路径对齐）。
- **G6**：只有 thinking / redacted-thinking 块的 assistant 回放不再退化成空 content 消息或静默消失，而是在编码阶段显式报 `ErrInvalidRequest` 并说明原因（不可回放/缺签名）。
- **G7**：未知模型的负缓存（30s TTL）——串行批量校验同一未知 id 不再对 `/models` 打出 1:1 放大流量。
- **G8**：`StopRefusal` 映射对齐——OpenAI chat 的 `finish_reason:"refusal"` 与 Responses 的 `incomplete_details.reason:"refusal"` 现在都映射到 `StopRefusal`（此前落到 `StopOther`）。
- **G9**：调用方主动 `Close()` 的流不再被计入 `UsageMissing`，与"provider 未报 usage"区分开，统计口径更准。
- **G10**：`ErrNoEndpoint` 保留为防御性断言并加注释说明其不可达性（`buildSettings` 对已知协议必然填默认端点）。
- **G11**：in-band 错误（200 body / 流事件内的 `error`）新增 `APIError.InBand` 标记，同时保留 `StatusCode=200`（传输层确实成功）；按 `StatusCode >= 400` 判定失败的调用方应同时检查 `InBand`。

### 工程

- **清理审计工作产物**：历轮审计报告与证据文件（复现脚本、race 证据、测试基线）移出仓库，各轮修复的结论已并入本更新日志的对应版本条目；仓库只保留面向使用者的文档（基础指南、流式、模型体系、协议与兼容、用量统计）。

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
- 文档同步：Go 最低版本定为 1.27+；`APIError.Retryable` 语义在指南中明确。

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

### 修复（2026-09-13 第二轮审计）

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

Go 1.27 现代化 + 正确性加固。

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
