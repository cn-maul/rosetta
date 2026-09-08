# 更新日志

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
