# M2 / M3 / M6 Kind 整合验收

本记录用于验证 main `209ff8d` 的完整部署，测试目标为本 Draft PR。

## 验收范围

- 从原 M3 数据库升级，保留历史 run 并补齐 M6 迁移。
- 经 gateway 接收签名 webhook，创建隔离的 fetch / runner Job。
- runner 无真实凭据，外网不可达，内部服务可达。
- 快照保存后删除 Pod，新尝试恢复并完成，GitHub 无重复 Check / Review。
- 重试使用剩余预算，旧 key 和最终 key 被吊销。
- controller 指标、Prometheus 和 Grafana 展示累计用量与恢复次数。

## 结果

验收进行中；完成后填写实际 run、时间、费用及检查结果。
