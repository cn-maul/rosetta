# work/

诊断与回归取证用的独立 Go module。**不是发布物的一部分**——每个子目录都有自己的
`go.mod`（`replace` 指向根模块），根目录的 `go test ./...` 不会扫到它们，
所以一个"断言缺陷存在"的用例不会把 CI 打红。

| 目录 | 用途 | 怎么跑 |
|---|---|---|
| `anthropic-stopseq-repro/` | Anthropic 适配器丢失 `stop_sequences` / `service_tier` 的复现用例与问题报告（v1.3.2 修复后转为 PASS，保留作回归取证） | `make work-test` |
| `anthropic-live-check/` | 对真实 Anthropic 兼容端点的端到端冒烟：合法值必须 200，OpenAI 词表的值必须本地拒绝 | `TIANTOKEN_KEY=... make work-smoke` |

约定：

- 往 `work/` 放实验代码，**必须自带 `go.mod`**，否则会污染根模块的 `go test ./...`。
- 不要把 key 写进这些目录里的任何文件；一律走环境变量。
- 一次性诊断完成后，要么改成可复跑的回归用例，要么删掉——别留半成品。
