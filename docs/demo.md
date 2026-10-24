# 三分钟演示

在本机 Kind 部署上现场跑一次完整闭环：PR 转为待评审 → webhook → 独立 runner Pod 评审代码 → 中途删除 Pod，从快照恢复 → 部署预览环境，agent 用 kubectl 取证 → 在 PR 上投票并给出修复建议 → 看成本。驱动脚本是 [`demo/demo.sh`](../demo/demo.sh)。

演示 PR 修改的是预览示例应用（[`demo/preview`](../demo/preview)）：把路由改成查表，并新增 `/version`。脚本里的这份改动经过压缩和编码：评审 agent 会读整个仓库，包括这个脚本，它需要自己判断这次改动。

## 准备（不计时）

前置：已按 [README](../README.md) 部署并接入 GitHub App，服务端开启了[预览](preview.md)，`gh` 已登录。

```bash
make webhook-forward SMEE_URL=<smee 通道地址>
```

```bash
kubectl -n ai-pr-review-cloud port-forward svc/ai-pr-review-cloud-grafana 3000:3000
```

```bash
demo/demo.sh prep
```

`prep` 从 `main` 建分支、提交改动、开 **draft** PR，然后等 CI 把 `:<head_sha>` 预览镜像推到 GHCR（约 1 分钟）。draft 不触发评审，所以开演时镜像已经就绪，不用在镜头前等 CI。

浏览器准备三个标签页：PR 的 Files changed、PR 的 Conversation（看 Check Run 和评审）、Grafana 首页。终端字号调大。

## 时间线（3:00）

开场一句话后立刻启动脚本，在 agent 读代码的同时讲 PR 的改动。时间点取自三次彩排，模型每次的探索路径不同，各段会前后浮动 10 到 20 秒。

| 时间 | 画面 | 讲什么 |
| --- | --- | --- |
| 0:00 | 终端执行 `KILL_AFTER=25 demo/demo.sh run <pr>` | 脚本只把 PR 标为待评审，之后的一切由 GitHub 事件驱动 |
| 0:00–0:15 | 终端：`queued` → `running`，出现 runner Job | gateway 验签写库，controller 用 `SKIP LOCKED` 领取 run，为这个 PR 建一个独立 Job；runner 只有带预算的虚拟 key，没有 GitHub 凭据，出网只能到 LiteLLM 和 controller |
| 0:15–0:30 | 浏览器：PR 的 Files changed；终端滚动 `agent read/grep/git ...` | 一个看起来无害的小重构。agent 自己决定读什么：被改的文件、chart 的模板与 values、相关文档，做跨文件核对 |
| 约 0:35 | 终端：`chaos: deleted the runner Pod`，随后 `attempt 2`、`resuming from a snapshot` | 模拟节点故障。runner 每轮上传 session 快照，新 Pod 从第 N 轮接着跑，不从头再来，PR 上也不会出现第二份评论 |
| 0:50–1:55 | 终端：`previewing` 和预览 Pod 的状态；浏览器：PR 的 Check Run | controller 把 PR 的镜像部署到 `pr-<repo>-<n>` 独立 namespace（配额、默认拒绝网络策略），最多等 45 秒就绪；Check Run 显示第 2 次尝试 |
| 1:35–2:15 | 终端：`agent kubectl describe/get/logs`，`submit_review`，最后是汇总 | 等待结束后 agent 拿到只读 kubectl，自己取证：Pod 状态、事件、容器日志。结论通过带 schema 的 `submit_review` 交给 controller；汇总里是各阶段耗时和本次花费（约 $0.01 到 $0.03） |
| 2:15–2:45 | 浏览器：PR 的 Conversation | bot 的 COMMENT review：运行时证据、逐行评论、一行修复建议；Check Run 为 neutral。发现问题只评论、不阻塞合并；结论为 approve 时它投的是能计入审批数的 APPROVE |
| 2:45–3:00 | Grafana | 看板上的吞吐、各阶段耗时、每次评审成本的 P50 / P95 |

演示结束后关掉 PR（不计时）。controller 每分钟检查一次 PR 状态，约 1.5 分钟内删除预览 namespace：

```bash
demo/demo.sh cleanup <pr>
```

不想演示崩溃恢复时去掉 `KILL_AFTER`。彩排中有无故障的用时相同，恢复多花的建 Pod 时间被模型探索的随机波动掩盖了。

## 彩排记录

2026-10-24，Kind，`deepseek/deepseek-flash`，thinking high，预览 `readyTimeout: 45s`。计时从脚本启动到投票完成，三次分别为 1:57、1:57、2:12，留给讲解结果约 45 秒：

| PR | 场景 | 结果 | `run` 用时 | 各阶段（排队 / 建 Pod / 拉代码 / 评审 / 投票） | 花费 |
| --- | --- | --- | --- | --- | --- |
| [#16](https://github.com/RooobinYe/ai-pr-review-cloud/pull/16) | 无故障 | request_changes，2 条评论（1 high） | 1:57 | 3s / 4s / 4s / 96s / 3s | $0.0187，13 轮 |
| [#17](https://github.com/RooobinYe/ai-pr-review-cloud/pull/17) | 第 25 秒删除 runner Pod | 第 2 次尝试从第 5 轮恢复；request_changes，1 条 high 评论，PR 上只有一份 review 和一个 Check Run | 1:57 | 1s / 30s / 5s / 71s / 2s | $0.0139，10 轮 |
| [#18](https://github.com/RooobinYe/ai-pr-review-cloud/pull/18) | 第 25 秒删除 runner Pod | 第 2 次尝试从第 6 轮恢复；request_changes，1 条 high 评论 | 2:12 | 2s / 31s / 4s / 90s / 2s | $0.0260，14 轮 |

三次的 high 评论都引用了 kubectl 读到的事件或 Pod 状态，并给出一行修复。#18 多花的 15 秒来自模型：恢复后它在预览阶段开始前两次调用尚未开放的 `submit_review`，取证时又有两条 kubectl 参数被只读白名单拒绝，都由它自己改用其他命令完成。评审阶段里约 45 秒是等待预览就绪：演示应用正常时几秒就绪，`readyTimeout` 从 2 分钟降到 45 秒，起不来的 PR 不用多等。

彩排的 PR 都以 `docs/m7-demo` 为目标分支，之后已关闭。
