# 用量统计

统一的 token 记账：请求级 `Usage` 归一 + 可插拔的 `UsageTracker` 汇总查询。

## Usage 字段

| 字段 | Chat 来源 | Responses 来源 | Anthropic 来源 |
|---|---|---|---|
| `InputTokens` | `prompt_tokens` | `input_tokens` | `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens` |
| `OutputTokens` | `completion_tokens` | `output_tokens` | `output_tokens` |
| `TotalTokens` | `total_tokens`（缺省时求和） | `total_tokens`（缺省时求和） | input+output 求和（input 含缓存读/写） |
| `CachedInputTokens` | `prompt_tokens_details.cached_tokens` | `input_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| `CachedCreationTokens` | —（自动缓存，不单独回报写入量） | — | `cache_creation_input_tokens` |
| `ReasoningTokens` | `completion_tokens_details.reasoning_tokens` | `output_tokens_details.reasoning_tokens` | — |

`Usage.IsZero()` 判断上游是否完全没有回报用量；任一维度有值（**包括缓存与思考**）都算"报过了"。

## 记账

```go
client, _ := rosetta.NewClient(
	rosetta.WithEndpoint(url), rosetta.WithAPIKey(key),
	rosetta.WithUsageTracker(rosetta.NewMemoryUsageTracker()), // 不传则完全不记账
)
```

每次 `Chat` 成功返回、每个 `Stream` 终止（正常、出错或中途 `Close`）记录一条 `UsageRecord`：

```go
type UsageRecord struct {
	Time         time.Time
	Protocol     Protocol
	Model        string
	Usage        Usage
	UsageMissing bool // 上游 200 但没有返回任何用量数据（兼容服务常见）
}
```

## 查询

```go
stats := client.Stats() // UsageSnapshot 快照

stats.TotalRequests        // 总请求数
stats.InputTokens          // 输入合计
stats.OutputTokens         // 输出合计
stats.TotalTokens          // 总 token
stats.CachedInputTokens    // 缓存命中（读）
stats.CachedCreationTokens // 缓存写入（Anthropic 专有；OpenAI 系自动缓存恒为 0）
stats.ReasoningTokens      // 思考消耗
stats.UsageMissing         // 无用量响应次数
stats.ByModel["deepseek-chat"]     // ModelUsage：按模型
stats.ByProtocol[rosetta.ProtoOpenAIChat] // ModelUsage：按协议
```

`ModelUsage` 与总量同构（Requests / 各 token 维度 / UsageMissing）。`Stats()` 返回深拷贝，可安全持有。

各协议对 `InputTokens` 的口径已经统一：**都包含**命中缓存的部分。OpenAI 系的 `prompt_tokens`/`input_tokens` 原生如此；Anthropic 线格式的 `input_tokens` 只计未缓存部分，适配器已把 `cache_read_input_tokens` 与 `cache_creation_input_tokens` 折进 `InputTokens`/`TotalTokens`，让跨协议的 `Input`/`Total` 可直接比较。因此 **Anthropic 侧的真实输入量就是 `InputTokens`**：`CachedInputTokens` 是它的子集，`CachedCreationTokens` 也已含在其中，两者只用于看缓存明细。**不要再把它们相加**——那会把 Anthropic 的输入量最高虚增一倍。

## 自定义持久化

实现 `UsageTracker` 接口接入任意存储：

```go
type UsageTracker interface {
	Record(ctx context.Context, r UsageRecord) // 会被请求路径调用，勿长时间阻塞
	Snapshot() UsageSnapshot
}
```

要求并发安全（Client 被多 goroutine 共享时 Record 会并发到达）。内置的 `MemoryUsageTracker` 用互斥锁实现，可作为参考。
