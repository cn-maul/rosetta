# Rosetta
[![CI](https://github.com/cn-maul/rosetta/actions/workflows/ci.yml/badge.svg)](https://github.com/cn-maul/rosetta/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cn-maul/rosetta.svg)](https://pkg.go.dev/github.com/cn-maul/rosetta)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **v1.3.0（未发布）** · 活跃维护 · 安全策略见 [SECURITY.md](SECURITY.md) · English: [README.en.md](README.en.md)

统一接入 LLM API 的 Go SDK：同一套 API 说三种协议——**OpenAI Chat Completions**、**OpenAI Responses**、**Anthropic Messages**，以及兼容这些协议的第三方服务（DeepSeek、Moonshot、Qwen、GLM、OpenRouter、vLLM、Ollama 等）。协议差异（认证方式、system 位置、thinking 参数、流式事件、usage 字段）全部由 SDK 吸收。对话之外还覆盖同一套客户端下的 **Embeddings**（OpenAI 格式）与 **Rerank**（Cohere 格式），支持图文/音频/文档多模态输入。零第三方依赖。

## 安装

```bash
go get github.com/cn-maul/rosetta
```

要求 Go 1.27+（SDK 自身使用 `encoding/json/v2` 等新标准库能力；零第三方依赖）。

## 使用

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cn-maul/rosetta"
)

func main() {
	client, err := rosetta.NewClient(
		rosetta.WithEndpoint("https://api.deepseek.com/v1"),
		rosetta.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
	)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []rosetta.Message{rosetta.User("你好")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Text())
}
```

换服务商只需换 endpoint、key 和 model id；协议不对时加一行 `rosetta.WithProtocol(...)`，或改用 `rosetta.DetectClient` 自动探测。

> **Go 版本提示**：SDK 最低要求 Go 1.27。Go 1.26 引入的可选字段 `new(expr)` 写法（例如 `Temperature: new(0.7)`）可用于兼容代码；`Float`/`Bool` 仍然保留。错误匹配同理，`errors.AsType[*rosetta.APIError](err)` 可替代声明变量的 `errors.As`。

## 范围与边界

这个库是**底层协议适配层**，不是应用框架。它负责：一个 endpoint + 一把 key 的协议翻译、流式、重试、错误归因、精确的 token 用量统计（含缓存命中拆分）。

**明确不做**，请交给上层库或自行实现：

- 多供应商路由、负载均衡、账号池、自动回退
- Agent 循环、工具编排、并行工具执行
- 响应缓存、会话历史管理、上下文裁剪与摘要
- 本地 MCP 客户端、Realtime 实时语音
- 向量库、Assistants、Fine-tuning 等独立 API 面
- 费用统计与价格表（价格是上层业务策略，SDK 只算准 token，不折算成本）

原因：这些是「用这个 SDK 构建的产品」，不是 SDK 本身。放进来会让核心依赖变重、升级变慢，也逼所有用户为少数人的需求买单。

SDK 同样不替你做业务决策：预算上限、多 key 轮换、降级策略，应该由调用方基于 `UsageRecord` 和 `APIError.Category` 决定。

## 文档

| 文档 | 内容 |
|---|---|
| [基础指南](docs/guide.md) | 客户端配置、消息与工具调用、thinking、上下文校验、错误处理、全部选项 |
| [流式响应](docs/streaming.md) | 统一事件模型、迭代惯用法、中断恢复、工具调用流 |
| [模型体系](docs/models.md) | 两级合并注册表、协议探测、能力声明与门控 |
| [协议与兼容](docs/protocols.md) | 三协议映射、自动降级、quirks、脏数据处理、重试策略 |
| [用量统计](docs/usage-stats.md) | Usage 字段、Tracker 接口、Snapshot 查询 |

可运行示例见 [examples/](examples/)：chat / stream / responses / anthropic / embedding / rerank / usage / models / tools / errors / timeout / concurrent。

## 许可证

MIT，见 [LICENSE](LICENSE)。
