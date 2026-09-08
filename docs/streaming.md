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

`Stream` 不是并发安全的，单 goroutine 驱动。

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
}
```

只想要最终结果不关心过程时，`Collect` 一步到位：

```go
resp, err := stream.Collect()
```

## 中断与收尾语义

- **连接中断 / 服务端中途断开**：`Next()` 返回 false，`Err()` 非 nil（`*APIError` 或 `*TransportError`），`Partial()` 保留已收到的内容（时点快照，深拷贝）——长回答场景可以降级展示半截结果。
- **服务端没发终止事件就关闭连接**：许多 OpenAI 兼容服务不发 `[DONE]`、不发 finish_reason。SDK 容忍这种情况并合成结束事件，但 `StopReason` 会是 `StopOther` 而非 `StopEnd`，调用方可据此识别非正常收尾。流的 input/output usage 因此可能不完整（取决于服务端发到了哪一步）。
- **主动放弃**：`Close()` 取消底层请求上下文、释放连接；调用方 ctx 取消同样会中断流（`ChatStream` 的流生命周期由调用方 ctx 约束，`WithTimeout` 不作用于流）。
- **用量记账**：流终止时（无论正常、出错还是中途 Close）记录一次，见[用量统计](usage-stats.md)。

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
