# M5 预览验证

预览用于收集运行时证据。controller 部署应用。runner 中的模型读取 Pod、事件和日志，再提交评审。模型的 `request_changes` 在 GitHub 上对应 COMMENT，不会提交阻塞合并的 REQUEST_CHANGES。

## 本仓库的验收应用

`demo/preview/server.mjs` 是独立 HTTP 服务。`/` 和 `/healthz` 返回 200。其他路径返回 404。它不需要数据库或任何密钥。

`.github/workflows/preview-image.yaml` 为本仓库的 PR 构建 `linux/amd64` 和 `linux/arm64` 镜像。标签是完整的 PR head SHA，不是 GitHub 的合并测试 SHA。镜像地址为 `ghcr.io/rooobinye/ai-pr-review-cloud-preview:<head_sha>`。构建不传入密钥参数或密钥文件。fork PR 不执行发布。其他仓库使用此示例时，需要更改镜像地址，并将镜像包设为公开，供集群匿名拉取。

根目录 `.ai-pr-review.yaml` 指向示例 chart。正常的 readiness 路径为 `/healthz`。故障验收 PR 只把 `demo/preview/chart/values.yaml` 中的路径改为 `/readyz`。评审服务自己的探针不变。不要合并故障 PR。

验收必须同时满足：

1. CI 发布与 PR head 一致的镜像。
2. 预览 Pod 已运行，但 readiness 请求得到 404。
3. agent 实际调用 `kubectl`，读取 Pod、事件或日志。只根据 diff 找到错误不算通过。
4. 最终结论为 `request_changes`，并引用运行时证据。
5. PR 关闭后，预览 namespace 被删除。

## 开启服务端预览

服务端 Helm chart 默认关闭预览。开启需要 `preview.enabled=true`，并配置 `preview.apiServerCIDRs`。列表必须包含 Kubernetes Service IP 和实际 API endpoint IP；不同 CNI 对 Service 地址转换的处理不同。端口仅放行 TCP 443 和 6443。

查询地址不需要读取任何 Secret：

```sh
kubectl get service kubernetes -o jsonpath='{.spec.clusterIP}'
kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes -o yaml
```

将每个 IPv4 地址写成 `/32` CIDR；IPv6 地址使用 `/128`。以下地址仅为示例，执行前替换为实际地址：

```sh
helm upgrade ai-pr-review-cloud deploy/helm/ai-pr-review-cloud \
  --namespace ai-pr-review-cloud --reuse-values \
  --set preview.enabled=true \
  --set 'preview.apiServerCIDRs[0]=10.96.0.1/32' \
  --set 'preview.apiServerCIDRs[1]=172.18.0.2/32' \
  --set image.tag=<已加载或已发布的新镜像标签> \
  --wait --timeout 5m
```

开启预览后，controller 必须为单副本，更新策略为 Recreate。同一个 PR 的部署和清理在一个进程中串行执行，不同 PR 可以并行部署。首次从旧 chart 升级时，Helm 可能保留旧的 `rollingUpdate` 默认字段。如果升级报告策略字段冲突，先执行下面的迁移命令，再重试升级：

```sh
kubectl patch deployment ai-pr-review-cloud-controller \
  -n ai-pr-review-cloud --type=merge \
  -p '{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}'
```

`--reuse-values` 会保留旧镜像标签。必须明确设置新标签，并确认 controller 和 runner Job 实际使用该标签。不要把“镜像构建成功”当作“新版本已部署”。

## 配置与范围

```yaml
preview:
  enabled: true
  helm:
    chart: demo/preview/chart
    # valuesFile: demo/preview/values-preview.yaml
  readyTimeout: 2m
```

`readyTimeout` 默认 5 分钟，允许 1 秒至 10 分钟。部署等待计入 runner Job 的总时限。镜像尚未发布时，Kubernetes 会重试拉取；镜像等待也计入 readiness 时限。超时后不立即删除应用，agent 可以继续诊断。

初版支持自包含的 Helm chart。controller 离线运行 `helm template`，不下载依赖或运行插件，不执行 schema 的外部引用。它强制设置 `image.tag=<head_sha>`，并检查实际 Deployment 镜像使用该标签。chart 文件最多 100 个，总内容最多 1 MiB，渲染结果最多 30 个资源。

允许的资源是 Deployment、ClusterIP Service 和 ConfigMap。不支持 chart 自带的 Secret、RBAC、hook、外部 Service、卷、init container、自定义 ServiceAccount 或 Pod 注解。预览应用没有 ServiceAccount token，使用 Restricted Pod Security admission、ResourceQuota 和默认拒绝网络策略。初版适合不依赖外部服务的演示应用。

controller 为预览创建只读 ServiceAccount。它只能 get/list/watch 本 namespace 中的 Pod、日志、事件、Deployment 和 ReplicaSet。只读令牌有效期为一小时。runner 只在进程内存中保存它，通过 stdin 交给 `kubectl`。令牌不写入配置文件、命令参数、session 快照或日志。controller 的令牌和 GitHub 凭据不会传给模型。

`kubectl` 工具只接受固定的只读命令。namespace 和 kubeconfig 由程序设置。工具不允许 exec、删除资源、读取 Secret、切换 namespace、覆盖 API 地址或无限跟随日志。工具输出中的日志和事件均视为不可信数据。

## 生命周期与已知限制

每个 PR 使用独立 namespace。同一 head 重试时复用环境；新 head 到达时，controller 检查归属并重建该 PR 的环境，避免旧资源残留。旧尝试不能推进新尝试的状态。

controller 每分钟检查 PR 状态。PR 关闭或合并后删除 namespace。超过 24 小时且没有进行中评审的环境，或 PR 已不可访问的环境，作为孤儿删除。GitHub 暂时出错时保留环境。这条清理路径不依赖收到 closed webhook。

动态 namespace 所需的权限不能只包含 `namespaces`。预览开关会给 controller 增加明确列出的集群级资源操作权限。Kubernetes RBAC 无法按 namespace 前缀约束这些权限；归属检查由 controller 执行。这是单实例演示部署的权限边界，不是多租户部署方案。

当前成本指标的 `review` 阶段包含预览部署等待和运行时检查；没有单独的 `preview` 耗时指标。部署状态和工具调用可从 runner 日志及 session 快照核对。
