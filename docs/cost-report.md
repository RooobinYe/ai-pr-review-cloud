# 成本与延迟测算报告

> 对应赛题考核点 5（成本与性能），方法见 [架构 §7.2](architecture.md#72-成本测算方法)。
> 状态：**进行中**。基线已完成 20 个 PR 中的 18 个（每个 3 次）；剩余 2 个 PR 和三组优化对比在测算途中因 DeepSeek 账户余额耗尽中断，补跑后更新本文（见文末"待补"）。

## 结论

- **一次评审的模型成本约 $0.03**（P50 $0.029，P95 $0.043），端到端约 1.5 到 3.5 分钟（P50 104 秒，P95 216 秒）。
- **成本不随 diff 变大而增长，取决于模型探索得多深。** 180 行的 chi#1148 最贵（$0.049），1276 行、几乎全是删除的 ai-pr-review#13 只要 $0.017。三档的成本均值都在 $0.022 到 $0.031 之间。
- **前缀缓存把成本降到约四分之一。** 93% 的输入 token 命中 DeepSeek 的缓存（单价是未命中的 1/50）；同样的 token 不走缓存，成本是现在的 4 到 6 倍。
- **剩下的大头是输出（含思考）token**，占小、中两档成本的约 65%。因此最直接的优化是降低思考强度和缩短探索，而不是压缩上下文。
- **每 100 个 PR 每月约 $8**（按每个 PR 平均评审 3 次估算，P95 口径约 $13），runner Pod 的算力成本在此之下一个数量级。
- **结论稳定**：18 个 PR 各跑 3 次，结论全部一致；同一 PR 三次成本的变异系数中位数 7%。

## 方法

| 项 | 取值 |
| --- | --- |
| 模型 | `deepseek/deepseek-flash`，thinking `high`（与 M0、M1 验收相同） |
| 单价 | 每百万 token：输入 $0.30、缓存读取 $0.006、输出 $1.20（pi 模型目录） |
| 运行方式 | runner 本地模式（`--stats`），与集群模式共用同一评审流程（§4.2）；并发 6 |
| PR | 20 个已合并或已关闭的真实 PR，按改动行数（增 + 删）分三档，见下表 |
| 次数 | 每个 PR 3 次 |
| 驱动与汇总 | [`bench/run.mjs`](../bench/run.mjs)、[`bench/report.mjs`](../bench/report.mjs)；每次运行的原始数据在 [`bench/results/`](../bench/results/) |

本地模式测到的是"拉取 PR + 克隆 + 模型评审"。集群里多出的排队、建 Pod、投票三个阶段由 controller 的指标单独记录（§7.1），不影响模型成本。

| 档位 | PR（改动行数） |
| --- | --- |
| 小（<100 行） | [ai-pr-review#17](https://github.com/RooobinYe/ai-pr-review/pull/17)（10）、[#12](https://github.com/RooobinYe/ai-pr-review/pull/12)（19）、[#5](https://github.com/RooobinYe/ai-pr-review/pull/5)（88）、[go-chi/chi#1097](https://github.com/go-chi/chi/pull/1097)（22）、[#1159](https://github.com/go-chi/chi/pull/1159)（45）、[#1218](https://github.com/go-chi/chi/pull/1218)（67）、[#1220](https://github.com/go-chi/chi/pull/1220)（98） |
| 中（100–500 行） | [ai-pr-review#15](https://github.com/RooobinYe/ai-pr-review/pull/15)（135）、[#8](https://github.com/RooobinYe/ai-pr-review/pull/8)（165）、[#11](https://github.com/RooobinYe/ai-pr-review/pull/11)（237）、[#9](https://github.com/RooobinYe/ai-pr-review/pull/9)（476）、[go-chi/chi#1148](https://github.com/go-chi/chi/pull/1148)（180）、[#1195](https://github.com/go-chi/chi/pull/1195)（249）、[#1214](https://github.com/go-chi/chi/pull/1214)（281） |
| 大（>500 行） | [ai-pr-review#14](https://github.com/RooobinYe/ai-pr-review/pull/14)（515）、[#10](https://github.com/RooobinYe/ai-pr-review/pull/10)（661）、[#13](https://github.com/RooobinYe/ai-pr-review/pull/13)（1276）、[#4](https://github.com/RooobinYe/ai-pr-review/pull/4)（3097）、[ai-pr-review-cloud#1](https://github.com/RooobinYe/ai-pr-review-cloud/pull/1)（2153，待补）、[#2](https://github.com/RooobinYe/ai-pr-review-cloud/pull/2)（4752，待补） |

## 基线结果

以下三张表由 `node bench/report.mjs` 生成。

### 按改动规模分档

| 档位 | PR × 次数 | 改动行数 P50 | 成本 P50 | 成本 P95 | 成本均值 | 端到端 P50 | 端到端 P95 | 平均轮数 | 失败 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 小（<100 行） | 7 × 3 = 20 | 45 | $0.0258 | $0.0316 | $0.0217 | 122s | 221s | 12.8 | 0 |
| 中（100–500 行） | 7 × 3 = 21 | 237 | $0.0345 | $0.0522 | $0.0313 | 105s | 214s | 14.3 | 0 |
| 大（>500 行） | 4 × 3 = 12 | 969 | $0.0304 | $0.0404 | $0.0292 | 91s | 104s | 15.9 | 0 |
| **全部** | 18 × 3 = 53 | 180 | $0.0287 | $0.0430 | $0.0272 | 104s | 216s | 14.1 | 0 |

小档有一次（ai-pr-review#5 第 2 次）在拉取 PR 时遇到 GitHub API 502，没有进入评审，待补跑。

### 时间和 token 花在哪里

| 档位 | 拉取 + 克隆 | 评审（模型） | 工具调用 | 未缓存输入 | 缓存读取 | 缓存命中率 | 输出 | 输出占成本 | 无缓存时成本 | 实际成本 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 小（<100 行） | 5s | 112s | 23.3 | 17.2k | 220.0k | 93% | 12.7k | 65% | $0.0864 | $0.0217 |
| 中（100–500 行） | 5s | 105s | 25.3 | 30.0k | 399.5k | 93% | 16.6k | 64% | $0.1487 | $0.0313 |
| 大（>500 行） | 5s | 75s | 28.1 | 40.1k | 537.4k | 93% | 11.6k | 46% | $0.1872 | $0.0292 |

"无缓存时成本"把缓存读取的 token 按未命中单价重新计价，是关闭前缀缓存的估算。

### 同一 PR 跑 3 次的稳定性

- 18 个 PR 中 18 个三次结论一致。
- 同一 PR 三次成本的变异系数：中位数 7%，最大 35%。

结论与 PR 的实际情况相符：故意带安全问题的 [ai-pr-review#15](https://github.com/RooobinYe/ai-pr-review/pull/15)（标题写明 DO NOT MERGE）三次都是 request_changes，平均 3.7 条 critical/high 评论；go-chi/chi 的 7 个已合并 PR 全部 approve，没有 critical/high 评论。

## 瓶颈分析

**模型评审占端到端时间的 95% 以上。** 拉取 PR 和克隆合计约 5 秒，其余都是模型的多轮探索：平均 14 轮、24 次工具调用。集群模式下的排队、建 Pod、投票耗时见看板，不改变这个结论。

**成本由探索深度决定，与 diff 大小关系很弱。** 每一轮都把完整上下文重新发给模型，上下文随读过的文件增长，所以成本近似"轮数 × 平均上下文"。diff 本身只占首轮输入的一部分：

| PR | 改动行数 | 平均轮数 | 平均成本 | 说明 |
| --- | --- | --- | --- | --- |
| go-chi/chi#1148 | 180 | 17.7 | $0.049 | 路由树的冲突处理；一次运行里 `tree.go` 读了 5 次，输出约 3 万 token |
| ai-pr-review#11 | 237 | 21.3 | $0.039 | 改了 agent 循环 |
| ai-pr-review#13 | 1276 | 15.0 | $0.017 | 几乎全是删除，输出只有约 4.5k token |
| ai-pr-review#17 | 10 | 4.0 | $0.005 | 只改文档 |

**架构 §7.2 预计的"上下文膨胀"被前缀缓存消化了。** 每轮新增的只是上一轮的工具结果，前面的部分都命中缓存，所以 93% 的输入按 1/50 计价。真正的成本来自输出：deepseek-flash 的输出单价是输入的 4 倍、缓存读取的 200 倍，而 `high` 思考强度每轮都会产生几百到上千 token 的推理。

**延迟的长尾来自少数 PR 的长探索，不是排队或 I/O。** P95（216 秒）对应的是 chi#1148、ai-pr-review#12 这类模型反复确认的 PR；同一 PR 三次的耗时也有波动（ai-pr-review#12 从 151 秒到 254 秒）。

## 每 100 个 PR 的月成本

| 口径 | 每次评审 | 每 PR 评审 1 次 | 每 PR 评审 3 次 |
| --- | --- | --- | --- |
| 均值 | $0.027 | $2.7 | $8.2 |
| P95 | $0.043 | $4.3 | $12.9 |

"每 PR 评审 3 次"对应一次打开加两次推送（每个新 `head_sha` 评审一次，§3），M1 实测的 PR 就是这个节奏。算力方面，runner Pod 申请 250m CPU、512Mi 内存，每次约 2 分钟，300 次评审合计约 2.5 vCPU·小时，按常见云厂商价格不到 $0.2；gateway、controller、PostgreSQL 和监控是固定开销，与评审次数无关。

## 优化对比（待补）

同一批 20 个 PR 上对比三种调整，每种各跑 1 次：

| 方案 | 调整 | 针对的瓶颈 |
| --- | --- | --- |
| `thinking-low` | `LLM_THINKING=low` | 输出（推理）token |
| `budget-0.02` | 预算 $0.02，花到 80% 时提示模型提交（§7.3） | 探索轮数 |
| `pro` | 换 `deepseek-v4-pro`（单价约 4 倍） | 作为质量上限的参照 |

prompt cache 一项不需要额外跑：DeepSeek 的前缀缓存默认开启，上面的"无缓存时成本"一列就是关闭缓存的对照，缓存把成本降到 1/4 到 1/6。

三组各完成了 2 到 3 个小 PR 后账户余额耗尽，样本太少，暂不下结论；已完成的几次结论都与基线一致。

"小模型探索、大模型下结论"没有做：当前流程里探索和提交发生在同一个 agent 循环中，模型在循环内调用 `submit_review`，拆成两个模型需要改评审流程。上面的数据也说明收益有限：成本主要是输出 token，而不是长上下文的输入。

## 集群内各阶段耗时（待补）

本地模式测不到排队、建 Pod、投票三个阶段。它们由 controller 的 `review_run_duration_seconds{phase}` 记录，在 Grafana 看板的"各阶段耗时"面板展示。M6 的 Kind 验证集群已经部署了看板并能采到数据，余额耗尽前只完成了一次部分评审，补跑后在这里记录。

## 复现

```bash
node --env-file=.env bench/run.mjs --variant baseline --runs 3
```

```bash
node bench/report.mjs
```

`.env` 里需要 `LLM_API_KEY`；`GITHUB_TOKEN` 缺省时取 `gh auth token`。已有结果的运行会跳过，中断后重新执行即可续跑。优化方案用 `--variant <名称>` 加 `--thinking`、`--budget`、`--model` 参数。

## 局限

- 每档只有 6 到 7 个 PR，P95 是 18 到 21 个样本里插值得到的，只能看量级。
- PR 来自三个 Go 仓库（其中两个是本项目和它的前身），没有覆盖前端或大型单体仓库。
- 测算期间 6 个并发 runner 共用一个 DeepSeek 账户，耗时包含了模型 API 在这种负载下的排队。
- 金额按 pi 模型目录的单价计算，没有和 DeepSeek 账单逐笔核对；M2 接入 LiteLLM 后可以用它的花费统计交叉校验。

## 待补

- [ ] 充值后补跑：基线缺的 7 次（ai-pr-review-cloud#1、#2 各 3 次，ai-pr-review#5 第 2 次），三组优化各约 17 次。
- [ ] 在 Kind 验证集群里跑几次真实评审，记录集群内各阶段耗时并截取看板。
