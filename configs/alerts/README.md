# 告警规则（E1）

`gateway-platform.rules.yml` 是可直接加载的 Prometheus 告警规则，覆盖 E1 DoD 要求的五个方向
（平台级 transport 拒绝、DLQ 增长、stream pending 积压、`UsageSource=missing` 占比、防饿死闸触发），
外加 B4 计费闭环与 A12 明细的埋点。

**通知通道（谁收、怎么收）属部署边界，按 E1 DoD 不在本目录范围内。** 这里只交付规则与阈值。

## 阈值

**所有阈值均未经真实流量校准。** 每条规则的 `threshold_source` 注解写明来源，分两类：

- `uncalibrated` —— 只是"能让规则跑起来的起始值"，上线后按实际分布回调。
- `……推出` —— 由后果推导，可容忍量本身为 0（例如进 DLQ 就是计量缺口），与流量无关。

## 验证

仓库内、无需 Docker 的那条（这是常驻校验）：

```sh
go test ./internal/observability/ -run 'AlertRules|EveryRule|RequiredCoverage'
```

它做三件事：每条规则引用的指标必须**在本仓库源码里真的被发射**（规则写错指标名会永远不触发，
读起来却像有覆盖）；每条规则必须写明阈值来源；DoD 要求的五个方向必须都在。

PromQL 语法本身用 promtool 校验：

```sh
docker run --rm -v "$PWD/configs/alerts:/rules:ro" --entrypoint promtool \
  prom/prometheus:v3.7.3 check rules /rules/gateway-platform.rules.yml
```

本仓库当前没有 CI，两条命令都是手动执行。

## 加载

```yaml
rule_files:
  - /etc/prometheus/rules/gateway-platform.rules.yml
```

`job` 标签由部署侧 scrape 配置决定，规则文件不假设其取值。gateway 与 control 是两个独立进程的
注册表，需要分别被抓取，否则对应分组的规则不会有数据。
