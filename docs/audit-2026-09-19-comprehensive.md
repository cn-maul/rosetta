# Rosetta 全面审计报告（2026-09-19，第三轮）

- **基准**：`main` @ `852c00f`（含 #1–#11 修复后工作树）。
- **范围**：全部非测试源码（~6750 LOC）——core / providers / stream·sse·httpx·jsonx·errors / auxiliary。
- **方法**：4 个并行只读审计（各读全文 + `go vet` + `go test ./...` + 本地 `httptest` 复现，仓库代码零改动、未跑 git）。高/中危逐条回到当前源码复核。
- **基线**：`go build ./...` 通过；`go test ./...` 通过；`go vet ./...` 干净；`go test -race ./...`（短路径绕过后）全绿。
- **背景**：前两轮（09-13 F01–F13、09-19 #1–#11）已修复项不在本报告重复。本报告为**新发现**。

严重级别：P1 = 发布前应修的正确性/安全问题；P2 = 特定输入/配置下可复现的功能、静默数据丢失或防护失效；P3 = 边界/健壮性/可诊断性。"已核"= 本次回到源码确认；否则标注为审计复现或推理。

## 结论摘要

两轮加固后，核心并发契约（stream 锁、onEnd 锁外、SSE 有界读、错误脱敏主干、httpx 幂等/退避、aux wire 严格解码）总体稳固，未复现新的数据竞争或死锁。残留风险集中在三条主线：

1. **凭据在跨主机重定向下泄露**（P1，唯一被两个独立审计同时命中，且 repo 自身注释已承认该风险但只在探测路径修了）。
2. **解码侧的静默成功 / 静默数据丢失**（多个 P1/P2）：OpenAI 忽略 `refusal`/`function_call`、Responses `error` 信封读错、严格字段类型让整个 payload 失败、Responses 流式丢整项。
3. **"保护性"防护被旁路或失效**（多个 P2/P3）：Anthropic 断点上限是死代码、截断误报、200-非-SSE 吞成截断、httpx 预算耗尽回吐已关闭 body、redaction 可被越界浮点绕过、上下文门可被 int64 溢出与估算盲区绕过。

> 排除两条伪线索：SSE「无 data 的事件不分发」经核**符合 WHATWG 规范**，非缺陷；某审计引用的「09-26 #2」**文档不存在**。某审计提到的「P0 编译中断（StreamStopSignal）」为陈旧信息，当前树无此符号、`go build` 通过。

---

## 修复状态（2026-09-20）

按用户「都修」指令，全部 43 条发现已处理，`go build ./... && go vet ./... && go test ./...` 与 `go test -race ./...` 全绿；回归测试落在 `audit_comprehensive_test.go`（A/B/C/D/E 组）与 `audit_group_f_test.go`（F 组）。

- **A1–A3（P1）· 已修**：跨主机重定向在 `httpx.New()` 与 `WithHTTPClient` 拷贝路径统一装 `CrossHostSafeRedirect`；OpenAI Chat/Responses 解码接住 `refusal`→`BlockText`、`function_call`→`BlockToolCall`；Responses `error` 事件优先读嵌套 `error` 信封、回退扁平。
- **B1–B16（P2）· 已修**：Anthropic 断点计数命中 `[]map[string]any`；纯 `io.EOF` 且已见语义终止不再误判截断；200-非-SSE 走 `bufferedJSONResponse`/`bufferedStream`；`anthroUsage`→`FlexInt64` + `FlexJSONString`；Responses 流式按 `item_id`/`output_index` 归并不丢项；httpx 预算耗尽分支的 `drain` 移到确要 sleep 之后（body 保持打开）；`redactJSON` 用 `UseNumber` 且回退路径必掩码；`streamCore.apply` 改 `strings.Builder` 摊还 + 字节/块数上限（`ErrStreamOverflow`）；**B9：Anthropic 缓存量并入 `InputTokens`/`TotalTokens`（`CachedInput` 为子集），跨协议 `Total` 语义对齐——此为有意的语义变更**；零值 `MemoryUsageTracker` 惰性建 map；`ListModels` 缺 `data` 报协议错误不再清空远端层；`DetectClient` 尊重显式 `WithProtocol`；`validate` 拒 NaN/Inf/工具/Extra；token 字段上限 + 不回绕的上下文门；估算器计 signature/redacted；手动层同 id 字段级合并。
- **C1–C24（P3）**：C1/C8/B12 前序组已修，其余 C2–C7、C9、C10、C11、C12（`//go:build !nojsonv2` 守卫）、C13–C21、C23、C24 均已修。
- **C22 · 已修（2026-09-20）**：已对 Anthropic 官方 extended-thinking 文档复核并实现。交错思考的 `interleaved-thinking-2025-05-14` beta 头现在按**已构建的 payload** 判定（payload 同时含 `thinking` 与 `tools` 时附带，正是官方限定的 Messages API 工具用法），且与扩展缓存 TTL 的 beta **合并为一个逗号分隔头**（此前逐个 `Set` 会互相覆盖）。新选项 `WithInterleavedThinking(v)` 可强制开关——经 Bedrock / Vertex 转发时必须设 `false`，因为这两家会拒绝白名单外模型的该头，而 Claude API 对任何模型都接受并忽略不支持者。复核结论见 `docs/protocols.md` 的「Anthropic beta 头」。

---

## P1

### A1 · 生产 HTTP 客户端跨主机重定向时原样重发 `x-api-key`（凭据 + 请求体泄露）
- `internal/httpx/httpx.go:56-64`（`New()` → 裸 `&http.Client{}`，无 `CheckRedirect`）、`client.go:77-80`（`NewClient` 采用默认或原样套用 `WithHTTPClient`）；auth 头 `provider_anthropic.go:42-50`；对照 `detector.go:42-54`（守卫**只在探测路径**）。
- **What**：net/http 跨主机重定向时只自动剥离 `Authorization`/`Cookie`/`Proxy-*`，**从不剥离 `x-api-key`**——`detector.go:39-41` 的注释本身已点明这一点，但 `blockCrossHostRedirect` 只装到 `newProbeClient`，从未装到 SDK 默认或用户传入的 `http.Client`。Anthropic 协议每一次真实调用（Chat/Stream/Embed/Rerank/ListModels）都会把 key 发给重定向目标；307/308 还会连请求体（含 prompt）一起重放。
- **触发**：Anthropic 协议 client，端点回 `302 Location: http://<另一主机>` → 目标收到 `x-api-key: sk-ant-...`。
- **影响**：能被一次 3xx 影响（反代开放重定向、DNS/CDN 误配、纯 http 端点重绑定）即等于向第三方泄露 Anthropic key。OpenAI（Bearer）用户受 stdlib 保护，Anthropic 用户不受保护。
- **修复**：在 `httpx.New()` 装上跨主机 `CheckRedirect`；对 `WithHTTPClient` 走"拷贝后强制注入 `CheckRedirect`"（同 `DetectClient` 的做法）；并在 `attempt()` 里当 `req.URL.Host != call.URL` 主机时主动删除 `x-api-key`/自定义 auth 头（自定义 client 无法强制 `CheckRedirect`）。
- **已核**（源码确认三处；两审计独立复现）。

### A2 · OpenAI Chat/Responses 解码丢弃 `refusal` 与旧版 `function_call` → 看似成功的空回复
- `provider_openai_chat.go:677-687`（`oaResponse.Choices[].Message` 仅 `Content`/`ReasoningContent`/`ToolCalls`，无 `Refusal`/`FunctionCall`）；Responses 内容仅收 `output_text`（`provider_openai_responses.go:686-691`）。
- **What**：OpenAI assistant 消息带 `refusal:string|null`（`content` 为 null），旧版 `function_call:{name,arguments}` 仍被 API 与多数 Anthropic→OpenAI 翻译器返回。字段被无声忽略。
- **触发**：`{"choices":[{"message":{"content":null,"refusal":"I can't"},"finish_reason":"stop"}]}` → `err=nil, Content=[], Text()=""`；`{"message":{"content":null,"function_call":{"name":"get_weather","arguments":"{...}"}},"finish_reason":"function_call"}` → `err=nil, Content=[]` 但 `stop=tool_use`（`mapOpenAIStop` 把 `function_call`→`StopToolUse`，`provider_openai_chat.go:574`）。
- **影响**：整段回答蒸发，或调用方被告知"调工具"却拿不到任何 tool call → agent 循环停摆/无限重问。调用方无从察觉。
- **修复**：wire 结构加 `Refusal`、`FunctionCall{Name,Arguments}`；refusal 折成 `BlockText`（或 `ChatResponse` 上的独立标志），`function_call` 折成 `BlockToolCall`；当 content/tool_calls/reasoning 全空但有 refusal 时不得默认 `StopEnd`。Responses `refusal` 内容部分同理。
- **已核**（结构体源码确认；字段名来自公开 OpenAI schema）。

### A3 · Responses `error` 流事件按扁平结构解析，丢失整条错误信息
- `provider_openai_responses.go:552-555`（读 `ev.Message`/`ev.Code`），`oaRespEvent` 无 `Error` 字段（`:659-672`）。
- **What**：OpenAI `error` 事件把详情嵌在 `error` 下：`{"type":"error","error":{"message":..,"type":..,"code":..}}`。当前按扁平读，拿到空串。同文件 `response.failed`（`:545-549`）却正确读 `ev.Response.Error`——三协议里 chat（`oaChunk.Error`）、anthropic（`anthroChunk.Error`）也都读嵌套形式，唯独此处不一致。
- **触发**：`data: {"type":"error","error":{"message":"Rate limit exceeded for TPM","type":"rate_limit_error","code":"rate_limit_exceeded"}}` → `APIError{Status:200, Type:"api_error", Code:"", Message:""}`。
- **影响**：诊断信息全丢、`Type` 被伪造；调用方无法区分限流/鉴权/参数错误，`isValueRejection`/重试策略看到空串。
- **修复**：给 `oaRespEvent` 加 `Error *oaErrorBody \`json:"error"\``，`case "error"` 优先用它、回退到扁平以兼容非规范实现。
- **已核**（源码确认信封不一致；结果为审计复现）。

---

## P2

### B1 · Anthropic「最多 4 个 cache 断点」守卫是死代码
- `provider_anthropic.go:78-99`（`countCacheControl` 只递归 `map[string]any` 与 `[]any`），调用点 `:214-216`；容器实际构造为 `[]map[string]any`（`tools` `:191`、`messages` `:229`、`system` 数组）。
- **What**：Go 类型切换下 `[]map[string]any` **不匹配** `case []any`，遍历到 payload 根即停止、恒返回 0。
- **触发**：一条含 6 个带 `EphemeralCache()` 的文本块 → `err=nil`，6 个 `cache_control` 照样上 wire。反例：同样 6 个断点若经 `Extra` 以 `[]any` 注入则被计数并拒绝——证明遍历只作用在 SDK 从不产出的类型上。无测试覆盖该守卫。
- **影响**：本应本地报 `ErrInvalidRequest` 的场景变成上游 400 + Anthropic 隐晦措辞。属 09-19 #10 引入但未生效的防护。
- **修复**：改为构造期计数（把计数器穿透 `putCC`/tools 循环/`renderAnthropicSystem`），或统一容器为 `[]any`；补 5 块/5 工具/5 system 段的表驱动测试。
- **已核**。

### B2 · 已收到语义终止信号（`finish_reason`/`stop_reason`）的流仍被判为截断
- `provider_openai_chat.go:487-490`；`provider_anthropic.go:636-640`；`provider_openai_responses.go:439-443`。
- **What**：EOF 分支只要没见 `[DONE]`/`message_stop`/`response.completed` 就一律 `truncated=true`，无视 chunk 里已到的 `finish_reason:"stop"`。而 `io.EOF`（此处）本已表示 HTTP body 干净结束。
- **触发**：chat 流末 `data: {…,"finish_reason":"stop"}` + 空行后关闭，无 `data: [DONE]`（OpenAI 兼容网关、翻译器常见）。
- **影响**：`Collect()` 同时给出完整文本、`stop=end` 与 `ErrStreamTruncated` → 调用方对非幂等完成重试（重复消费、重复工具副作用）或丢弃正确答案。
- **修复**：当为纯 `io.EOF` **且**已见语义终止（`stop != ""`；Anthropic 已到 `message_delta`）时，置 `ended=true` 但 `truncated=false`；`io.ErrUnexpectedEOF` 与"无任何终止信号"保持现状。
- **已核**。

### B3 · HTTP 200 携带 JSON（非 SSE）错误体被吞成"stream truncated"
- `provider_openai_chat.go:417-420`；`provider_anthropic.go:569-572`；`provider_openai_responses.go:176-187`。
- **What**：流路径只按 `StatusCode==200` 分支，不查 `Content-Type`。许多网关对 `stream:true` 回缓冲的 `200 application/json` 错误；SSE 扫描找不到 `data:` 行 → EOF → 把本可解析的 `error` 对象整体丢弃。
- **触发**：`200`, `Content-Type: application/json`, `{"error":{"message":"model_not_found","type":"invalid_request_error"}}`。
- **影响**：三协议均报 `ErrStreamTruncated`、`stop=other`、空文本——把不可重试的配置错误包装成可重试的网络故障，重试循环空转。
- **修复**：2xx 且 `Content-Type != text/event-stream` 时 `readBody` 并走 `parse{OpenAI,Anthropic}Error`（或按正常补全解析）。
- **已核**。

### B4 · 单个异常字段类型让整个 payload 解码失败，违背 jsonx"宽松解码"契约
- `provider_openai_chat.go:647-655`（`oaToolCall`：`ID`/`Function.Arguments` 为严格 `string`）、`:665`/`:680`（`ReasoningContent *string`）；`provider_openai_responses.go:624-638`（`Arguments string`）；`provider_anthropic.go:836-841`（`anthroUsage` 用裸 `int64`，非 `jsonx.FlexInt64`）。
- **What**：`internal/jsonx` 包文档承诺"单个怪字段绝不使整个 payload 失败"，`content`/`usage`/`finish_reason` 遵守，但工具调用字段、`reasoning_content`、**全部 Anthropic usage 计数**是严格标量。`function.arguments` 声明为 `string`，而 Anthropic 风格网关天然在 `input` 处回吐**对象**。
- **触发**：`tool_calls[0].function.arguments` 为 `{"city":"NY"}` → `json: cannot unmarshal object into Go struct field …arguments of type string`，连同文本一起丢；Anthropic 流式 `message_start.usage` 字符串化 → 首个事件即 malformed、流未收文本即死。
- **影响**：一处字段形状差异即整条响应失败。
- **修复**：`anthroUsage` 用 `FlexInt64`；为工具 `arguments`/`id` 增设"接受字符串或任意 JSON 值（非字符串再紧凑重序列化）"的 jsonx 类型；绝不让单个标量使 `Unmarshal` 失败。
- **已核**（结构体源码确认）。

### B5 · Responses 流式在缺增量事件/`output_index` 时丢整项，工具调用无稳定键
- `provider_openai_responses.go:469-520`（done 折叠 + 参数增量按 `ev.OutputIndex`）、`oaRespEvent.Item` 无 `content`（`:659-672`）、`default:` 忽略 `response.text.delta`。
- **What**：三处独立静默丢失——(1) `output_item.done` 仅在 `!announced[index]` 时补投，若网关发了空 `added` + 完整 `done` 而无 `…arguments.delta`，最终 arguments 被跳过；(2) `Item` 无 `content`，仅经 `output_item.done` 定稿的 message 项文本永不产出；(3) 忽略仍被服务的 `response.text.delta` 别名。(4) 参数增量仅按 `output_index`（int，无法区分"缺省"与"合法的 0"）归并，从不解析同事件已带的 `item_id`。
- **触发**：见各例（审计复现）；例如并行工具调用只带 `item_id` 的 delta → `[0]id=c1 args=""`, `[1]id="" args="{a:1}{b:2}"`, `[2]id=c2 args=""`。
- **影响**：工具参数或整段回复静默消失，调用方以空参数执行工具或什么都不打印（后两者还表现为"成功的空回合"）。
- **修复**：按 `output_index` 记录是否见过 delta，`output_item.done` 时若未见则补投定稿值（新增"替换 arguments"事件类型避免串联）；`Item` 加 `Content[]{Type,Text}`；接受 `response.text.delta`/`response.refusal.delta`；解析 `item_id` 并以其为参数增量稳定键，缺 `output_index` 时归到最近未定稿的 function call。
- **审计复现**（高可信，属 #9 done 折叠修复的残留缺口）。

### B6 · 重试预算耗尽时 `Do` 回吐已关闭的 response → 真实 APIError 被伪传输错误顶替
- `internal/httpx/httpx.go:82-97`——`drain(resp)`（`:91`）在预算判断（`:95-96`）**之前**执行。
- **What**：预算耗尽分支 `return resp, err` 时，`drain` 已读过 8 KiB 并 `Close()` 了 body；契约（`:66-68`）却称"调用方拥有返回的 body"，所有调用方随后 `readBody(resp,…)`。
- **触发**：`WithMaxRetries(30)` + 持续 503；或 `WithMaxRetries(4)` + 每个 429 带 `Retry-After:60`（默认 `MaxRetries=2` 触不到，故测试漏过）。
- **影响**：对已关闭 body `ReadAll` 得 `http: read on closed response body` → 归类 `TransportError`，真实 429/500 的 `APIError`（status、`Retryable`、provider message、`Retry-After`）全丢，且以分钟级停顿收场。（body 不泄漏：双 `Close` 返 nil。）
- **修复**：把 `drain(resp)` 移到"确实要 sleep"之后，或保留 `io.ReadAll(io.LimitReader)` 的字节挂到返回 response 供调用方解析最终错误。
- **已核**（源码顺序确认）。

### B7 · 脱敏可被"`json.Valid` 通过而 `Unmarshal` 失败"绕过；`float64` 往返损坏 `Raw`
- `errors.go:164-167`（`safeTruncateBody` 以 `json.Valid` 为闸）+ `:191-201`（`redactJSON` 遇 `Unmarshal` 错误 `return raw`）。
- **What**：`json.Valid` 与"解入 `any`"不一致：`{"error":{"api_key":"sk-ant-…","budget":1e999}}` 里 `1e999` 语法合法但 `Unmarshal` 溢出报错 → `redactJSON` 原样返回，敏感键脱敏与末尾 `secretRe` 两层全跳过，凭据落进 `Raw`（注释自陈 `Raw` "进入日志与遥测"）。即使成功脱敏，经 `float64` 重序列化也会损坏大整数（`…7890`→`…7000`）与浮点。
- **影响**：攻击者可控输入即可绕过一项安全控制；`Raw` 号称"原始响应体"实则被篡改。
- **修复**：失败回退路径不得直接返回 `raw`——至少先 `secretRe.ReplaceAll` + 逐字节 `isSensitiveKey` 扫描，再退化为掩码 JSON 串；用 token 级重写（`redactValue` on tokens）保证数字按字节往返，令 `Valid`/`Unmarshal` 分歧无法关闭脱敏。
- **已核**（stdlib 行为，审计在本机复现）。

### B8 · `streamCore.apply` 二次方拷贝且对累计字节无上限 → 恶意流放大
- `stream.go:253-315`（`appendBlockText`/`appendBlockThinking`/`appendToolDelta` 用 `+=` 串联），叠加 `Partial()` 每次 O(n) 克隆（`:189-197`）；`toolPos` 随攻击者指定的 `ToolIndex` 无界增长。
- **What**：Go 字符串 `+=` 全量重分配，k 个 delta 成本 O(k²)；无对累计文本/arguments 或块数的上限，SSE 单事件可达 16 MiB 且事件数无上限。
- **触发**：单 goroutine：流式端点/坏网关猛发小 delta。审计实测 64k×40B（2.5 MB）纯 memcpy ≈ 14.3s，输入翻倍耗时×4。
- **影响**：正是 `bodyLimit`/`auxBodyLimit` 想挡的威胁，流式路径仍敞开——每流 CPU/GC 放大与无界堆增长；每事件调 `Partial()` 再加一层 O(n²)。
- **修复**：块内用 `strings.Builder`/`[]byte` 摊还累积（读时再成形），在 `apply` 加 `maxStreamBytes` 上限、超限以独立哨兵失败，并对不同 `ToolIndex` 数量设上限。
- **审计实测**（代码形状已核）。

### B9 · Anthropic usage 把缓存量排除在总量外，跨协议 `TotalTokens` 语义不一致
- `provider_anthropic.go:843-851`（`toUsage`：`Total=input+output`）对照契约 `usage.go:12-24`。
- **What**：Anthropic 的 `input_tokens` 是**非缓存**输入，`cache_read`/`cache_creation` 为**不相交**量；OpenAI 的 `prompt_tokens` 已含缓存为子集。统一 `Usage` 取 OpenAI 语义，但 Anthropic 映射把缓存全丢出总量。
- **触发**：`{input:10,output:5,cache_read:900,cache_creation:200}` → `Total=15`（真实 1115），且 `CachedInput > Input` 破坏一切命中率/聚合。
- **影响**：所有带 prompt 缓存的 Anthropic 请求在 tracker 聚合里低报；跨协议 `Input`/`Total` 对比变成张冠李戴且无文档警告。
- **修复**：统一约定——Anthropic 侧把缓存并入 `InputTokens`/`TotalTokens`（`CachedInput` 留作子集信息，与 OpenAI 齐平），或在 `usage.go` 明确记录不相交语义并让 tracker 总量并入。
- **审计复现**（`toUsage` 源码已核）。

### B10 · 零值 `MemoryUsageTracker` 首次 `Record` 即 nil-map panic
- `usage.go:79-117`（`byModel`/`byProto` 仅在 `NewMemoryUsageTracker` 初始化，`Record` 无条件写 `:111-116`）；`:76` 文档称"safe for concurrent use"。
- **触发**：`var t rosetta.MemoryUsageTracker; …WithUsageTracker(&t); c.Chat(…)` → "assignment to entry in nil map"。
- **影响**：导出类型零值不可用，崩溃落在 `Client.Chat` 深处；`Snapshot` 零值却正常，掩盖陷阱。
- **修复**：`Record` 内 `t.mu` 下惰性 `if t.byModel==nil {…}`，或文档明确"必须经构造函数创建"。
- **已核**。

### B11 · `ListModels` 把缺失/空 `data` 当作成功，并以空结果覆盖远端目录
- `provider_openai_common.go:68-84`（值类型 `Data` slice）→ `client.go:184-191` → `registry.go:105-125`（`SetRemote` 替换语义）。
- **触发**：端点回 200 `{"object":"list"}`（无 `data`）→ `ListModels` 仅返回手动条目、`err=nil`，先前学到的远端条目被清空。
- **影响**：静默能力缺失伪装成成功；一次不同形状/抖动响应即降级注册表状态，`refreshModels` 等待方共享该"成功"。
- **修复**：`Data *[]…`，字段缺失/null 时报协议错误（与 embedding/rerank 的 null 严格性一致）；可选地拒绝用空结果覆盖远端层。
- **已核**（机制源码确认）。

### B12 · `DetectClient` 静默覆盖调用方显式的 `WithProtocol`
- `detector.go:153-157`（探测结果 `WithProtocol(proto)` 追加在用户选项之后，后设者胜；`_ = classified` 丢弃"无法分类"信号）；与 `:25-27` 文档"可用 WithProtocol 覆盖"矛盾。
- **触发**：`DetectClient(…, WithProtocol(ProtoOpenAIResponses))` 实际得 `openai-chat`、`err=nil`。
- **影响**：显式钉死 Responses 的调用方拿到错误协议，后续全走 `/chat/completions`，故障表现为无关的 provider 错误。
- **修复**：`settings` 记 `protocolSet bool`，显式设置时跳过探测（或仅填默认）；至少探测与显式选项冲突时告警/返回，并修正文档。
- **已核**（选项顺序源码确认）。

### B13 · `ChatRequest.validate` 放行 JSON 序列化会拒绝的值，故障错分类为 `TransportError`
- `request.go:224-262`（`NaN` 过不了 `<0||>2` 比较故被放行 `:233/:236`；`Tools[i]` 仅校验 `CacheControl`；`Extra` 从不试序列化）。
- **触发**：`Temperature:Float(NaN)` → 后续 `json.Marshal(payload)` 失败，httpx 标"permanent"包成 `*TransportError`，`errors.Is(err,ErrInvalidRequest)=false`；`Extra` 含 chan/自引用同理。
- **影响**：本地编程错误被报成网络故障，`errors.Is/As` 与重试/告警分支失效，且把未触网的完整 URL 塞进错误串。
- **修复**：显式拒非有限浮点（`IsNaN/IsInf`）；`json.Valid(t.Parameters)` 且 `t.Name` 非空；对 `Extra`（或 prepare 期的 payload）做廉价可序列化性探测，返回 `ErrInvalidRequest`。
- **审计复现**（校验条件源码已核）。

### B14 · `MaxOutputTokens`/`BudgetTokens` 无上限，int64 溢出绕过严格上下文门并产生负 `max_tokens`
- `request.go:230/245-248`（仅拒负值）、`client.go:302`（`in<=window && (out<=0 || in+out<=window)`——`out` 近 `MaxInt` 时和回绕为负，误判"放得下"），`provider_anthropic.go:125-129`（`b+4096` 亦回绕）。
- **触发**：`WithStrictContextCheck(true)` + `MaxOutputTokens: math.MaxInt` → `err=nil`（`300000` 则正确报错）；Anthropic `Thinking.BudgetTokens:MaxInt` → wire `"max_tokens":-9223372036854771713`。
- **影响**：严格模式用户失去"本地拦截超限请求"的保证；溢出场景发出 provider 非法的负 `max_tokens`，白跑一次得隐晦 400。
- **修复**：`validate` 给这两个字段设合理上限（如 ≤`1<<31-1`）；门判断改为不回绕形式 `in>window || (out>0 && out>window-in)`。
- **已核**（条件形式源码确认）。

### B15 · 估算器忽略 `redacted_thinking` 与 thinking `Signature`，违背"绝不低估"契约
- `estimator.go:57-80`（`BlockThinking` 仅计 `b.Thinking`，无 `b.Signature`；**完全无 `BlockRedactedThinking` 分支**）；契约文档 `estimator.go:31-35`；redacted 块 payload 存于 `Thinking`（`provider_anthropic.go:928`）。
- **触发**：Anthropic `ContextWindow:100` + 严格模式，历史含 400KB 的 redacted/signature base64 → `err=nil`（同样字节作纯文本则正确报 `ErrContextTooLong`）。
- **影响**：多轮 extended-thinking 会话（正是该门最需要的场景）系统性低估，默认模式无告警、严格模式不拦截，改为上游 400。
- **修复**：`BlockThinking` 加 `EstimateTokens(b.Signature)`；新增 `BlockRedactedThinking` 分支计 `EstimateTokens(b.Thinking)`。
- **已核**（类型与 switch 源码确认）。

### B16 · 稀疏 `WithModelInfo` 条目整体覆盖同 id 的 `WithModelsFile` 条目（手动层内不做字段合并）
- `registry.go:81-103`（`layer[m.ID]=cloneModelInfo(m)` 整条赋值），`client.go:93-96`；与 `docs/models.md`（"手动层内部条目合并、id 冲突时 WithModelInfo 优先"）及 `mergeInfo` 跨层字段级语义矛盾。
- **触发**：models.json 设 `context_window:131072,aliases:["mm"]` + `WithModelInfo{ID:"my-model",SupportsThinking:true}` → `ContextWindow:0, MaxOutputTokens:0, Aliases:[]`；`ModelInfo("mm")` 报 unknown；百万 token 请求 + 严格模式 → `err=nil`。
- **影响**：看似"追加"的配置静默清零上下文门、输出上限与所有别名，即禁用文档承诺的安全网。
- **修复**：手动层同 id 走字段级合并（file 为 low、显式 WithModelInfo 为 high 经 `mergeInfo`），或对差异非零的重复 id 报错/告警；统一 `client.go:93` 与 `docs/models.md`。
- **已核**（行为源码确认）。

---

## P3（择要）

- **C1 错误串泄露 URL userinfo**：`url.go:31-41` `displayEndpoint` 只剥 `RawQuery`，`validateEndpoint` 拒绝消息用 `%q` 回显原始 URL，故 `https://sk-...:pw@host` 完整出现在返回的 error（`client.go:53/57`、`detector.go`、`provider_anthropic.go`）。修复：`displayEndpoint` 一并掩码 `u.User`。**已核**。
- **C2 `BlockImage.ImageURL` 接受任意 scheme**：`message.go:244-247` 仅查非空，`file://`、裸 base64、`data:text/html` 全透传给 `image_url.url`；文件块上轮已加语法校验，图像没有。修复：复用与 `validateFileData` 同源的 scheme/base64 校验。
- **C3 工具 `Arguments` 合法但非对象时静默替换为 `{}`**：`message.go:270-276` 仅 `json.Valid`；`provider_anthropic.go:495-501` `parseToolInput` 把非对象转空 map 无错。模型收到伪造空输入、会话工具配对损坏。修复：要求首个非空字节为 `{`，adapter 报错而非替换。
- **C4 `CacheControl.Type` 从不校验**：`request.go:65-90` 只校验 TTL，`toWire` 原样透传，拼错成上游 400。修复：限定 `{"","ephemeral"}`。
- **C5 `WithMaxTokensField` 从不校验**：`options.go:190-194` 任意非空串成为输出上限键并钉死 sticky（`provider_openai_chat.go:312`），写错名 → 静默失去输出上限（无界成本/延迟）。修复：仅允许 `max_tokens`/`max_completion_tokens`，否则 `NewClient` 报错。
- **C6 `models.json` 未知键/无 id 条目静默忽略**：`registry.go:57-76` 未 `DisallowUnknownFields`，拼错键得零值或丢弃条目，`Known` 又强置 true，`ContextWindow:0` 被当"无限制"。修复：`DisallowUnknownFields` + 空 id 报错。
- **C7 保留-Extra 并集屏蔽了任何 adapter 都不会设的键**：`request.go:161-204`，`tool_choice`/`reasoning` 跨协议一律冲突，只能全局 `WithExtraOverrides(true)`（连其它保护一起关）。修复：按协议定义保留集。
- **C8 探测重定向按 `URL.Host` 字面比较**：`detector.go:46-53` 含端口、区分大小写，`localhost:8080→:9090`、大小写别名、host→IP 全被拒并降级为 OpenAI-Chat 默认；>4MiB 的 `/models` 触发 `ErrBodyTooLarge` 也被当作"传输失败"静默回退（`:86-96/:116`）。修复：`Hostname()` 大小写折叠比较；`classified==false` 时 Warn 并返回错误。
- **C9 `APIError/TransportError.Error()` 明文渲染 URL/path**：`errors.go:51-53/76-79` 打印未掩码的 `e.URL`，而 path 型凭据（`…/proxy/sk-ant-…`）是常见代理配置。修复：`maskSecrets(displayEndpoint(e.URL))`。
- **C10 脱敏键/模式漏非 `sk-` 凭据**：`errors.go:182/204-232`，`access_key`→`accesskey` 不匹配，Azure 32-hex、AWS `AKIA…`、GitHub `ghp_…`、Slack `xoxb-` 逃过两层。修复：扩大子串与正则、敏感键下任意值类型都掩。
- **C11 `ChatStream` 非 `*streamCore` 分支 + attach\* 无锁**：`client.go:163-167` 该（当前死）分支返回已取消且不关 body 的流；`stream.go:111-121` `attachCancel/attachCloser` 无锁写而 `releaseLocked` 有锁读（当前仅靠发布顺序规避）。修复：`!ok` 时关 body 或返 `ErrNotSupported`；attach 取锁。
- **C12 `GOEXPERIMENT=nojsonv2` 下宽松解码硬失败**：`internal/jsonx/jsonx.go` 只实现 v2 入口 `UnmarshalJSONFrom`，无 `UnmarshalJSON`，`"120"` 字符串化数字 → `UnmarshalTypeError` 使整 payload 失败，反转宽松契约。修复：加 `UnmarshalJSON` 垫片或 `//go:build !nojsonv2` 守卫。
- **C13 Responses sticky 丢弃后仍永久丢 `temperature`/`top_p`**：`provider_openai_responses.go:218-237` 采样参数按 `req.Thinking!=nil` 而非"是否真发 `reasoning`"来门；`st.reasoning==false` 时并无冲突仍丢采样。修复：把丢弃逻辑放进 `if st.reasoning`。
- **C14 Responses 宽松化 `sanitize` 忽略 `error.param`**：`provider_openai_responses.go:79-116` 只扫 `Message`，不像 chat 先看 `Param`（`provider_openai_chat.go:294-306`）；`param` 命名被拒字段而 message 泛化时永不匹配，兼容阶梯对该网关整个生命周期失效。修复：把 chat 的 `param` 感知匹配抽进 `provider_openai_common.go` 共用。
- **C15 chat 无 `choices` 的 200 抛未分类 `errors.New` 并丢 usage**：`provider_openai_chat.go:697-699`，无 `Method/URL/RequestID/Raw`，`errors.Is` 无匹配、计费 usage 丢失。修复：返回空的合法 `ChatResponse` 保留 ID/Model/Usage/Raw，或包进哨兵并附上下文。
- **C16 Anthropic 流式 `message_delta` 缺 `output_tokens` 时把基线清零**：`provider_anthropic.go:728-744`（`:734` 对 `OutputTokens` 无条件赋值）。修复：与其余字段一样 `!=0` 才合并。
- **C17 Anthropic thinking/signature 累加忽略 content-block index**：`provider_anthropic.go:702-723` 把 thinking/text/signature 折进"最后一个块"，`ch.Index`（`:872`）未用于非工具块；乱序 emitter 下 signature 落错块、真 thinking 丢签名（重放时被 `:315-319` 丢弃）并产生幻影空 thinking 块。修复：仿 `appendToolDelta` 按 index 归并。
- **C18 一元解码里带内 200 error 丢失请求上下文**：`provider_anthropic.go:913`/`provider_openai_chat.go:694`/`provider_openai_responses.go:679` 调 `apiError(200)` 裸返回，不像流式路径附 `Method/URL/RequestID`，`Error()` 渲染空 method/url、`Retryable=false`。修复：把已在作用域内的 method/url/requestID 传入解码函数。
- **C19 工具名/停止序列无本地校验**：`request.go:254-260`，空名/重名/越界字符/含 `""` 的 `StopSequences` 全部透传 → 上游隐晦 400。修复：`validate` 拒空名、重名、空停止串。
- **C20 `ExtraOverrides` 覆盖后校验缺口**：`embedding.go:103-110/145/157`、`rerank.go:110-113/171`——(a) 仅对 `[]string`/`int` 生效，解析自 JSON 的 `[]any`/`float64` 使有效响应被误判协议错误；(b) `Extra{"documents":[]string{}}` 令 `expected==0`，跳过数量与 index 上界检查，越界 index 直达 `Results` 使调用方切片越界 panic。修复：覆盖再派生走 JSON 往返归一；wire 计数为 0 时硬报错。
- **C21 provider token 数无量级上限**：`usage.go:100-105`，恶意/异常 `total_tokens:MaxInt64` 过"非负"检查并 `+=` 使聚合回绕为负。修复：per-record 超合理阈值（如 `1<<40`）拒绝或饱和。
- **C22 低置信：Anthropic thinking 重放不发 `interleaved-thinking` beta 头**（**已于 2026-09-20 修复**）：`provider_anthropic.go:25-28/314-333`，扩展缓存 TTL 有 beta 门、交错思考没有对应项。**未能从本仓库核实各模型当前 beta 要求，动作前请对 Anthropic 文档复核**。
- **C23 `DisableThinking` 下层否决手层（潜在，文档反向）**：`registry.go:191-194` `low||high` OR 使远端 `DisableThinking` 能撤销手动 `SupportsThinking:true`，与 `docs/models.md`（手动声明不可被远端撤销）相反；当前无 provider 解码器置该位，属潜在。修复：manual-high 时 `out.DisableThinking=high.DisableThinking`。
- **C24 `ThinkingConfig` 文档与 `effort()` 优先级矛盾**：`request.go:17-30` 称"两者都设时 BudgetTokens 在 Anthropic 胜出、他处映射为最近 effort"，但 `effort()`（`:265-277`）`Effort` 非空即返回，OpenAI 上 `BudgetTokens` 从不"胜出"。修复：按文档先判 `BudgetTokens`，或把文档改成"显式 Effort 在非 Anthropic 协议胜出"。

---

## 建议修复顺序

1. **A1**（凭据泄露，两审计独立命中，且修复已在本仓库、只差装错地方）。
2. **A2 / A3 / B1 / B3 / B4**（解码静默丢数据与防护失效，多为局部小改，影响面大）。
3. **B2**（截断误报，注意保留 `ErrUnexpectedEOF` 与"无终止信号"分支）。
4. **B5–B16**（含 httpx 已关闭 body、脱敏绕过、流式放大、usage/tracker/registry 语义）。
5. **C\***（健壮性与可诊断性；C22 先核文档再动）。

每项修复应配公开入口（`Chat/ChatStream/Embed/Rerank`）级回归测试——上述问题恰因现有测试未从公开路径覆盖而漏网。
