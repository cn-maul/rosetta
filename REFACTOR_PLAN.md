# Rosetta 改造计划书 — Go 1.27 现代化 + 正确性加固

| 项目 | 内容 |
|---|---|
| 目标版本 | v0.3.0 |
| 日期 | 2026-09-06 |
| 状态 | 已执行（2026-09-06，随 v0.3.0 发布）|
| 前置依据 | 2026-09-06 代码审计 + Go 1.26 / 1.27 release notes 盘点 |

---

## 1. 背景与目标

当前代码（v0.2.0，约 4300 行，零第三方依赖）架构清晰、vet 干净，但审计发现：

- **1 条可复现 panic 路径**（Anthropic 流式 error 事件缺 error 字段时 `streamCore` 对 `nil` 事件解引用）；
- 若干健壮性/契约问题（`FlexInt64` 违反本包自述契约、`Retry-After` 无上限、Anthropic `/models` 不分页等）；
- **零测试**、CI 只有 build+vet，与 PLAN.md G8"完整测试覆盖"的承诺不符；
- 少量死代码（`quirksSet`、`genericAPIError`）。

同时，工具链已就绪（本机 go1.27.0，CI 用 `stable`）：Go 1.27 中 `encoding/json/v2` 正式转正、v1 `encoding/json` 改为 v2 后端实现、`testing/synctest` 配套的 `httptest.NewTestServer` 落地、goroutine 泄漏档案 GA。

| # | 目标 | 说明 |
|---|------|------|
| G1 | 工具链现代化 | go.mod 升至 `go 1.27`，CI 增加 `go test -race` |
| G2 | 正确性问题清零 | 审计发现的全部 P1/P2/P3 条目修复并有回归测试锁定 |
| G3 | 兑现测试覆盖 | 三种协议适配器、SSE、jsonx、httpx、streamCore、registry 全部有单测 |
| G4 | 应用新特性 | jsonx 基于 `encoding/json/v2` + `jsontext` 重写；synctest 时间假化测试；goroutineleak 断言 |
| G5 | 文档同步 | README/docs 反映 go 1.27 要求与新用法，版本升至 0.3.0 |

---

## 2. 版本策略

- **go.mod：`go 1.22` → `go 1.27`**。激进策略下消费者需 1.27+ 工具链；`GOTOOLCHAIN=auto`（1.21+ 默认）会让旧工具链自动拉取 1.27，摩擦可控。
- 1.27 的 `go test` 默认运行 **stdversion** vet 检查：代码使用比 go 指令更新的标准库符号会直接报错，这是我们的护栏而不是负担。
- **不改动就白拿的收益**（v1 API 不动、语义由工具链提供）：
  - v1 `encoding/json` 已是 v2 后端 → 每条 SSE chunk 的 `json.Unmarshal`（流式热路径）显著提速；
  - `io.ReadAll` 约 2 倍提速 → 各 provider 响应体读取受益；
  - HTTP/1 响应体 close 时自动 drain → 错误路径连接复用变好；
  - Green Tea GC（1.26 起默认）。
  - 注意：错误文案可能与旧实现不同 → **测试断言错误类型（`APIError`/`TransportError`），不断言错误字符串**。

---

## 3. 阶段计划

### P0 地基（工具链与卫生）

| 任务 | 位置 | 说明 |
|---|---|---|
| go.mod 升至 `go 1.27` | `go.mod` | 一行改动，全项目重新构建验证 |
| 跑一轮 `go fix ./...` | 全仓 | 1.26 重写的 modernizers 集合地（1.27 新增 atomictypes/embedlit 等）；逐条 review 后采纳 |
| `math/rand` → `math/rand/v2` | `internal/httpx/httpx.go:105` | 1.22 即可用，趁机迁移；抖动逻辑不变 |
| 删除死字段 `quirksSet` | `options.go:43` | 只写不读；确认无"显式 Quirks 关闭探测"语义后删除 |
| 删除死函数 `genericAPIError` | `errors.go:100-113` | 无调用点 |
| CI 增加 `go test ./... -race` | `.github/workflows/ci.yml` | 可选加覆盖率上传；`stable` 工具链即可 |

**验收**：`go build/vet/test -race ./...` 全绿，diff 无行为变化。

### P1 测试安全网（先立网，含红测试）

布局：与被测代码同包 `_test.go`；HTTP 层统一用 `httptest.NewTestServer`（1.27 新增，内存假网络）+ `testing/synctest`（假时间），测试零真实等待、完全确定。

| 覆盖对象 | 用例要点 |
|---|---|
| `internal/httpx` | 重试次数与退避时序（synctest 假时钟）、Retry-After 解析（delay-seconds / HTTP-date）、jitter 范围、ctx 取消、Body 逐次重建 |
| `internal/sse` | 多行 data 用 LF 连接、CRLF、注释行、id 持久化、无终止空行的尾部事件、超长行 |
| `internal/jsonx` | 表驱动脏数据：字符串数字、`null`、数组 content、浮点计数、非数字字符串（红测试） |
| 三个 provider | `buildPayload`/`encodeMessages` 纯函数直测 + golden 快照（thinking 映射、温度丢弃、工具包装、Extra 合并）；`sanitize`/`rectify` 降级与粘性；错误体解析（对象/裸字符串/非 JSON） |
| `streamCore` | 事件聚合（text/thinking/tool 拼接）、`Collect`/`Partial`、cancel 与 onEnd 恰好执行一次、**Anthropic `{"type":"error"}` 缺 error 字段 → panic 复现（红测试）** |
| `registry` | mergeInfo 覆盖语义、alias 解析、SetRemote/SetManual 重建 |
| 其余 | `EstimateTokens` 边界、`MemoryUsageTracker` 并发（-race）、`DetectProtocol` 分类 |

**验收**：覆盖率基线报告（目标：核心包语句覆盖 ≥ 75%）；已知 bug 的红测试明确失败。

### P2 正确性修复（红转绿）

按严重度排序，每条修复附带 P1 中的回归测试：

| # | 级别 | 问题 | 位置 | 方案 |
|---|---|---|---|---|
| 1 | P1 | 流式 error 事件缺 error 字段 → `apiError` 返回 nil → `apply(nil)` panic | `provider_anthropic.go:415`；`stream.go:106` | anthropic 侧加 `ch.Error != nil` 守卫（缺体时返回通用 `APIError`）；`streamCore.Next` 防御 `(nil, nil)`：视为错误 |
| 2 | P1 | `FlexInt64` 对非数字字符串返回 error，连累整个 payload | `internal/jsonx/jsonx.go:63-73` | 先打补丁：解析失败吞掉、留零值 `Set=false`（P3 重写后语义不变） |
| 3 | P2 | `Retry-After` 无上限，异常网关可让客户端挂起数小时 | `internal/httpx/httpx.go:62` | `min(Retry-After, 60s)` 硬上限 |
| 4 | P2 | Anthropic `/models` 未分页，默认只取前 20 条 | `provider_anthropic.go:481` | `limit=100` + `has_more`/`last_id`（`after_id`）循环 |
| 5 | P2 | `ModelInfo` 内部远端发现未应用 `WithTimeout` | `client.go:183` | 与 `ListModels` 一致套 `settings.timeout` |
| 6 | P2 | `EstimateTokens` 向下取整，与"故意保守"相悖 | `estimator.go:16` | `(ascii+3)/4 + other` |
| 7 | P2 | 终止信号后若服务端继续发数据，仍会产出 MessageEnd 之后的事件 | 三个 `streamEvents` | `ended` 后循环顶部短路返回 `io.EOF` |
| 8 | P3 | `truncateBody` 按 4096 字节截断可能切断 UTF-8 | `errors.go:92` | 回退到 rune 边界（最多 3 字节），Raw 保证合法 UTF-8 |
| 9 | P3 | 首条 user 消息 blocks 全空被跳过后，payload 首条变 assistant → Anthropic 400 | `provider_anthropic.go:191` | 先构建 `out` 再校验"首条为 user" |
| 10 | P3 | `DetectClient` 的 `append(opts, ...)` 写调用方底层数组 | `detector.go:94` | `opts[:len(opts):len(opts)]` 三索引切片强制拷贝 |
| 11 | P3 | sanitize 关键词过宽，一次误判永久粘性翻转 | `provider_openai_chat.go:237` | 命中提示词且 `type` 存在时要求形如 `invalid_request_error` 才降级 |
| 12 | P3 | 显式 `WithMaxTokensField` 会被 sticky 学到的状态覆盖 | `provider_openai_chat.go:53` | pin 优先于 sticky：`maxTokensField != ""` 时跳过 stickyLegacy 覆盖 |
| 13 | P3 | 执行中新发现：`httpx` 退避 Cap 非硬上限（jitter 后可超 20%） | `internal/httpx/httpx.go` | jitter 后再 clamp 到 Cap |
| 14 | P3 | 执行中新发现：SSE 无 data 的被丢弃事件把 `id`/`event` 泄漏给下一个事件 | `internal/sse/sse.go` | `take()` 两个分支都重置字段 |
| 15 | P3 | 执行中新发现：流式响应 body 从不显式关闭，连接无法复用 | 三个 provider 的 `StreamChat` | `streamCore` 增加 `attachCloser`，release 时关闭 |

执行补充说明：P2 共修复 15 项（上表 12 项 + 执行中新增 3 项）。goroutineleak 断言以 `runtime.NumGoroutine()` + `synctest.Wait()` + `srv.Close()` 的组合实现（1.27 的泄漏检测并入 goroutine profile，测试内用计数对比更直接）。

**验收**：P1 红测试全部转绿；`-race` 全绿；行为变化在测试名中可见。

### P3 特性落地（Go 1.26/1.27）

| 任务 | 位置 | 说明 |
|---|---|---|
| jsonx 基于 `encoding/json/v2` + `jsontext` 重写 | `internal/jsonx/jsonx.go` | 用 `jsontext.Decoder` 的 `PeekKind` 分派（`'"'` 字符串 / `'0'` 数字 / `'n'` null），实现 v2 的 `UnmarshalJSONFrom` 风格接口，彻底去掉 string→`[]byte`→`Unmarshal` 的往返；字段语义自控，天然规避 v2 严格性（UTF-8/重复键）问题；表驱动测试沿用 P1 |
| wire 类型**不**迁移 json/v2 | 三个 provider | v1 `encoding/json` 已是 v2 后端、免费提速；迁移只增加 diff 无语义收益。等官方 `go fix` 的 jsonv2 迁移器成熟再评估 |
| `new(expr)` 进文档示例 | `docs/`、README | `Temperature: new(0.7)`（1.26+）；`Float`/`Bool` 保留并在注释注明新写法 |
| `errors.AsType[T]` 进示例 | `docs/`、examples | `if ae, ok := errors.AsType[*rosetta.APIError](err); ok { ... }`（1.26+） |
| goroutineleak 断言 | 流式测试 cleanup | 1.27 GA 的泄漏档案：流关闭后断言无泄漏 goroutine，锁住 `streamCore` cancel/release 路径 |
| 基准对比 | `_test.go` benchmark | chunk 解码 benchmark 前后对比（v1→v2 后端 + jsonx 重写），记录到 CHANGELOG |
| （可选）sticky 字段 atomic 化 | `provider_openai_chat.go:29` | 若 `go fix` 的 atomictypes 提示则评估；默认不动 |

**验收**：jsonx 测试全绿；benchmark 数据归档；文档更新。

### P4 收尾

- README/docs：go 1.27 要求、新特性示例；新建 `CHANGELOG.md` 记录行为变化（go 指令、修复清单、性能）。
- `rosetta.go` 的 `Version` → `"0.3.0"`。
- 覆盖率门禁（可选）：CI 里 `go test -cover` 低于阈值即失败。

---

## 4. 风险与对策

| 风险 | 对策 |
|---|---|
| go 指令 1.27 提高消费者门槛 | `GOTOOLCHAIN=auto` 自动下载；CHANGELOG 明示 |
| v2 后端下 v1 错误文案变化 | 测试只断言错误类型，不断言字符串 |
| jsonx 重写引入行为回归 | P1 表驱动用例先行锁定；逐字段 diff 对照 |
| sanitize 收紧后个别第三方不再自动降级 | 保留粘性机制与 Debug 日志；文档给出 `WithQuirks` 显式逃生口 |
| synctest/NewTestServer 学习成本 | 仅测试代码使用，不进运行时路径 |

## 5. 不做清单

simd（实验性、场景不符）、crypto/mldsa、uuid、hpke、泛型方法、reflect 迭代器、wire 类型手工迁移 json/v2、atomictypes（默认）。

## 6. 里程碑

1. **M1（P0+P1）**：go 1.27 就位、死代码清除、CI 出测试数字、红测试就位；
2. **M2（P2）**：审计问题清零，红转绿；
3. **M3（P3+P4）**：jsonx v2 化、文档与版本收尾，v0.3.0 发布。

建议按 PR 粒度推进：P0 一个 PR；P1 按包拆 2–3 个 PR；P2 每 2–3 条修复一个 PR；P3 jsonx 单独一个 PR。
