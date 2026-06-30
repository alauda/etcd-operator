# HCP etcd operator 改造实施计划

本文把 `docs/design/hcp-etcd-design.md` 的设计拆成可并行执行的工程计划，便于多个 Claude Code 会话或多人分阶段落地。

## 目标与边界

### 本期目标

- HCP managed 节点升级时 etcd 保持 quorum 和可用性。
- 支持 etcd 版本安全升级。
- 支持单成员故障自动恢复。
- 明确 quorum 丢失后的 DR/备份恢复路径，但不在首批实现自动化。

### 本期非目标

- 自动备份/自动恢复调度。
- quorum 丢失后的自动恢复。
- TLS 证书自动轮换。
- 自动 defrag。
- DataStore/Kamaji 集成默认纳入核心路径；该能力作为 ACP 专属可选后置。

## 当前实现基线

当前 `etcd-operator` 已具备：

- `EtcdCluster` API：`api/v1alpha1/etcdcluster_types.go`
- reconcile 主流程：`internal/controller/etcdcluster_controller.go`
- StatefulSet / Service / TLS / endpoint helpers：`internal/controller/utils.go`
- etcd member 与 health 工具：`internal/etcdutils/etcdutils.go`
  - `ClusterHealth` 已使用 serializable `Get("health", WithSerializable())`
  - `FindLeaderStatus` / `FindLearnerStatus` / `IsLearnerReady`
  - `AddMember` / `PromoteLearner` / `RemoveMember`
- 生成与测试入口：`Makefile`
  - `make generate`
  - `make manifests`
  - `make api-docs`
  - `make test`
  - `make test-e2e`

主要缺口：

- `EtcdClusterStatus` 为空，controller 不写 status。
- `spec.podTemplate` 只支持 metadata，不支持调度字段。
- 无 PDB。
- 无 `:9980` readyz/healthz probe。
- 无 `<name>-client` Service。
- 无版本升级 guard。
- 无 recovery 状态机、Job、PVC/Pod 删除、reset-member initContainer。
- RBAC 无 PDB、Job、Pod delete、PVC delete、DataStore 权限。

## 工作量评估

按 1 名熟悉 controller-runtime 和 etcd 的开发者估算：

| 阶段 | 内容 | 复杂度 | 预估 |
| --- | --- | --- | --- |
| 0 | 设计澄清与基线验证 | 低 | 0.5-1 人日 |
| 1 | API / status / conditions | 中 | 2-4 人日 |
| 2 | PDB、client Service、podTemplate.spec、podManagementPolicy | 中 | 4-7 人日 |
| 3 | readyz / healthz probe | 中到中高 | 4-8 人日 |
| 4 | reconcile 状态化、status 写回、动作互斥 | 中高 | 5-8 人日 |
| 5 | 版本升级 guard | 中 | 3-5 人日 |
| 6 | 单成员 recovery 执行链路 | 高 | 8-15 人日 |
| 7 | DataStore reconciler（可选） | 中高 | 4-8 人日 |

核心 HCP HA 能力约 **26-48 人日**。如果 `:9980` probe 需要新增 sidecar 或二进制，额外约 **3-6 人日**。如果 DataStore/Kamaji 也纳入同一里程碑，总体约 **30-56 人日**。

## 推荐执行顺序

### Phase 0：需求边界与基线验证

目标：确保后续会话从相同边界开始。

任务：

- 明确本期实现范围：status、PDB、readyz、调度字段、client Service、版本 guard、单成员 recovery。
- 明确后置范围：自动备份/恢复、quorum 丢失自动恢复、TLS 自动轮换、自动 defrag。
- 记录当前基线测试结果。

关键文件：

- `docs/design/hcp-etcd-design.md`
- `Makefile`
- `test/e2e/*.go`

验证：

```sh
make test
# e2e 成本高，按需运行
make test-e2e
```

### Phase 1：API、Status、Conditions

目标：让 CRD 能表达 HCP 生产可用能力，并为后续状态机提供稳定数据结构。

任务：

- 扩展 `EtcdClusterSpec`：
  - `spec.podTemplate.spec.nodeSelector`
  - `spec.podTemplate.spec.tolerations`
  - `spec.podTemplate.spec.affinity`
  - `spec.podTemplate.spec.topologySpreadConstraints`
  - `spec.podTemplate.spec.priorityClassName`
  - `spec.recovery.enabled`
  - `spec.recovery.gracePeriod`
  - `spec.recovery.timeout`
  - `spec.recovery.maxRetries`
- 填充 `EtcdClusterStatus`：
  - `phase`
  - `readyReplicas`
  - `memberCount`
  - `leaderID`
  - `members[]`
  - `recovery`
  - `conditions[]`
  - `observedGeneration`
- 建议 condition 类型：
  - `EtcdClusterCreated`
  - `EtcdClusterReady`
  - `QuorumAvailable`
  - `SingleMemberRecoveryActive`
  - `DataStoreReady`，仅 DataStore 功能启用时维护

关键文件：

- `api/v1alpha1/etcdcluster_types.go`
- `api/v1alpha1/zz_generated.deepcopy.go`
- `config/crd/bases/operator.etcd.io_etcdclusters.yaml`
- `docs/api-references/docs.md`
- `docs/api-references/config.yaml`

验证：

```sh
make generate
make manifests
make api-docs
make test
```

注意：`docs/api-references/config.yaml` 当前忽略 `status` 文档。如果希望 API reference 展示 status，需要同步调整该配置。

### Phase 2：基础 HA 资源渲染

目标：先交付低破坏性、收益直接的 Kubernetes 资源能力。

任务：

- StatefulSet：
  - 设置 `podManagementPolicy: Parallel`。
  - 透传 `podTemplate.spec` 调度字段。
  - 默认设置高优先级 `priorityClassName`，并允许用户覆盖。
- 保留 peer discovery Service：
  - `<name>`
  - `ClusterIP: None`
  - `PublishNotReadyAddresses: true`
- 新增 client Service：
  - `<name>-client`
  - headless
  - `publishNotReadyAddresses: false`
  - 暴露 2379
- 新增 PDB：
  - 3 member 时 `maxUnavailable: 1`
  - `unhealthyPodEvictionPolicy: AlwaysAllow`
  - selector 与 StatefulSet pod labels 对齐
- RBAC 增加：
  - `policy/v1/poddisruptionbudgets`

关键文件：

- `internal/controller/utils.go`
- `internal/controller/etcdcluster_controller.go`
- `config/rbac/role.yaml`
- `internal/controller/*_test.go`

验证：

```sh
make manifests
make test
```

新增测试建议：

- StatefulSet 包含 `PodManagementPolicy=Parallel`。
- podTemplate 调度字段正确透传。
- headless Service 保持 `PublishNotReadyAddresses=true`。
- client Service 为 `PublishNotReadyAddresses=false` 且暴露 2379。
- PDB spec 与 selector 正确。

### Phase 3：readyz / healthz probe

目标：让 Ready 语义从 “Pod Running” 变成 “本地 etcd 健康且为 voting member”。

推荐语义：

- `GET :9980/healthz`：宽松探活。
- `GET :9980/readyz`：
  - 对本地 etcd 执行 serializable `Get("health", WithSerializable())`。
  - 判断本地 member 不是 learner。

StatefulSet 注入：

- liveness probe: `/healthz`
- readiness probe: `/readyz`
- startup probe: `/readyz`

关键决策：

- `:9980` server 用 sidecar、新二进制，还是复用现有镜像实现。
- 若需要新增二进制或 sidecar，建议该阶段独立 PR，避免和 PDB/Service 渲染混在一起。

关键文件：

- `internal/controller/utils.go`
- `internal/etcdutils/etcdutils.go`
- 可能新增健康探针包或 sidecar 入口
- 镜像构建相关文件，如最终选择新增二进制

验证：

```sh
make test
make test-e2e
```

新增 e2e 建议：

- learner promote 前不 Ready。
- `<name>-client` Service 只命中 Ready Pod。
- 1 个 member 异常时其他健康 member 不因 quorum 判断被误置 NotReady。

### Phase 4：reconcile 状态化与 status 写回

目标：把当前线性 reconcile 改造成 “观察状态 → 计算意图 → 执行动作 → 写 status”，并为 upgrade/recovery 互斥打基础。

当前流程：

- `fetchAndValidateState`
- `bootstrapStatefulSet`
- `performHealthChecks`
- `reconcileClusterState`

任务：

- 扩展 observed state：
  - StatefulSet
  - Pods
  - Services
  - PDB
  - optional Jobs/PVCs for recovery later
  - etcd MemberList
  - member health
  - leader/learner
- 统一计算：
  - quorum 是否可用
  - 是否有 learner
  - 是否 scale in/out 中
  - 是否 StatefulSet rolling update 中
  - 是否 recovery active
- reconcile 末尾或错误路径上 patch status。
- 避免 status update hot loop：只在 semantic diff 后写回。
- 修正 `targetReplica == spec.size` 时提前返回导致健康检查/状态更新不完整的问题。

关键文件：

- `internal/controller/etcdcluster_controller.go`
- `internal/controller/utils.go`
- `internal/etcdutils/etcdutils.go`
- `api/v1alpha1/etcdcluster_types.go`

验证：

```sh
make test
make test-e2e
```

新增测试建议：

- `status.phase=Ready`。
- `status.readyReplicas=3`。
- `status.members[]` 数量正确。
- leader、learner、healthy 状态正确。
- condition 变化不造成 reconcile hot loop。

### Phase 5：版本升级 guard

目标：防止误降级和跨 minor 升级导致 unsupported etcd 混版窗口。

任务：

- 在修改 StatefulSet image/version 前执行版本检查。
- 校验：
  - patch upgrade allowed
  - adjacent minor upgrade allowed
  - downgrade blocked
  - cross-minor blocked，推荐首版直接 block
  - invalid version string blocked
- blocked 时：
  - 不更新 StatefulSet image
  - 更新 condition reason/message
  - 记录 event
- upgrade in-progress 时 recovery 不得触发。

关键文件：

- `internal/controller/etcdcluster_controller.go`
- `internal/controller/utils.go`
- `api/v1alpha1/etcdcluster_types.go`
- `internal/controller/*_test.go`

验证：

```sh
make test
make test-e2e
```

新增测试建议：

- patch upgrade allowed。
- adjacent minor upgrade allowed。
- downgrade blocked。
- cross-minor blocked。
- blocked 时 StatefulSet image 不进入非法版本。
- condition/event 正确。

### Phase 6：单成员自动恢复

目标：实现单成员故障自动恢复。该阶段风险最高，必须依赖 Phase 1-5。

准入条件：

- `spec.recovery.enabled=true`
- quorum 可用
- 恰好 1 个 member 异常
- 异常持续超过 `gracePeriod`
- 无 scale in/out
- 无 StatefulSet rolling upgrade
- 无 learner promotion 中
- 无 recovery job active
- member 能稳定映射到 StatefulSet ordinal、Pod、PVC

任务：

- 新增 recovery 状态机：
  - Pending
  - Running
  - Succeeded
  - Failed
  - Blocked
- 新增 recovery Job：
  - preflight 再确认
  - 调用 etcd member remove/add 或执行设计中的确认动作
  - controller 读取 Job Complete/Failed 更新 status
- StatefulSet 注入 `reset-member` initContainer：
  - `/var/lib/etcd/member/snap/db` 存在时 no-op
  - 空盘时 remove old member + add new peer
- operator 执行破坏性动作：
  - 删除异常 member 对应 Pod
  - 删除对应 PVC
- RBAC 增加：
  - `batch/jobs`
  - `core/pods` get/list/watch/delete
  - `core/persistentvolumeclaims` get/list/watch/delete

安全规则：

- 多于 1 个异常 member 时只告警，不自动恢复。
- quorum 丢失时只告警，不自动恢复。
- `NOSPACE` 不进入自动恢复，应该告警并走 compact/defrag。
- recovery 必须幂等，重复 reconcile 不得重复删 PVC。

关键文件：

- `api/v1alpha1/etcdcluster_types.go`
- `internal/controller/etcdcluster_controller.go`
- `internal/controller/utils.go`
- `internal/etcdutils/etcdutils.go`
- `config/rbac/role.yaml`
- `test/e2e/*.go`

验证：

```sh
make test
make test-e2e
```

新增 slow e2e 建议：

- 三节点 quorum 可用时单 member 恢复成功。
- 多 member 异常不自动恢复。
- quorum 丢失不自动恢复。
- upgrade/scale/learner 中不自动恢复。
- recovery Job 幂等。
- PVC/Pod 删除只作用于目标 ordinal。

### Phase 7：DataStore reconciler（可选）

目标：实现 ACP/Kamaji 专属接入，不阻塞通用 HCP HA 核心能力。

任务：

- annotation 触发：`hcp.alauda.io/datastore: <name>`
- `EtcdCluster` Ready 后创建/更新 DataStore。
- endpoint 指向 `<name>-client.<ns>.svc:2379`。
- TLS 指向 client secret。
- 使用 dynamic client 或 feature gate，避免无 DataStore CRD 环境下 manager 启动失败。

建议：

- 独立 PR。
- annotation gate。
- 不污染核心 reconcile 路径。

## 建议 PR 拆分

1. **PR1：API / status / conditions**
   - 扩 CRD 字段与 status。
   - 生成 deepcopy、CRD、API docs。
   - 不改破坏性逻辑。

2. **PR2：PDB、client Service、podTemplate.spec、podManagementPolicy**
   - 完成基础资源渲染。
   - 补 RBAC 和单测。

3. **PR3：readyz / healthz probe**
   - 实现 `:9980`。
   - 注入 probes。
   - 验证 learner 不进入 Ready/client Service。

4. **PR4：status 写回与 reconcile 状态化**
   - 统一 observed state。
   - 写 conditions/members/leader/quorum。
   - 建立 scale/upgrade/recovery 互斥基础。

5. **PR5：版本升级 guard**
   - 禁降级。
   - 禁跨 minor。
   - blocked 时不更新 StatefulSet，并写 condition/event。

6. **PR6：recovery preflight 骨架**
   - 只做准入判断、状态和拒绝条件。
   - 不删除 PVC、不执行 Job。

7. **PR7：recovery 执行链路**
   - recovery Job。
   - reset-member initContainer。
   - PVC/Pod 删除。
   - slow e2e。

8. **PR8：DataStore reconciler（可选）**
   - ACP/Kamaji 专属能力。
   - annotation gate。
   - 独立测试。

9. **PR9：文档、样例、dist、CI 补齐**
   - README/install/API docs/sample。
   - HCP HA 示例。
   - recovery/DR 运维边界。
   - 可选补 PR CI/e2e workflow。

## 多会话执行建议

每个会话开始时应先阅读：

1. 本文档。
2. `docs/design/hcp-etcd-design.md` 中与当前 Phase 对应的章节。
3. 当前 Phase 的关键文件。

每个会话结束时应更新对应 PR/Phase 的进度，至少记录：

- 已修改文件。
- 已运行命令。
- 测试结果。
- 未解决问题。
- 下一步建议。

不要并行修改同一批核心文件，尤其是：

- `api/v1alpha1/etcdcluster_types.go`
- `internal/controller/etcdcluster_controller.go`
- `internal/controller/utils.go`
- `internal/etcdutils/etcdutils.go`

建议并行方式：

- API/status 与 docs 可单独会话。
- PDB/client Service 和 readyz probe 可分会话，但需先约定 probe server 方案。
- recovery preflight 与 recovery execution 不要并行，必须先完成 preflight 审查。
- DataStore reconciler 可在核心 HA 能力稳定后独立会话处理。

## 总体验收矩阵

| 能力 | 单测 | e2e | 关键验收 |
| --- | --- | --- | --- |
| API/status | 必须 | 建议 | CRD/schema/deepcopy/docs 正确 |
| podTemplate.spec | 必须 | 建议 | StatefulSet pod spec 字段正确 |
| PDB | 必须 | 建议 | `maxUnavailable=1`，selector 正确 |
| client Service | 必须 | 必须 | 只命中 Ready pod，端口 2379 |
| readyz/healthz | 必须 | 必须 | learner NotReady，本地健康不依赖 quorum |
| status 写回 | 必须 | 必须 | phase/members/leader/quorum/conditions 正确 |
| 版本 guard | 必须 | 建议 | 禁降级、禁跨 minor，blocked 不改 STS |
| recovery preflight | 必须 | 建议 | 多异常/quorum lost/upgrade/scale 均阻止 |
| recovery execution | 必须 | 必须 slow | 单成员恢复成功且幂等 |
| DataStore | 必须 | 可选 | annotation gate，不影响无 CRD 环境 |

## 首批里程碑建议

首批建议只做到：

1. API/status/conditions
2. podTemplate.spec 调度字段
3. PDB
4. client Service
5. readyz/healthz probe
6. status 写回
7. 版本升级 guard

这部分完成后，即使还没有自动 recovery，也能显著提升 HCP managed 节点升级和 etcd 版本升级时的可用性与可观测性。

单成员 recovery 和 DataStore 建议作为后续独立里程碑，避免把破坏性恢复逻辑和基础 HA 改造混在一个大变更中。

## 2026-06-29 开发进度更新（Phase 3 起）

### 本轮已完成

- Phase 3 readyz / healthz probe：
  - 新增 `cmd/etcd-probe` 与 `internal/probe` 的本地 probe server。
  - `/healthz` 返回宽松探活；`/readyz` 对本地 etcd endpoint 执行 serializable `Get("health")`，并通过 endpoint status 拦截 learner。
  - StatefulSet 支持通过 controller `--probe-image` 注入 `etcd-probe` sidecar。
  - 注入 etcd 主容器 liveness `/healthz`、readiness `/readyz`、startup `/readyz`。
  - TLS 场景下 probe sidecar 挂载 `<cluster>-client-tls` 为 `client-secret`，与 `--cacert/--cert/--key` 参数匹配。
  - Dockerfile 同时构建并打包 `/manager` 与 `/etcd-probe`。
- Phase 4 status 写回基础：
  - reconcile 末尾按语义 diff 更新 `.status`，包含 `phase`、`readyReplicas`、`memberCount`、`leaderID`、`members[]`、`observedGeneration`、conditions。
  - condition 当前维护 `EtcdClusterCreated`、`EtcdClusterReady`、`QuorumAvailable`、`SingleMemberRecoveryActive`。
  - 修正 steady-state 时提前返回导致 StatefulSet template 不被 patch 的问题；在 steady-state 仍会经过安全校验后 patch StatefulSet。
- Phase 5 版本升级 guard：
  - 新增版本解析与校验：允许同版本、patch 升级、相邻 minor 升级；阻止降级、跨 minor、major 变化、非法版本字符串。
  - guard 已前置到所有可能 patch StatefulSet image 的路径之前，包括 bootstrap-from-zero、member/replica drift 修复、scale in/out、steady-state patch。
  - blocked 时设置 `EtcdClusterReady=False` / `Reason=EtcdVersionUpgradeBlocked`，并记录 warning event（Recorder 存在时）。
- 测试补充：
  - StatefulSet probe sidecar/probe/TLS volume 单测。
  - etcd version guard 单测。

### 本轮修改文件

- `cmd/main.go`
- `cmd/etcd-probe/main.go`
- `Dockerfile`
- `config/manager/manager.yaml`
- `internal/probe/server.go`
- `internal/controller/etcdcluster_controller.go`
- `internal/controller/etcdcluster_controller_test.go`
- `internal/controller/utils.go`
- `internal/controller/utils_test.go`
- `docs/design/hcp-etcd-operator-implementation-plan.md`
- 以及 `make manifests generate` 更新的生成文件/CRD/RBAC（如有 diff）。

### 已运行验证

```sh
go test ./cmd ./internal/controller ./internal/probe ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过

make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet
# 结果：通过

go test ./cmd ./internal/controller ./internal/probe ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过（generation/fmt/vet 后复跑）
```

### 仍未完成 / 风险

- Phase 4 尚未完整实现：
  - `status.members[].nodeName` 已从 StatefulSet 对应 Pod 列表填充。
  - observed state 已收集 Pods，但尚未统一收集 Services/PDB/Jobs/PVCs。
  - scale/rolling update/recovery active 的完整互斥状态尚未沉淀为独立 observed state。
  - status 更新失败目前仅记录日志，不会改变 reconcile 返回值；需要评估是否应重试。
- Phase 6 单成员自动恢复尚未实现执行链路：
  - 尚未实现 recovery Job、Pod/PVC 删除、reset-member initContainer、Job Complete/Failed 状态同步。
  - 当前仅 API/status 中已有 recovery 字段基础，自动恢复仍需后续实现。
- Phase 7 DataStore reconciler 仍未实现，仍按设计标记为 ACP/Kamaji 可选后置能力。
- `:9980` probe 使用 sidecar 方案；部署时必须确保 controller image（或 `--probe-image` 指定镜像）内包含 `/etcd-probe`。当前 `config/manager/manager.yaml` 默认传 `--probe-image=controller:latest`，发布镜像需与 manager 镜像一致或由部署系统替换。
- 尚未运行 `make test` 全量和 `make test-e2e`；本轮只运行了聚焦测试、manifests/generate/fmt/vet。

### 下一步建议

1. 完成 Phase 4 observed state：列 Pod 并填充 `members[].nodeName`，显式计算 rolling update / scale in-out / learner / recovery active。
2. 实现 Phase 6 preflight 骨架：只做准入判断和 Blocked/Suppressed 状态，不先执行删除动作。
3. 在 preflight 单测稳定后，再实现 recovery Job、reset-member initContainer、目标 ordinal 的 Pod/PVC 删除与幂等保护。
4. 增加 probe server 单测（mock Checker 覆盖 readyz 成功/失败/learner）和 controller status 单测。
5. 条件允许时运行 `make test`；有集群环境时补 `make test-e2e` 验证 learner NotReady 与 client Service 只命中 Ready Pod。

### 本轮追加修正

- Phase 4 status 增量：controller 已列出 StatefulSet 对应 Pod，并在 `status.members[].nodeName` 中填充 Pod 所在节点。
- RBAC 已通过 kubebuilder marker 增加 Phase 6/后续所需基础权限：`pods` get/list/watch/delete、`persistentvolumeclaims` get/list/watch/delete、`batch/jobs` get/list/watch/create/update/patch/delete。
- 版本 guard 已再次确认前置于所有 StatefulSet patch 路径。
- 第二轮复验发现 `cmd/main.go` 已解析 `--probe-image` 但未传入 `EtcdClusterReconciler.ProbeImage`；已修复该 runtime wiring blocker，默认部署传入的 `--probe-image=controller:latest` 现在会驱动 StatefulSet 注入 `etcd-probe` sidecar/probes。

追加验证：

```sh
make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet && \
  go test ./cmd ./internal/controller ./internal/probe ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过
```

第二轮复验后补充修复验证：

```sh
gofmt -w /Users/mac/go/workdir/src/github.com/alauda/etcd-operator/cmd/main.go && \
  go test ./cmd ./internal/controller ./internal/probe ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过

make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator vet
# 结果：通过
```

## 2026-06-29 Phase 6 开发进度更新

### 本轮已完成

- Phase 6 单成员自动恢复链路（安全优先、controller 直接执行 destructive 动作，暂未引入独立 Job）：
  - 增加 recovery preflight：
    - `spec.recovery.enabled=false` 不触发。
    - StatefulSet / member count / spec.size 不一致时阻止恢复。
    - StatefulSet rolling update 中阻止恢复。
    - learner 存在时阻止恢复。
    - quorum 不可用时阻止恢复。
    - 多成员异常时阻止恢复。
    - 发现 `NOSPACE` 文本时阻止恢复并记录 Blocked（当前仅基于 endpoint status/errors 字符串识别，不能替代完整 AlarmList）。
    - 单成员异常且 quorum 仍可用、无 learner/scale/rolling update 时允许恢复。
  - 增加 `status.recovery` 状态更新：支持 `Pending`、`Running`、`Succeeded`、`Failed`、`Blocked`。
  - 增加 `SingleMemberRecoveryActive` condition：恢复动作执行后置 True，Blocked/Pending/Failed/Succeeded 时置 False。
  - `performHealthChecks` 在已拿到 member list 和部分 health 结果时，即使发现 unhealthy member 也继续进入 reconcile，使 recovery preflight 有机会判断；member list 不可用仍按错误处理。
  - 增加 destructive 执行链路：controller 直接删除目标 ordinal 对应 Pod 和 PVC，删除使用精确名称：
    - Pod：`<cluster>-<ordinal>`
    - PVC：`etcd-data-<cluster>-<ordinal>`
    - `NotFound` 视为成功，重复 reconcile 幂等。
  - 增加 `reset-member` initContainer 渲染：
    - 仅在 `spec.recovery.enabled=true` 且存在 `storageSpec` 时注入。
    - 数据库 `/var/lib/etcd/member/snap/db` 存在时 no-op。
    - 空盘时执行 `etcdctl member remove` 旧 member（如存在）并 `member add` 当前 Pod peer URL。
    - TLS 场景挂载 client secret。
  - `etcdutils.FindLeaderStatus` / `FindLearnerStatus` 对 nil status 做了防御，避免部分 endpoint 不健康时 panic。
  - RBAC 已包含 pods/PVC delete 和 batch/jobs 权限；本轮 controller 直接执行，Job 权限保留给后续切换到 Job 执行链路。

### 本轮新增/更新测试

- `internal/controller/etcdcluster_controller_test.go`
  - disabled 不触发。
  - quorum lost 不触发。
  - 多成员异常不触发。
  - 单成员异常且满足准入时触发恢复，并只删除目标 Pod/PVC。
- `internal/controller/utils_test.go`
  - `reset-member` initContainer 注入。
  - reset-member 数据卷和 TLS client secret mount。

### 本轮验证结果

```sh
make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet
# 结果：通过

go test ./cmd ./internal/controller ./internal/probe ./internal/etcdutils ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过
```

### 剩余风险 / 下一步

- 本轮采用 controller 直接删除 Pod/PVC 的等效执行链路，尚未实现设计中独立 recovery Job 的二次 preflight / Complete / Failed 生命周期。后续如要进一步贴近设计，应把 destructive 动作迁移到 Job 或至少加入 Job marker/lease，controller 只观察 Job 状态。
- `NOSPACE` 目前仅通过 endpoint health/status error 字符串保守识别，尚未调用 etcd `AlarmList`；因此无法完整覆盖所有 NOSPACE 场景。后续 recovery Job/preflight 应显式执行 `AlarmList` 并将 NOSPACE Blocked 写入 status。
- reset-member initContainer 使用 shell + `etcdctl`，要求 etcd 镜像内包含 `/bin/sh` 和 `etcdctl`；需要在实际镜像中验证。如果镜像不满足，应改为专用 recovery/probe 镜像或把 reset-member 逻辑编译为本仓库二进制。
- 当前恢复完成判定较保守：当下一轮 status 观察到集群 Ready 且上次 recovery 为 Running 时标记 Succeeded；尚未实现 timeout 后 Failed、Job 日志留存、按 `maxRetries` 的完整失败重试窗口。
- 尚未运行 e2e。需要在真实集群验证：单成员 CrashLoop/空盘重建、reset-member remove/add、client Service endpoints 恢复、quorum lost/multi failure/rolling update 中不会误删。

### Phase 6 补充修正

- 已确认 `EtcdRecoveryResult` enum 扩展包含 `Pending` / `Running`，并通过 `make manifests generate` 同步 CRD/deepcopy。
- `reset-member` initContainer 调整为连接其他健康 peer endpoint 执行 `member list/remove/add`，避免空盘目标 Pod 自身 etcd 尚未启动时把本地 endpoint 作为唯一 endpoint。
- 复跑最终验证：

```sh
make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet && \
  go test ./cmd ./internal/controller ./internal/probe ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过
```

## 2026-06-29 Phase 6 Job lifecycle 补齐

### 本轮已完成

- Recovery Job 生命周期：
  - controller 现在会按目标 member/attempt 创建稳定命名 Job：`<cluster>-recover-<ordinal>-<attempt>`。
  - Job 带 ownerRef、稳定 labels、target/attempt/generation/preflight annotations，日志会保留到 Job Pod 中。
  - Job `backoffLimit=0`，`ttlSecondsAfterFinished=86400`。
  - Running 状态下优先观察已有 Job，不重复删除 Pod/PVC，也不重复创建 Job。
  - Complete -> `status.recovery.lastResult=Succeeded`，`SingleMemberRecoveryActive=False`。
  - Failed -> `status.recovery.lastResult=Failed`；未到 `maxRetries` 时允许下一轮创建新 attempt；到达 `maxRetries` 后不再新建 attempt。
  - timeout 到期 -> `status.recovery.lastResult=Failed`，不再在同一 timeout Job 上重复删除 Pod/PVC。
- 二次确认 / 幂等：
  - destructive 动作前再次调用同一 preflight，并校验 target member/pod/pvc 未变化。
  - Job 创建成功后才执行精确删除目标 Pod/PVC。
  - 已有 Running Job 时只观察 Job 状态，不重复 destructive delete。
- `maxRetries` / `timeout`：
  - `maxRetries` 基于 Job attempt lifecycle 计数。
  - `spec.recovery.timeout` 会让 Running Job 标记 Failed 并停止对该 Job 重复执行 destructive 动作。
- 测试补充：
  - Job 创建并删除目标 Pod/PVC。
  - Running Job 不重复 delete。
  - Complete -> Succeeded。
  - Failed -> Failed 并可创建下一 attempt。
  - timeout -> Failed 且不重复 delete。
  - maxRetries -> Blocked。
  - 保留原有 disabled/quorum lost/multi unhealthy/reset-member 测试。

### 本轮验证结果

```sh
make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet && \
  go test ./internal/controller -run 'TestRecovery|TestCreateOrPatchStatefulSetWithReset' -v && \
  go test ./cmd ./internal/controller ./internal/probe ./internal/etcdutils ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过
```

### 仍需真实环境验证 / 风险

- 当前 Job 是日志/attempt/ownerRef/状态载体；真正的 destructive delete 仍由 controller 在 Job 创建后执行。这样满足幂等和生命周期观察，但不是“Job 内部执行删除”。如需严格 Job 内 destructive 动作，需要给 Job serviceAccount 授权并把 k8s/etcd preflight 与 delete 逻辑放进 Job 镜像。
- Job 内没有访问 apiserver/etcd 做第三次 preflight；当前等效保证是 controller 创建 Job 前和 destructive delete 前连续执行同一 preflight，并把 generation/target/attempt marker 写入 Job annotation。
- `NOSPACE` 仍是基于 health/status error 文本的保守识别，后续应显式执行 etcd `AlarmList`。
- reset-member 仍需验证实际 etcd 镜像包含 `/bin/sh` 和 `etcdctl`。

## 2026-06-29 Phase 7 DataStore reconciler 开发进度

### 本轮已完成

- Phase 7 ACP/Kamaji DataStore 发布：
  - annotation gate：仅当 `metadata.annotations["hcp.alauda.io/datastore"]` 非空时启用。
  - EtcdCluster Ready 后创建/更新 cluster-scoped `DataStore`。
  - 使用 `unstructured.Unstructured` + controller-runtime client，未引入 Kamaji typed API，避免无 DataStore Go 类型/CRD 时 manager 启动失败。
  - DataStore GVK 为 `kamaji.clastix.io/v1alpha1, Kind=DataStore`，已按 `cluster-api-control-plane-provider-kamaji/chart/charts/kamaji/templates/crds/kamaji.clastix.io_datastores.yaml` 的真实 schema 对齐；CRD `scope: Cluster`，因此 DataStore 不设置 namespace。
  - DataStore endpoint：`<cluster>-client.<namespace>.svc:2379`。
  - TLS 按既有 etcd-operator 部署文档的 cert-manager CA Issuer 模型解析（参考 <https://docs.alauda.io/hosted-control-plane/1.0/how_to/deploy-etcd-cluster.html>）：
    - `EtcdCluster.spec.tls.providerCfg.certManagerCfg.issuerKind=Issuer` / `issuerName=<CA_ISSUER_NAME>`；
    - operator 读取 namespaced `Issuer.spec.ca.secretName` 定位 CA Secret（`tls.crt` / `tls.key`），填入 `spec.tlsConfig.certificateAuthority.{certificate,privateKey}.secretReference`；
    - `spec.tlsConfig.clientCertificate.{certificate,privateKey}.secretReference` 指向 operator 生成的 `<cluster>-client-tls`（`tls.crt` / `tls.key`）。
    - 首版 DataStore 自动发布只支持 namespaced cert-manager CA Issuer；`ClusterIssuer` / Vault / ACME / external issuer 暂不自动定位 CA private key。
  - 无 annotation 时安全 no-op，并清理历史残留的 `DataStoreReady` condition，确保该 condition 仅在 DataStore 功能启用时维护。
  - 有 annotation 但 EtcdCluster 未 Ready 时不创建 DataStore，设置 `DataStoreReady=False, Reason=EtcdClusterNotReady`。
  - DataStore CRD 不存在时捕获 NoKindMatch，安全 no-op，设置 `DataStoreReady=False, Reason=DataStoreCRDNotFound`。
  - 创建/更新成功后设置 `DataStoreReady=True, Reason=DataStoreReady`。
  - RBAC 已增加 `kamaji.clastix.io/datastores` get/list/watch/create/update/patch。

### 本轮测试

- 新增 DataStore 聚焦测试：
  - 无 annotation 不创建，并在 annotation 被移除后清理历史 `DataStoreReady` condition。
  - 非 Ready 不创建并写 `DataStoreReady=False`。
  - 有 annotation + Ready 时创建 DataStore。
  - 已存在 DataStore 时更新 spec。
  - 无 DataStore CRD/NoKindMatch 时不失败并写 condition。
  - endpoint 与 TLS client secret 字段正确。

### 本轮验证结果

```sh
make -C /Users/mac/go/workdir/src/github.com/alauda/etcd-operator manifests generate fmt vet && \
  go test ./internal/controller -run 'TestDataStore' -v && \
  go test ./cmd ./internal/controller ./internal/probe ./internal/etcdutils ./pkg/certificate/cert_manager ./api/v1alpha1
# 结果：通过
```

### 剩余风险 / 待确认

- DataStore schema/scope 已用目标 Kamaji CRD 验证：`scope: Cluster`，`tlsConfig` 使用 `secretReference.name/namespace/keyPath` 结构。
- DataStore 自动 CA 定位依赖 namespaced cert-manager CA Issuer；如果用户配置 `ClusterIssuer` / Vault / ACME / external issuer，当前会设置 `DataStoreReady=False, Reason=DataStoreCASecretUnavailable`，后续可按需要扩展显式 CA Secret 配置或 ClusterIssuer namespace 推断。
- 当前未 watch DataStore 自身，只在 EtcdCluster reconcile 时 upsert；如外部修改 DataStore，需等 EtcdCluster 下次 reconcile 修复。

## 2026-06-30 文档对齐审查后的待处理问题

本节记录本轮按 `docs/design/hcp-etcd-design.md` 与本文档反查实现后发现的剩余问题，供下个会话继续处理。

### 已在本轮修复

- `reset-member` initContainer 的 member list 解析错误已修复：改为使用 `etcdctl member list -w simple`，按逗号分隔匹配 member name 并取 member ID；`member remove` / `member add` 不再用 `|| true` 静默吞错。
- 已新增回归测试 `TestResetMemberInitContainerScriptParsesSimpleMemberList`，覆盖类似 OCP HyperShift reset-member 的 `member list -w simple` 输出格式。
- 已运行验证：

```sh
gofmt -w internal/controller/utils.go internal/controller/utils_test.go
go test ./internal/controller -run 'TestCreateOrPatchStatefulSetWithResetMemberInitContainer|TestResetMemberInitContainerScriptParsesSimpleMemberList' -v
go test ./internal/controller
```

### P0：recovery destructive path 必须按文档收敛到 AlarmList CORRUPT

当前实现把 endpoint health failed、endpoint status nil、member health missing、Pod CrashLoopBackOff 等信号合并为 `unhealthy`，在 quorum 可用且只有 1 个 unhealthy 时可能进入自动删除 Pod/PVC。该行为偏离 `hcp-etcd-design.md` §10.2 的分层语义。

下个会话应修为：

- 增加真实 etcd `AlarmList` 能力，不再只靠 `EpHealth.Error` / `Status.Errors` 文本猜测 alarm。
- 本期 destructive recovery 只支持单 member `AlarmType_CORRUPT`：
  - quorum 可用；
  - 恰好 1 个目标 member；
  - 无 scale / rolling update / learner / recovery active；
  - 超过 `spec.recovery.gracePeriod` 后才允许删除目标 Pod/PVC。
- `AlarmType_NOSPACE` 必须 `Blocked` / 告警，不自动删除 Pod/PVC；该场景要等 compact / defrag / disarm 能力落地后再支持。
- 普通 endpoint health failed / missing 只能用于 status、quorum、degraded 观测，不得作为 destructive recovery 的直接准入。
- 需要补测试：
  - endpoint health failed 不触发 destructive recovery；
  - member health missing 不触发 destructive recovery；
  - CrashLoop 但没有 CORRUPT alarm 不触发 destructive recovery（或只 Pending/Blocked，按最终策略）；
  - 单 member CORRUPT + quorum 可用 + 超过 gracePeriod 触发 recovery；
  - NOSPACE blocked；
  - 多 member alarm blocked；
  - quorum lost blocked。

### P0：recovery Job lifecycle 仍不符合设计

当前 recovery Job 只是 echo 记录，不访问 etcd / apiserver 做二次 preflight，也不执行真正恢复逻辑；Job Complete 后 controller 直接把 `status.recovery.lastResult` 标为 `Succeeded`。这不满足 `hcp-etcd-design.md` §10.2 对 Job 确认、恢复与验收的要求。

下个会话应二选一明确实现方向：

1. 严格按设计：把 preflight / destructive action / 等待验收放入 Job，controller 只观察 Job Complete/Failed 并同步 status。
2. 保留 controller 执行删除：则 Job 只能作为 marker / 日志载体，不能用 Job Complete 代表 recovery success；controller 必须在观察到目标 Pod Ready、member count 恢复到 `spec.size`、endpoint health 3/3、无 learner 后才标记 `Succeeded`。

同时修复幂等问题：

- create Job 与 delete Pod/PVC 的顺序当前不是可恢复事务；Job 创建后 controller 崩溃或删除失败，会导致后续 reconcile 只观察旧 Job 而不再执行删除。
- 同一 member 的旧 completed Job 会在 TTL 内被后续故障复用，阻止新的恢复 attempt。
- 需要为一次故障引入 recovery id / target pod UID / PVC UID / firstObservedAt / generation 等标识，active Job 不能仅按 target member 匹配。

### P1：version guard 首次创建路径未覆盖 invalid version

当前 `isVersionUpgradeAllowed` 只在 StatefulSet 已存在时执行。首次创建 `EtcdCluster` 时，非法 `spec.version` 会直接进入 StatefulSet image tag，而不是被 `EtcdVersionUpgradeBlocked` 阻止。

下个会话应修为：

- bootstrap 前始终校验 desired `spec.version` 格式；
- `current == desired` 也不能跳过 desired version 格式校验；
- invalid version blocked 时不创建/patch 非法 image，并写 condition / event。

### P1：PDB 字段与设计要求不一致

设计要求 3 member 时 `maxUnavailable: 1` + `unhealthyPodEvictionPolicy: AlwaysAllow`。当前实现使用 `minAvailable = size/2 + 1`，对 size=3 语义等价，但 manifest 字段与文档验收项不一致；size=1 时还会阻止 voluntary eviction。

下个会话应二选一：

- 按文档改为 `maxUnavailable: 1`（至少 HA size>=3 时）；或
- 修正文档，明确采用 `minAvailable=floor(size/2)+1`，并说明 size=1 / 非 HA 行为。

### P1：已有 StatefulSet 的 podManagementPolicy 迁移未处理

新建 StatefulSet 已设置 `podManagementPolicy: Parallel`，但更新已有 StatefulSet 时只 patch `replicas` 和 `template`，旧集群可能继续保留默认 `OrderedReady`。

下个会话应确认 Kubernetes 当前版本下该字段是否可变：

- 如果可变，patch 已有 StatefulSet 的 `podManagementPolicy`；
- 如果不可变，增加 condition / event / 文档迁移步骤，提示需要安全重建 StatefulSet（保留 PVC / Pod 数据）。

### P1：probe image 默认值发布风险

`config/manager/manager.yaml` 当前默认 `--probe-image=controller:latest`。发布时 kustomize image transformer 通常只替换 manager container image，不会替换 args 字符串，可能导致 Etcd Pod 注入无法拉取的 `controller:latest` probe sidecar。

下个会话应修为：

- 默认不传 `--probe-image` 时使用 manager 同镜像；或
- 通过 env / kustomize replacement / 发布模板显式把 manager image 同步到 probe image；
- 补 manifest / unit 测试防止回归。

### P2：status condition reason 保留问题

recovery blocked/failed 时设置的 `SingleMemberRecoveryActive=False` 具体 reason（如 `QuorumUnavailable`、`MultipleUnhealthyMembers`、`NoSpaceAlarm`、`MaxRetriesExceeded`）会在 deferred `updateStatus` 中被 `NoRecoveryActive` 覆盖，因为当前只保留 `Status=True` 的 recovery condition。

下个会话应修为：

- 如果本轮 reconcile 直接设置了 `SingleMemberRecoveryActive`，无论 True/False 都应保留其 reason/message；或
- 将 blocked/failed reason 放入 `status.recovery.message` 并保证 condition 不覆盖排障信息。

### 文档本身需要澄清的问题

- `hcp-etcd-design.md` §8.6 对 cross-minor upgrade 写成“可选加 guard”，而本文 Phase 5 要求 cross-minor blocked。建议统一为：本期 operator 必须阻止 cross-minor，升级流程需逐 minor 递进。
- `hcp-etcd-design.md` §8.3 写 `:9980` “HTTPS 健康 server”，但当前实现与实施计划是 kubelet HTTP probe，TLS 只用于 probe sidecar 访问 etcd。建议澄清为：`:9980` probe server 是 HTTP；sidecar 到 etcd 使用 client TLS。
- DataStore 的 `tlsConfig` 仍是占位结构，缺少目标 Kamaji DataStore CRD 的真实 schema、scope 和完整 YAML。集成真实 CRD 前，该部分只能作为假设实现。
