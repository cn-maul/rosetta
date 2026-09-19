# 审计修复进度（2026-09-19）

全局审计共 12 项，逐条修复。#1–#10 已完成并 `go test ./...` 全绿；**#11 尚未实现**，本文档记录其剩余步骤，供下次继续。

## 已完成（#1–#10，全绿）

| # | 范围 | 状态 |
|---|------|------|
| 1 | usage.go：暴露 Anthropic 缓存写入量（cache_creation_input_tokens） | ✅ |
| 2 | Anthropic cache_control 断点能力（build cache） | ✅ |
| 3 | sse：有界行读取 + CR-only 行尾 | ✅ |
| 4 | errors：脱敏覆盖 Message/Code/Type + 前缀 | ✅ |
| 5 | httpx：幂等判定 + 永久错误不重试 + 退避预算 | ✅ |
| 6 | detector：端点校验 + 重定向脱敏 + 失败可见 | ✅ |
| 7 | 响应体上限统一（bodyLimit/auxBodyLimit）+ ErrBodyTooLarge 归类 | ✅ |
| 8 | OpenAI Chat：per-model sticky + `error.param` 匹配 + 流头/index | ✅ |
| 9 | OpenAI Responses：StopToolUse + done 折叠 + 宽松解码 | ✅ |
| 10 | Anthropic：缓存 beta 头 + 断点上限 + redacted_thinking 透传 + 流式 usage 合并 + ListModels 分页 | ✅ |

## 未完成：#11 —— stream.go

文件：`stream.go`。两点，第二点是**真正的 bug**，第一点已确认无需改动。

### 11a. 截断暴露时机（clean-end vs cut）—— 已确认无需改动

三个 provider 的 `streamEvents` 闭包（`provider_openai_chat.go:443`、`provider_openai_responses.go`、`provider_anthropic.go`）均已实现：
- EOF/ErrUnexpectedEOF 且未见终止信号 → `ended=true, truncated=true`，先 `return endEvent(), nil`（StopOther），下次调用再返回 `ErrStreamTruncated`。
- 见终止信号（`[DONE]` / `response.completed` / `message_stop`）→ `ended=true`，下次 `return nil, io.EOF`（真·clean end，`Err()` 为 nil）。

`streamCore.Next()`（stream.go:123）对 `io.EOF` 保持 `s.err==nil`，对其它 error（含 ErrStreamTruncated）设置 `s.err`。行为正确，**本项无需修改**。审计条目可在关闭 #11 时标注为"已满足"。

### 11b. Body.Close 错误不应冒充流失败 —— 待修（本任务核心）

**现状** `releaseLocked()`（stream.go:224）：

```go
if s.closer != nil {
    s.closeErr = s.closer.Close()
    if s.err == nil {
        s.err = s.closeErr   // ← 问题：clean end 后 Close 报错会污染 Err()
    }
}
```

后果：流已成功收尾（`s.err==nil`）后，若响应体 `Close()` 返回错误，会被写入 `s.err`，导致消费方 `stream.Err()` 在非失败场景返回 error——一次干净的完成被误判为失败。`Close()` 自身的返回值 `closeErr` 已单独保留，无需再塞进 `s.err`。

**待改**：删除 `if s.err == nil { s.err = s.closeErr }`。Close 错误应只在 debug 日志层面暴露，不作为 `Err()`。

**待加**：`streamCore` 需要一个 logger 引用来 debug 记录该 close 错误。当前 `streamCore`/`newStream` 无 logger 字段。方案二选一：
- 给 `streamCore` 加 `logger` 字段，`newStream` 签名加参，各 provider 构造处传入 `p.c.settings.logger`；`releaseLocked` 里 `if closeErr != nil { s.logger.Debug("stream: response body close failed", "err", closeErr) }`（注意 releaseLocked 持锁，日志调用需在锁外或确认可重入——建议由调用方在 `takeOnEnd` 之后统一记录，避免持锁调用用户 logger）。
- 或最小化：仅删除污染行，closeErr 仍从 `Close()` 返回，暂不加日志（审计意图"不冒充失败"即已满足）。

**验证**：新增/复用测试——构造一个 `closer` 返回 error 的流，走 clean end（next 先返回终止事件再返回 io.EOF），断言 `stream.Err()==nil` 且 `stream.Close()` 返回该 close error。

## 剩余动作顺序

1. 实现 #11b（删除污染行；决定是否加 logger 字段并 debug 记录）。
2. `go build ./... && go test ./...` 确认全绿（重点关注 provider 流式测试是否触发 httptest.Server Close 阻塞，上次未复现，若再现需查）。
3. 标记任务 #11 完成。
4. 删除本 pending 文档或改名为最终审计记录。
5. 提交并推送。
