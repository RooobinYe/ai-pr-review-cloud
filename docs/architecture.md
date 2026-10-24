# ai-pr-review-cloud 架构设计

> 状态：M1、M2、M3、M4、M5、M6 完成，最小闭环与删除 Pod 后恢复完成均在本仓库的 PR 上实测通过，成本测算见 [cost-report](cost-report.md)；其余按 [roadmap](roadmap.md) 推进 · 目标：赛道 B「云原生 Agent 实战」参赛作品
> 前身：[ai-pr-review v1](https://github.com/RooobinYe/ai-pr-review)（Go CLI，见 §8.1） · 赛题见 [赛道B-AI-Application-赛题.md](赛道B-AI-Application-赛题.md)

## 0. 背景与目标

ai-pr-review v1 是一个本地 / CI 里运行的 CLI：输入 PR URL，克隆仓库，用自研的 agent 循环做两阶段评审（先用工具探索代码，再输出结构化 JSON），用 `--fail-on` 当质量门禁。

本项目在新仓库里从零实现，目标是把"一个人用的命令行工具"变成"装在 GitHub 上、跑在 k8s 上、给团队用的评审服务"：

| 目标 | 对应赛题考核点 |
| --- | --- |
| 安装为 GitHub App，PR 事件自动触发，无需人工运行 | 1 场景价值、4 工程完成度 |
| 每个 PR 一个隔离的 Pod 环境，并发评审 | 2 云原生架构（容器化、弹性、调度） |
| 可选：为 PR 部署预览环境，agent 用 kubectl 验证运行效果 | 1 场景价值、3 Agent 能力 |
| 模型拿不到真实密钥 | 2 云原生架构、3 链路质量 |
| 等待审批时不占算力；崩溃后自动恢复 | 2 分布式状态、3 容错与异常恢复 |
| 每个 PR 的 token 与金额统计、预算上限 | 5 成本与性能 |
| agent 循环基于 pi SDK，不再自研 agent 运行时 | 4 代码规范、可维护性 |

### 非目标（demo 阶段）

- **不做合并安全加固。** 默认的 `vote` 模式下 AI 只投一票，什么时候合并由仓库的 branch protection 和人决定。仓库可以选择 `auto` 模式，让 controller 在结论为 approve 时直接合并；这条路径不做额外加固。合并决策集中在 controller 的一个函数里（§6.3），生产环境在这里加确定性规则即可，agent 不用改。
- 不做多集群、多租户；只支持一个 GitHub App 实例。
- 不做 Web UI；交互全部发生在 GitHub（PR 评论、Check Run）和 Grafana。

## 1. 场景与价值

### 1.1 目标用户

用 GitHub 协作、合并前要求代码评审的研发团队，尤其是：

- **评审人手紧的小团队和开源维护者。** 仓库要求"至少 N 个 approval"，PR 经常要等审批人有空才能往前走。
- **把服务部署在 Kubernetes 上的团队。** 很多问题只看代码看不出，要部署起来才暴露：配置写错、镜像起不来、探针失败、资源不足。
- **对代码和密钥外流敏感的团队。** 希望评审服务部署在自己的集群里，模型只拿到受限的临时凭据。

### 1.2 痛点与现有做法

| 现有做法 | 能做到 | 做不到 |
| --- | --- | --- |
| 人工评审 | 理解改动意图，对合并负责 | 审批人时间有限，PR 要排队等；大 PR 容易扫一遍就过；不会为每个 PR 部署一遍再看运行效果 |
| CI 静态检查（lint、单测、SAST） | 确定、便宜、可重复 | 只能发现写成规则的问题，不理解改动意图，也不会去读改动牵连到的其他代码 |
| SaaS 类 AI 评审机器人 | PR 上自动出评论 | 结论停在评论层面，不进入审批流程（GitHub 文档明确说 Copilot 的 review 不计入必需审批数）；代码要发给第三方服务；一般只看代码，不部署运行 |
| 本项目的前身 v1 CLI（§8.1） | agent 主动探索代码，给出结构化结论，`--fail-on` 当门禁 | 要人手动运行或写进 CI；单进程，崩溃就从头再来；没有团队维度的成本统计和预算控制 |

### 1.3 方案定位

**基础能力：PR 评审。** 沿用 v1 验证过的两阶段评审（先探索、再结论）。结论由模型自己决定：approve 还是 request_changes，加上一段评审正文和逐行评论，以 PR Review 和 Check Run 的形式呈现（§4.4）。

**AI 作为审批人之一。** 安装后，AI 以 GitHub App bot 的身份参与评审，和人类审批人一样投票：结论为 approve 时提交 APPROVE review；发现问题时只提交 COMMENT review，指出问题但不阻塞合并。仓库要求"N 个 approval 才能合并"时，AI 的 approve 算作其中一票（前提是 App 有 Contents: write，§6.4），其余由人来投。这是默认的 `vote` 模式，controller 不执行合并（§6.3）。

**差异化能力：预览环境验证（按仓库开启）。** 只看代码看不出的问题，在运行起来以后才暴露：配置错误、镜像起不来、探针失败、资源不足、依赖服务连不上。开启后，controller 为 PR 创建独立的 namespace 并部署，agent 用 kubectl 查看 Pod 状态、事件和日志，把运行时证据和代码评审结论合在一起，再给出结论。

这也是 agent 需要 kubectl 的唯一理由。纯代码评审阶段不给集群权限。

### 1.4 Agent 带来的改进

这里用 agent 而不是一次性把 diff 交给模型，是因为评审需要多步取证：看到一处改动，要去读调用方、查历史、确认边界条件，开启预览时还要查 Pod 状态和日志。每一步看什么，取决于上一步看到了什么。

| 改进 | 做法 | 证据 |
| --- | --- | --- |
| 不需要人去触发 | GitHub App 收到 PR 事件后自动评审，每个新提交评审一次 | M1 验收（[roadmap](roadmap.md#m1-最小闭环)） |
| 不止看 diff | 只读工具 `read`、`grep`、`find`、`ls`、`git`（§4.2），平均每次评审 14.6 轮、26 次工具调用 | [成本报告](cost-report.md#瓶颈分析) |
| 发现运行时问题 | 为 PR 部署预览环境，agent 用 kubectl 读事件和日志 | M5：探针路径写错的 PR，agent 查到探针返回 404，给出 request_changes（[roadmap](roadmap.md#m5-预览环境验证)） |
| 结论进入审批流程 | AI 以 bot 身份投票，approve 计入"需要 N 个 approval"；发现问题只评论、不阻塞合并 | M1 实测：App 有 Contents: write 时这一票计入（§6.4） |
| 判断可信 | 结论由模型给出，`failOn` 做确定性兜底（§6.5） | 20 个真实 PR 各跑 3 次，19 个三次结论一致；标明 DO NOT MERGE 的安全问题 PR 三次都是 request_changes；go-chi/chi 的 7 个已合并 PR 全部 approve（[成本报告](cost-report.md#稳定性与结论质量)） |
| 成本和时长可控 | 每个 run 的虚拟 key 带预算硬上限，runner 侧还有软预算（§7.3） | 一次评审 P50 $0.029、端到端 P50 106 秒；每 100 个 PR 每月约 $9 |
| 中断后接着跑 | 每轮上传 session 快照，新 Pod 从快照继续（§4.3） | M3：评审中途删 Pod，run 自动恢复，PR 上没有重复评论（[roadmap](roadmap.md#m3-可靠性)） |

## 2. 总体架构

```mermaid
flowchart LR
    GH[GitHub] -- webhook --> GW[gateway]
    GW --> DB[(PostgreSQL)]
    CTRL[controller] <--> DB
    CTRL -- 创建 Job / namespace --> K8S[Kubernetes API]
    K8S --> RUN[runner Pod<br/>pi SDK]
    RUN -- 虚拟 key --> LLMP[LiteLLM proxy]
    LLMP -- 真实 key --> LLM[模型厂商 API]
    RUN -- 结果 / session 快照 --> CTRL
    RUN -. kubectl 只读 .-> PREV[PR 预览 namespace]
    CTRL -- 评论 / approve / merge<br/>GitHub App token --> GH
    CTRL & LLMP --> PROM[Prometheus / Grafana]
```

| 组件 | 语言 | 职责 | 不做什么 |
| --- | --- | --- | --- |
| `gateway` | Go | 校验 webhook 签名，按 delivery ID 去重，记录评审触发或人工合并确认，立即返回 202 | 不调 GitHub，不建 Job |
| `controller` | Go | 驱动 run 状态机：排队、建 Job、建预览环境、接收结果、执行所有 GitHub 写操作、超时与重试 | 不调用模型 |
| `fetch` | Go | runner Pod 的 init 容器：用只读 installation token 拉取代码和 PR 元信息，写入共享卷后退出 | 不在主容器里留下任何凭据 |
| `runner` | TypeScript（pi SDK） | 每个 run 一个 Job：探索代码、可选的 kubectl 验证、通过 `submit_review` 工具提交结构化结论 | 不持有 GitHub 凭据，不直接写 GitHub |
| LiteLLM proxy | 现成组件 | 持有真实模型 key；为每个 run 签发带预算上限的虚拟 key；记录花费 | — |
| PostgreSQL | 现成组件 | run 状态机、GitHub 副作用记录、session 快照、用量 | — |

设计原则：

- **所有副作用集中在 controller。** runner 只产出数据，不改变外部世界（预览环境除外，且只读）。这样 runner 可以随意重试，GitHub 上不会出现重复评论。
- **状态都在 PostgreSQL。** gateway、controller、runner 都可以随时重启。
- **等待不占 Pod。** 任何需要等人或等事件的环节，都表现为数据库里的一个状态，而不是一个挂着的进程。

### 2.1 关键技术选型

| 决策 | 选择 | 考虑过的方案 | 理由 |
| --- | --- | --- | --- |
| 服务拆分 | gateway、controller、runner 三个组件，外加现成的 LiteLLM 和 PostgreSQL | 单体服务 | 三者的信任级别和伸缩方式不同。gateway 面向公网，要在 GitHub 的超时内快速返回，不该持有 GitHub 凭据；controller 是唯一执行副作用的地方，便于做幂等；runner 运行受 PR 内容影响的模型，必须隔离、可随时丢弃。拆开后每个组件的权限可以按最小原则给 |
| 评审的执行单元 | 每个 run 一个 Kubernetes Job，每次尝试一个新 Pod | 常驻 worker 进程池 | PR 内容不可信，提示注入可能让模型乱用工具。一次性 Pod 天然隔离：独立的文件系统、本次尝试专用的凭据 Secret、单独的 NetworkPolicy 和资源上限，结束即销毁。空闲时不占资源。代价是每次几秒的冷启动，集群内实测建 Pod 加拉代码 6 到 37 秒，相比 100 秒以上的模型评审可以接受（[成本报告](cost-report.md#集群内实测)） |
| 状态与调度 | PostgreSQL 里的状态机，controller 轮询 reconcile，用 `FOR UPDATE SKIP LOCKED` 领取任务 | 消息队列（Kafka、Redis Streams）；工作流引擎（Temporal、Argo Workflows） | 评审要等的东西可能持续几天（manual 模式等人确认），用一行数据表示比一条挂着的消息或一个长期运行的工作流更简单。唯一约束、CHECK 约束和事务直接表达"一个提交只评审一次""结论非空才能投票"等规则。少引入一个有状态组件，Kind 上一键就能拉起整套系统 |
| GitHub 写操作的幂等 | `github_action` 表记录每个写操作，写之前先在 GitHub 上找已存在的结果（§3.1） | 依赖 GitHub 去重；分布式事务 | GitHub API 不支持幂等键。先记账、再查找、再写，可以让 controller 在任何时刻崩溃后重跑，都不会重复评论或重复投票 |
| Agent 运行时 | pi SDK（`@earendil-works/pi-coding-agent`，精确锁定版本） | 继续用 v1 自研的循环；LangChain / LangGraph | pi 的 session 可以序列化成 JSONL 再原样载入，这是崩溃恢复的基础（§4.3）；自带只读工具、模型重试和用于测试的 faux 模型。v1 自研循环的维护成本高；LangGraph 的状态是图节点粒度，要让它和 Pod 生命周期、per-turn 上传对齐，改造量不比直接用 pi 小 |
| 结论的输出方式 | 模型调用带 schema 的 `submit_review` 工具 | 让模型输出 JSON 再解析（v1 的做法） | 参数由 schema 约束，评论锚点由工具当场校验并把错误返回给模型修改，彻底消除"JSON 格式不对"一类的失败（§4.2） |
| 模型凭据 | LiteLLM proxy 持有真实 key，每个 run 签发带预算上限、会过期的虚拟 key | 把真实 key 注入 runner；自己写一个代理 | 模型拿不到真实 key，被注入时最多花掉本 run 的预算；吊销、预算和花费统计 LiteLLM 都已实现，不必自己造（§5） |
| 模型 | DeepSeek `deepseek-flash`，thinking `high` | 更大的模型；降低思考强度 | 成本约 $0.03 一次，93% 以上的输入命中前缀缓存；测算的 100 次运行都成功调用了 `submit_review`。降低思考强度会漏掉问题（[成本报告](cost-report.md#优化对比)） |
| 编程语言 | gateway、controller、fetch 用 Go；runner 用 TypeScript | 全部用 TypeScript | pi SDK 是 TypeScript 的。控制面用 Go 静态编译，镜像约 13 MB，直接调 Kubernetes REST API、不引入 client-go（§6.1） |
| GitHub 集成 | GitHub App | 个人 token（PAT）；GitHub Actions | App 有自己的 bot 身份，有 Contents: write 时 approve 能计入审批数；按 installation 签发短期 token，fetch 拿到的是只读、只能访问本仓库的 token（§6.4）。Actions 要求每个仓库各自配置 workflow 和 secret，也无法跨 PR 统一调度和统计 |
| 网络隔离 | NetworkPolicy：runner 只能连 DNS、LiteLLM、controller；fetch 经 controller 的 CONNECT 代理访问 GitHub | 允许 runner 出网 | 即使模型被注入，也无法把代码或凭据发到外部（§4.1、§5） |
| 部署与观测 | Helm chart；controller 输出 Prometheus 指标，Grafana 看板随 chart 部署 | 手写 YAML；在内存里累计指标 | 一条 `make deploy` 拉起整套系统。指标在抓取时由数据库计算，controller 重启或多副本时数值一致（§7.1） |

### 2.2 负载、状态、扩缩容与故障恢复

**负载特征。** PR 事件是突发的：一次推送触发一个 run，团队集中提交时会同时来好几个。每个 run 的端到端 P50 106 秒、P95 224 秒，其中 95% 以上是模型评审的时间（[成本报告](cost-report.md#瓶颈分析)）。runner 主要在等模型 API，申请的资源很少（250m CPU、512Mi 内存）。所以吞吐的上限不在集群算力，而在模型 API 的并发、限流和预算。

**扩缩容。** 各组件的伸缩方式不同：

| 组件 | 状态 | 怎么扩 | 当前配置 |
| --- | --- | --- | --- |
| gateway | 无状态，去重靠数据库主键 | 水平扩副本 | 2 副本 |
| controller | 内存里不保存状态，指标从数据库计算 | 领取任务（advisory lock 加 `SKIP LOCKED`）和迁移已在多副本间串行化；领取之后的步骤靠"先查再写"保证幂等，没有在多副本并发下验证过；开启预览时必须单副本（§6.2） | 1 副本，故障时由 Deployment 重建。reconcile 是轻量的数据库扫描，不是瓶颈 |
| runner | 一次性 Job | 并发数由 `maxRunning` 控制：有空位就从队列领取，空闲时 0 个 Pod | `runs.maxRunning` 默认 4 |
| LiteLLM | 虚拟 key 和花费在它自己的数据库里 | 可以多副本 | 1 副本 |
| PostgreSQL | 全部业务状态 | 生产环境接外部托管数据库（`database.existingSecret`） | 开发用单副本，不持久化 |

runner 的弹性来自"按需建 Job"：排队的 run 越多，同时存在的 Pod 越多，直到 `maxRunning`；评审结束 Pod 就被回收。没有用 HPA 按 CPU 扩缩，因为 runner 的瓶颈不是 CPU，而是模型 API 的并发和花费；并发上限直接按模型账户的限额设置。按 `maxRunning` 4、P50 106 秒估算，约每小时 135 次评审，远高于"每 100 个 PR 每月约 300 次评审"的预估量。节点资源不够时 Job 的 Pod 会 Pending，托管集群上可以配合节点自动扩容补节点。下一步可以按 `review_queue_depth` 指标（§7.1）动态调整并发，例如接入 KEDA。

**状态管理。** Pod 里没有不能重建的东西，所有状态都在 PostgreSQL：

| 状态 | 存在哪里 | 丢了 Pod 会怎样 |
| --- | --- | --- |
| 评审进度 | `review_run.state` 和状态机（§3） | 不受影响 |
| 哪些 GitHub 写操作已完成 | `github_action`（§3.1） | 不受影响；重跑时跳过已完成的写操作 |
| agent 的上下文 | `run_session`，每轮一份 JSONL 快照 | 新 Pod 从最近一轮继续 |
| 已花的钱 | `run_usage`，每轮上报 | 下次签发的虚拟 key 只给剩余预算 |
| 凭据 | 每次尝试一个 Secret，归 Job 所有 | 随 Job 删除，新尝试签发新的 |
| 等待人工确认 | 一行 `awaiting_approval` 状态 | 等待期间本来就没有 Pod |

**故障恢复与异常处理。** 下表按故障来源汇总，细节见 §4.3、§6.3、§6.4：

| 故障 | 影响 | 处理 |
| --- | --- | --- |
| 工具调用出错（读不存在的文件、git 子命令不允许等） | 一次工具调用失败 | 错误作为工具结果返回给模型，由模型换一种方式继续 |
| 模型输出异常：评论锚点不在 diff 上 | `submit_review` 拒绝 | 把错误返回给模型修改后重新提交（§4.2） |
| 模型输出异常：结束时没有提交结论 | 没有结论 | 追加提示，最多 2 次，仍不提交则以确定性失败结束 |
| 模型 API 超时、限流、5xx | 当前请求失败 | pi 重试 3 次；仍失败则 Pod 以可重试的退出码结束，controller 从快照重试 |
| runner Pod 被删除、驱逐、OOM，节点故障 | 当前一轮白跑 | controller 把 run 放回队列，新 Pod 从快照继续，最多 3 次尝试（§4.3） |
| 单次尝试超时 | 评审卡住 | Job 的 `activeDeadlineSeconds` 到期即失败；controller 另有按领取时间计算的兜底超时 |
| 超出预算 | LiteLLM 拒绝请求 | 以 `budget_exceeded` 失败，不重试；runner 在花到 80% 时会先提醒模型尽快提交（§7.3） |
| controller 崩溃或重启 | 状态推进暂停 | 状态都在数据库，重启后重新扫描；所有步骤幂等，不会重复评论或投票 |
| GitHub API 失败 | 写操作没完成 | 限流、网络错误和 5xx 下一轮重试；确定性错误（404、422 等）直接失败 |
| webhook 投递失败 | 漏掉一次触发 | gateway 在数据库就绪前不接流量；controller 每 5 分钟重投 GitHub 记录为失败的投递（§6.4） |
| 同一 PR 来了新提交 | 旧评审过时 | 旧 run 标为 `superseded`，删除 Job，Check Run 完成为 cancelled |

**人工介入。** 系统不替人做最终决定，也不在出错时静默：

- 每次失败都写进 Check Run，附失败原因和尝试次数，开发者在 PR 页面就能看到。
- 失败的 run 不会无限重试。要重新评审，推送一个新提交即可（`/review` 命令尚未接入，§6.4）。
- 默认的 `vote` 模式只投一票，是否合并由人和 branch protection 决定；`manual` 模式要有写权限的人评论 `/merge` 或提交 approve 才合并（§6.3）。
- 本地开发经 smee 转发时，自动重投帮不上忙，需要在 GitHub App 设置里手动重投（§10）。

## 3. Run 状态机

一次 run 对应 `(repo, pr_number, head_sha)`。同一个 PR 推送新提交，旧 run 被标记为 `superseded`，对应的 Job 被删除。

```mermaid
stateDiagram-v2
    [*] --> queued: 非 draft PR opened / synchronize / reopened / ready_for_review
    queued --> running: controller 领取并创建 Job
    running --> previewing: 仓库开启了预览，且评审阶段完成
    previewing --> running: 预览就绪或等待超时，进入验证阶段
    running --> reviewed: runner 提交结论
    reviewed --> done: 已投票；approvalMode = vote（默认），或结论不是 approve
    reviewed --> awaiting_approval: approvalMode = manual 且结论为 approve
    reviewed --> failed: GitHub 拒绝投票（确定性错误）
    reviewed --> merging: approvalMode = auto 且结论为 approve
    awaiting_approval --> merging: 收到有写权限的人的 /merge 评论或当前提交的 approve
    awaiting_approval --> failed: GitHub 返回确定性错误
    merging --> done
    merging --> failed: 合并被 GitHub 拒绝
    running --> queued: Pod 失败且未超过重试次数
    running --> failed: 超过重试次数或超时
    previewing --> failed: 尝试超时或不可恢复错误
    previewing --> queued: Pod 失败，重试部署与评审
    queued --> superseded: 新的 head_sha 到达
    running --> superseded
    previewing --> superseded
    awaiting_approval --> superseded: 新提交或 PR 已关闭
    done --> [*]
    failed --> [*]
    superseded --> [*]
```

进入 `reviewed` 后，controller 先发布 PR Review（即投票，§6.3），再按 `approvalMode` 决定去向。默认的 `vote` 模式下 run 到 `done` 就结束，之后的合并与本系统无关。

### 3.1 数据表

表结构以 [`go/internal/store/migrations/`](../go/internal/store/migrations/) 下按序执行的迁移为准，这里只说明每张表的作用和约束：

| 表 | 作用 | 关键约束 |
| --- | --- | --- |
| `review_run` | 一次评审，状态机的载体；记录 `installation_id`（controller 换 token 用）、当前尝试的序号 `attempt` 与领取时间 `claimed_at`（单次尝试的时限从这里算），以及失败原因 `error`（写到 Check Run；重试时记录上一次尝试的失败原因） | `UNIQUE (repo, pr_number, head_sha)`；`state`、`verdict` 用 CHECK 限定取值；处于 `reviewed`、`awaiting_approval`、`merging`、`done` 时 `verdict` 必须非空；活跃 run 有部分索引供 controller 扫描 |
| `webhook_delivery` | 已记录的 webhook delivery ID，GitHub 重投时去重 | 主键 `delivery_id` |
| `github_action` | 每个 GitHub 写操作一行：`dismiss_stale_approvals`（撤销旧票）、`check_run_create`、`check_run_complete`、`review`（投票，§6.3）、`merge` | 主键 `(run_id, kind)`；`external_id` 非空表示已完成；取值在 `go/internal/store/store.go` 里也有一份 |
| `merge_signal` | gateway 记录的人工确认，绑定当前已产出结论的 manual run；controller 核对权限后消费 | 主键 `delivery_id`，引用 `webhook_delivery`；不跨 run 复用，拒绝的确认标记 `handled` |
| `run_session` | runner 每轮结束上传的 pi session 快照（JSONL），用于崩溃恢复 | 每个 run 只保留一份；轮次更早的上传被忽略，迟到的请求不会把快照回退；只在 `running` 时接受 |
| `run_usage` | 每轮的模型、输入、输出、缓存读写 token 和金额 | 主键 `(run_id, turn)`，重复上报覆盖而不是累加；任何状态都接受，花掉的钱照记 |

`review_run.repo_config` 和 `config_sha` 保存 controller 第一次领取后读取的仓库配置及目标分支提交；配置保存和 `budget_usd` 更新在同一条 SQL 中完成，首次写入后不再覆盖。升级前已经进入 `reviewed` 且无配置快照的 run 仍按 `vote` 完成。

`review_run` 还记录各阶段的时间戳（`started_at`、`fetch_started_at`、`fetch_finished_at`、`reviewed_at`、`finished_at`，见 `0002_metrics.sql`），供 §7.1 的阶段耗时使用。

M3 与 M6 独立部署过的迁移分别保留 `0002_recovery.sql` 和 `0002_metrics.sql` 文件名；共同新增的缓存 token 列用 `IF NOT EXISTS`，支持从任一版本升级。就绪检查要求全部内嵌迁移都已执行。`started_at` 保留首次领取时间，`claimed_at` 记录当前尝试时间。

状态取值在 [`go/internal/run/state.go`](../go/internal/run/state.go) 里也有一份，同时定义了上图的全部合法转换，改一处必须同步另一处。

controller 启动时执行嵌入的迁移（`schema_migration` 表记录已执行的版本，advisory lock 串行化多副本）。领取任务用 `SELECT ... FOR UPDATE SKIP LOCKED`，多副本 controller 也不会重复领取。GitHub 写操作前先查 `github_action`，`external_id` 非空就跳过；写成功后回填 `external_id`，再推进状态。写成功但回填前崩溃的窗口由"写之前先在 GitHub 上找"补上：Check Run 按 `external_id`（即 run ID）查找，投票按 review 正文末尾的隐藏标记 `<!-- ai-pr-review run=<id> -->` 查找，找到就直接回填，不再写第二次。撤销旧票本身幂等（已撤销的票不再是 APPROVED），完成 Check Run 是 PATCH，重复执行无害。

## 4. runner 设计（pi SDK）

### 4.1 Pod 结构

```
Pod (Job: review-<run-id>-<attempt>)
├── initContainer: fetch（alpine + git）
│     env: GITHUB_TOKEN = 只读、1 小时有效、只能访问本仓库的 installation token
│          PROXY_TOKEN  = 使用 controller 出站代理的凭据
│                         （两者都来自 Secret review-<run-id>-<attempt>-credentials，归 Job 所有，随 Job 删除）
│          HTTPS_PROXY  = http://<run-id>:$(PROXY_TOKEN)@<controller>
│          REPO, PR_NUMBER, HEAD_SHA, BOT_LOGIN, WORKSPACE=/workspace
│     作用: 经 controller 的代理克隆 head（往前 20 个提交）到 /workspace/repo、拉取 base、写出 /workspace/pr.json（元信息、diff、bot 之前的逐行评论）
│     token 经 git 的环境变量配置传入，不进 argv 和 .git/config；结束后随容器退出，主容器读不到
├── container: runner
│     env: RUN_ID, CONTROLLER_URL, RUN_TOKEN（回调 controller 用）,
│          LLM_API_KEY（本次 run 的 LiteLLM 虚拟 key）, LLM_BASE_URL（LiteLLM 的 Service）
│          两个 key 来自同一个 Secret，fetch 读不到；GITHUB_TOKEN、PROXY_TOKEN 不注入主容器
│     volume: /workspace、/tmp (emptyDir)；根文件系统只读
│     serviceAccount: 不挂载 token；预览阶段使用绑定到 PR namespace 的只读 SA
└── NetworkPolicy: 整个 Pod 出站只允许 DNS、LiteLLM、controller；入站全部拒绝（M5 预览阶段再放行 kube-apiserver）
```

NetworkPolicy 作用于整个 Pod，init 容器也连不上外网，所以 fetch 通过 controller 访问 GitHub：controller 的 HTTP 端口同时是一个只做 CONNECT 的代理，目标限定为 `runs.fetchEgressHosts`（默认 `github.com:443`、`api.github.com:443`），并要求 `Proxy-Authorization` 带上本 run 的 ID 和 `PROXY_TOKEN`，且 run 仍处于 `running`。TLS 在 fetch 与 GitHub 之间端到端建立，controller 看不到 GitHub token。主容器能连到 controller，但没有 `PROXY_TOKEN`，用不了这个代理。

两个容器都以 UID 1000（runner 镜像的 `node` 用户）运行，git 才会信任 fetch 留下的工作区。blob 一次拉全，不用部分克隆：runner 没有凭据，之后也没有外网，无法按需补拉。fetch 发现 PR 的 head 已不是本 run 的 `head_sha` 时直接失败（退出码 3，不重试），新提交有自己的 run。

同一 PR 再次评审时，fetch 用 GraphQL 取回 bot 自己的逐行评论线程（最近 100 个线程中最多 30 条，每条截断到 2000 字节），连同 `isResolved`、`isOutdated` 写进 `pr.json` 的 `previousComments`。提示词要求模型不重复已解决或已修复的问题，仍然存在的问题注明"上次评审未解决"，把精力放在新改动上。这些评论引用了 PR 代码，在提示词里同样按不可信输入包起来。取不到时只打警告，评审照常进行。GraphQL 里 bot 的登录名没有 REST 的 `[bot]` 后缀，比较时去掉。

runner 在集群模式下把失败原因写进 `/dev/termination-log`。容器失败时，退出信息（`terminationMessagePolicy: FallbackToLogsOnError`，即日志末尾）由 controller 读出，写进 `review_run.error` 和 Check Run。退出码 3（`run.ExitFinal`）表示重试也没用：fetch 遇到 head 已变、GitHub 401/404/410/422，runner 遇到结果被 controller 拒绝、`pr.json` 不合法、模型被提醒两次仍不提交、预算耗尽。其余失败都会重试（§4.3）。

### 4.2 执行流程

沿用 v1 的两阶段思路，但第二阶段从"让模型输出一段 JSON 再解析"改为"调用一个带 schema 的工具"，彻底消除 JSON 解析失败的情况。

1. 读 `/workspace/pr.json`，构造系统提示词和用户提示词。提示词按 v1 的防幻觉规则重写，放在 `runner/src/prompts.ts`。
2. **探索阶段**：pi 内置的只读工具 `read`、`grep`、`find`、`ls`，加上自定义的 `git` 工具（只允许 `log`、`show`、`diff`、`blame` 等只读子命令，不经过 shell），`cwd` 为 `/workspace/repo`。评审阶段不给 `bash`：本地模式跑在开发者机器上，一个带注入的 PR 不应能执行任意命令。
3. **预览验证阶段**（可选）：controller 部署好预览环境后，runner 收到一条 follow-up 提示，附上 namespace 名称；此时才启用受限的 `kubectl` 工具，只允许读取这个 namespace 的 Pod、事件、Deployment、ReplicaSet 和日志，不开放 shell。有预览时 `submit_review` 也只在这一阶段才提供。系统提示词按是否有预览生成：有预览时写明两个阶段和各自可用的工具，探索阶段的用户提示词和恢复提示只要求探索完结束本轮，只有第二阶段的提示要求调用 `submit_review`；预算提醒也按阶段措辞。不靠"工具不存在"的报错教模型。
4. **提交阶段**：模型调用 `submit_review`，参数是 §4.4 的 `verdict`、`body`、`comments`。工具先检查每条评论的 `path:line` 是否落在 diff 的新增行或上下文行上（GitHub 只接受这些位置），不合法就把错误返回给模型让它改；合法则记录结果并结束本轮 agent。若模型结束时没调用，追加提示要求提交，最多 2 次，之后判为失败。最后一轮是模型服务报错（pi 重试后仍失败，例如 402 余额不足）时不再提醒，直接以 `model API error: ...` 失败，免得被误读成模型不肯提交。
5. 结束 session，进程以 0 退出。集群模式把结果 `POST {CONTROLLER_URL}/runs/{id}/result`，带 `Authorization: Bearer <RUN_TOKEN>`，controller 读本 run 的 Secret 比对，不符返回 401：网络错误和 5xx 退避重试，4xx 不重试；controller 对同一结果的重复 POST 返回成功，所以响应丢失后重试是安全的。

本地模式（`--pr <url>`）走同样的流程，只是第 1 步自己用 GitHub API 拉取 PR、部分克隆（`--filter=blob:none`，head 往前 20 个提交加 base 提交）到临时目录，最后把结果按 `--format markdown|json` 打印到 stdout，token 与金额打印到 stderr。

prompt 和资源完全由 runner 提供：不使用 pi 默认的资源发现，被评审仓库里的 `AGENTS.md`、`.pi/` 扩展和技能都不会加载。它们是 PR 作者可控的内容，加载扩展等于执行 PR 里的代码。PR 标题、描述和 diff 在提示词里用 `*_untrusted` 标签包起来，并转义其中伪造的闭合标签。

以下摘自 `runner/src/review.ts`，基于 pi SDK `1.3.0`（`runner/package.json` 精确锁定）：

```ts
const { session } = await createAgentSession({
  cwd,
  agentDir,                                          // 临时目录，不读 ~/.pi
  model,
  modelRuntime,
  thinkingLevel,
  resourceLoader: isolatedResources(systemPrompt(language, preview)), // 有预览时写明两个阶段
  sessionManager: SessionManager.inMemory(cwd, undefined, resumeFrom), // 上一次尝试的快照，见 4.3
  settingsManager: SettingsManager.inMemory({ retry: { enabled: true, maxRetries: 3 } }),
  tools: ["read", "grep", "find", "ls", "git", "submit_review"],
  customTools: [gitTool(cwd), submitReviewTool(commentableLines(pr.diff), onSubmit)],
});

// 集群模式：每轮结束时同步取快照，排队依次上传；上传失败只记日志
session.subscribe((e) => {
  if (e.type === "turn_end") {
    const snapshot = snapshotOf(sessionManager, e.message);        // 轮次、JSONL、本轮用量
    uploads = uploads.then(() => onTurn(snapshot));                // PUT /session，PUT /usage
  }
});

// 有预览时第一条提示只要求探索；崩在第二阶段的恢复直接回到第二阶段
if (!resumeInPreview) await session.prompt(resumeFrom ? resumePrompt(preview) : userPrompt(pr, { preview }));
if (preview) {
  previewAccess = await preparePreview();
  session.setActiveToolsByName([...baseTools, "kubectl", "submit_review"]);
  await session.prompt(previewPrompt(previewAccess.namespace, previewAccess.status, resumeInPreview));
}
for (let i = 0; !submitted && i < MAX_SUBMIT_REMINDERS; i++) await session.prompt(SUBMIT_REMINDER);
```

### 4.3 崩溃恢复

runner 每轮（一次模型回复加它触发的工具调用）结束时，把整个 pi session 序列化成 JSONL（header 一行，之后每个 entry 一行，与 pi 的 session 文件格式相同）。先把本轮用量 `PUT /runs/{id}/usage`，失败记日志，然后独立尝试 `PUT /runs/{id}/session?turn=N`；快照被拒绝不会跳过用量上报，用量上传失败也不会跳过快照。快照在 `turn_end` 事件里同步取，上传在后台排队依次进行，失败不中断评审。新 Pod 启动时 `GET /runs/{id}/session`，有快照就用 `SessionManager.inMemory(cwd, undefined, entries)` 载入，追加一条"进程中断后已恢复，从上次完成的步骤继续"的提示，否则从头开始。有预览时按快照判断崩在哪个阶段：快照里已有第二阶段提示的，重新取得预览访问后直接回到预览验证；否则继续探索，提示里不要求提交。预算提醒计入快照中已花费的金额。这三个接口和结果回调一样用本次尝试的 `RUN_TOKEN` 鉴权，旧尝试的 token 随旧 Job 的 Secret 一起失效；旧 token 被拒绝时仍可能丢失最后一轮用量。

| 场景 | 处理 |
| --- | --- |
| runner Pod 被删除、驱逐、OOM、节点故障 | Job 失败（`backoffLimit: 0`）。controller 判断可重试且 `attempt` 未到上限（`MAX_ATTEMPTS`，默认 3），就把 run 放回 `queued`，Check Run 显示"第 2 次尝试"；下一轮领取时 `attempt + 1`，新 Job 带新 Secret 和新 token，runner 从快照继续 |
| 中途崩在某次工具调用里 | 快照只在轮次结束时取，恢复后被打断的那一轮重跑。探索阶段工具都是只读的，重跑安全；那一轮已经花掉的 token 不会上报 |
| `submit_review` 已被接受但结果还没送到 controller | 恢复时在快照里找到被接受的 `submit_review` 调用，直接上报它的参数，不再调用模型 |
| 结果已送到、进程退出前崩溃 | controller 已推进到 `reviewed`，Job 失败被忽略 |
| 模型 API 超时、限流、余额不足 | pi 自带重试；仍失败的请求不算"模型没提交"，不消耗提交提醒，runner 以退出码 1 退出，走第一行 |
| 确定性失败（退出码 3，见 §4.1） | 不重试，直接 `failed` |
| 单次尝试超时 | Job 的 `activeDeadlineSeconds` 到期（`DeadlineExceeded`）直接 `failed`，不重试。另外从 `claimed_at` 起超过时限加 5 分钟还没结果的尝试也会失败并删除 Job，覆盖 Job 一直建不出来、GitHub 调用一直失败等情况 |
| controller 重启 | 状态全在数据库，重新扫描即可；写 GitHub 之前先查 `github_action`，再在 GitHub 上找本 run 已写过的 Check Run 和投票（§3.1），所以不会重复评论或重复投票 |
| 超过 `attempt` 上限 | `failed`，Check Run 写明尝试次数和最后一次的原因 |

每一种故障都有对应的测试：controller 侧见 `go/internal/controller/recovery_test.go`（丢 Pod 后续跑、重试上限、确定性失败、尝试超时、写 GitHub 后回填前崩溃、新提交取代旧 run），runner 侧见 `runner/src/review.test.ts`（用 pi 的 faux 模型验证从快照续跑、已提交的结论不再调用模型）。

### 4.4 评审结论：模型决定，形状对齐 GitHub Review

结论由模型自己给出：它读完代码后决定 approve 还是 request_changes，并直接写出给人看的评审正文。runner 不再像 v1 那样让模型填一份 `must_fix` / `points_of_interest` 报告、再由程序拼成 Markdown。

但这个决定仍然要以结构化数据交出来，由 controller 代表 AI 去 GitHub 投票（§6.3），而不是让模型自己调用 GitHub：

- runner 没有 GitHub 凭据（§5），所有写操作集中在 controller，结论只能作为数据交给它；
- controller 要靠 `verdict` 推进状态机（`reviewed` → `merging` / `awaiting_approval` / `done`）；
- 逐行评论需要 `path` 和 `line`；`failOn` 门禁需要 `severity`。

所以结构只保留机器真正用到的字段，其余都是模型写的 Markdown。形状和一次 GitHub PR Review 一一对应。`request_changes` 表示模型认为"还不能合并"，是结论的取值，不是 GitHub 的 review 事件名：

| 字段 | 含义 | 去向 |
| --- | --- | --- |
| `verdict` | `approve` / `request_changes` | 投票：`approve` 提交 APPROVE review，`request_changes` 提交 COMMENT review（§6.3）；`auto` / `manual` 模式下还决定是否进入合并 |
| `body` | Markdown 正文，要求简短：改了什么、结论理由，以及（有的话）未验证的疑点、没看的部分和至多 3 条次要建议 | review 正文、Check Run 摘要 |
| `comments[]` | `path`、`line`（新文件行号）、`severity`（critical/high/medium/low）、`body` | 逐行评论；`severity` 供 `failOn` 使用 |

runner 交给 controller 的 `ReviewResult` 在此之外加上 `schema_version: 4`、`pr`（`repo`、`number`、`head_sha`，controller 用来拒绝不属于当前 run 的结果）、`model`、`generated_at`。评论最多 15 条，所有字段必填，不允许多余字段。

两侧定义：TypeScript 在 [`runner/src/schema.ts`](../runner/src/schema.ts)（TypeBox，同时作为 `submit_review` 的参数 schema），Go 在 [`go/internal/review`](../go/internal/review/review.go)。[`contract/review-result.schema.json`](../contract/review-result.schema.json) 由 TypeBox 生成，`contract/examples/` 里的合法、非法样例由两侧测试共同校验；Go 测试还会核对字段、枚举、上限与生成的 schema 一致，并检查 `verdict` 取值与数据库 CHECK 约束一致。

逐行评论只留给对照代码（或预览阶段的运行时证据）核实过、合并前值得处理的 critical、high、medium 问题，通常 0 到 5 条；没能核实的疑点写进正文并注明未核实，不做逐行评论；低级别和风格建议最多 3 条，一行一条写进正文。每条评论都要占用作者的注意力，评论太多反而会淹没真正的问题。

提示词里的判定规则（`verdict_policy`）：存在核实过的 critical 或 high 问题时 request_changes；medium、low、风格建议和未核实的疑点不阻塞合并；没能审到最可能出问题的部分时也应 request_changes 并说明原因。防幻觉规则沿用 v1：每条评论必须引用读到的代码，不得编造没读过的代码，critical/high 要做反向检验。探索不设固定配额（v1 要求至少读 3 个文件、追 2 个调用方），改为"读到足以证实或排除每个疑点为止"，单文件 PR 不必遍历十几个文件。

schema v4 去掉了 v3 逐行评论的 `confidence`（high/medium/low）。代码里没有任何地方按它过滤，它只被渲染成 "confidence medium"，读者难以据此判断；与其让模型自评置信度，不如只让核实过的发现成为逐行评论。

### 4.5 模型接入

runner 通过环境变量选择模型，`ModelRuntime` 不读写 `~/.pi`：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `LLM_API_KEY` | — | 必填。集群模式下是本次 run 的 LiteLLM 虚拟 key |
| `LLM_PROVIDER` | `deepseek` | pi 内置 provider 保留其模型目录（价格、兼容参数）；其他名称按 OpenAI 兼容接口注册 |
| `LLM_MODEL` | `deepseek-flash` | |
| `LLM_BASE_URL` | — | 覆盖 provider 地址，集群模式指向 LiteLLM 的 Service |
| `LLM_THINKING` | `high` | `off` 到 `max` |

花费按 pi 模型目录的单价计算，`usage` 含 `input`、`output`、`cacheRead`、`cacheWrite`、`cost.total`。DeepSeek 会返回缓存命中量：M0 验收的三个 PR 里，缓存读取的 token 是非缓存输入的 11 到 14 倍。

集群模式下 pi 仍使用内置的 `deepseek` provider（保留模型目录和兼容参数），只把 `baseUrl` 指向 LiteLLM。LiteLLM 的配置由 chart 从 `llm.*` 生成：`model_name` 与 `llm.model` 一致，`litellm_params.model` 取 `llm.upstream.model`（`deepseek/deepseek-flash`），真实 key 从 `llm.existingSecret` 注入。两处需要注意：

- LiteLLM 的 DeepSeek 适配会丢弃 `reasoning_effort`，pi 用它表达 thinking 级别，所以配置里加了 `allowed_openai_params: [reasoning_effort]`。`thinking`、assistant 消息上的 `reasoning_content`、`tools` 的 `strict` 都原样转发（v1.104.2 实测）。
- LiteLLM 按 `llm.pricing`（每百万 token 的美元价，与 pi 目录一致）给虚拟 key 记账，`max_budget` 依赖这份价格。

LiteLLM 的 readiness 和 liveness 探针超时设为 5 秒；liveness 每 10 秒检测一次，连续失败 6 次才重启，CPU request 为 250m。长评审的流式请求会延迟探针响应，默认 1 秒超时曾导致 proxy 重启、中断正在进行的模型请求。

## 5. 安全边界

demo 不做合并加固，但"模型拿不到真实密钥"是本项目的目标之一，而且实现成本低。PR 内容（代码、描述、评论）全部视为不可信输入。

| 凭据 / 能力 | 谁持有 | runner 主容器能否拿到 |
| --- | --- | --- |
| 模型厂商真实 API key | LiteLLM proxy | 否。只有本次 run 的虚拟 key，带预算上限，run 结束即吊销 |
| LiteLLM master key | controller、LiteLLM | 否 |
| GitHub App 私钥 | controller | 否 |
| GitHub 只读 installation token | fetch initContainer | 否。initContainer 退出后不可见 |
| 出站代理凭据（`PROXY_TOKEN`） | controller、本 run 的 fetch | 否。只注入 fetch；没有它，主容器无法经 controller 访问 GitHub |
| 回调 token（`RUN_TOKEN`） | controller、本 run 的 runner 主容器 | 是，只能为本 run 提交结果；没有它无法伪造结论让 bot 投 APPROVE |
| GitHub 写操作（评论、approve、merge） | controller | 否。runner 只能提交结论 |
| 集群访问 | 预览阶段的只读 SA | 只限 PR 自己的 namespace：pods、pods/log、events、deployments 的 get/list/watch |
| 外网访问 | — | 否。NetworkPolicy 只放行 DNS、LiteLLM 和 controller（M5 预览阶段加 apiserver） |

虚拟 key 的生命周期：

- **签发。** controller 创建 Job 后、创建本 run 的 Secret 前调用 LiteLLM 的 `/key/generate`：`max_budget` 取 run 的 `budget_usd` 减去已上报的 `run_usage.cost_usd`，余额小于等于 0 则以 `budget_exceeded` 失败，不再签发；`models` 只含 `llm.model`，`key_alias` 为 Job 名，`duration` 为 Job 的 `activeDeadlineSeconds` 加 5 分钟。Secret 已存在时删除本次多签发的 key；其他创建错误可能是响应丢失但 Secret 已持久化，因此保留 key 并在下一轮检查 Secret，不确定未使用的 key 由到期机制兜底。
- **吊销。** run 离开 `running` 时（收到结论后进入投票、失败、重试或被新提交取代）controller 从 Secret 读出 key 并调用 `/key/delete`；超时、重试和 superseded 路径先吊销再删除旧 Job，避免 Secret 被级联删除后失去 key。吊销失败只记日志：key 到期自动失效，且预算有上限。controller 在签发与写入 Secret 之间崩溃会留下一个没人拿到的 key，同样到期失效。
- **超预算。** LiteLLM 在请求前检查花费，超出后返回 `budget_exceeded`。pi 不重试这类错误；runner 不再追加提交提醒，以 `budget_exceeded: ...` 为失败原因退出，controller 原样写进 `review_run.error` 和 Check Run（§7.3）。

LiteLLM 自己的入站也用 NetworkPolicy 限定为 runner 和 controller。

被注入的模型最多能做到：浪费本次 run 的预算、给出错误结论。默认的 `vote` 模式下，错误的 approve 只是一票，仍要满足仓库其余的审批要求；`auto` 模式下错误结论可能导致合并，这是 §0 里明确接受的风险。

## 6. controller 关键逻辑

### 6.1 并发与调度

- 全局最大并发 `maxRunning`（`runs.maxRunning`，默认 4），超出的 run 留在 `queued`，按创建时间先后领取。按仓库限制并发尚未实现，一个仓库集中推送时会占满全部并发。
- runner Pod 设置 requests 和 limits（`runner.resources`，默认申请 250m CPU、512Mi 内存，内存上限 1536Mi），防止一次大 PR 吃光节点。runner 所在 namespace 尚未配 `ResourceQuota` 和 `LimitRange`，同时存在的 Pod 数由 `maxRunning` 限制；预览 namespace 有自己的 ResourceQuota（§6.2）。
- Job 设置 `activeDeadlineSeconds`、`ttlSecondsAfterFinished`；`backoffLimit: 0`，重试由 controller 统一决定（这样才能带上 session 快照恢复，§4.3）。
- 冷启动优化：runner 镜像预装 pi 依赖、git 和 kubectl；fetch 只拉 head 往前 20 个提交加 base（§4.1）。集群内实测拉代码 4 到 9 秒，暂不需要 git mirror 缓存。
- 权限：controller 只有自己 namespace 内的 Role，可以管理 `jobs`、读取 `pods`、创建、读取和删除每个 run 的 `secrets`（fetch 的 GitHub token 和代理凭据、runner 的回调 token 和虚拟 key）。开启 M5 预览时，另外启用 §6.2 的 ClusterRole，授予动态 namespace 及明确列出的预览资源权限。
- Kubernetes 访问：controller 只用到 Job、Secret、Pod 的少数几个调用，`go/internal/kube` 直接调 REST API（in-cluster token 每次请求重读），不引入 client-go，controller 镜像保持在十几 MB。M5 需要部署 Helm chart 时再评估。
- 每轮 reconcile（5 秒一次，收到结果或放回队列时立即触发）：把有更新提交的未完成 run 标为 `superseded`；领取 `queued`；对 `running` 先检查尝试是否超时，再依次撤销旧票、创建 Check Run、确保 Job 和 token Secret 存在、按 Job 状态决定重试或失败；对 `reviewed` 投票、完成 Check Run，并按仓库策略推进到 `done`、`awaiting_approval` 或 `merging`；等待人工确认时回收 runner Job 和虚拟 key，确认权限及当前 head 后才合并；对已失败但 Check Run 还没收尾的 run 补写失败结论；对 `superseded` 的 run 删除 Job、把 Check Run 完成为 `cancelled`。每一步都是幂等的，崩溃后重跑即可。
- 同一 PR 的新提交到达后，旧 run 只要还没到 `reviewed`（`queued`、`running`、`previewing`、`awaiting_approval`）就被取代，它的 Job 连同 Pod 和 Secret 一起删除（`propagationPolicy: Background`），之后再到的结果、快照都被拒绝。已经 `reviewed` 的 run 让它投完：投票前会检查 head（§6.3）。

### 6.2 预览环境

- 仓库在 `.ai-pr-review.yaml` 里声明 Helm chart 路径和可选 values 文件（初版不支持 kustomize）。
- controller 创建 `pr-<owner>-<repo>-<n>` namespace，配 ResourceQuota，用 PR 的镜像 tag 部署，等待 Ready 或超时。
- PR 关闭或合并时删除 namespace；另有定时清理，删掉超过 24 小时的孤儿 namespace。
- 构建镜像依赖仓库自己的 CI。demo 里约定 PR 的 CI 推送 `:<head_sha>` 镜像，controller 等这个 tag 出现。

M5 预览实现约定（验收记录见 roadmap）：

- 本仓库新增独立 HTTP 示例应用作为预览目标；故障验收 PR 只修改示例 chart 的 readiness 路径，避免破坏评审服务本身。
- runner 在代码探索后把配置和 chart 的普通文件作为有大小上限的数据包交给 controller。controller 离线执行 `helm template`，强制操作者映射中的 `image.repository` 和 `image.tag=<head_sha>`，并检查实际镜像完整匹配，先校验全部资源，再部署；不执行 PR 程序、Helm 插件或下载依赖。初版只支持 Deployment、ClusterIP Service 和 ConfigMap。
- 预览 namespace 使用 ResourceQuota、Restricted Pod Security admission 和默认拒绝网络策略。只读 ServiceAccount 的短期 token 在等待 Ready 或超时后才交给 runner，只在内存中保存，经 stdin 传入 kubectl，不写入文件或工具输出；超时保留现场，由模型读取实际证据，不能仅凭超时推断探针错误。
- `previewing` 表示部署等待，结束后返回 `running` 做运行时评审。此阶段失败可回到 `queued` 重试；状态取值未新增，数据库 CHECK 中已有 `previewing`。
- “ClusterRole 只含 namespaces”的权限不足以在动态 namespace 中创建工作负载、配额和只读授权。采用显式开关的预览 ClusterRole，列出所需资源及操作；Kubernetes RBAC 不能按 namespace 名称前缀约束它，controller 必须校验归属。chart 通过 `preview.enabled` 显式开启，默认关闭。开启时 controller 必须为单副本，升级使用 Recreate，避免多个进程同时部署同一 PR。
- PR 关闭清理由 controller 定时查询 GitHub 状态执行，使漏收 closed webhook 时也能清理；超过 24 小时、PR 已不可访问或没有进行中评审的 namespace 作为孤儿清理；GitHub 暂时出错时不删除。清理在取得该 PR 的锁后重新读取 namespace 的归属和创建时间，避免旧列表误删刚重建的环境。gateway 保持只记录触发的边界。

### 6.3 投票与合并

AI 的身份是 GitHub App bot（如 `ai-pr-review[bot]`）。每个 run 结束时，controller 调用 `POST /repos/{owner}/{repo}/pulls/{n}/reviews` 提交一条 PR Review，这就是 AI 的一票，和人在 PR 页面选择 Approve 后提交是同一个接口：

| 结论（`failOn` 校验后） | review 事件 | `vote`（默认） | `auto` | `manual` |
| --- | --- | --- | --- | --- |
| `approve` | APPROVE | 投票后结束 | 投票后合并 | 投票后等待 `/merge` 或人工 approve |
| `request_changes` | COMMENT | 投票后结束 | 不合并 | 不合并 |

- **发现问题时用 COMMENT，不用 REQUEST_CHANGES。** REQUEST_CHANGES 会一直阻塞合并，直到 bot 改投或有人 dismiss；AI 误判时会卡住 PR。COMMENT 把问题写清楚，是否合并留给人。
- **新提交到达时撤销旧票。** COMMENT 不会覆盖 bot 之前的 APPROVE，旧票会一直计数。所以同一个 PR 的新 `head_sha` 的 run 开始时，controller 先 dismiss bot 在该 PR 上针对其他提交、仍然有效的 APPROVE，再评审新提交；新的 approve 只针对评审过的 `head_sha`（review 的 `commit_id`）。仓库开了"Dismiss stale pull request approvals"时 GitHub 也会自动撤销，两者不冲突。
- **只在评审的提交仍是 head 时投 APPROVE。** 旧提交的 run 可能在新 run 撤销旧票之后才完成；此时投 APPROVE 会给没看过的代码计一票。所以投票前查一次 PR 的 head，不一致就改投 COMMENT。还在评审中的旧 run 会更早被 `superseded` 终止（§6.1），这条规则只兜底已经交出结论的那些。
- **每个 run 只投一票。** review 正文末尾带 `<!-- ai-pr-review run=<id> -->`，投票前先在 bot 的 review 里找这个标记，controller 在投票后、记录前崩溃也不会再投一次（§3.1）。
- **逐行评论被拒时折叠进正文。** `submit_review` 已保证锚点落在 diff 内，GitHub 仍返回 422 时，controller 把评论按严重程度列进 review 正文重新提交，不丢内容。
- **Check Run 的结论**：approve 为 `success`，request_changes 为 `neutral`（结论由投票表达；若仓库把 "AI Review" 设为必需检查，`failure` 会阻塞合并），run 失败为 `failure` 并附原因。
- **AI 的票计入"需要 N 个 approval"的前提**是 App 对仓库有写权限（Contents: write，M1 实测，§6.4）。branch protection 要求 Code Owners 审批时，这一项只能由人满足，App 不能成为 code owner。

是否合并只在一个函数里决定：

```go
// decideMerge 是 auto 和 manual 共用的唯一合并决策。
// confirmed 表示 controller 已核实当前 run 的人工确认。
func decideMerge(r store.Run, confirmed bool) bool {
    if r.Config == nil || r.Verdict != review.VerdictApprove {
        return false
    }
    return r.Config.ApprovalMode == repoconfig.Auto ||
        (r.Config.ApprovalMode == repoconfig.Manual && confirmed)
}
```

合并调用 GitHub 的 [merge API](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request)，使用 squash，并携带已评审的 `head_sha`。仓库需要允许 squash；branch protection 仍由 GitHub 执行。合并前再次读取 PR：提交变了或 PR 关闭则不调用合并；GitHub 返回 405、409、422 等确定性拒绝时 run 进入 `failed`，Check Run 改为 failure；限流、网络或 5xx 错误保留 `merging`，下一轮重试。若合并成功但记录前崩溃，重启后根据相同 head 的 `merged` 与 `merge_commit_sha` 补记 `github_action.merge`，不重复合并。

**manual 的人工确认。** gateway 仅接受 PR 上新建、去掉首尾空白后恰好等于 `/merge` 的评论，或新提交的 APPROVED review；忽略编辑事件、普通 issue、bot 和非审批 review。确认人的身份与时间来自签名 webhook。事件绑定到接收时已经 `reviewed` 或 `awaiting_approval` 的 manual run，且结论为 approve；review 必须匹配 head，事件创建时间不得早于本次评审完成（按 GitHub 时间戳的秒精度比较）。允许 `reviewed` 是为了覆盖“投票刚发布，人已确认，controller 尚未更新状态”的窗口。提前确认、旧事件或重复投递不会授权后续 run。

controller 查询确认人当前的仓库权限，只允许 write、maintain、admin；若是 review，还重新检查它尚未被撤销且对应当前 head。通过后进入 `merging`。没有确认时只保留数据库状态；等待前撤销虚拟 key、删除 runner Job 及所属 Secret/Pod，Kubernetes 的后台 GC 可能短暂保留 Terminating Pod。controller 重启不会重建等待中的 runner。新提交或 PR 关闭会使等待失效。

投票后 Check Run 先显示评审结果，让 GitHub 合并规则可以检查它；之后若合并失败或等待被取消，store 在状态变更事务中清除 `check_run_complete` 的完成标记，让 controller 可恢复地发布最终失败或取消结果。

### 6.4 GitHub App

| 项 | 取值 |
| --- | --- |
| 订阅事件 | `pull_request`（opened、synchronize、reopened、ready_for_review、closed）、`issue_comment`（`/review`、`/merge`）、`pull_request_review` |
| 权限 | Pull requests: write（投票、撤销旧票）、Contents: write（见下）、Checks: write、Metadata: read |
| 呈现方式 | 一个 "AI Review" Check Run 显示进度和结果摘要；一条 PR Review，正文为结论的 `body`，逐行评论来自 `comments` 的 `path`、`line`；review 事件按 `verdict` 取 APPROVE 或 COMMENT（§6.3） |

gateway 已实现签名校验（`X-Hub-Signature-256`）和 `pull_request` 的 opened、synchronize、reopened、ready_for_review 四种触发，仅在 payload 的 `pull_request.draft=false` 时记录评审；draft PR 的事件返回 204，不写入 run，转为正式 PR 时由 `ready_for_review` 触发当前 head 的评审。此过滤只控制新评审触发，转回 draft 不取消已创建的 run。按 delivery ID 去重后写入 `queued` 的 run；M4 已接入 `issue_comment.created` 的 `/merge` 和 `pull_request_review.submitted` 的人工 approve，gateway 只记录确认；`closed` webhook 与 `/review` 命令尚未接入。等待合并期间由 controller 查询 PR 状态处理关闭。

GitHub 只投递一次 webhook，失败不会自动重试，所以有两道保护。一是 gateway 的就绪检查（`/readyz`）确认最新的迁移已执行，数据库还没就绪时不接流量。二是 controller 每 5 分钟（`REDELIVER_INTERVAL`）用 App JWT 列出最近 100 次投递（`GET /app/hook/deliveries`），按 GUID 分组：24 小时内、每次尝试都没拿到 2xx、尝试不满 5 次的，重投最新那次（`POST /app/hook/deliveries/{id}/attempts`）。gateway 按 delivery ID 去重，重投与迟到的成功撞在一起也安全。这一步不依赖 controller 自己的状态，重启后照常工作。

**Contents 必须是 write，否则 AI 的票不算数。** 分支保护只计算对仓库有写权限的账号的审批，对 App 来说就是 Contents: write。M1 实测：App 只有 Contents: read 时，bot 的 APPROVE 显示在 PR 上，但 `reviewDecision` 仍为 `REVIEW_REQUIRED`；授予 write 后，同一张票不用重投就变成 `APPROVED`，说明 GitHub 按 App 当前的权限计算。代价是 controller 的 installation token 能写代码；它只在 controller 里，fetch 的 token 仍然限定为只读（`contents: read`）。`auto` / `manual` 模式的合并（M4）也依赖这项权限。

controller 用 App 私钥签 JWT（RS256，9 分钟有效），换取 installation token：自己的写操作用全权限 token，按 installation 缓存、过期前 5 分钟续期；fetch 的 token 每个 run 单独签发，限定到被评审的仓库和 `contents`、`pull_requests`、`metadata` 的只读权限。bot 的登录名（`<slug>[bot]`）启动时通过 `GET /app` 取得，用来识别要撤销的旧票。

本地开发时，GitHub App 的 webhook 指向一个 smee.io 通道，`make webhook-forward SMEE_URL=...` 把事件转发到 Kind 里的 gateway。

### 6.5 仓库配置 `.ai-pr-review.yaml`

```yaml
approvalMode: vote        # vote（默认，只投票）| auto（approve 后合并）| manual（approve 后等 /merge）
failOn: high              # 有该级别及以上的评论时不投 APPROVE
budgetUSD: 0.50           # 单个 run 的预算上限
language: zh              # 评论语言
preview:
  enabled: true
  helm:
    chart: deploy/chart
    valuesFile: deploy/values-preview.yaml
  readyTimeout: 5m
```

controller 在创建 runner Job 前，从 PR **目标分支的 base SHA** 读取根目录 `.ai-pr-review.yaml`，将解析结果与 SHA 一起保存到 run；重试与恢复复用快照。PR 中修改配置不会改变自身的合并模式，新配置合入目标分支后才对新 run 生效。

没有配置文件或省略字段时，`approvalMode` 默认 `vote`，`failOn` 默认 `high`，预算和语言沿用部署默认值（语言为空时用 `zh`）。模式只接受 `vote`、`auto`、`manual`，阈值接受 `critical`、`high`、`medium`、`low`，预算必须是有限非负数；零预算会在发放虚拟 key 时结束为预算耗尽。配置最多 64 KiB，未知字段、重复字段、多 YAML 文档和非法值使 run 失败并报告 Check Run，不启动 runner。`preview` 字段在策略快照中保留解析兼容。M5 的部署参数和 chart 来自被评审的 head，由 runner 打包后交给 controller 校验；可信镜像仓库映射和集群开关由操作者设置。它们不会改变 base SHA 决定的合并策略。

`failOn` 在 controller 里对 `result.comments` 做确定性校验：模型给了 approve 但存在 `severity` 达到阈值的评论时，结论改写为 request_changes，即只发 COMMENT、不投 APPROVE。这是把原 CLI 的 `--fail-on` 门禁搬到服务端，作为模型判断之外的一道确定性兜底。

## 7. 可观测性与成本

### 7.1 指标

controller 的 `GET /metrics`（与 runner 回调同一个端口）输出以下指标，实现在 [`go/internal/metrics`](../go/internal/metrics/metrics.go)：

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `review_runs_total{state}` | counter | 已结束的 run，`state` 为 done、failed、superseded |
| `review_queue_depth`、`review_running` | gauge | 排队中、有 runner Job 的 run 数 |
| `review_verdicts_total{verdict}`、`review_retries_total` | counter | done run 的结论；超出第一次的尝试次数 |
| `review_failures_total{reason}` | counter | 失败原因：timeout、fetch、no_review（模型没提交）、budget、model_api（模型服务拒绝调用，如余额不足）、runner、github、other。由 `error` 文本归类；容器失败时 `error` 是日志末尾，只看最后一行（致命错误），因为前面的进度行可能提到预算或 `submit_review` |
| `review_run_duration_seconds{phase}` | histogram | done run 各阶段耗时，见下表 |
| `review_run_cost_usd` | histogram | 每个 done run 的模型花费 |
| `review_tokens_total{model,direction}` | counter | `direction` 为 input（未命中缓存）、output、cache_read、cache_write |
| `review_model_cost_usd_total{model}`、`review_cost_usd_total{repo}` | counter | 模型花费，按模型、按仓库 |

| 阶段 | 起止 | 时间戳来源 |
| --- | --- | --- |
| `queue` | run 创建 → controller 领取 | `created_at` → `started_at`（`Claim` 写入） |
| `startup` | 领取 → fetch 开始：建 Job、调度、拉镜像 | `started_at` → fetch 容器的 `startedAt` |
| `fetch` | fetch 容器克隆代码 | Pod 状态里 fetch 容器的 `startedAt` → `finishedAt` |
| `review` | fetch 结束 → 收到结果：模型探索与提交 | fetch 的 `finishedAt` → `reviewed_at`（`SubmitResult` 写入） |
| `publish` | 收到结果 → done：投票、完成 Check Run | `reviewed_at` → `finished_at`（进入终态时写入） |
| `total` | 创建 → done | `created_at` → `finished_at` |

fetch 的起止时间由 controller 在发布投票前读 runner Pod 的状态得到（只用于指标，读取失败只记日志）。M5 的预览部署等待和运行时检查目前计入 `review`，尚未拆分独立阶段指标。

**指标在抓取时由数据库计算**，controller 不在内存里累计。这样 controller 重启后计数不归零，多副本时每个副本报出的值一致，Prometheus 只需抓任意一个副本。代价是每次抓取扫描所有 done run，演示规模下可以忽略；run 数量大时改为只扫描最近一段时间，或在状态转换时累加计数。Prometheus 文本格式手写输出，不引入 client 库，与不用 client-go 的理由相同（§6.1）。

**用量由 runner 每轮上报**：`turn_end` 时 runner 把本轮 assistant 消息的 `usage`（含 `cacheRead`、`cacheWrite`）`PUT /runs/{id}/usage`，带 `RUN_TOKEN`；同一轮重复上报覆盖，不累加。上报在后台按顺序进行，失败只少一个样本，不影响评审；提交结果前等待上报完成，所以 done run 的花费是完整的。用量在任何状态下都可记录；已经花费的金额不会因状态变化而丢失。

| 来源 | 内容 |
| --- | --- |
| controller `/metrics` | 上面的全部指标 |
| LiteLLM（M2） | 每个虚拟 key 的花费，作为核对口径 |

所有日志带 `run_id`、`repo`、`pr`、`head_sha`。

Grafana 看板 [`dashboards/review-cost.json`](../deploy/helm/ai-pr-review-cloud/dashboards/review-cost.json) 随 chart 部署（`monitoring.dev.enabled`，仅开发用、不持久化），也可以导入到已有的 Grafana。面板：所选时间范围内的完成数、失败数、总花费、每次成本 P50 / P95、端到端耗时 P50 / P95、缓存命中率；吞吐（按结果）、排队与运行中；各阶段耗时 P50 / P95 与 P95 趋势；每次评审成本分布、端到端耗时分布、按仓库和按模型的花费；token 速率、失败原因、结论分布。

### 7.2 成本测算方法

1. 选 20 个真实 PR，按改动行数分三档（<100、100–500、>500）。
2. 每个 PR 跑 3 次，记录 token、金额、各阶段耗时。
3. 给出每档的 P50 / P95 成本和端到端延迟，算出"每 100 个 PR 的月成本"。
4. 找出主要瓶颈，预计在探索阶段的上下文膨胀，对比优化效果：限制探索轮数、开启 prompt cache、小模型做探索加大模型做结论。

结果见 [成本测算报告](cost-report.md)：一次评审 P50 $0.029、P95 $0.068。上下文膨胀大部分被前缀缓存消化，瓶颈在输出（思考）token 和探索轮数；软预算是最有效的优化。

### 7.3 预算控制

- 虚拟 key 自带 `max_budget`，超出后 proxy 直接拒绝，run 以 `failed(budget_exceeded)` 结束，并在 Check Run 上说明（§5）。LiteLLM 在请求前检查预算，单次尝试可能超出最后一个请求的花费；重试签发按 run 的预算减去已上报的 `run_usage.cost_usd`，余额耗尽不再发 key。这里的用量按 pi 模型目录计价，需与 LiteLLM 的 `llm.pricing` 保持一致；崩溃前未上报的请求花费可能遗漏，代理花费落库也有延迟，因此不是严格的账单硬上限。
- runner 侧是软上限：`turn_end` 时累计 `usage.cost.total`，达到预算 80% 且还没提交时，用 pi 的 `steer` 插入一条提示（[`budgetReminder`](../runner/src/prompts.ts)），要求停止探索、按已核实的内容提交，并在正文里写明没查到的部分。提示在当前轮的工具调用执行完、下一次模型调用前送达，每个 run 只发一次。有预览时提示按阶段措辞：探索阶段还不能提交，只要求结束本轮；进入预览验证阶段时若仍超过阈值，再在第二阶段的提示后附一条要求提交的提醒，所以这种 run 最多收到两次。集群模式的预算来自 `review_run.budget_usd`（controller 以 `REVIEW_BUDGET_USD` 传入），本地模式用 `--budget`。
- 软上限按轮检查，所以实际花费会超过阈值大约一到两轮（提示送达前的那一轮，加上提交的那一轮）。测算里预算设为 $0.02 时的实际花费见 [成本测算报告](cost-report.md)。

## 8. 与 v1 的关系、仓库结构与交付形式

### 8.1 与 v1 的关系

本项目是 [ai-pr-review v1](https://github.com/RooobinYe/ai-pr-review) 的重新设计。v1 是一个本地 / CI 里运行的 Go CLI，自研了 agent 循环、工具、TUI 和权限系统。本仓库从零开始，不复制 v1 的代码，只沿用它验证过的设计：

| 沿用自 v1 | 在本项目中的形态 |
| --- | --- |
| 两阶段评审：先用工具探索仓库，再给出结构化结论 | runner 的探索阶段 + `submit_review` 工具（§4.2）；模型直接给出 verdict |
| 评审提示词与防幻觉规则（XML 结构、证据要求、置信度说明） | runner 的系统提示词，按新流程重写；置信度换成"只有核实过的发现才做逐行评论" |
| 输出 schema v2：`must_fix` / `points_of_interest`、严重级别、置信度、`schema_version` | 改为 schema v4（§4.4）：保留严重级别，去掉置信度，`must_fix` 的判断交给模型的 `verdict`，报告式字段换成模型直接写的正文和逐行评论；Go 与 TS 两侧用契约测试对齐 |
| `--fail-on` 质量门禁 | controller 里对评论 `severity` 的确定性校验，超标时不投 APPROVE（§6.5） |
| 按文件类型分类改动（code / test / config / doc） | 提示词里的文件分类与评论分组 |

v1 中由 pi SDK 取代的部分（agent 循环、模型 provider、上下文压缩、内置工具），以及服务端不需要的部分（TUI、交互式权限确认、本地凭据管理），不再保留。v1 仓库继续作为独立的 CLI 存在。

### 8.2 仓库结构

```
ai-pr-review-cloud/
├── go/                              # Go module
│   ├── cmd/gateway/                 # webhook 入口（装配）
│   ├── cmd/controller/              # 状态机驱动（装配、迁移）
│   ├── cmd/fetch/                   # runner 的 init 容器（装配）
│   ├── internal/gateway/            # webhook 处理：验签、解析触发、写入 run
│   ├── internal/controller/         # reconcile 循环、Job 模板、投票与 Check Run 呈现、结果回调、fetch 的出站代理
│   ├── internal/fetch/              # 拉取 PR 元信息与 diff、克隆、写 pr.json
│   ├── internal/githubapp/          # webhook 签名与触发、App JWT 与 installation token、REST 客户端
│   ├── internal/kube/               # 最小 Kubernetes REST 客户端（Job、Secret、Pod）
│   ├── internal/litellm/            # LiteLLM 虚拟 key 的签发与吊销
│   ├── internal/metrics/            # /metrics：由数据库快照生成 Prometheus 指标
│   ├── internal/store/              # PostgreSQL 访问与迁移
│   ├── internal/review/             # 评审结论 schema v4（与 runner 对齐）
│   ├── internal/run/                # run 状态与合法转换
│   ├── internal/store/migrations/   # PostgreSQL 表结构
│   └── Dockerfile                   # 用 --build-arg CMD=<组件> 构建三个镜像
├── runner/                          # TypeScript，pi SDK 1.3.0
│   ├── src/main.ts                  # 集群模式（RUN_ID）与本地模式（--pr）入口
│   ├── src/controller.ts            # 集群模式回调 controller（结果、每轮用量）
│   ├── src/usage.ts                 # 每轮用量、预算提示阈值
│   ├── src/stats.ts                 # 本地模式 --stats 的测算数据
│   ├── src/pr.ts                    # PR 上下文及 pr.json 校验
│   ├── src/review.ts                # pi session、工具、提交提醒
│   ├── src/prompts.ts               # 系统提示词与用户提示词
│   ├── src/schema.ts                # 评审结论 schema v4（TypeBox）
│   ├── src/tools.ts                 # git、submit_review 工具
│   ├── src/github.ts                # 本地模式拉取 PR 与克隆
│   └── Dockerfile
├── contract/                        # 生成的 JSON schema、两侧共用的样例、pr.json 与 diff 契约
├── deploy/helm/ai-pr-review-cloud/  # Helm chart；dashboards/ 下是 Grafana 看板 JSON
├── bench/                           # 成本测算：PR 列表、驱动与汇总脚本、每次运行的原始数据
├── hack/kind-up.sh                  # 创建本地 Kind 集群
├── hack/webhook-forward.sh          # smee.io → gateway 的 webhook 转发
├── Makefile                         # test、lint、images、kind-up、deploy
├── .github/workflows/ci.yaml        # Go、runner、chart、镜像构建
└── docs/
```

### 8.3 交付形式

面向 k8s 交付，分三层：

| 层 | 产物 | 用途 |
| --- | --- | --- |
| 容器镜像 | `gateway`、`controller`（Go 静态编译，distroless nonroot，各约 13 MB）、`fetch`（Go + alpine git，约 28 MB）、`runner`（Node 24 slim + git，约 460 MB） | 每个组件一个镜像，独立发布和扩缩容；命名为 `<registry>/ai-pr-review-cloud-<组件>:<tag>` |
| Helm chart | `deploy/helm/ai-pr-review-cloud` | 集群部署的唯一入口 |
| 本地复现 | `make kind-up && make deploy` | 在本机 Kind 集群里一键拉起整套系统 |

chart 目前包含：

- gateway（2 副本）和 controller（1 副本）的 Deployment 与 Service，都以非 root、只读根文件系统、丢弃全部 capability 运行；gateway 在数据库迁移完成前不就绪；
- controller 的 ServiceAccount 和最小 Role（§6.1）；
- GitHub App 的 webhook secret、App ID 和私钥：可以引用已有 Secret（`github.existingSecret`），开发时也可由 chart 根据 `github.*` 生成；
- LiteLLM proxy（`litellm.image`，单副本，非 root、只读根文件系统）：真实模型 key 来自 `llm.existingSecret` 或 `llm.apiKey`，只注入 LiteLLM；master key 未指定时由 chart 生成并在升级时保留；它自己的数据库在开发模式下是 dev PostgreSQL 上的 `litellm` 库（init 容器创建），否则通过 `litellm.database.existingSecret` 接入；
- 模型参数 `llm.*`，以及 `runs.*`（并发上限、每个 run 尝试次数、单个 Job 时限、评审语言、默认预算、fetch 可访问的 GitHub 地址）；
- runner 与 LiteLLM 的 NetworkPolicy（`networkPolicy.enabled`，默认开启，DNS 的位置由 `networkPolicy.dns` 指定）；
- `webhooks.redeliverInterval` 配置失败 webhook 的重投间隔，0 关闭；
- 仅供开发用的单副本 PostgreSQL（`postgres.dev.enabled`，数据不持久化）；关闭后通过 `database.existingSecret` 接入外部数据库。
- 仅供开发用的 Prometheus 和 Grafana（`monitoring.dev.enabled`，不持久化）：Prometheus 抓取 controller 的 `/metrics`，Grafana 预置数据源和成本看板，匿名只读。生产环境关闭它，用已有的监控抓取 controller Service 并导入看板 JSON。

预览环境所需的 ClusterRole 由 `preview.enabled` 显式开启，默认关闭。配置、升级和验收步骤见 [预览验证](preview.md)。

`make deploy` 的密钥只从环境变量读取：`GITHUB_WEBHOOK_SECRET`（缺省时用一个固定的开发值）、`GITHUB_APP_ID`、`GITHUB_APP_PRIVATE_KEY_FILE`、`LLM_API_KEY`（真实模型 key，交给 LiteLLM）。没有 App ID 时 controller 照常启动，但 run 停在 `queued`，所以不配置 GitHub App 也能把整套服务拉起来。

runner 镜像也可以脱离集群单独使用：`docker run` 加 `--pr <url>` 即为本地模式，等价于 v1 的 CLI 用法。

## 9. 里程碑

里程碑、每个阶段的任务清单与验收标准见 [roadmap.md](roadmap.md)。

## 10. 风险与待确认

- **run 进行中的失败处理。** Pod 失败按 §4.3 重试并从快照续跑；超时、确定性退出码、GitHub 返回确定性错误（401、403（限流除外）、404、410、422）时，run 直接置为 `failed`；投票被拒也一样（`reviewed` → `failed`）。失败一律写到 Check Run，Check Run 还没创建时补建一个已失败的；连 Check Run 都写不进去时只记日志，不再重试。
- **快照的体积。** 每轮上传完整 session，总流量随轮数平方增长。M3 实测 9 轮约 250 KB，集群内网可以接受；单个快照上限 32 MiB。若长评审成为瓶颈，再改为只上传新增 entry。
- **pi SDK 版本变动快。** 包名已从 `@mariozechner` 迁到 `@earendil-works`，初始化方式也从 `AuthStorage` 换成了 `ModelRuntime`，两周内从 1.1 发到 1.4。目前精确锁定 `1.3.0`，升级单独提交并跑一遍 M0 的验收。
- **pi 的 `usage` 字段。** 已确认含 `cacheRead`、`cacheWrite`，DeepSeek 会填充 `cacheRead`（§4.5）；金额按 pi 目录单价计算，LiteLLM 按 `llm.pricing` 单独记账，两者的交叉校验在 M6 的测算里做。
- **自定义模型接入。** DeepSeek 是 pi 内置 provider，指向 LiteLLM 只需覆盖 `baseUrl`；`deepseek-flash` 在 M0 的三个 PR 上都一次调用成功 `submit_review`。换用其他模型时要重新验证工具调用的稳定性。
- **LiteLLM 虚拟 key 与预算。** 依赖其 key 管理和花费统计（需要它自己的数据库，不能与本系统共用一个库的不同 schema：v1.104.2 的迁移在非 `public` schema 下失败）。已用 v1.104.2 验证 `max_budget` 在花费达到上限后拒绝请求（422，`type: budget_exceeded`），删除后的 key 立即失效；`/key/info` 里的 `spend` 是批量写库的，比实际花费滞后，不能用来判断是否超预算。
- **DNS 仍是一条出站通道。** runner 可以查询任意域名，集群 DNS 会替它递归解析，理论上能把少量数据编码进查询名带出去。代价是必须先拿到值得带出的数据：runner 只有本 run 的虚拟 key（有预算、会吊销）和被评审的代码。需要堵住时，可以给 CoreDNS 配置只解析集群内域名的视图。
- **NetworkPolicy 依赖 CNI。** Kind 的 kindnet 通过 kube-network-policies 实施，策略对新 Pod 的生效有几秒延迟（Pod IP 被异步加入规则集，此前 fail-open）。runner Pod 里先跑的是 fetch，模型在这之后才启动，所以这段窗口里没有不可信代码在运行；不实施 NetworkPolicy 的 CNI 上这层隔离不存在，chart 无法替你检查。
- **工具链版本较新。** runner 用 TypeScript 7.0、Go 镜像用 `golang:1.27`，本地验证通过，GitHub Actions 上还没跑过。
- **评论锚点与后续推送。** `submit_review` 保证评论落在 diff 内，controller 发布时以结果里的 `head_sha` 为 `commit_id`，PR 之后的推送不会让锚点错位；GitHub 仍拒绝时把评论折叠进正文（§6.3）。
- **GitHub App 的 approve 是否计入"需要 N 个 approval"。** 这是 `vote` 模式的前提。GitHub 文档没有明确说明 App 的 review 是否计入，只明确说 Copilot 的 review 不计入；M1 已实测：App 有 Contents: write 时计入，只有 read 时不计入（§6.4）。在要求 1 个审批的分支上，bot 的一票就能满足要求；"AI 加一个人"的 2 票场景需要第二个人类账号，机制相同，留到 M7 演示时验证。
- **撤销旧票的权限。** dismiss review 需要 App 有 Pull requests: write。M1 已在没有分支保护的仓库上验证 App 能 dismiss 自己的 APPROVE；若 branch protection 限制了谁能 dismiss review，要把 App 加入允许名单。
- **webhook 丢失。** controller 会重投 GitHub 记录为失败的 delivery（§6.4）。本地开发经 smee 转发时，GitHub 看到的是 smee 返回的成功，gateway 那一侧的失败它不知道，所以重投帮不上忙；这时仍要在 App 设置的 Advanced → Recent Deliveries 里手动重投。列表只取最近 100 次投递，更早的失败不会被发现。
- **预览环境的镜像约定。** 需要 demo 仓库的 CI 配合推送 `:<head_sha>` 镜像。
