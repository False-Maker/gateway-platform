# E2 wrapper executor（no-cred 部分）

> 本文覆盖 `docs/TODO.md` §E2 的三条 DoD：
> ① device flow executor 骨架；② `FixtureExecutor` 生产回落收紧；
> ③ CLI / PoW / turnstile 的 **no-cred 前置**清单（只确认，不实现）。
>
> **证据边界**：本文第 3 节的结论来自**本地参考实现**
> （`~/projects/chatgpt2api`、`~/projects/elucid-relay`，即总览 §9 指名的重写蓝本），
> **不是对真实上游的实测**。凡标注「蓝本记载」的，都只说明"蓝本是这么做的"，
> 不构成"上游现在仍然如此"。真实验证属 C4 / live-gate。

---

## 1. device flow executor（RFC 8628）

`internal/control/wrapper/device.go`。与 PKCE 的根本差别：device flow **没有回环回调**，
worker 无法被通知用户何时完成授权，只能轮询 token 端点直到授权服务器改口。

### 已实现（本轮，全部由本地 fixture server 验证）

| 部分 | 实现 | 依据 |
| --- | --- | --- |
| 参数生成 | device-code 请求带 `client_id` + `scope`；token 轮询带 `grant_type` + `device_code` + `client_id` | RFC 8628 §3.1 / §3.4 |
| grant type | `urn:ietf:params:oauth:grant-type:device_code` | RFC 8628 §3.4 |
| 轮询节奏 | 默认 5s；服务器 `interval` 优先；`slow_down` 每次 **+5s** | RFC 8628 §3.5 |
| 超时 | `Timeout` 与服务器 `expires_in` **取短** | 本地决定 |
| 用户指令 | `Prompt` 回调收 `user_code` / `verification_uri` / `verification_uri_complete` / `expires_in` | — |

### 错误归类

`error` 字段先于 HTTP 状态码判断——RFC 8628 §3.5 把 `authorization_pending` 放在
**HTTP 400 的 OAuth error 响应**里，那是正常态而非失败。

| 上游 | code | retryable | 理由 |
| --- | --- | --- | --- |
| `access_denied` | `device_denied` | **false** | 用户的答复；重试只会再问一次同样的问题 |
| `expired_token` | `device_code_expired` | **true** | 过期的是这个 code，不是这个账号；新 job 换新 code |
| 其他 `error` | `device_token_invalid` | false | 未知错误不假设可恢复 |
| 轮询超时 | `device_authorization_timeout` | true | 与"调用方取消"区分：后者原样返回 `ctx.Err()` |
| profile 不完整 | `device_profile_pending` | **false** | 缺的是上游事实，重试补不上 |

### 未实现 / 留给 C4 的确切内容

`defaultDeviceProfile` 对 copilot 返回的 `TokenURL` 是**空的**，因此生产路径必然以
`device_profile_pending` 显式失败。这**不是**因为"查不到端点"：

- 端点蓝本有记载：`elucid-relay/apps/oauth-wrapper/src/strategies.mjs` `deviceDefaults()`
  给 `github_device` 的 `token_url` 是 `https://github.com/login/oauth/access_token`。
- 但**只轮询它拿到的是 GitHub OAuth token，不是 Copilot API 凭据**。蓝本在其后还有第二次交换
  （`githubCopilotBundleFromGitHubToken`，走 `provider.GitHubCopilotProfile().OAuthTokenURL`
  = `https://api.github.com/copilot_internal/v2/token`），**本轮没有实现那一步**。

若现在把 URL 填进去，job 会**成功完成并返回一个无法服务流量的 token**——正是本轮在
fixture 路径上着力消除的"静默成功"。所以 C4 必须**同时**补端点与交换，缺一不可；
`TestDefaultDeviceProfileIsIncompleteAndFailsExplicitly` 钉住了这一点。

`DeviceProfile` 刻意不复用 `provider.EndpointProfile`：后者只有一个 `OAuthTokenURL`，
而 device flow 涉及三个不同端点（device-code / device-token / Copilot 交换）。
拓宽共享契约等于把未落地的端点写进公共结构，用本地 struct 才能让缺口保持可见。

---

## 2. `FixtureExecutor` 生产回落收紧

**先更正 TODO 里的旧基线**：§E2 原文称 `run.go:43` "在无配置时仍回落到 `FixtureExecutor`"。
该描述在本轮开始前**已经不成立**——`executorFromConfig` 早已把 `""` 与 `"oauth"` 映射到
`PKCEExecutor`，`ConfigFromEnv` 的默认值也是 `ExecutorOAuth`。

真实缺口是另一个：`GATEWAY_WRAPPER_EXECUTOR=fixture` 在生产里**依然可选**，
且没有任何东西区分"生产"与"测试"。stub 的失败模式是静默的——job 被 claim、complete、ACK，
worker 各项指标健康，而**没有任何账号被授权**。

本轮收紧为**两个相互独立的确认**，单个配置失误无法把生产 worker 送上 stub：

```
GATEWAY_WRAPPER_EXECUTOR=fixture     # 选中 stub
GATEWAY_WRAPPER_ALLOW_FIXTURE=true   # 确认这不是生产 worker（严格等于 "true"）
```

缺第二个即**启动失败**，错误信息指名所缺的变量；两者齐备则启动时打一条显式日志。

同时新增 `authModeRouter`：按**加密 job input 里的 `auth_mode`** 而非明文 `provider` 字段分发。
理由是让决定权留在拥有该流程的一侧——control 入队时决定跑哪种流程；
没有 executor 认领的 `auth_mode` 会显式失败（`unsupported_job`），
而不是落到"恰好配置了的那个 executor"上。

---

## 3. CLI / PoW / turnstile：no-cred 前置清单

**本节只做确认，不实现**（DoD 明确）。

### 3.1 CLI 类（`codex_cli` / `claude_cli` / `kiro` / `windsurf_cli` / `external_cli`）

蓝本记载（`strategies.mjs`）的做法是：**读本机凭据文件**，必要时**起子进程**跑登录命令。
例如 `codexCli` 读 `auth.json` 后回落到 `codexDeviceLogin`；
`windsurfCli` 扫 Codeium 配置文件取 `api_key`；
`externalCli` 直接 `runCommand(login_command)` 再读 `token_bundle_file`。

**no-cred 前置（与真实账号无关，可以先做的）**：

1. **确定这是否仍是 wrapper 模型。** CLI 类需要 worker 主机具备**文件系统状态 + 子进程执行权**。
   这与当前 wrapper 的定位冲突：本仓库的 worker 是**无状态、可横向复制、无数据库句柄**的，
   而 CLI executor 会把 worker 绑死在"装了对应 CLI 且已登录的那台机器"上。
   **这是架构决定，不是编码任务，必须先定。**
2. 若保留：需要新增 job input 字段（`login_command` / `auth_file` / `token_bundle_file`）
   以及**命令执行的安全边界**——命令来自 control 下发，等于远程代码执行入口，
   必须有白名单或固定命令表。当前 `contracts.WrapperJob` 没有任何相关字段。
3. 子进程超时与输出截断（蓝本用 120s）。
4. `contracts.TokenBundle` 需容纳 `type: "api_key"` 形态（windsurf 返回的是 api_key 而非 OAuth bundle）。

### 3.2 PoW / turnstile / sentinel

**关键确认：这三者不属于 wrapper。**

蓝本 `chatgpt2api` 里，PoW 与 turnstile 的产物是两个**请求头**——
`OpenAI-Sentinel-Proof-Token` 与 `OpenAI-Sentinel-Turnstile-Token`
（`services/openai_backend_api.py:431` / `:433`），
由 `_get_chat_requirements()` 在**每次会话调用前**现算
（`:1372` / `:1862` / `:2574` / `:2611` 四个调用点，**无缓存**）。

也就是说它是**网关热路径**的每请求负担，不是"授权一次拿到长期凭据"的 wrapper 职责。
总览 §9 标注的 **turnstile 保留 Python** 与此一致：它是解释器式的 `dx`/`p` 变换
（`utils/turnstile.py: solve_turnstile_token`），重写为 Go 收益低、跟随上游改版的成本高。

**本轮推进了总览附录里的 P2 未决问题**：

> 附录原文：「**P2 打通 codex 前必须先确认**：codex CLI 的 API 端点是否需要每请求 PoW/turnstile。」

蓝本证据指向**不需要**：codex 走 `/backend-api/codex/responses`
（`services/openai_backend_api.py:785`），其请求头由 `_codex_responses_headers()`
（`:596`）构造，**只有 `Authorization: Bearer` 与 `Content-Type`**，
不含任何 sentinel 头，且该路径**不调用** `_get_chat_requirements()`。
sentinel 负担集中在 ChatGPT web 的 `/backend-api|backend-anon/sentinel/chat-requirements` 通道。

**结论（限于蓝本证据）**：codex 渠道与本平台的无状态热路径模型**兼容**；
需要每请求 PoW/turnstile 的是 ChatGPT web 渠道，若要接入，它应当被当作
"该 provider 的 per-request minter 旁路"，而不是塞进 wrapper。
**此结论未经真实上游验证**，C4 / P2 落地前需实测复核。

### 3.3 不做的

- CLI / PoW / turnstile 的任何实现代码。
- `contracts.WrapperJob` / `TokenBundle` 的字段扩展（等 3.1 第 1 条的架构决定）。
- 真实授权码交换（C4，live-gate）。
