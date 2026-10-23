# ai-pr-review-cloud

装在 GitHub 上、跑在 Kubernetes 上的 AI PR 评审服务。PR 打开或更新时自动触发，每个 PR 在独立的 Pod 里由 agent 探索代码并给出结构化结论，结果以 Check Run 和 PR Review 呈现；可按仓库开启自动合并和预览环境验证。

> 状态：M0 完成。runner 可以在本地评审任意 GitHub PR；集群闭环按 [roadmap](docs/roadmap.md) 逐步实现。

## 架构

```mermaid
flowchart LR
    GH[GitHub] -- webhook --> GW[gateway]
    GW --> DB[(PostgreSQL)]
    CTRL[controller] <--> DB
    CTRL -- Job / PR --> RUN[runner Pod · pi SDK]
    RUN -- 虚拟 key --> LLMP[LiteLLM proxy]
    CTRL -- 评论 / approve / merge --> GH
```

| 组件 | 目录 | 职责 |
| --- | --- | --- |
| gateway | [go/cmd/gateway](go/cmd/gateway) | 校验 webhook 签名，记录评审触发 |
| controller | [go/cmd/controller](go/cmd/controller) | 驱动评审状态机，创建 runner Job，执行所有 GitHub 写操作 |
| fetch | [go/cmd/fetch](go/cmd/fetch) | runner Pod 的 init 容器，持有只读 token 拉取代码后退出 |
| runner | [runner](runner) | 基于 pi SDK 的评审 agent，只产出结论，不持有 GitHub 凭据 |

完整设计见 [docs/architecture.md](docs/architecture.md)，进度见 [docs/roadmap.md](docs/roadmap.md)。

## 本地运行

前置：Go、Node.js ≥ 22.19、Docker、Kind、Helm、kubectl。

```bash
make test
```

```bash
make kind-up
```

```bash
make deploy
```

`make deploy` 会构建 4 个镜像、加载进 Kind，并用 Helm 安装到 `ai-pr-review-cloud` 命名空间（附带一个仅供开发用的 PostgreSQL）。

runner 也可以脱离集群单独运行（本地模式）：评审一个 PR，把结论打印到 stdout。默认使用 DeepSeek 的 `deepseek-flash`，其他模型和参数见 [架构 §4.5](docs/architecture.md#45-模型接入)。

```bash
export LLM_API_KEY=<DeepSeek API key>
```

```bash
docker run --rm -e LLM_API_KEY -e GITHUB_TOKEN local/ai-pr-review-cloud-runner:dev --pr https://github.com/owner/repo/pull/1
```

不用 Docker 时在 `runner/` 下执行 `node src/main.ts --pr <url>`。常用参数：`--format json` 输出 [schema v3](contract/review-result.schema.json) JSON，`--language Chinese` 指定评审语言。`GITHUB_TOKEN` 可选，私有仓库需要。

## 目录结构

```
go/                         gateway、controller、fetch（Go）
runner/                     评审 agent（TypeScript，pi SDK）
contract/                   评审结论的 JSON schema 与 Go / TS 共用样例
deploy/helm/                Helm chart
hack/                       本地环境脚本
docs/                       架构设计、roadmap 与赛题
```
