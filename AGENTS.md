# ai-pr-review-cloud 开发指南

设计的唯一来源是 [docs/architecture.md](docs/architecture.md)。改动行为前先读相关章节；设计变化要同步更新文档。进度记录在 [docs/roadmap.md](docs/roadmap.md)，完成任务后勾选对应条目，里程碑验收后更新总览里的状态。

## 组件边界

- **gateway** 只做 webhook 校验和记录触发，不调 GitHub，不建 Job。
- **controller** 是唯一执行副作用的地方：创建 Job、写 GitHub、推进状态机。合并决策只在一个函数里。
- **runner** 只产出数据（评审结论、session 快照、用量），不持有 GitHub 凭据，不写 GitHub。
- **store** 只负责持久化和事务约束，不调用外部服务。数据库事务不跨网络调用。
- 状态取值在 `go/internal/run/state.go` 和 `go/internal/store/migrations/` 的 CHECK 约束里各有一份，改一处必须改另一处。

## 常用命令

| 任务 | 命令 |
| --- | --- |
| 全部测试 | `make test` |
| 格式、vet、类型检查、chart lint | `make lint` |
| 构建镜像 | `make images` |
| 本地 Kind 部署 | `make kind-up && make deploy` |

## 约定

- Go：纯逻辑写成小而无副作用的函数并配表驱动测试；`cmd/` 里只做装配。
- TypeScript：ESM，`strict`，测试用 `node --test`，直接运行 `.ts` 源文件。
- 依赖版本精确锁定（`--save-exact`、`go.sum`）。pi SDK 升级单独提交。
- 不要提交密钥。开发用的 webhook secret 只通过 `make deploy` 的环境变量传入。
- 禁止直接推送 `main`，走分支加 PR。PR 标题用约定式提交（`feat:`、`fix:`、`docs:` …）。
- 计划和草稿放在已 gitignore 的 `.plans/`。
