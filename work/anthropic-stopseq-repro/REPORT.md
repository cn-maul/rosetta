# rosetta SDK 问题报告：Anthropic 协议丢失 `stop_sequences` / `service_tier`

- **报告人**：rosetta-gateway 维护者
- **报告日期**：2026-11-11
- **受影响版本**：v1.3.0 起（v1.3.1 仍存在）
- **引入提交**：`9bd798c` "feat: 对齐三家协议最新规范并补齐生产能力"（2026-11-06）
  - 附注：`946b0d1`（v1.3.0 发布提交）只是把同一处错误表述扩散到 4 个文档文件，`provider_anthropic.go` 零改动。归因到文档提交是错的。
- **严重度**：中高 — 字段被**静默丢弃**，无报错、默认日志级别下无告警，且是**对外行为在小版本内无提示回归**
- **修复状态**：**已修复于 v1.3.2（2026-11-11）** — 恢复发送 `stop_sequences` / `service_tier`，
  同步更正文档与测试，按协议加了 `service_tier` 值域校验，并一并支持了 7.6 的 `speed` 字段；
  `work/` 复现用例已转为 PASS。上游实测仍缺（见 2.3 / 2.4），证据等级为文档级。

---

## 一、结论

Anthropic 在 API 版本 `2026-10-21` 中**只移除了 `temperature` / `top_p` / `top_k`**。
`stop_sequences` 与 `service_tier` **仍在该版本官方 Messages API 的请求参数里**。

rosetta 从 v1.3.0 起把这五个字段一并当作「已移除」，导致 Anthropic 协议下
`stop_sequences` 与 `service_tier` 永远不进入上游请求体。

v1.2.1 及更早行为正确（`provider_anthropic.go:268` 直接写 `payload["stop_sequences"]`）。

---

## 二、证据与证据等级

本节刻意区分「证明了什么」与「没证明什么」。上游行为的主张必须标清依据类型，
不能用确定语气掩盖证据缺口。

### 2.1 已取得的证据（官方 SDK 仓库一手文件）

**a. 官方 TypeScript SDK 的请求类型契约。**
`MessageCreateParamsBase` 的实际字段全集（`package/resources/messages/messages.d.ts`，
npm `@anthropic-ai/sdk@1.8.0`，2026-11-10 发布）：

```ts
export interface MessageCreateParamsBase {
  max_tokens: number;
  messages: Array<MessageParam>;
  model: Model;
  cache_control?: CacheControlEphemeral | null;
  container?: MessageCreateParamsContainer | null;
  diagnostics?: DiagnosticsParam | null;
  inference_geo?: string | null;
  metadata?: Metadata;
  output_config?: OutputConfig;
  service_tier?: 'auto' | 'standard_only';
  speed?: 'standard' | 'fast' | null;
  stop_sequences?: Array<string>;
  stream?: boolean;
  system?: string | Array<TextBlockParam>;
  thinking?: ThinkingConfigParam;
  tool_choice?: ToolChoice;
  tools?: Array<ToolUnion>;
  user_profile_id?: string;
  workspace_id?: string;
}
```

`temperature` / `top_p` / `top_k` 已不在其中；`stop_sequences`、`service_tier` 在。

**b. 与 `2026-10-21` 前一版（`@anthropic-ai/sdk@0.137.0`，2026-10-20）的差分。**
该版本新增 `output_config` / `service_tier` / `speed`，移除的**只有**三个采样字段：

    temperature?: number;
    top_k?: number;
    top_p?: number;

**c. 官方 Python SDK 的 `MIGRATION.md`：移除清单只列三个。**
`anthropics/anthropic-sdk-python` 仓库 `main` 分支 `MIGRATION.md`，小节
「Removed: deprecated request parameters」，逐条枚举如下（原文）：

| Method(s) | Removed parameter | Use instead |
|---|---|---|
| `messages.create()`、`messages.stream()`、`messages.parse()` 及其 `beta.messages` 对应方法、`beta.messages.tool_runner()`、`messages.batches.create()` 的 per-request `params` | `temperature`、`top_p`、`top_k` | Remove them. Current models do not use these sampling parameters; for an older model that still does, pass them through `extra_body` |

Quick reference 表同口径：

| You have… | Do this |
|---|---|
| `temperature=` / `top_p=` / `top_k=` on message methods | remove; use `extra_body={"temperature": ...}` if an older model still needs it |

**移除清单只列了三个参数，`stop_sequences` 与 `service_tier` 全篇未出现。**
该文件自述的升级目标即官方迁移指南
`platform.claude.com/docs/en/api/migration-2026-10-21`（TS / Go 仓库的 `MIGRATION.md`
同样只是指向该页的链接，内容不另列）。

一处需要如实标注的细节：该文件同时说明这三个采样参数「只是从方法签名里移除，不是从 API 移除，
改动前发布的模型仍接受（经 `extra_body` 透传）」。这句与「新版对当前模型 400」不矛盾
（针对的是不同模型代际），但本报告的主论证不依赖这一点，列在此处避免读者被单句误导。

**d. 官方 Python SDK 的参数类型契约仍在，且文档字符串直接描述了 `stop_sequence` 行为。**
`src/anthropic/types/message_create_params.py` 的 `MessageCreateParamsBase` 中：

```python
    service_tier: Literal["auto", "standard_only"]
    """Determines whether to use priority capacity (if available) or standard capacity
    for this request. ..."""

    stop_sequences: SequenceNotStr[str]
    """Custom text sequences that will cause the model to stop generating.

    Our models will normally stop when they have naturally completed their turn,
    which will result in a response `stop_reason` of `"end_turn"`.

    If you want the model to stop generating when it encounters custom strings of
    text, you can use the `stop_sequences` parameter. If the model encounters one of
    the custom sequences, the response `stop_reason` value will be `"stop_sequence"`
    and the response `stop_sequence` value will contain the matched stop sequence.
    """
```

这段是对「该字段有效」最直接的官方表述：当前 SDK 仍用现在时描述「命中后
`stop_reason` 为 `stop_sequence`」。同时该 TypedDict 里既无 `temperature`、
也无 `top_p` / `top_k`。

补充一层排他性推理：这些类型由官方从 OpenAPI 规范生成。若 `stop_sequences` 在
`2026-10-21` 真被移除，官方 SDK 必须同步删除该字段——留着它意味着每个传了它的调用方
都能编译通过、运行期 400，这是官方不会留下的错误。

**e. 多语言 SDK 同形（报告人核对）。** Go / Java / Ruby / PHP 的 `MessageNewParams`
同样保留 `StopSequences` 与 `ServiceTier`，均无 `Temperature` / `TopP` / `TopK`。
六套 SDK 由官方从同一份 OpenAPI 规范生成，方向一致。
（本轮逐字核对了 TS 与 Python 两份原文，其余四份按报告人核对结果记录。）

**f. 清单完整性。** 这些官方文件对「移除项」都是**逐条枚举**形式
（MIGRATION.md 表格、类型定义字段列表）。若 `stop_sequences` 或 `service_tier`
也在移除之列，必然出现在同一份枚举里；而它们一致地只出现在「仍在」的一侧。

### 2.2 未取得的证据

**没有做真实上游实测**（发一发带 `stop_sequences` 的请求看 200 还是 400）。原因两条：
`platform.claude.com` 对本报告核对环境返回**区域不可用重定向**
（`anthropic.com/app-unavailable-in-region`），`docs.anthropic.com` 同样无法直连，
官方文档页抓不到；报告人手上也没有可直接调 Anthropic 的凭据
（网关 `providers` 表内无 claude 条目）。

所以本报告的证据等级是：**「官方契约认为这两个字段有效」，而非「实测确认 200」**。
这个区别必须写明，不能被读成「已端到端验证」。2.1(c)~(f) 给出两类互相独立的官方证据
（MIGRATION.md 的移除清单只列三个 + 六套 SDK 的请求类型仍保留这两个字段），
可信度高于单一来源，但它终究是文档级，不是实测级。

### 2.3 给维护者的 5 分钟闭环

若手上有 Anthropic 凭据**且能直连 `api.anthropic.com`**，两条命令即可把证据升级为实测级：

```sh
curl -sS https://api.anthropic.com/v1/messages \
  -H "x-api-key: $KEY" -H "anthropic-version: 2026-10-21" \
  -H "content-type: application/json" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["END"]}'
```

`service_tier` 同样单独发一次，两次都记录 HTTP 状态。
`200` = 本报告成立；`400` 且报文指向未知字段 = 本报告不成立，请以此驳回，我不再坚持。

前置条件不是可有可无的：本报告核对环境直连官方端点返回
`HTTP 403 {"error":{"type":"forbidden","message":"Request not allowed"}}`（区域限制，不是鉴权失败），
所以**即使有官方 key 也打不进去**，必须走海外出口。

### 2.4 已尝试的中转实测，以及它为什么不算证据

后续用一台 `new-api` 架的中转（原生 Anthropic 格式，`claude-sonnet-5-5` / `claude-opus-5-5`）
做了真机调用，结果**不能用来支撑本报告**，原因如下：

| 探测 | 结果 | 含义 |
|---|---|---|
| 完全未知字段 `totally_unknown_param_xyz` | 200 | 该中转对未知字段一律放行，**200 不构成校验证据** |
| `service_tier: "flex"`（官方词表非法） | 200 | 同上，字段被忽略而非校验 |
| `stop_sequences` 传成字符串 | 500，报错文本含 `ClaudeRequest.stop_sequences []string` | 它确实建模了该字段，但不等于透传给上游 |
| `stop_sequences` 是否真的截断 | **没有**：sonnet / opus / 流式三种打法、stop 取 `"B"`（首字符）或 `"NAN"`，一律返回完整输出 + `stop_reason: end_turn`、`stop_sequence: null` | 该中转收到字段但未让其实效 |
| 响应是否回传 `service_tier` | 没有该字段 | 无法据此实现响应侧回显 |

结论有两条，都很实用：

1. **就本报告而言**：中转实测只能得出「发送这两个字段不会导致 400」，即修复没有部署风险；
   不能得出「上游接受并生效」。真正的定案仍依赖 2.3。
2. **就调用方而言**：`new-api` 这类中转会把 `stop_sequences` 吃掉。走此类中转时
   「停止序列不生效」应优先归因中转，而不是 SDK —— 这一条与 rosetta 无关，但足以混淆排障方向。


---

## 三、错误溯源：从「三个」扩到「五个」的那一步没有依据

这比「代码写错了」更值得修，因为它是错误代码的来源。

`9bd798c` 引入的 `CHANGELOG.md:22-25`（v1.3.0「破坏性变更」）写着：

> 新版移除了 `temperature` / `top_p` / `top_k` / `stop_sequences` / `service_tier` 五个请求
> 参数（传了直接 400），Anthropic 适配器因此不再发送它们

`provider_anthropic.go:240-242` 的注释复述了同一句：

> Anthropic removed temperature / top_p / top_k / stop_sequences /
> service_tier in API version 2026-10-21: sending any of them is a 400
> even at the default value.

问题是**范围**：官方 `MIGRATION.md` 把 `temperature` / `top_p` / `top_k` 列为 removed，
这一半没错；但把它扩到 `stop_sequences` / `service_tier` 没有任何依据支撑——
既没有对应测试，也没有任何官方文件提到这两个字段被移除。

需要如实标注：注释与 changelog 里「**400 even at the default value**」这半句，
本报告无法用一手材料核对。SDK 的 `MIGRATION.md` 只描述「不再被生成的请求方法接受
（传了是 `TypeError`）」，不涉及服务端状态码。若维护者手上有官方页面原文，以原文为准；
本报告的结论不依赖这半句。

**大概率是两件事叠出来的**：把「删除了三个采样参数」读成「删除了全部采样/序列/等级参数」，
再把「这三个参数会被上游拒绝」这条成立的结论，直接顺延到扩容后的五个字段上。

建议维护者回溯当时的依据来源。如果是误读，修复范围应包含这句断言本身，
否则下一个对齐上游版本的人会踩同一个坑。

---

## 四、复现

`work/anthropic-stopseq-repro/main_test.go` 为完整可运行的复现用例（独立 Go module，
不影响根模块的 `go test ./...`）：

```go
c, _ := rosetta.NewClient(
    rosetta.WithEndpoint(srv.URL),
    rosetta.WithAPIKey("k"),
    rosetta.WithProtocol(rosetta.ProtoAnthropic),
)
req := &rosetta.ChatRequest{
    Model:         "claude-sonnet-4-5",
    Messages:      []rosetta.Message{rosetta.User("hi")},
    StopSequences: []string{"END"},
    ServiceTier:   "auto",
}
c.Chat(context.Background(), req)
```

在该目录执行 `go test ./... -v`，实测输出：

```
上游实际收到的 payload:
{"max_tokens":4096,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"claude-sonnet-4-5"}

BUG: stop_sequences 未到达上游（官方 API 2026-10-21 仍支持该字段）
BUG: service_tier 未到达上游（官方 API 2026-10-21 仍支持该字段）
```

请求体里**只有** `model` / `messages` / `max_tokens`。
调用方设置的停止序列与服务等级被无声吞掉 —— 上游照常按自己的默认行为生成，
调用方拿到 200，却等不到那个本该触发的 `stop_reason: "stop_sequence"`。

---

## 五、代码定位

### 5.1 丢弃点

`provider_anthropic.go:171-192` — `dropRemovedFields`：

```go
func (p *anthropicProvider) dropRemovedFields(req *ChatRequest) {
	var dropped []string
	if req.Temperature != nil {
		dropped = append(dropped, "temperature")
	}
	if req.TopP != nil {
		dropped = append(dropped, "top_p")
	}
	if req.TopK != nil {
		dropped = append(dropped, "top_k")
	}
	if len(req.StopSequences) > 0 {          // ← 误删
		dropped = append(dropped, "stop_sequences")
	}
	if req.ServiceTier != "" {               // ← 误删
		dropped = append(dropped, "service_tier")
	}
	...
}
```

调用点只有一处：`provider_anthropic.go:245`（`buildPayload` 内）。而 `buildPayload`
被三处使用——`673`（unary）、`722`（stream）、`1002`（另一条发送路径）——
因此**单点修复即覆盖流式与非流式**，无需分头改。

### 5.2 第二处障碍：保留键

`request.go:439-447` — `anthropicReservedPayloadKeys`：

```go
anthropicReservedPayloadKeys = map[string]bool{
	"model": true, "messages": true, "system": true, "stream": true,
	"max_tokens":  true,
	"temperature": true, "top_p": true, "top_k": true, "stop_sequences": true,
	"metadata": true,
	"tools":    true, "tool_choice": true, "thinking": true,
	"output_config": true,
}
```

`stop_sequences` 在集合内，这一条**本来是对的**，修复时不要动（详见 7.2）。
`service_tier` 不在集合内。

### 5.3 现有测试锁定了错误行为

`provider_anthropic_test.go:82-89`：

```go
// temperature / stop_sequences were removed in API 2026-10-21 and must
// not reach the wire.
if _, has := pl["stop_sequences"]; has {
	t.Fatal("stop_sequences must be omitted (removed in 2026-10-21)")
}
```

修复时这条断言需要反转。

### 5.4 文档共三处（不是两处）

- `docs/guide.md:212`：「Anthropic 在 2026-10-21 版本移除了采样参数（`temperature`/`top_p`/`top_k`/`stop_sequences`），SDK 对 Anthropic 协议不再发送这些字段。」
- `docs/protocols.md:14` 协议对照表：「`stop` | 已移除（2026-10-21 版本不再接受，SDK 静默忽略并记日志）」
- `docs/guide.md` 的字段表：`StopSequences` 注释写作「Anthropic 2026-10-21 起移除，均丢弃并记日志」；
  `request.go:358` 的字段本身没有注释，建议一并补上正确语义。

另：`CHANGELOG.md:22-25` 那段「五个字段（传了直接 400）」需在 v1.3.2 条目里更正
（已发布版本的 changelog 不回改，用新条目说明）。

### 5.5 解析侧无需连带改动

`provider_anthropic.go:1148-1160` 的 `mapAnthropicStop` 已把 `"stop_sequence"` 与
`end_turn` 并列映射为 `StopEnd`。恢复字段后 `StopReason` 解析不需要任何改动。
写在这里是为了省一轮往返。

---

## 六、影响面与严重度

1. **停止序列静默失效**：客户端设置 `stop_sequences` 后，Anthropic 上游不会
   在命中时停止，而是继续生成到 `max_tokens`。对依赖停止词做输出截断的
   调用方（大量 agent 框架用它做结构化输出截断），结果是**内容变长**、
   `stop_reason` 从 `stop_sequence` 变成 `end_turn`，且无任何错误。
2. **服务等级失效**：`service_tier` 无法下发，Anthropic 的
   `auto` / `standard_only` 分流不可用。
3. **影响面限于 Anthropic 协议**：OpenAI Chat / Responses 两条路径由
   `provider_openai_chat.go:137` / `provider_openai_responses.go:293` 自行处理，
   `stop_sequences`（Chat 侧转成 `stop`）与 `service_tier` 表现正常。
4. **无告警**：`dropRemovedFields` 只写 `Debug` 级日志，默认日志级别下不可见。

定级「中高」而非「中」的理由：不只是字段丢失，而是**对外行为在小版本内无提示回归**
（v1.3.0 之前工作正常），失效方向是「多烧 token + 调用方截断约定失效」，
且不会自己暴露，只能靠账单或下游内容异常发现。

---

## 七、建议的修法

### 7.1 必改：恢复转发

`provider_anthropic.go` — `buildPayload` 中把两个字段按 Anthropic 原生名写回：

```go
if len(req.StopSequences) > 0 {
	payload["stop_sequences"] = req.StopSequences
}
if req.ServiceTier != "" {
	payload["service_tier"] = req.ServiceTier
}
```

`dropRemovedFields` 收敛为只处理三个采样字段，建议改名 `dropRemovedSamplingFields`，
注释更正为「Anthropic 在 2026-10-21 移除了 temperature/top_p/top_k；
stop_sequences 与 service_tier 仍然有效」。

### 7.2 必改：保留键只增不减

- `stop_sequences`：**保持原样，不要删**。`request.go` 上方的约定是「保留集精确覆盖
  各适配器 payload builder 实际写入的顶层字段」；7.1 恢复渲染后它必须继续留在集合内。
  历史也印证这一点：v0.4.0（`79c5df3`）时它同时出现在渲染逻辑与保留集里，两者配套存在。
- `service_tier`：**新增**到 `anthropicReservedPayloadKeys`。理由同上——一旦由适配器渲染，
  它就该被标为 SDK 管理字段。

删掉 `stop_sequences` 不会带来逃生口，只会更糟：冲突检查发生在渲染之后
（`request.go:463-473`，`maps.Copy` 在 472），把已渲染的键移出保留集等于允许 `Extra`
**静默覆盖** SDK 写入值，从「报错」退化成「悄悄改」。

### 7.3 必改：测试

- 反转 `provider_anthropic_test.go:82-89`：断言 `stop_sequences` 到达 wire。
- 补 `service_tier` 到达 wire 的断言。
- 补一条 `buildPayload(req, true, ...)`（stream=true）的同类断言，防止日后有人在
  流式路径上重新加回过滤。

### 7.4 必改：文档三处 + changelog

见 5.4。口径统一为「Anthropic 在 2026-10-21 移除了 temperature/top_p/top_k；
stop_sequences 与 service_tier 仍支持」。

### 7.5 必改：复现目录不得污染根模块

`work/anthropic-stopseq-repro/` 原先没有独立 `go.mod`，被根模块的 `go test ./...`
（`Makefile:10/13/16/19`）扫到，导致 `make test` 直接失败——报告自带的复现用例
把仓库 CI 打红了。已在该目录补独立 `go.mod` + `replace` 指向根模块。
以后往 `work/` 放实验代码，沿用同一方式。

### 7.6 可选：`speed` 字段

官方 `MessageCreateParamsBase` 另有 `speed?: 'standard' | 'fast' | null`
（fast 模式部分模型需 `fast-mode-2026-02-01` beta 头）。
`ChatRequest` 当前无对应字段。这是**新增能力**而非缺陷，建议另开 PR。

### 7.7 可选：补 `ChatResponse.ServiceTier` 的 Anthropic 回显

`response.go:33-34` 定义了 `ServiceTier`，OpenAI 两条路径会回填
（`provider_openai_chat.go:967`、`provider_openai_responses.go:1193`），
但 Anthropic 的 `anthroResponse`（`provider_anthropic.go:1254` 附近）没有该字段，
回显恒为空。恢复请求侧 `service_tier` 后，这个字段在统一类型里仍是半残的。
可与本修复一并处理，也可单列。

---

## 八、我们这边的临时绕行

在 SDK 修复发布前，rosetta-gateway 侧自救，避免功能阻塞：

1. 对 Anthropic 目标，通过 `WithExtraOverrideKeys("stop_sequences")` 放行该键，
   并在网关的 Anthropic 出站路径上用 `Extra` 显式补回 `stop_sequences`。
   该绕行成立的前提正是：适配器当前不渲染此键，所以不存在与 SDK 渲染值打架的问题。
2. `service_tier` 暂不透传 —— 网关对 `Extra` 覆盖有一道「只放行少数几个键」的护栏
   （见网关仓库 `internal/upstream/extra_override_test.go`），放开范围会扩大攻击面。

该绕行在 SDK 修复后移除。注意：7.1 落地后第 1 条必须同步撤掉——届时适配器会渲染该键，
再叠加 `Extra` 覆盖就是覆盖 SDK 写入值，属于双重来源。

---

## 九、附：核对方法与证据边界

结论只来自官方一手资料（官方 SDK 仓库的源码、类型定义与 MIGRATION.md），未使用任何二级来源：

- `anthropics/anthropic-sdk-python` → `MIGRATION.md`（移除参数表：只有
  `temperature` / `top_p` / `top_k`）
- `anthropics/anthropic-sdk-python` → `src/anthropic/types/message_create_params.py`
  （`MessageCreateParamsBase`：含 `stop_sequences`、`service_tier`，无三个采样参数；
  `stop_sequences` 的 docstring 以现在时描述 `stop_reason: "stop_sequence"`）
- `anthropics/anthropic-sdk-typescript` → `src/resources/messages/messages.ts`
  （`MessageCreateParamsBase` 字段全集，见 2.1(a)）
- `anthropics/anthropic-sdk-typescript` / `anthropic-sdk-go` → `MIGRATION.md`
  （内容为指向 `platform.claude.com/docs/en/api/migration-2026-10-21` 的链接）
- npm `@anthropic-ai/sdk` 的 `1.8.0` / `1.0.0` / `0.137.0` 三版类型文件差分（2.1(b)）
- Go / Java / Ruby / PHP SDK 的 `MessageNewParams`（报告人核对，见 2.1(e)）

证据边界（详见第二节）：**文档级证据，无官方端点实测**。
`platform.claude.com/docs/en/api/migration-2026-10-21` 与 `docs.anthropic.com`
在本报告核对环境下无法访问（前者被区域重定向到
`anthropic.com/app-unavailable-in-region`）；直连 `api.anthropic.com` 返回
`403 forbidden: Request not allowed`（区域限制）。故本节改用
**官方 SDK 仓库内的生成物**（类型定义、MIGRATION.md）作为一手依据 ——
这些文件由官方从同一份 OpenAPI 规范生成，与该文档页同源。
中转站实测见 2.4：只能证明「不会因此报错」，不具证据力。
若维护者能在海外出口完成 2.3 的两次调用，结论可升级为实测级；
若实测为 400，本报告结论按实测作废。
