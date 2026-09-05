# 模型体系

模型元数据（`ModelInfo`）来自两个来源，按优先级合并；协议探测负责把「endpoint + key」变成可用的客户端。SDK 不内置任何厂商预设或模型知识库——一切以你的声明和服务商自己的 `/models` 为准。

## 两级合并

```
手动配置（WithModelInfo / WithModelsFile）
    > 远程发现（GET /models，ListModels 拉取）
```

- 合并按字段逐层覆盖：上层只覆盖非零字段，其余继承下层。稀疏的手动条目可以只写关心的字段。
- 布尔字段遵循 OR 语义：手动条目声明 `SupportsThinking: true` 后，远端条目无法撤销它。
- **Known 语义**（原则：自定义模型不猜测）：手动配置 `Known=true`；仅靠 `/models` 发现的条目 `Known=false`，不推断任何能力。thinking 门控与上下文校验只对 `Known=true` 的条目生效。
- 别名：手动条目可声明 `Aliases`，查询时自动归一到规范 ID。

## 手动配置

```go
// 方式一：代码注入（可多次调用）
rosetta.WithModelInfo(
	rosetta.ModelInfo{ID: "my-finetune", ContextWindow: 32768, MaxOutputTokens: 8192, SupportsThinking: true},
)

// 方式二：JSON 文件
rosetta.WithModelsFile("models.json")
```

```json
{"models": [{"id": "my-finetune", "context_window": 32768, "max_output_tokens": 8192}]}
```

文件不存在或格式非法时 `NewClient` 直接报错。

## 查询

```go
info, err := client.ModelInfo(ctx, "my-model")     // 单个；别名可解析
models, err := client.ListModels(ctx)              // 拉取远端目录并返回合并视图
models, err := client.RefreshModels(ctx)           // 同 ListModels（显式刷新语义）
```

- `ListModels` 每次调用都拉取远端并写入注册表的 remote 层；`Chat`/`ChatStream` 的门控只用手动层，**不会**暗中发起网络请求。
- `ModelInfo` 在手动层查不到时，会做一次 best-effort 的远端发现再查一次，仍无则返回 `ErrUnknownModel`。

## 协议探测

```go
client, err := rosetta.DetectClient(ctx,
	rosetta.WithEndpoint("https://your-gateway.example.com"),
	rosetta.WithAPIKey(key),
)
```

判定方式：主动探测 `GET /models`（先 Bearer 后 x-api-key），响应条目含 `type:"model"` → Anthropic，含 `object:"model"` → OpenAI；探测失败兜底 OpenAI Chat（兼容服务的最大公约数；需要 Responses 语义时用 `WithProtocol` 显式指定）。

## 能力门控

两项请求前置检查（`Chat` / `ChatStream` 自动执行，不发额外网络请求）：

1. **thinking 门控**：手动声明 `SupportsThinking: false` 的模型请求 thinking → 默认 `ErrThinkingUnsupported`；`WithThinkingFallback(true)` 静默去掉 thinking 配置。未声明的模型原样透传。
2. **上下文校验**：手动声明 `ContextWindow` 的模型，估算超限默认告警；`WithStrictContextCheck(true)` 返回 `ErrContextTooLong`。估算规则见[基础指南](guide.md#上下文校验)。
