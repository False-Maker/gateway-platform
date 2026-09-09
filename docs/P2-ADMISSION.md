# P2 准入与实现验收记录

本记录覆盖 provider/OAuth、wrapper lease、凭据密文、import/refresh 和 fencing 的 P2
实现边界。不包含真实账号、生产请求、P3 quota/协议互转、计费、控制台或部署操作。

## 证据

### Codex

OpenAI Docs 的 Codex 认证入口支持 ChatGPT 登录和 API key 两种模式：
[Codex authentication](https://developers.openai.com/codex/auth)。对照官方 Codex CLI
源码（审计时 checkout 为 `19e2f218271a0795ef490909e978d8ff62bbb3de`）：

- 浏览器 OAuth 使用 `https://auth.openai.com/oauth/authorize`、PKCE 和
  `https://auth.openai.com/oauth/token`；refresh 使用同一个 token endpoint。
- authorization-code exchange 使用 form body；refresh 和 revoke 使用 JSON。revoke
  必须带 `token_type_hint`，refresh token revoke 还带公开 `client_id`。
- ChatGPT 登录的推理 profile 是
  `https://chatgpt.com/backend-api/codex`，API key profile 是
  `https://api.openai.com/v1`；Responses client 在这两个 base URL 上分别请求
  `/responses`。
- 官方 CLI client source 中没有 `chat-requirements`、Turnstile、PoW，或与 anti-bot
  challenge 相关的 sentinel 实现。这个结果只证明 CLI 请求路径没有已知的客户端
  challenge，不把服务端未来行为当成已验证事实。

对应源码入口：[Codex login](https://github.com/openai/codex/blob/19e2f218271a0795ef490909e978d8ff62bbb3de/codex-rs/login/src/server.rs)、
[model provider defaults](https://github.com/openai/codex/blob/19e2f218271a0795ef490909e978d8ff62bbb3de/codex-rs/model-provider-info/src/lib.rs)、
[Responses client](https://github.com/openai/codex/blob/19e2f218271a0795ef490909e978d8ff62bbb3de/codex-rs/core/src/client.rs)。

结论：P2 不实现 Web `chat-requirements` 逆向流程，也不预设每请求 PoW/Turnstile。
Codex 的 code exchange、refresh、revoke、Responses 路径、401/错误分类已使用本地
HTTP fixture 验证；没有向 OpenAI 发出账号请求。

### Claude

Claude Code 官方认证文档区分 Claude.ai 登录、Claude Console/API key、云厂商和
自定义 gateway；设置 `ANTHROPIC_API_KEY` 会跳过登录，OAuth profile 会自动刷新：
[Claude Code authentication](https://code.claude.com/docs/en/authentication)。公开
Anthropic API 的推理接口是 Messages API：[Messages API](https://platform.claude.com/docs/en/api/messages)。

本机只读检查的 Claude Code 2.1.241 binary 及本地解包 source 还确认了实际 OAuth 路径：

- Console OAuth authorize：`https://platform.claude.com/oauth/authorize`
- Claude.ai OAuth authorize：`https://claude.com/cai/oauth/authorize`
- OAuth token/refresh：`https://platform.claude.com/v1/oauth/token`
- API key 创建和 usage：`https://api.anthropic.com/api/oauth/claude_cli/create_api_key`
  与 `https://api.anthropic.com/api/oauth/usage`
- API key 请求使用 `x-api-key`；OAuth 请求使用 `Authorization: Bearer`，并带
  `anthropic-beta: oauth-2025-04-20`。refresh token 从未进入 gateway lease。

## 直接冲突与边界判定

1. 现有文档把 `Provider.Refresh(context.Context, Credential)` 当成 OAuth 刷新
   边界，但 Codex 和 Claude 的实际 refresh 都需要控制面保存的 refresh token。
   `contracts.Credential` 只有 access token，因此原接口不能安全实现 OAuth refresh。
2. 文档要求 wrapper 通过 claim/complete/fail lease 协作；本轮已补齐跨 Go/Python
   编解码的 job、lease、terminal message 契约，并把账号 `fence_epoch` 绑定到
   wrapper 消息。
3. migration 只有 `credentials.encrypted_secret BYTEA`，没有密文格式、AAD 或密钥
   来源约定。仅有列名不足以证明“已加密”；密钥托管和轮换仍是部署边界。
4. `Fencer.UpdateCredential` 的第二个 fenced `accounts` 更新此前忽略了
   `RowsAffected`，旧 epoch 在该更新影响 0 行后仍会提交事务。现在将其改为和
   credential 更新一样返回 `ErrFenceLost`，不改变成功路径或 P0/P1 数据结构。

## 已实现切片

- `provider.OAuthRefresher` / `OAuthRevoker` 是可选的 control-plane capability；旧 `Provider` 接口
  保持不变，refresh token 不会进入 `contracts.Credential` 或普通 gateway `Lease`。
- `pkg/contracts` 新增带 schema version、opaque encrypted input/output、worker/lease
  identity 和 fence epoch 的 wrapper job/claim/lease/complete/fail JSON 契约及校验。
- `internal/control/credentials` 使用 AES-256-GCM：版本字节、随机 nonce、account ID
  AAD 和 1 MiB 上限。control 仅在显式提供 base64 `GATEWAY_CREDENTIAL_KEY` 后启用
  refresh loop；生产 KMS 托管和轮换仍是部署门槛。
- fencing 只增加受影响行校验；没有改动已验收的事件、gateway lease 或 migration schema。
- `internal/control/provider/{codex,claude}` 已实现并由 control 注册；支持直接
  TokenBundle import、PKCE code exchange、refresh、Codex revoke、静态 profile 和
  P2 error classification。quota 仍返回合法的“缺失”，实际 quota adapter 属于 P3。
- `internal/control/provider/live_admission_test.go` 提供显式 opt-in 的真实 inference/usage、OAuth refresh 和 quota smoke-test harness；默认不联网，缺少 live 凭据时严格 skipped。
- `ImportService` 加密保存完整 TokenBundle，只向 `Account.Credential` 返回 access token；
  重复来源 import 递增 fence epoch。`RefreshService` 使用账号 advisory lock 防止并发消费
  可轮换 refresh token，并以 fence epoch 条件写回密文和权威错误状态。
- wrapper Redis Stream 队列实现 enqueue 幂等、claim、lease TTL、过期 reclaim、
  complete/fail、terminal 幂等和坏消息 DLQ。job/result 只接受有大小上限的加密 envelope。
- `internal/control/wrapper` 新增无数据库的 worker：只执行
  `claim → fixture execute → complete/fail`，支持 context 取消和 SIGTERM 正常退出；
  fixture executor 只复制 opaque encrypted envelope，不读取真实账号或 provider。
- `cmd/gwd --role=wrapper` 和兼容的 `gwd wrapper run` 均可启动 worker；配置只读取
  `GATEWAY_WRAPPER_REDIS_*`、`GATEWAY_WRAPPER_WORKER_ID` 和 wrapper 运行参数，不回退到
  gateway/control Redis 凭据。
- `configs/redis-wrapper.acl` 提供独立 `wrapper-worker` 身份，仅允许
  `control:wrapper:*` 键域及队列所需命令；gateway ACL 不得读取 job、lease 或 terminal。
- `internal/control/wrapper/testdata/fixture_consumer.py` 与 Go 测试互相编解码同一 JSON
  契约，证明 wrapper 消息不是 Go 私有格式。

## 历史临时环境验收记录（非本轮复现）

以下结果来自此前独立临时容器中的 PostgreSQL 16.15 和 Redis 7.0.15 验收，作为历史
证据保留；本轮环境未配置对应连接变量，因此没有复现这些步骤，也不把它们计入本轮
P3 admission 通过项：

- PostgreSQL 实测通过 OAuth fixture import、TokenBundle 密文落库、refresh token 轮换、
  credential version 更新、账号 advisory lock 竞争、旧 fence epoch 写入拒绝，以及使用同一
  base64 AES key 重建 control cipher 后解密已有密文。
- Redis 实测通过 wrapper enqueue 幂等、lease 到期接管、旧 worker 写入拒绝、terminal
  幂等和 pending 清理；gateway ACL 无法读取 wrapper job 或 terminal key。
- 进程级 Redis 7.0.15 实测通过 control enqueue、wrapper 子进程 claim 并完成 fixture
  job、重复完成幂等、SIGKILL 后 lease reclaim、旧 worker fencing/lease 拒绝，以及
  gateway 对 job/lease/terminal 的读取拒绝。
- 独立 wrapper ACL 仅开放 `control:wrapper:*` 键域，并以队列实际需要的 Redis 命令集
  通过验收；事务 pipeline 需要显式开放 `MULTI` 和 `EXEC`。
- 真实 wrapper 集成测试要求显式提供
  `GATEWAY_TEST_REDIS_WRAPPER_USERNAME` / `GATEWAY_TEST_REDIS_WRAPPER_PASSWORD`，不再
  允许复用 control 测试账号。

## 准入结论

P2 的仓库内最小工程切片、wrapper 进程闭环及其 PostgreSQL/Redis 验收已完成；除真实
provider 验收外，可以准备进入 P3。Codex/Claude 真实 OAuth、refresh、推理和服务端
行为仍为 pending，不能据此准入生产流量。生产 AES key 的 KMS/secret 托管与轮换仍是
部署门槛。真实 provider 账号、生产连接、账号登录、切流和生产写入本轮均未执行。

（本记录写于 2026-09-02，当时仓库尚无 `AGENTS.md`；该文件已于 2026-09-03 建立，本段保留为历史。）最高剩余风险是未验证真实 Codex/Claude
服务端 OAuth/refresh/推理行为，以及生产密钥托管/轮换；这些不属于本轮 fixture worker
闭环的证据范围。
