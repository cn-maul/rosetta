# 安全策略

## 报告漏洞

**不要开公开 issue。** 请通过仓库的 GitHub Security Advisories（Security → Report a vulnerability）私密报告，或直接联系维护者。我们会在 3 个工作日内确认收到，并在修复发布后公开致谢（如你愿意）。

请尽量包含：受影响的版本、最小复现、影响面（凭据泄露 / 拒绝服务 / 请求伪造），以及你建议的修复方向。

## 支持范围

| 版本 | 支持 |
|---|---|
| 最新 minor | 安全修复 + 缺陷修复 |
| 上一个 minor | 安全修复 |
| 更早 | 仅建议升级 |

## 凭据处理

SDK 自身不持久化任何凭据，并按以下方式避免泄露：

- `APIError` 的 `Error()`、`Raw` 与重试日志都经过 `httpx.MaskSecrets` 脱敏；`displayEndpoint` 会剥掉 endpoint 里的 userinfo、query 与 fragment。
- `WithEndpoint` 拒绝带 query 的 endpoint，避免凭据藏在 URL 里进入错误信息。
- 跨主机重定向默认拒绝转发凭据（`httpx.CrossHostSafeRedirect`）。

调用方需要自行保证的：

- 通过 `WithHeader` 注入的凭据不会被 SDK 额外脱敏——不要把它拼进 endpoint 或自定义错误信息。
- `WithLogger` 输出的 debug 日志可能包含模型名、endpoint 与重试次数；不要在生产环境输出到不受控的 sink。
