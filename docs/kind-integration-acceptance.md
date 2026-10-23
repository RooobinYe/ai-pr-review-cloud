# M2 / M3 / M6 Kind 整合验收

验收时间：2026-10-24（Asia/Shanghai）。部署版本为 main `209ff8d`，测试目标为 [PR #7](https://github.com/RooobinYe/ai-pr-review-cloud/pull/7) 的 `eb86d64`。

## 验收范围

- 从原 M3 数据库升级，保留历史 run 并补齐 M6 迁移。
- 经 gateway 接收签名 webhook，创建隔离的 fetch / runner Job。
- runner 无真实凭据，外网不可达，内部服务可达。
- 快照保存后删除 Pod，新尝试恢复并完成，GitHub 无重复 Check / Review。
- 重试使用剩余预算，旧 key 和最终 key 被吊销。
- controller 指标、Prometheus 和 Grafana 展示累计用量与恢复次数。

## 部署与触发

- Kind：`ai-pr-review-cloud`；namespace：`ai-pr-review-cloud`；Helm revision 10。
- gateway、controller、fetch、runner 使用 `main-209ff8d` 镜像；LiteLLM `v1.104.2`、Prometheus `v3.5.0`、Grafana `12.1.0` 就绪。
- 原 PostgreSQL Pod 未重建，原有 18 个终态 run 保留；在既有 `0001_init`、`0002_recovery` 上补齐 `0002_metrics`。
- 原 smee 转发未运行。本次向 gateway 重放由既有 webhook secret 签名的 PR 事件；未验证 GitHub → smee 的实时转发。错误签名返回 401，有效签名及同 delivery ID 的重放均返回 202，只创建一个 run。

## 恢复与 GitHub 幂等

run：`6a4854be-c2d2-4e1f-ad03-c3ff626033cd`。

| 时间（Asia/Shanghai） | 事件 |
| --- | --- |
| 00:35:49 | 第 1 次尝试领取，创建 Check Run |
| 00:36 左右 | 第 1 轮快照保存后删除 runner Pod |
| 00:36:04 | controller 将同一 run 放回队列并领取第 2 次尝试 |
| 00:36:26 | 从 8 个 entry 的快照恢复，输出 approve，run 进入 done |

两次尝试共用 Check Run `120532383002`（success），只有一份 Review `5631474585`（APPROVED、0 条逐行评论）。用量累计 6 行，费用 $0.0044472。结束后重启 controller，run 仍为 done、attempt 仍为 2，没有新增 Review；GitHub action 共 4 行，创建和完成 Check 的 ID 相同。

## M2 安全边界

- 真实 fetch 经 controller 的 GitHub CONNECT 代理完成克隆，runner 经 LiteLLM 完成真实模型请求；LiteLLM 全程 0 次重启。
- 评审结束较快，环境与外网探测使用从真实 Job 复制 runner 容器配置、移除 fetch init 的临时 Pod，保持相同镜像、Secret 引用、安全配置及 NetworkPolicy 标签。凭据变量只有 `RUN_TOKEN` 和 25 字符虚拟 `LLM_API_KEY`；没有 GitHub token、代理 token、master key、App 私钥或 webhook secret。
- 稳定后对 `example.com`、`api.github.com`、`1.1.1.1`、`api.deepseek.com` 连续两轮访问均超时；controller 和 LiteLLM 健康接口返回 200。无策略标签、无凭据的对照 Pod 对相同外网目标分别得到 200、200、200、401，排除了外网自身不可达造成的假阳性。
- 结束后 LiteLLM 的虚拟 key 表为 0 行，旧尝试及最终尝试 key 都未遗留。未捕获短暂存活的第 2 次 key 的 max_budget，本次不把“剩余预算数值”和“超预算拒绝”列为新增实测结论；相关自动化回归覆盖见 M2/M3 整合记录。

### 发现的已有限制：Pod 启动隔离窗口

首次探测在 Pod 刚 Ready 时访问 `example.com` 得到 200，随后外网被阻断。在保留的 `ai-pr-review-m2` namespace 使用旧 `:m2` 镜像再次验证，同样是启动立即访问返回 200，10 秒后访问超时。当前 kindnet 镜像为 `kindest/kindnetd:v20260820-69b56db7`。

这表明稳态隔离有效，但该环境不能据此承诺“从容器启动第一瞬间就没有外网”。该行为在旧环境也存在，不是此次 M2/M3/M6 合并新增的问题。实际评审有 fetch init，本次未观察到评审 runner 的外网泄露，但 init 耗时不应被当作隔离保证。未通过固定 sleep 掩盖这一限制，也未在验收中替换整个集群 CNI。

[Kubernetes NetworkPolicy 生命周期说明](https://kubernetes.io/docs/concepts/services-networking/network-policies/#pod-lifecycle) 要求已处理策略下的新 Pod 从容器启动时即被隔离；这里记录的是实际环境的不足，不将启动放行解释为普遍允许的安全语义。

## M6 指标与看板

- controller `/metrics`、Prometheus API、Grafana Prometheus datasource proxy 三处均返回有效数据；Prometheus controller target 为 up，Grafana health 的 database 为 ok，看板 `ai-pr-review-cost` 已加载。
- 全库累计费用从 $0.504798936 增至 $0.509246136，差值等于本 run 的 $0.0044472；重试计数从 6 增至 7。
- 数据库与指标一致：input 671522、output 177466、cache_read 15805056；新 run 的 queue/startup/fetch/review/publish/total 各阶段 histogram count 均为 1。
- controller 重启后累计成本、token 和重试指标保持一致。旧 run 没有补写历史阶段时间戳，因此阶段 histogram 仅包含新版本完整记录的 run，未把历史数据缺失误判为丢失 run。

## 结论

最新 main 已部署并完成真实模型评审、删 Pod 恢复、GitHub 幂等写入和监控数据核对，未发现这些链路的整合回归。M2 的启动瞬间隔离限制仍然存在，不能宣称安全边界完全无缺口。临时探测 Pod 和本次端口转发清理，保留部署、历史数据库及验收 run。
