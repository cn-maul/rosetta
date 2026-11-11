# 贡献指南

## 环境

- Go 1.27+（`go.mod` 固定了最低版本）
- 零第三方依赖：`go.mod` 不得出现 `require` 块
- 推荐 `make check` 一次性跑完格式、vet 与测试

## 提交前

```sh
make check          # gofmt 检查 + go vet + go test
make race           # 并发相关改动必须过 -race
```

CI 还会跑 staticcheck、覆盖率门槛（75%）、`go mod tidy -diff` 与 govulncheck。

## 代码约定

- **不静默丢弃数据。** 上游返回了、而 SDK 不认识的内容，要么映射到 `Unknown`/`EventUnknown`，要么显式报错；不允许 `continue` 了事。
- **协议差异在适配器里吸收。** 不要在 `Client` 层做协议分支；三协议统一语义由 `provider_*.go` 负责。
- **本地能校验的错误不要留给上游 400。** 参数组合冲突（如 Anthropic thinking + 强制工具）在 `validate()` 或 `buildPayload()` 里报 `ErrInvalidRequest`。
- **错误要可分类。** 新增失败路径时说明它属于 `ErrInvalidRequest`（调用方的错）、`ErrUpstreamMalformed`（上游的错）还是新的哨兵；流式路径不要挂 `ErrUpstreamMalformed`，除非确认重试安全。
- **注释解释「为什么」，不解释「是什么」。** 尤其是绕过某个上游怪癖的代码。

## 测试

- 新增公开行为要有 httptest 用例；缺陷修复要有能复现原问题的回归用例。
- 测试文件按功能命名（`provider_anthropic_test.go`、`streaming_test.go`）；不要用 `audit_*` / `fix_*` 这类批次名前缀——审计批次是一次性事件，不是测试的归属维度。
- 外部包测试（`package rosetta_test`）用于验证公开 API 与 godoc 示例。

## 提交信息

`<type>: <subject>`，type 取 `feat` / `fix` / `docs` / `refactor` / `test` / `ci` / `chore`。正文说明「为什么」，不要只复述 diff。
