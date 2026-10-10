# rosetta SDK × magpie 对照分析与借鉴建议

> 基线：rosetta SDK v1.0.1（`C:\Users\louis\Desktop\project\rosetta`）
> 参考：magpie 0.1.1157（`C:\Users\louis\Downloads\magpie-0.1.1157`）
> 姊妹篇：网关侧分析在 `rosetta-gateway/AUDIT/magpie-benchmark.md`，两份独立成立、互不依赖。

## 〇、定位判断（决定什么能抄）

magpie 是**单机订阅聚合网关**，rosetta 是**零依赖通用 SDK**。层级不同：
**调度、账号池、亲和性、sink、lane 全是网关层概念，SDK 不该碰**——SDK 的边界是
「一个 endpoint + 一把 key 的协议适配 + 传输韧性」，这个分层 rosetta 是对的，
magpie 的对应物全部长在它的 `internal/gateway` 而非传输层，恰好印证。

**能抄的是三块**：错误信息的完备度（调用方分类所需的字段）、传输层对厂商劣化的
防御（magpie 在网关层做的很多「脏数据容错」，SDK 其实是更合适的家）、
以及流式的看护职责边界。

---

## 一、最值得抄的（按性价比排序）

### 1. 把 `Retry-After` 暴露到 `APIError` —— 成本最低、收益最直接

**现状**（已核实）：`internal/httpx/httpx.go:299 retryAfter()` 完整实现了
秒数/HTTP-date 解析、`maxRetryAfter=60s` 上限，但**只在重试决策里用，从不透传**
——`APIError`（errors.go:109）没有该字段，退避时长只进 debug 日志。

magpie 的做法：`routing.go:584` 解析 `Retry-After` 及 `ratelimit-reset` 类头，
既用于自身退避，也进 rest 状态和 trace（Routing 页直接展示「这个账号几点回来」）。

**建议**：`APIError` 加一个字段：

```go
// RetryAfter is the server-stated wait before retrying, parsed from the
// Retry-After header (delay-seconds or HTTP-date), capped at 60s.
// Zero when the server gave none. It is a hint for callers; the SDK's own
// retry loop has already honored it.
RetryAfter time.Duration
```

十几行改动（httpx 把解析结果随响应带回、`parseOpenAIError`/`parseAnthropicError`
落字段），直接解锁网关侧三件事：429 冷却精确化（网关 DESIGN.md:797 明确记录
「Rosetta v0.5.1 未在 APIError 上暴露 Retry-After，故 429 统一按 60s 处理」）、
下游透传 `Retry-After` 响应头、以及网关 §二.4 全部 «按上游时长冷却» 的设想。
**这是两个项目之间杠杆最大的一颗螺丝。**

### 2. 流空闲超时 `WithStreamIdleTimeout(d)` —— 附录 B #2，magpie 的实践证明了这个需求

magpie 没有等价选项，但它的网关在**每个协议上自己实现了看门狗**，且文档反复出现
「流静默 X 秒」「stalled」的处理——这个需求是真实的、跨网关的。

SDK 侧的实现位置很清楚：`streamCore` 已经是单点收口（`Next` 单 goroutine 驱动、
producer 在锁外阻塞读），在 producer 的每次 `sse.Next()` 读上加
`time.AfterFunc` 空闲计时器、超时调 `Close()` 即可——**而 `Close()` 并发安全
正是为看门狗设计的**（docs/streaming.md 原话），说明原作者预留了这个钩子的位置，
只是没把计时器做进来。

配套要修一个已知边界：`Close()` 不置 `Err()`（网关被迫用 `atomic.Bool` 自行留痕，
DESIGN §8.1）。若 SDK 提供官方 idle timeout，超时应产生一个可区分的结果
（新哨兵 `ErrStreamIdleTimeout`，或至少在 `Err()` 里留痕），否则调用方仍然
分不清「我关的」与「真断了」。

### 3. 厂商错误语义分类进 SDK —— magpie 词表的正确归宿

magpie 在**网关层**维护了一张正则词表（`routing.go:167-195`：
`creditWords`/`quotaWords`/`rateWords`/`plannedWords`/`brokeWords`，含中文
「余额不足/请充值」），把 status+body 归成 20+ 种 `failXXX`。它的 LESSONS 里
有对应教训：「Decide whether a refusal is about the account, the model or the
request content before you rest an account」——分错类会**连坐健康账号**。

**这张词表长在 SDK 更合理**：SDK 已经拥有协议解码器（`parseOpenAIError` /
`parseAnthropicError`），是唯一能统一看到「状态码 + 协议错误体 + 厂商」的位置；
而分类需求（欠费/限流/配额/模型不存在/模型下线）是**每个网关调用方都要重写的**。

建议 API 形状（不替调用方做决策，只给结构化提示）：

```go
// APIError 增补
Category ErrorCategory // 0 = unclassified; 只做归因提示，不改变 Retryable 语义

type ErrorCategory int8
const (
    CatUnclassified ErrorCategory = iota
    CatOutOfCredit      // 402 / "insufficient quota"(OpenAI) / "billing"
    CatRateLimited      // 429 + Retry-After 语义
    CatQuotaExhausted   // 订阅窗口耗尽（resets_at 类）
    CatModelUnavailable // 模型不在套餐内 / 已下线（403/404/410 + 词表）
    CatContentRefused   // 安全审查拒绝——账号无辜，不 rest
)
```

其中 `CatContentRefused` 尤其值钱：magpie 教训里「ChatGPT backend 的 502
response protection 是内容问题不是账号问题，换账号照样失败」——SDK 按协议位置
识别这类错误，能帮所有调用方避免无意义的全账号轮询。

### 4. `CatModelUnavailable` 对应的「降级粒度」提示 —— 配合 magpie 的 restID 思想

magpie 的三键分层（`restKey` 账号级 / `restID` 账号+模型级）解决「该 key 的套餐
不含此模型」**只停模型不停账号」。SDK 侧的对应物是：`APIError.Param` 已经携带
厂商点名的字段，但**模型维度的失败没有标记**。

若采纳 §3 的 `CatModelUnavailable`，再补一个 `AffectsModel string`（厂商明确
指向某模型时非空），调用方就能实现 magpie 式的模型级隔离，而不必自己解析 body。
成本极低——Anthropic 的 `param`、OpenAI 的 `code:"model_not_found"` 都已在手。

### 5. 看门狗范式文档 + `Close()` 留痕 —— 附录 B #4 的收尾

SDK 已把「WithTimeout 不作用于流」文档化了，但**没有推荐范式**。网关自建看门狗
+ `atomic.Bool` 留痕是当前唯一做法，而 `Close()` 不置 `Err()` 意味着每个调用方
都要重新发明一遍「我怎么知道是我自己掐的」。

两条路任选：做 §2（官方 idle timeout，问题消失），或在 docs/streaming.md 给出
范式示例（含留痕写法）。后者是纯文档工作，半天量级。

---

## 二、确认**不要**抄的（SDK 边界之外）

| magpie 的东西 | 为什么不进 SDK |
|---|---|
| 账号池 / 候选循环 / rest 状态机 / sink / lane / affinity | 网关层概念。SDK 保持「一个 Client = 一个 endpoint+key」的哑客户端定位是对的；magpie 也把它们全放在 gateway 层而非传输层，恰好是同一判断 |
| `rpmTransport` 统一计量 | 网关用 `WithHTTPClient` 注入自己的 `RoundTripper` 即可实现（这是 SDK 已有的正确扩展点），SDK 不需要内建 |
| 词表正则里的**中文**厂商文案（「余额不足/请充值」） | 若做 §3，词表应只收**协议规范与主流厂商的英文 canonical 文案**（OpenAI `insufficient_quota`、Anthropic `credit_too_low` 等）；中文文案是 magpie 面向国内订阅relay 的私货，进 SDK 会变成无限维护的兜风词表 |
| loop_guard（回复死循环检测） | 有意思但属产品策略（magpie 可配置关闭）；SDK 若要提供应作为**可选**的 Stream 包装器而非默认行为，优先级放最后 |
| sealed reasoning（`Part.Sealed`/`encrypted_content` 原样携带） | **值得评估但先核对现状**：SDK 的 `Block.Signature` 已透传 Anthropic thinking 签名；Responses 的 `encrypted_content` 是否在统一模型中存活需要确认。若当前丢弃，这是 Responses 回放场景的真实缺口（OpenAI 要求 reasoning item 原样回传），magpie 为此专门设计了 `sealedReasoning`。建议单独立项核实 |

---

## 三、落地顺序建议

1. **`APIError.RetryAfter`**（§1）——十几行，双仓联动收益最大；做完网关的
   429 精确冷却立刻解锁。
2. **看门狗范式文档**（§5 后半）——纯文档，把网关的 workaround 沉淀为官方姿势。
3. **`ErrorCategory` + `AffectsModel`**（§3/§4）——一个 minor 版本的量级；
   词表从协议 canonical 文案起步，厂商私货走 `Extra`/后续扩展。
4. **`WithStreamIdleTimeout`**（§2）——动 streamCore 收尾路径，需要完整测
   （synctest + 竞态），适合单独一个 minor。
5. （核实后）**Responses `encrypted_content` 存活性**（§二 末条）。

---

## 四、方法论借镜（与网关侧报告 §二.9 同源，SDK 侧补充一条）

SDK 的注释质量已经很高（`ErrUpstreamMalformed` 的范围裁定注释是教科书级的），
但 magpie 有一条 SDK 值得直接吸收的验证纪律：**「反向测试钉住语义边界」**——
magpie 为「流式解码**不得**命中 malformed 哨兵」专门写了反向用例，防止后人
「顺手统一」。SDK 的 `upstream_malformed_test.go` 已有同构设计（反向断言
500/429/401/InBand 均不命中），**这个做法在新增 `ErrorCategory` 时必须延续**：
每加一个类别，同时钉住「哪些已知错误不得落入该类」，否则分类体系会像 magpie
LESSONS 里警告的那样「比不加更糟」。
