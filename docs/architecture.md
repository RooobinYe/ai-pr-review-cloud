# ai-pr-review-cloud 架构设计

> 状态：M1、M3、M6 完成，最小闭环与删除 Pod 后恢复完成均在本仓库的 PR 上实测通过，成本测算见 [cost-report](cost-report.md)；其余按 [roadmap](roadmap.md) 推进 · 目标：赛道 B「云原生 Agent 实战」参赛作品
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

## 1. 场景定位

**基础能力：PR 评审。** 沿用 v1 验证过的两阶段评审（先探索、再结论）。结论由模型自己决定：approve 还是 request_changes，加上一段评审正文和逐行评论，以 PR Review 和 Check Run 的形式呈现（§4.4）。

**AI 作为审批人之一。** 安装后，AI 以 GitHub App bot 的身份参与评审，和人类审批人一样投票：结论为 approve 时提交 APPROVE review；发现问题时只提交 COMMENT review，指出问题但不阻塞合并。仓库要求"N 个 approval 才能合并"时，AI 的 approve 算作其中一票（前提是 App 有 Contents: write，§6.4），其余由人来投。这是默认的 `vote` 模式，controller 不执行合并（§6.3）。

**差异化能力：预览环境验证（按仓库开启）。** 只看代码看不出的问题，在运行起来以后才暴露：配置错误、镜像起不来、探针失败、资源不足、依赖服务连不上。开启后，controller 为 PR 创建独立的 namespace 并部署，agent 用 kubectl 查看 Pod 状态、事件和日志，把运行时证据和代码评审结论合在一起，再给出结论。

这也是 agent 需要 kubectl 的唯一理由。纯代码评审阶段不给集群权限。

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
| `gateway` | Go | 校验 webhook 签名，按 delivery ID 去重，写入一条 run 记录，立即返回 202 | 不调 GitHub，不建 Job |
| `controller` | Go | 驱动 run 状态机：排队、建 Job、建预览环境、接收结果、执行所有 GitHub 写操作、超时与重试 | 不调用模型 |
| `fetch` | Go | runner Pod 的 init 容器：用只读 installation token 拉取代码和 PR 元信息，写入共享卷后退出 | 不在主容器里留下任何凭据 |
| `runner` | TypeScript（pi SDK） | 每个 run 一个 Job：探索代码、可选的 kubectl 验证、通过 `submit_review` 工具提交结构化结论 | 不持有 GitHub 凭据，不直接写 GitHub |
| LiteLLM proxy | 现成组件 | 持有真实模型 key；为每个 run 签发带预算上限的虚拟 key；记录花费 | — |
| PostgreSQL | 现成组件 | run 状态机、GitHub 副作用记录、session 快照、用量 | — |

设计原则：

- **所有副作用集中在 controller。** runner 只产出数据，不改变外部世界（预览环境除外，且只读）。这样 runner 可以随意重试，GitHub 上不会出现重复评论。
- **状态都在 PostgreSQL。** gateway、controller、runner 都可以随时重启。
- **等待不占 Pod。** 任何需要等人或等事件的环节，都表现为数据库里的一个状态，而不是一个挂着的进程。

## 3. Run 状态机

一次 run 对应 `(repo, pr_number, head_sha)`。同一个 PR 推送新提交，旧 run 被标记为 `superseded`，对应的 Job 被删除。

```mermaid
stateDiagram-v2
    [*] --> queued: pull_request opened / synchronize 或 /review 评论
    queued --> running: controller 领取并创建 Job
    running --> previewing: 仓库开启了预览，且评审阶段完成
    previewing --> running: 预览部署就绪，进入验证阶段
    running --> reviewed: runner 提交结论
    reviewed --> done: 已投票；approvalMode = vote（默认），或结论不是 approve
    reviewed --> awaiting_approval: approvalMode = manual 且结论为 approve
    reviewed --> failed: GitHub 拒绝投票（确定性错误）
    reviewed --> merging: approvalMode = auto 且结论为 approve
    awaiting_approval --> merging: 收到 /merge 评论或人工 approve
    merging --> done
    merging --> failed: 合并被 GitHub 拒绝
    running --> queued: Pod 失败且未超过重试次数
    running --> failed: 超过重试次数或超时
    previewing --> failed: 预览部署超时
    queued --> superseded: 新的 head_sha 到达
    running --> superseded
    previewing --> superseded
    awaiting_approval --> superseded
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
| `github_action` | 每个 GitHub 写操作一行：`dismiss_stale_approvals`（撤销旧票）、`check_run_create`、`check_run_complete`、`review`（投票，§6.3）、`merge`（M4） | 主键 `(run_id, kind)`；`external_id` 非空表示已完成；取值在 `go/internal/store/store.go` 里也有一份 |
| `run_session` | runner 每轮结束上传的 pi session 快照（JSONL），用于崩溃恢复 | 每个 run 只保留一份；轮次更早的上传被忽略，迟到的请求不会把快照回退；只在 `running` 时接受 |
| `run_usage` | 每轮的模型、输入、输出、缓存读写 token 和金额 | 主键 `(run_id, turn)`，重复上报覆盖而不是累加；任何状态都接受，花掉的钱照记 |

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
│                         （来自 Secret review-<run-id>-<attempt>-credentials，归 Job 所有，随 Job 删除）
│          REPO, PR_NUMBER, HEAD_SHA, BOT_LOGIN, WORKSPACE=/workspace
│     作用: 克隆 head（往前 20 个提交）到 /workspace/repo、拉取 base、写出 /workspace/pr.json
│          （元信息、diff，以及 bot 在该 PR 上留过的逐行评论）
│     token 经 git 的环境变量配置传入，不进 argv 和 .git/config；结束后随容器退出，主容器读不到
├── container: runner
│     env: RUN_ID, CONTROLLER_URL, RUN_TOKEN（回调 controller 用，同一个 Secret 的另一个 key，fetch 读不到）,
│          LLM_API_KEY（M1 为共享 Secret，M2 换成 LiteLLM 虚拟 key）
│     volume: /workspace、/tmp (emptyDir)；根文件系统只读
│     serviceAccount: 不挂载 token；预览阶段使用绑定到 PR namespace 的只读 SA
└── NetworkPolicy: 出站只允许 LiteLLM proxy、controller、kube-apiserver（预览阶段）
```

两个容器都以 UID 1000（runner 镜像的 `node` 用户）运行，git 才会信任 fetch 留下的工作区。blob 一次拉全，不用部分克隆：runner 没有凭据，之后也没有外网，无法按需补拉。fetch 发现 PR 的 head 已不是本 run 的 `head_sha` 时直接失败（退出码 3，不重试），新提交有自己的 run。

同一 PR 再次评审时，fetch 用 GraphQL 取回 bot 自己的逐行评论线程（最近 100 个线程中最多 30 条，每条截断到 2000 字节），连同 `isResolved`、`isOutdated` 写进 `pr.json` 的 `previousComments`。提示词要求模型不重复已解决或已修复的问题，仍然存在的问题注明"上次评审未解决"，把精力放在新改动上。这些评论引用了 PR 代码，在提示词里同样按不可信输入包起来。取不到时只打警告，评审照常进行。GraphQL 里 bot 的登录名没有 REST 的 `[bot]` 后缀，比较时去掉。

容器失败时，退出信息（`terminationMessagePolicy: FallbackToLogsOnError`，即日志末尾）由 controller 读出，写进 `review_run.error` 和 Check Run。退出码 3（`run.ExitFinal`）表示重试也没用：fetch 遇到 head 已变、GitHub 401/404/410/422，runner 遇到结果被 controller 拒绝、`pr.json` 不合法、模型被提醒两次仍不提交。其余失败都会重试（§4.3）。

### 4.2 执行流程

沿用 v1 的两阶段思路，但第二阶段从"让模型输出一段 JSON 再解析"改为"调用一个带 schema 的工具"，彻底消除 JSON 解析失败的情况。

1. 读 `/workspace/pr.json`，构造系统提示词和用户提示词。提示词按 v1 的防幻觉规则重写，放在 `runner/src/prompts.ts`。
2. **探索阶段**：pi 内置的只读工具 `read`、`grep`、`find`、`ls`，加上自定义的 `git` 工具（只允许 `log`、`show`、`diff`、`blame` 等只读子命令，不经过 shell），`cwd` 为 `/workspace/repo`。评审阶段不给 `bash`：本地模式跑在开发者机器上，一个带注入的 PR 不应能执行任意命令。
3. **预览验证阶段**（可选）：controller 部署好预览环境后，runner 收到一条 follow-up 提示，附上 namespace 名称；此时才加入 `bash`，其中的 `kubectl` 只能访问这个 namespace。
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
  resourceLoader: isolatedResources(systemPrompt(language)),
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

await session.prompt(resumeFrom ? RESUME_PROMPT : userPrompt(pr));
for (let i = 0; !submitted && i < MAX_SUBMIT_REMINDERS; i++) await session.prompt(SUBMIT_REMINDER);
```

### 4.3 崩溃恢复

runner 每轮（一次模型回复加它触发的工具调用）结束时，把整个 pi session 序列化成 JSONL（header 一行，之后每个 entry 一行，与 pi 的 session 文件格式相同）。先把本轮用量 `PUT /runs/{id}/usage`，失败记日志，然后独立尝试 `PUT /runs/{id}/session?turn=N`；快照被拒绝不会跳过用量上报，用量上传失败也不会跳过快照。快照在 `turn_end` 事件里同步取，上传在后台排队依次进行，失败不中断评审。新 Pod 启动时 `GET /runs/{id}/session`，有快照就用 `SessionManager.inMemory(cwd, undefined, entries)` 载入，追加一条"进程中断后已恢复，从上次完成的步骤继续"的提示，否则从头开始。预算提醒计入快照中已花费的金额。这三个接口和结果回调一样用本次尝试的 `RUN_TOKEN` 鉴权，旧尝试的 token 随旧 Job 的 Secret 一起失效；旧 token 被拒绝时仍可能丢失最后一轮用量。

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
| `body` | Markdown 正文：改了什么、结论理由、验证了什么、没看的部分、自检行 | review 正文、Check Run 摘要 |
| `comments[]` | `path`、`line`（新文件行号）、`severity`（critical/high/medium/low）、`confidence`（high/medium/low）、`body` | 逐行评论；`severity` 供 `failOn` 使用 |

runner 交给 controller 的 `ReviewResult` 在此之外加上 `schema_version: 3`、`pr`（`repo`、`number`、`head_sha`，controller 用来拒绝不属于当前 run 的结果）、`model`、`generated_at`。评论最多 15 条，所有字段必填，不允许多余字段。

两侧定义：TypeScript 在 [`runner/src/schema.ts`](../runner/src/schema.ts)（TypeBox，同时作为 `submit_review` 的参数 schema），Go 在 [`go/internal/review`](../go/internal/review/review.go)。[`contract/review-result.schema.json`](../contract/review-result.schema.json) 由 TypeBox 生成，`contract/examples/` 里的合法、非法样例由两侧测试共同校验；Go 测试还会核对字段、枚举、上限与生成的 schema 一致，并检查 `verdict` 取值与数据库 CHECK 约束一致。

逐行评论只留给合并前值得处理的问题（critical、high，以及中高置信度的 medium），通常 0 到 5 条；风格和低级别建议最多 3 条，以一行一条的形式写进正文的 "Minor" 部分。每条评论都要占用作者的注意力，评论太多反而会淹没真正的问题。

提示词里的判定规则（`verdict_policy`）：存在中高置信度的 critical 或 high 问题时 request_changes；中低级别、风格建议和不确定的发现只作为评论，不阻塞合并；没能审到最可能出问题的部分时也应 request_changes 并说明原因。防幻觉规则沿用 v1：每条评论必须引用读到的代码，不确定的标 `[Uncertain]` 并给低置信度，critical/high 要做反向检验。

### 4.5 模型接入

runner 通过环境变量选择模型，`ModelRuntime` 不读写 `~/.pi`：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `LLM_API_KEY` | — | 必填。集群模式下是 LiteLLM 虚拟 key（M2） |
| `LLM_PROVIDER` | `deepseek` | pi 内置 provider 保留其模型目录（价格、兼容参数）；其他名称按 OpenAI 兼容接口注册 |
| `LLM_MODEL` | `deepseek-flash` | |
| `LLM_BASE_URL` | — | 覆盖 provider 地址，集群模式指向 LiteLLM |
| `LLM_THINKING` | `high` | `off` 到 `max` |

花费按 pi 模型目录的单价计算，`usage` 含 `input`、`output`、`cacheRead`、`cacheWrite`、`cost.total`。DeepSeek 会返回缓存命中量：M0 验收的三个 PR 里，缓存读取的 token 是非缓存输入的 11 到 14 倍。

## 5. 安全边界

demo 不做合并加固，但"模型拿不到真实密钥"是本项目的目标之一，而且实现成本低。PR 内容（代码、描述、评论）全部视为不可信输入。

| 凭据 / 能力 | 谁持有 | runner 主容器能否拿到 |
| --- | --- | --- |
| 模型厂商真实 API key | LiteLLM proxy | 否。只有本次 run 的虚拟 key，带预算上限，run 结束即吊销 |
| GitHub App 私钥 | controller | 否 |
| GitHub 只读 installation token | fetch initContainer | 否。initContainer 退出后不可见 |
| 回调 token（`RUN_TOKEN`） | controller、本 run 的 runner 主容器 | 是，只能为本 run 提交结果；没有它无法伪造结论让 bot 投 APPROVE |
| GitHub 写操作（评论、approve、merge） | controller | 否。runner 只能提交结论 |
| 集群访问 | 预览阶段的只读 SA | 只限 PR 自己的 namespace：pods、pods/log、events、deployments 的 get/list/watch |
| 外网访问 | — | 否。NetworkPolicy 只放行 proxy、controller 和 apiserver |

被注入的模型最多能做到：浪费本次 run 的预算、给出错误结论。默认的 `vote` 模式下，错误的 approve 只是一票，仍要满足仓库其余的审批要求；`auto` 模式下错误结论可能导致合并，这是 §0 里明确接受的风险。

## 6. controller 关键逻辑

### 6.1 并发与调度

- 全局最大并发 `maxRunning`，单仓库最大并发 `maxRunningPerRepo`，超出的 run 留在 `queued`。
- runner 所在 namespace 配 `ResourceQuota` 和 `LimitRange`，防止一次大 PR 吃光节点。
- Job 设置 `activeDeadlineSeconds`、`ttlSecondsAfterFinished`；`backoffLimit: 0`，重试由 controller 统一决定（这样才能带上 session 快照恢复，§4.3）。
- 冷启动优化：runner 镜像预装 pi 依赖和 git（M5 再加 kubectl）；fetch 阶段浅克隆（depth 1 + 按需拉 base）。实测后再决定要不要加 git mirror 缓存。
- 权限：controller 只有自己 namespace 内的 Role，可以管理 `jobs`、读取 `pods`、创建、读取和删除每个 run 的 `secrets`（fetch 的 token；M2 起还有虚拟 key、回调 token）。M5 的预览环境需要创建 namespace，届时再加一个只含 `namespaces` 的 ClusterRole。
- Kubernetes 访问：controller 只用到 Job、Secret、Pod 的少数几个调用，`go/internal/kube` 直接调 REST API（in-cluster token 每次请求重读），不引入 client-go，controller 镜像保持在十几 MB。M5 需要部署 Helm chart 时再评估。
- 每轮 reconcile（5 秒一次，收到结果或放回队列时立即触发）：把有更新提交的未完成 run 标为 `superseded`；领取 `queued`；对 `running` 先检查尝试是否超时，再依次撤销旧票、创建 Check Run、确保 Job 和 token Secret 存在、按 Job 状态决定重试或失败；对 `reviewed` 投票、完成 Check Run、推进到 `done`；对已失败但 Check Run 还没收尾的 run 补写失败结论；对 `superseded` 的 run 删除 Job、把 Check Run 完成为 `cancelled`。每一步都是幂等的，崩溃后重跑即可。
- 同一 PR 的新提交到达后，旧 run 只要还没到 `reviewed`（`queued`、`running`、`previewing`、`awaiting_approval`）就被取代，它的 Job 连同 Pod 和 Secret 一起删除（`propagationPolicy: Background`），之后再到的结果、快照都被拒绝。已经 `reviewed` 的 run 让它投完：投票前会检查 head（§6.3）。

### 6.2 预览环境

- 仓库在 `.ai-pr-review.yaml` 里声明部署方式（Helm chart 路径或 kustomize 目录）。
- controller 创建 `pr-<owner>-<repo>-<n>` namespace，配 ResourceQuota，用 PR 的镜像 tag 部署，等待 Ready 或超时。
- PR 关闭或合并时删除 namespace；另有定时清理，删掉超过 24 小时的孤儿 namespace。
- 构建镜像依赖仓库自己的 CI。demo 里约定 PR 的 CI 推送 `:<head_sha>` 镜像，controller 等这个 tag 出现。

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
// decideMerge 是唯一决定是否合并的地方。默认的 vote 模式从不合并，
// 合并交给仓库的 branch protection 和人。auto 模式只看 AI 结论；
// 生产环境在这里加确定性规则（作者白名单、路径、改动行数、CI 状态）。
func decideMerge(run ReviewRun, cfg RepoConfig) bool {
	return cfg.ApprovalMode == ApprovalAuto && run.Verdict == VerdictApprove
}
```

合并调用 GitHub 的 merge API，仍受仓库 branch protection 约束。

### 6.4 GitHub App

| 项 | 取值 |
| --- | --- |
| 订阅事件 | `pull_request`（opened、synchronize、reopened、closed）、`issue_comment`（`/review`、`/merge`）、`pull_request_review` |
| 权限 | Pull requests: write（投票、撤销旧票）、Contents: write（见下）、Checks: write、Metadata: read |
| 呈现方式 | 一个 "AI Review" Check Run 显示进度和结果摘要；一条 PR Review，正文为结论的 `body`，逐行评论来自 `comments` 的 `path`、`line`；review 事件按 `verdict` 取 APPROVE 或 COMMENT（§6.3） |

gateway 已实现签名校验（`X-Hub-Signature-256`）和 `pull_request` 的 opened、synchronize、reopened 三种触发，按 delivery ID 去重后写入 `queued` 的 run；`closed`、`issue_comment`、`pull_request_review` 在 M4、M5 接入。

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

没有配置文件或省略某项时取默认值，`approvalMode` 默认为 `vote`。

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

fetch 的起止时间由 controller 在发布投票前读 runner Pod 的状态得到（只用于指标，读取失败只记日志）。预览阶段在 M5 加入。

**指标在抓取时由数据库计算**，controller 不在内存里累计。这样 controller 重启后计数不归零，多副本时每个副本报出的值一致，Prometheus 只需抓任意一个副本。代价是每次抓取扫描所有 done run，演示规模下可以忽略；run 数量大时改为只扫描最近一段时间，或在状态转换时累加计数。Prometheus 文本格式手写输出，不引入 client 库，与不用 client-go 的理由相同（§6.1）。

**用量由 runner 每轮上报**：`turn_end` 时 runner 把本轮 assistant 消息的 `usage`（含 `cacheRead`、`cacheWrite`）`PUT /runs/{id}/usage`，带 `RUN_TOKEN`；同一轮重复上报覆盖，不累加。上报在后台按顺序进行，失败只少一个样本，不影响评审；提交结果前等待上报完成，所以 done run 的花费是完整的。run 处于 `running` 或 `previewing` 以外的状态时不接受上报（409）。

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

- 虚拟 key 自带 `max_budget`，超出后 proxy 直接拒绝，run 以 `failed(budget_exceeded)` 结束，并在 Check Run 上说明（M2）。这是硬上限。
- runner 侧是软上限：`turn_end` 时累计 `usage.cost.total`，达到预算 80% 且还没提交时，用 pi 的 `steer` 插入一条提示（[`BUDGET_REMINDER`](../runner/src/prompts.ts)），要求停止探索、按已核实的内容提交，并在正文里写明没查到的部分。提示在当前轮的工具调用执行完、下一次模型调用前送达，每个 run 只发一次。集群模式的预算来自 `review_run.budget_usd`（controller 以 `REVIEW_BUDGET_USD` 传入），本地模式用 `--budget`。
- 软上限按轮检查，所以实际花费会超过阈值大约一到两轮（提示送达前的那一轮，加上提交的那一轮）。测算里预算设为 $0.02 时的实际花费见 [成本测算报告](cost-report.md)。

## 8. 与 v1 的关系、仓库结构与交付形式

### 8.1 与 v1 的关系

本项目是 [ai-pr-review v1](https://github.com/RooobinYe/ai-pr-review) 的重新设计。v1 是一个本地 / CI 里运行的 Go CLI，自研了 agent 循环、工具、TUI 和权限系统。本仓库从零开始，不复制 v1 的代码，只沿用它验证过的设计：

| 沿用自 v1 | 在本项目中的形态 |
| --- | --- |
| 两阶段评审：先用工具探索仓库，再给出结构化结论 | runner 的探索阶段 + `submit_review` 工具（§4.2）；模型直接给出 verdict |
| 评审提示词与防幻觉规则（XML 结构、证据要求、置信度说明） | runner 的系统提示词，按新流程重写 |
| 输出 schema v2：`must_fix` / `points_of_interest`、严重级别、置信度、`schema_version` | 改为 schema v3（§4.4）：保留严重级别和置信度，`must_fix` 的判断交给模型的 `verdict`，报告式字段换成模型直接写的正文和逐行评论；Go 与 TS 两侧用契约测试对齐 |
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
│   ├── internal/controller/         # reconcile 循环、Job 模板、投票与 Check Run 呈现、结果回调
│   ├── internal/fetch/              # 拉取 PR 元信息与 diff、克隆、写 pr.json
│   ├── internal/githubapp/          # webhook 签名与触发、App JWT 与 installation token、REST 客户端
│   ├── internal/kube/               # 最小 Kubernetes REST 客户端（Job、Secret、Pod）
│   ├── internal/metrics/            # /metrics：由数据库快照生成 Prometheus 指标
│   ├── internal/store/              # PostgreSQL 访问与迁移
│   ├── internal/review/             # 评审结论 schema v3（与 runner 对齐）
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
│   ├── src/schema.ts                # 评审结论 schema v3（TypeBox）
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
- runner 使用的模型 key（`llm.existingSecret` 或 `llm.apiKey`，M2 换成 LiteLLM 虚拟 key）和模型参数，`runs.*`（并发上限、每个 run 的尝试次数、单个 Job 时限、评审语言、默认预算），以及 `webhooks.redeliverInterval`（失败 webhook 的重投间隔，0 关闭）；
- 仅供开发用的单副本 PostgreSQL（`postgres.dev.enabled`，数据不持久化）；关闭后通过 `database.existingSecret` 接入外部数据库。
- 仅供开发用的 Prometheus 和 Grafana（`monitoring.dev.enabled`，不持久化）：Prometheus 抓取 controller 的 `/metrics`，Grafana 预置数据源和成本看板，匿名只读。生产环境关闭它，用已有的监控抓取 controller Service 并导入看板 JSON。

LiteLLM proxy 和 runner 的 NetworkPolicy 在 M2 加入，预览环境所需的 ClusterRole 在 M5 加入。

`make deploy` 的密钥只从环境变量读取：`GITHUB_WEBHOOK_SECRET`（缺省时用一个固定的开发值）、`GITHUB_APP_ID`、`GITHUB_APP_PRIVATE_KEY_FILE`、`LLM_API_KEY`。没有 App ID 时 controller 照常启动，但 run 停在 `queued`，所以不配置 GitHub App 也能把整套服务拉起来。

runner 镜像也可以脱离集群单独使用：`docker run` 加 `--pr <url>` 即为本地模式，等价于 v1 的 CLI 用法。

## 9. 里程碑

里程碑、每个阶段的任务清单与验收标准见 [roadmap.md](roadmap.md)。

## 10. 风险与待确认

- **run 进行中的失败处理。** Pod 失败按 §4.3 重试并从快照续跑；超时、确定性退出码、GitHub 返回确定性错误（401、403（限流除外）、404、410、422）时，run 直接置为 `failed`；投票被拒也一样（`reviewed` → `failed`）。失败一律写到 Check Run，Check Run 还没创建时补建一个已失败的；连 Check Run 都写不进去时只记日志，不再重试。
- **快照的体积。** 每轮上传完整 session，总流量随轮数平方增长。M3 实测 9 轮约 250 KB，集群内网可以接受；单个快照上限 32 MiB。若长评审成为瓶颈，再改为只上传新增 entry。
- **pi SDK 版本变动快。** 包名已从 `@mariozechner` 迁到 `@earendil-works`，初始化方式也从 `AuthStorage` 换成了 `ModelRuntime`，两周内从 1.1 发到 1.4。目前精确锁定 `1.3.0`，升级单独提交并跑一遍 M0 的验收。
- **pi 的 `usage` 字段。** 已确认含 `cacheRead`、`cacheWrite`，DeepSeek 会填充 `cacheRead`（§4.5）；金额按 pi 目录单价计算，M2 时与 LiteLLM 的花费交叉校验。
- **自定义模型接入。** DeepSeek 是 pi 内置 provider，指向 LiteLLM 只需覆盖 `baseUrl`；`deepseek-flash` 在 M0 的三个 PR 上都一次调用成功 `submit_review`。换用其他模型时要重新验证工具调用的稳定性。
- **LiteLLM 虚拟 key 与预算。** 依赖其 key 管理和花费统计（需要它自己的数据库）；落地前用选定版本验证 `max_budget` 是否按预期拒绝请求。
- **工具链版本较新。** runner 用 TypeScript 7.0、Go 镜像用 `golang:1.27`，本地验证通过，GitHub Actions 上还没跑过。
- **评论锚点与后续推送。** `submit_review` 保证评论落在 diff 内，controller 发布时以结果里的 `head_sha` 为 `commit_id`，PR 之后的推送不会让锚点错位；GitHub 仍拒绝时把评论折叠进正文（§6.3）。
- **GitHub App 的 approve 是否计入"需要 N 个 approval"。** 这是 `vote` 模式的前提。GitHub 文档没有明确说明 App 的 review 是否计入，只明确说 Copilot 的 review 不计入；M1 已实测：App 有 Contents: write 时计入，只有 read 时不计入（§6.4）。在要求 1 个审批的分支上，bot 的一票就能满足要求；"AI 加一个人"的 2 票场景需要第二个人类账号，机制相同，留到 M7 演示时验证。
- **撤销旧票的权限。** dismiss review 需要 App 有 Pull requests: write。M1 已在没有分支保护的仓库上验证 App 能 dismiss 自己的 APPROVE；若 branch protection 限制了谁能 dismiss review，要把 App 加入允许名单。
- **webhook 丢失。** controller 会重投 GitHub 记录为失败的 delivery（§6.4）。本地开发经 smee 转发时，GitHub 看到的是 smee 返回的成功，gateway 那一侧的失败它不知道，所以重投帮不上忙；这时仍要在 App 设置的 Advanced → Recent Deliveries 里手动重投。列表只取最近 100 次投递，更早的失败不会被发现。
- **预览环境的镜像约定。** 需要 demo 仓库的 CI 配合推送 `:<head_sha>` 镜像。
