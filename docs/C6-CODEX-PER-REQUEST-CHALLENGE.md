# C6：codex API 端点是否需要每请求 PoW / turnstile（no-cred 部分）

> **结论（限于蓝本证据）：不需要。** 本平台 codex 渠道所用的端点在参考实现里是
> **纯 Bearer** 调用，不携带任何 sentinel 头，也不调用 `chat-requirements`。
> 因此 `UpstreamProfile` 这个静态配方**不需要**为 codex 开 per-request minter 旁路，
> 总览 §5 的热/慢路径拆分**不破**。
>
> **证据等级**：本地参考实现（`~/projects/chatgpt2api`，总览 §9 指名的参考源）的**源码阅读**。
> **不是对真实上游的实测**，也没有真实 codex 账号。端到端确认仍属 live-gate，见 §5。

C6 原文把爆炸半径定为"契约级，不是一个渠道的适配问题"。本文的作用是把它**降级**：
若结论成立，剩下的只是单渠道的实测确认，不再牵动契约。

---

## 1. 先更正 TODO 里的指路

TODO §C6 写"可先做的 no-cred 部分：读 `chatgpt2api/services/proxy_service.py` 与 `utils/pow.py`
确认其 PoW 调用点"。**`proxy_service.py` 里没有任何 PoW / turnstile / sentinel 引用**
（`grep` 结果为空）。该文件负责代理链与 Cloudflare clearance
（`ClearanceBundle` / `FlareSolverrClearanceProvider`），与 sentinel 是两回事。

真正的调用点全部在 `services/openai_backend_api.py`。下文按实际代码走。

## 2. 三个 minter 的调用点普查

对 `build_proof_token` / `solve_turnstile_token` / `build_sentinel_token` /
`build_legacy_requirements_token` 做全仓库 grep（排除 `utils/` 自身定义）：

| minter | 唯一调用方 | 时机 |
| --- | --- | --- |
| `build_sentinel_token` | `services/account_service.py:695`，参数 `"password_verify"` | **仅登录** |
| `build_legacy_requirements_token` | `openai_backend_api.py:2667`（`_get_chat_requirements`） | 每次会话请求 |
| `build_proof_token` | `openai_backend_api.py:402`、`:2685` | 每次会话请求 |
| `solve_turnstile_token` | `openai_backend_api.py:413`、`:2696` | 每次会话请求 |

sentinel 与 PoW/turnstile **不是一回事**：前者只在登录（属 wrapper / C4 范围），
后者才是总览附录担心的每请求负担。

### 2.1 PoW / turnstile 确实是每请求的

`_get_chat_requirements()`（`:2664`）走 `prepare` + `finalize` 两步，产物落到请求头：

- `OpenAI-Sentinel-Chat-Requirements-Token`（`:428`、`:586`）
- `OpenAI-Sentinel-Proof-Token`（`:431`、`:589`）
- `OpenAI-Sentinel-Turnstile-Token`（`:433`）
- `OpenAI-Sentinel-SO-Token`（`:435`）

全仓库设置 sentinel 头的位置**只有上面这两个 header builder**，且都由 `ChatRequirements` 喂。

它被四个会话方法各自调用，**没有任何缓存**：

| 行 | 方法 |
| --- | --- |
| `:1372` | `_run_editable_conversation` |
| `:1862` | `_run_search_conversation` |
| `:2574` | `stream_conversation` |
| `:2611` | `_stream_picture_conversation` |

**所以总览附录的前半句是对的**：在 ChatGPT **web** 通道上，PoW/turnstile 确实是每条消息现算。

## 3. 但 codex 端点不走这条通道

参考实现里 `/backend-api/codex/responses` 只有**一个**调用点（`:785`），其请求头来自
`_codex_responses_headers()`（`:596`），完整内容是：

```python
{
    "Authorization": f"Bearer {self.access_token}",
    "Content-Type": "application/json",
}
```

**没有任何 sentinel 头**，且该方法**不调用** `_get_chat_requirements()`。

一个可能推翻结论的隐患已排除：该请求用 `urllib.request.Request` 直接构造（`:803`），
**不经过 `self.session`**，因此不存在"session 级默认头里偷偷带了 sentinel"的可能。

### 3.1 这正是本平台用的那个端点

本仓库 `internal/control/provider/profile.go:57-58`：

```go
APIBaseURL:    "https://chatgpt.com/backend-api/codex",
InferencePath: "/responses",
```

拼起来就是 `https://chatgpt.com/backend-api/codex/responses`——与参考实现 Bearer-only 调用的
**同一路径**。`codex.go:83` 把它原样填进 `contracts.UpstreamProfile`。

所以总览附录的条件句"**若 codex provider 走的是这种 web-backend 路径**"要拆开看：
codex 确实在 `chatgpt.com` 这台 web-backend **主机**上，但它走的是**另一条 path**，
而那条 path 在蓝本里不带 sentinel。**主机相同，负担不同**——附录把两者当成一回事了。

## 4. 对契约的影响：无需改动

- `contracts.UpstreamProfile` 作为**静态**配方对 codex **够用**。
- **不需要**给 gateway 开 per-request minter 旁路。
- **不需要**承认 codex 不适用无状态热路径模型。
- 总览 §5 的两角色深耦合点不受影响；`profile.go:78` 那条
  "见附录未决问题" 的注释所担心的情况，在 codex 上没有发生。

按 TODO §C6"不要在核实前先写实现"的要求，本轮**未写任何实现，也未改任何契约**。
核实结果是"不需要改"，所以正确的动作就是不动。

## 5. 明确未由本轮证实

1. **全部结论来自源码阅读，没有发过一个真实请求。** 参考实现可能已经落后于上游；
   反自动化策略正是最容易被上游静默改动的部分。
2. **蓝本对 codex/responses 的唯一调用是图像生成**（`iter_codex_image_response_events`），
   不是通用文本推理，而本平台把该端点用于通用推理（`protocol: openai_responses`）。
   路径与鉴权完全相同、只有 payload 不同，且 sentinel 门禁通常按端点而非按 payload 生效——
   但**这是推断，不是证据**。
3. **登录侧不在本结论范围内。** `_ensure_codex_source_account()`（`:602`）要求账号
   `source_type == "codex"`，即必须由 codex 专属登录取得；登录时的 challenge 属 wrapper / C4，
   本文不涉及，也不因本文而变简单。
4. **ChatGPT web 渠道仍然需要每请求 PoW/turnstile。** 本平台当前**没有**把它建模为 provider。
   若将来要接，附录那个契约级问题会**原样回来**，届时需重新评估，
   且总览 §9 标注的 **turnstile 保留 Python** 仍然适用
   （`utils/turnstile.py: solve_turnstile_token` 是解释器式的 `dx`/`p` 变换，
   重写为 Go 收益低、跟随上游改版成本高）。

## 6. C6 剩余部分（live-gate）

拿到真实 codex 账号后需要确认的，只剩一件事：

> 对 `https://chatgpt.com/backend-api/codex/responses` 发一次**只带 Bearer** 的通用文本推理请求，
> 观察是否被要求 sentinel（典型表现为 4xx 且响应体指向 `chat-requirements`）。

若通过，C6 关闭；若被拒，回到总览附录的两条路（per-request minter 旁路 / 承认该渠道不适用），
**并且要重新评估 §4 的"无需改动"结论**。
