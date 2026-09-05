# Rosetta

统一接入 LLM API 的 Go SDK：同一套 API 说三种协议——**OpenAI Chat Completions**、**OpenAI Responses**、**Anthropic Messages**，以及兼容这些协议的第三方服务（DeepSeek、Moonshot、Qwen、GLM、OpenRouter、vLLM、Ollama 等）。协议差异（认证方式、system 位置、thinking 参数、流式事件、usage 字段）全部由 SDK 吸收。零第三方依赖。

## 安装

```bash
go get github.com/cn-maul/rosetta
```

要求 Go 1.22+。零第三方依赖，`go get` 即取即用。

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

## 文档

| 文档 | 内容 |
|---|---|
| [基础指南](docs/guide.md) | 客户端配置、消息与工具调用、thinking、上下文校验、错误处理、全部选项 |
| [流式响应](docs/streaming.md) | 统一事件模型、迭代惯用法、中断恢复、工具调用流 |
| [模型体系](docs/models.md) | 两级合并注册表、协议探测、能力声明与门控 |
| [协议与兼容](docs/protocols.md) | 三协议映射、自动降级、quirks、脏数据处理、重试策略 |
| [用量统计](docs/usage-stats.md) | Usage 字段、Tracker 接口、Snapshot 查询 |
| [设计文档](PLAN.md) | 架构决策、里程碑、参考研究 |

可运行示例见 [examples/](examples/)：chat / stream / responses / anthropic / usage / models。

## 许可证

MIT，见 [LICENSE](LICENSE)。
