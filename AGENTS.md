# AGENTS.md

面向在本仓库工作的编码 agent。**先读这一份，再读 `docs/`。**

## 0. 最重要的一条规则：缺凭据不是停机理由

本项目没有真实的上游 provider 账号（OAuth / API key）。这是**已知且长期的**状态，不是待解决的阻塞。

- `docs/TODO.md` 里每条任务都带标签：
  - **`[no-cred]`** —— 不需要任何真实凭据、真实 provider、生产环境即可完成并验证。
  - **`[live-gate]`** —— 必须有真实账号才能完成，统一留到项目结束验收阶段。
- **缺少真实凭据只允许阻塞 `[live-gate]` 任务。**遇到 `[live-gate]` 就跳过它，去做下一条 `[no-cred]`，不要停下来等凭据、不要要求用户提供账号、不要把它写成全局阻塞。
- 只要 `docs/TODO.md` 里还有未完成的 `[no-cred]` 任务，本项目就**不处于**"等待真实账号"的状态。

同样地：**不要输出全局封顶结论**，例如"X 阶段全量不能宣称完成"。要按任务逐条给状态。全局封顶句会让后续 agent 误判为整体停机。

## 1. 证据与进度分离

两份文档职责不同，不要混写：

| 文件 | 写什么 | 不写什么 |
|---|---|---|
| `docs/EVIDENCE.md` | 已执行的命令与结果、验收证据、免责边界、历史环境记录 | 待办、计划 |
| `docs/TODO.md` | 编号任务、DoD、`[no-cred]`/`[live-gate]` 标签、当前进度 | 流水账、证据 |

`docs/AUDIT-CONTEXT.md`、`docs/P2-ADMISSION.md`、`docs/fluxgate-keyhive-overview.md`、
`keyhive/docs/architecture.md`、`fluxgate/docs/architecture.md` 是**设计与决策**文档，改动它们前先确认设计真的变了。

## 2. 反幻觉基线（保留，但不得升级为停机）

这些约束继续有效：

- 不把 fixture、本地回归、mock 结果写成"真实 provider 通过"或"生产准入"。
- 不伪造额度：未核实的 quota schema 一律返回 missing，不按零补齐、不猜测。
- 不猜测未验证的上游 endpoint、字段或退避时长；没有协议依据就明确留 pending。
- 报告里区分"本轮执行"与"历史证据"，历史证据不计入本轮结论。
- 不执行生产写入、部署、切流、真实计费。生产库只允许只读核对与 dry-run。

**边界**：以上是对**如何描述结果**的约束，不是对**是否继续开发**的约束。

## 3. 版本控制（强制）

仓库在 2026-09-03 才建立，此前 14k 行代码在无版本控制下迭代过，不要再重演。

- **每轮工作结束必须 `git commit`。**不允许留未提交的改动跨轮。
- commit message 说明改了什么、为什么，以及验证到什么程度。
- 远端 `origin` 为 `git@github.com:False-Maker/gateway-platform.git`（2026-09-10 建立）。
  **commit 后 `git push origin main`。**只能走 SSH；本机无 HTTPS 凭据、无 `gh` CLI。
- 不要碰上层 `d1/.git`——那是个只含 `info/exclude` 的坏空壳，与本仓库无关。

## 4. 构建与测试

Go toolchain：`/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go`

```
go build ./cmd/gwd
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./pkg/contracts ./internal/control/...
```

`-buildvcs=false` 曾因缺失 `.git` 而必需，**现在不再需要**，历史文档里的该写法已过时。

可选基础设施测试默认严格 skipped，需显式配置才运行：
`GATEWAY_TEST_DATABASE_URL`、`GATEWAY_TEST_REDIS_*`。本机用 Docker 一键起：

```
docker compose -f configs/test-infra/docker-compose.yml up -d
source configs/test-infra/test.env
go test -count=1 -p 1 ./...     # 必须 -p 1：多个包共用一个 Redis DB 并 FLUSHDB
```
真实 provider harness 需 `GATEWAY_RUN_LIVE_PROVIDER_TESTS=1` + `GATEWAY_LIVE_CONFIRM=provider-admission`，默认不联网。

## 5. 架构速览

单 repo、单二进制 `cmd/gwd`、三个角色：

- `--role=gateway`（fluxgate）—— 热路径 API 网关，无状态，读 Redis 快照
- `--role=control`（keyhive）—— 号池/控制面，PG 为唯一权威，每账号 `fence_epoch` fencing
- `--role=wrapper` —— OAuth 执行器，经 Redis Stream 租约队列取 job

代码在 `internal/{gateway,control}`，共享契约在 `pkg/contracts`，协议互转在 `pkg/protokit`。
`keyhive/`、`fluxgate/` 目录当前只放各自设计文档。

## 6. 改动边界

- 只做被要求的事；不做顺手重构、不做投机抽象、不做无关清理。
- 发现范围外的问题，在报告里说一次，不自行修复。
- 扩大范围前先问。
