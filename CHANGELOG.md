# 更新日志

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
