# 模型体系

模型元数据（`ModelInfo`）来自三个来源，按优先级合并；同时提供协议探测与厂商预设，把「endpoint + key 即可用」做完整。

## 三级合并

```
手动配置（WithModelInfo / WithModelsFile）
    > 远程发现（GET /models，ListModels 拉取）
        > 内置知识库（go:embed 的 registry_data/models.json）
```

- 合并按字段逐层覆盖：上层只覆盖非零字段，其余继承下层。稀疏的手动条目可以只写关心的字段（如只覆盖 `ContextWindow`），thinking 能力等照常继承。
- 布尔字段遵循 OR 语义：上层无法撤销下层声明的 `SupportsThinking`（写一个显式 false 不会生效）。
- **Known 语义**（原则：预设完整可靠，自定义不猜测）：手动配置与内置条目 `Known=true`；仅靠 `/models` 发现的条目 `Known=false`，不推断任何能力。thinking 门控与上下文校验只对 `Known=true` 的条目生效。
- 别名：内置与手动条目可声明 `Aliases`，查询时自动归一到规范 ID（如 `gpt-4o-2024-11-20` → `gpt-4o`）。

内置知识库覆盖主流模型（GPT-5/o 系、gpt-4o、gpt-4.1、Claude 4.x、DeepSeek、Kimi、Qwen、GLM 等），随版本更新。字段：`id / display_name / context_window / max_output_tokens / supports_thinking / protocol / aliases`。

## 手动配置

```go
// 方式一：代码注入（可多次调用）
rosetta.WithModelInfo(
	rosetta.ModelInfo{ID: "my-finetune", ContextWindow: 32768, MaxOutputTokens: 8192, SupportsThinking: true},
)

// 方式二：JSON 文件（与内置知识库同格式）
rosetta.WithModelsFile("models.json")
```

```json
{"models": [{"id": "my-finetune", "context_window": 32768, "max_output_tokens": 8192}]}
```

文件不存在或格式非法时 `NewClient` 直接报错。

## 查询

```go
info, err := client.ModelInfo(ctx, "deepseek-reasoner") // 单个；别名可解析
models, err := client.ListModels(ctx)                   // 拉取远端目录并返回合并视图
models, err := client.RefreshModels(ctx)                // 同 ListModels（显式刷新语义）
```

- `ListModels` 每次调用都拉取远端并写入注册表的 remote 层；`Chat`/`ChatStream` 的门控只用「手动 + 内置」两层，**不会**暗中发起网络请求。
- `ModelInfo` 在手动+内置都查不到时，会做一次 best-effort 的远端发现再查一次，仍无则返回 `ErrUnknownModel`。

## 协议探测

```go
client, err := rosetta.DetectClient(ctx,
	rosetta.WithEndpoint("https://your-gateway.example.com"),
	rosetta.WithAPIKey(key),
)
```

判定顺序：

1. **已知域名表**（无网络）：api.openai.com、api.anthropic.com、api.deepseek.com、api.moonshot.cn、dashscope.aliyuncs.com、open.bigmodel.cn、api.siliconflow.cn、openrouter.ai、api.groq.com、api.together.xyz、api.fireworks.ai、api.x.ai、api.mistral.ai、router.huggingface.co；
2. **主动探测** `GET /models`：先 Bearer 后 x-api-key，响应条目含 `type:"model"` → Anthropic，含 `object:"model"` → OpenAI；
3. **兜底** OpenAI Chat（兼容服务的最大公约数；Responses 端点同样暴露 chat/completions，需要 Responses 语义时用 `WithProtocol` 显式指定）。

## 厂商预设

`WithVendor(name)` 一次设置默认 endpoint、协议与 quirks；调用方显式给出的选项永远优先（与调用顺序无关）。

| name | endpoint | 协议 |
|---|---|---|
| `openai` | api.openai.com/v1 | chat |
| `anthropic` | api.anthropic.com/v1 | anthropic |
| `deepseek` | api.deepseek.com/v1 | chat |
| `moonshot` | api.moonshot.cn/v1 | chat |
| `qwen` | dashscope.aliyuncs.com/compatible-mode/v1 | chat |
| `zhipu` | open.bigmodel.cn/api/paas/v4 | chat |
| `siliconflow` | api.siliconflow.cn/v1 | chat |
| `openrouter` | openrouter.ai/api/v1 | chat |
| `groq` | api.groq.com/openai/v1 | chat |
| `together` | api.together.xyz/v1 | chat |
| `fireworks` | api.fireworks.ai/inference/v1 | chat |
| `xai` | api.x.ai/v1 | chat |
| `mistral` | api.mistral.ai/v1 | chat |
| `ollama` | localhost:11434/v1 | chat（API key 传占位符即可） |

`rosetta.VendorNames()` 列出全部预设名。预设端点过期时无需等版本更新——直接 `WithEndpoint` 覆盖即可。

## 能力门控

两项请求前置检查（`Chat` / `ChatStream` 自动执行，不发额外网络请求）：

1. **thinking 门控**：`Known && !SupportsThinking` 的模型请求 thinking → 默认 `ErrThinkingUnsupported`；`WithThinkingFallback(true)` 静默去掉 thinking 配置。未知模型原样透传。
2. **上下文校验**：有 `ContextWindow` 的已知模型，估算超限默认告警；`WithStrictContextCheck(true)` 返回 `ErrContextTooLong`。估算规则见[基础指南](guide.md#上下文校验)。
