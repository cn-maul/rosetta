# 流式响应

三种协议的类型化事件流（SSE）归一为一套拉取式迭代器。事件形状与协议无关，切换供应商不改消费代码。

## 基本用法

```go
stream, err := client.ChatStream(ctx, &rosetta.ChatRequest{
	Model:    "deepseek-reasoner",
	Messages: []rosetta.Message{rosetta.User("写一首七绝")},
})
if err != nil {
	return err
}
defer stream.Close()

for stream.Next() {
	switch ev := stream.Event(); ev.Type {
	case rosetta.EventThinkingDelta:
		fmt.Print(ev.Text) // 思考过程
	case rosetta.EventTextDelta:
		fmt.Print(ev.Text) // 正文
	}
}
if err := stream.Err(); err != nil {
	return err
}
```

`Next()` 必须由单个 goroutine 驱动；`Err()` / `Usage()` / `Partial()` / `Close()` 可以与其他 goroutine 并发调用（例如看门狗超时关闭流）。`Event()` 返回的当前事件仅在 `Next()` 返回 true 后由驱动 goroutine 读取，不在并发安全承诺内。与阻塞中的 `Next` 竞态发生的 `Close` 会丢弃竞态产生的事件并释放资源；流终止（正常、出错、Close）时收尾与用量回调恰好执行一次。

## 事件模型

| EventType | 有效字段 | 含义 |
|---|---|---|
| `EventMessageStart` | `ID`、`Model` | 首个事件，响应身份 |
| `EventTextDelta` | `Text` | 正文增量 |
| `EventThinkingDelta` | `Text`、`Signature` | 思考增量；`Signature` 为 Anthropic 签名，多轮回放时随 thinking 块原样回传 |
| `EventToolCall` | `ToolIndex`、`ToolID`、`ToolName`、`ArgumentsDelta` | 工具调用片段，按 `ToolIndex` 分组，`ArgumentsDelta` 依次拼接成完整 JSON |
| `EventMessageEnd` | `StopReason`、`Usage` | 结束事件，携带终止原因与用量 |

协议原始事件到统一事件的映射见[协议与兼容](protocols.md#流式事件映射)。

## 接口

```go
type Stream interface {
	Next() bool                 // 推进到下一事件；false = 结束（正常或出错，查 Err）
	Event() *Event              // 当前事件，仅 Next() == true 时有效
	Err() error                 // nil = 干净结束
	Usage() Usage               // 结束事件携带的用量
	Partial() *ChatResponse     // 时点快照：已累积的响应（深拷贝，可安全持有，不受后续事件影响）
	Collect() (*ChatResponse, error) // 拖完剩余事件并返回完整响应
	Close() error               // 释放连接；干净结束后调用是无害 no-op
	Abort(cause error)          // 以 cause 中断；Err() 之后匹配 cause（看门狗超时用）
}
```

`Next` 必须由单 goroutine 驱动；`Err` / `Usage` / `Partial` / `Close` / `Abort` 与之并发安全（例如看门狗超时中断流、旁路 goroutine 读取 `Partial`），快照与并发关闭都有 race 检测覆盖。

只想要最终结果不关心过程时，`Collect` 一步到位：

```go
resp, err := stream.Collect()
```

## 看门狗：流空闲超时

`WithTimeout` **只作用于非流式调用**；已建连的流没有字节间空闲上限，流生命周期只由调用方的 ctx 约束。因此空闲超时看门狗由调用方持有——SDK 提供的是把它做对所需的零件：

```go
const idle = 30 * time.Second

stream, err := client.ChatStream(ctx, req)
if err != nil {
	return err
}
defer stream.Close()

// 超时即中断，并把原因留在 Err() 里
watch := time.AfterFunc(idle, func() { stream.Abort(rosetta.ErrStreamIdleTimeout) })
defer watch.Stop()

for stream.Next() {
	watch.Reset(idle) // 每收到一个事件就续期
	switch ev := stream.Event(); ev.Type {
	case rosetta.EventTextDelta:
		io.WriteString(w, ev.Text)
	}
}
if errors.Is(stream.Err(), rosetta.ErrStreamIdleTimeout) {
	// 上游静默过久；已收到的内容仍在 stream.Partial() 里
}
```

**`Abort` 与 `Close` 的区别是刻意的**：

| | `Close()` | `Abort(cause)` |
|---|---|---|
| 语义 | 调用方主动走开（放弃这次请求） | 因 `cause` 而死 |
| `Err()` | 保持 `nil` | 匹配 `cause` |
| 用量记账 | 不计入 `UsageMissing`（主动放弃 ≠ 上游没报用量） | 照常触发，携带 `cause` |
| `Partial()` | 保留已收内容 | 保留已收内容 |

两者都可从任意 goroutine 调用（包括 `time.AfterFunc` 回调与阻塞中的 `Next` 竞争），且都是**恰好一次**收尾。若流已正常结束、或已因自身错误结束，迟到的 `Abort` 是**空操作**——不会覆盖真实结果，也不会把一次干净结束报成超时。`Abort(nil)` 退化为 `ErrStreamAborted`。

## 中断与收尾语义

- **连接中断 / 服务端中途断开**：`Next()` 返回 false，`Err()` 非 nil（`*APIError` 或 `*TransportError`），`Partial()` 保留已收到的内容（时点快照，深拷贝）——长回答场景可以降级展示半截结果。流内 `error` / `response.failed` 事件构造的 `APIError` 带 `InBand=true`、`StatusCode=200`（传输层确实成功），按状态码过滤失败的调用方应同时检查 `InBand`。
- **服务端没发终止事件就关闭连接（流截断）**：无论 EOF 还是真实 HTTP 断流（`unexpected EOF`，chunked 响应在终止零块前被掐断），SDK 都会先合成结束事件（交付已收到的 usage，`StopReason` 为 `StopOther`），随后 `Err()` 返回匹配 `ErrStreamTruncated` 的错误。调用方从此能区分"干净结束"（`Err() == nil`）与"连接被掐断"（`errors.Is(err, rosetta.ErrStreamTruncated)`），而不是把截断误当成功。
- **主动放弃**：`Close()` 取消底层请求上下文、释放连接；调用方 ctx 取消同样会中断流（`ChatStream` 的流生命周期由调用方 ctx 约束，`WithTimeout` 不作用于流）。
- **用量记账**：流终止时（无论正常、出错还是中途 Close）记录一次，见[用量统计](usage-stats.md)。记账回调在流锁之外执行——自定义 tracker 可以安全地同步读取 `Partial()` / `Err()` / `Usage()` 甚至 `Close()` 流，不会死锁。调用方主动 `Close()` 的流不计入 `UsageMissing`（那是主动放弃，不是"provider 未报用量"）。

## 工具调用流

`EventToolCall` 是增量片段而非完整调用；`Partial()` / `Collect()` 里的 `ToolCalls()` 已经替你按 `ToolIndex` 分组拼接：

```go
for stream.Next() {
	if ev := stream.Event(); ev.Type == rosetta.EventToolCall {
		fmt.Printf("fragment: tool[%d] %q\n", ev.ToolIndex, ev.ArgumentsDelta)
	}
}
resp, _ := stream.Collect()
for _, call := range resp.ToolCalls() {
	// call.ToolCallID / call.ToolName / call.Arguments 已是完整 JSON
}
```

并行多工具调用由 `ToolIndex` 区分，无需自己处理交错片段。
