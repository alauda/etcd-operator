# HCP Etcd Operator 回归测试用例

本文基于以下设计文档整理，用于后续回归测试、发布前验证和手工演练：

- `docs/design/hcp-etcd-operator-implementation-plan.md`
- `docs/design/hcp-etcd-design.md`

本用例集通过两个视角交叉校验形成：

- **需求 / 设计验收视角**：提取功能预期、验收标准、可观察信号和风险边界。
- **QA / 回归测试视角**：将设计目标转换为可执行的 P0/P1/P2 分层测试用例。

> 说明：本文是设计预期与回归用例清单，不代表已在真实 Kubernetes 集群完成执行。涉及 CAPI、Baremetal、TopoLVM、Velero、Kamaji DataStore 的场景需要在对应环境中验证。

## 1. 功能交叉验证摘要

| 功能域 | 设计预期是否清晰 | 回归测试可行性 | 关键验收点 | 交叉验证结论 |
|---|---:|---:|---|---|
| EtcdCluster CRD/API 扩展 | 清晰 | 高 | `podTemplate` 调度字段、`recovery` 字段、status 字段、CRD schema、deepcopy、docs 一致 | 应纳入 CI 生成物一致性测试 |
| StatefulSet 渲染 | 清晰 | 高 | `podManagementPolicy=Parallel`，调度字段透传，priorityClass 正确 | P0，防止 learner NotReady 阻塞 OrderedReady |
| HCP 专用节点调度 | 清晰 | 中高 | Pod 只调度到 HCP worker 节点，不落 master/control-plane | 依赖真实节点 label，建议 e2e |
| Pod 分散 / anti-affinity / topology spread | 清晰但依赖环境 | 中 | 一节点一 member，zone spread 不导致 2 zone/3 member Pending | hostname anti-affinity 是核心，zone spread 用例需按环境配置 |
| Peer Service | 清晰 | 高 | `<name>` headless，`publishNotReadyAddresses=true` | P0，服务发现路径 |
| Client Service | 清晰 | 高 | `<name>-client` headless，`publishNotReadyAddresses=false`，只暴露 Ready Pod | P0，客户端/DataStore 入口 |
| PDB | 清晰 | 高 | 3 member 时 `maxUnavailable=1`，`unhealthyPodEvictionPolicy=AlwaysAllow` | P0，节点 drain 保护 quorum |
| CAPI node drain | 清晰但依赖环境 | 中 | `nodeDrainTimeout=0/空`，drain 受 PDB 保护，不 force delete | 真实集成/发布前验证 |
| probe `/healthz` / `/readyz` | 清晰 | 高 | `readyz` 本地 serializable health，且 member 非 learner；不依赖 quorum | P0，防止故障放大 |
| TLS probe | 清晰 | 高 | 挂载 `<cluster>-client-tls`，CA/cert/key 参数正确 | P0，证书路径关键 |
| learner 行为 | 清晰 | 高 | promote 前 NotReady，不进入 client Service；promote 后 Ready | P0 |
| status/conditions | 清晰 | 高 | phase、readyReplicas、memberCount、leaderID、members、recovery、conditions、observedGeneration | P0/P1 |
| 版本升级 guard | 基本清晰 | 高 | 允许 patch/相邻 minor；禁止 downgrade、跨 minor/major、非法版本 | 个别口径需以当前实现为准 |
| 滚动升级 | 清晰 | 中高 | 一次只影响一个 member，readyz 把关，不丢 quorum | slow e2e |
| 扩容 | 清晰 | 高 | learner 串行 add/promote，最终 Ready | P0 |
| 缩容 | 较清晰 | 中 | member remove 与 StatefulSet/PVC 行为一致 | 需按实现确认 PVC 保留/删除策略 |
| 单 member recovery | 清晰 | 中高 | quorum 可用、恰好一个异常、超过 gracePeriod 后恢复 | P0 slow e2e |
| recovery destructive action | 清晰且高风险 | 高 | 只删除目标 ordinal 的 Pod/PVC，重复 reconcile 幂等 | P0，必须重点回归 |
| 多 member 异常 / quorum lost | 清晰 | 高 | 不自动恢复、不删 PVC，只告警/Blocked | P0 |
| NOSPACE | 清晰 | 中 | 不触发自动 recovery，提示 compact/defrag/disarm | P1 |
| CORRUPT/db 损坏 | 清晰但注入复杂 | 中 | 单 member 且 quorum 可用时可进入 recovery | P1/手工 |
| recovery Job lifecycle | 清晰但实现位置需确认 | 高 | stable Job name、attempt、timeout、maxRetries、状态写回 | 测试应验证外部行为，不绑定“删除动作在 controller 还是 Job 内” |
| DataStore annotation gate | 清晰 | 高 | 无 annotation 不创建，有 annotation 且 Ready 后创建 | P1 |
| DataStore CRD 缺失 | 清晰 | 高 | operator 不崩溃，condition 标记 `DataStoreCRDNotFound` | P1 |
| DataStore spec | 有假设 | 中 | endpoint 指向 `<name>-client.<ns>.svc:2379`，TLS 指向 client Secret | 需与真实 Kamaji DataStore schema 对齐 |
| 备份 / DR | 边界清晰 | 中/手工 | quorum lost 走 Velero + snapshot 手动恢复 | 非本期自动化恢复能力 |
| TLS 自动轮换 | 明确 Non-Goal | 低 | 不应把自动无损轮换作为验收标准 | 只测异常可诊断 |
| 自动 defrag / 自动备份 | 明确 Non-Goal | 低 | 不应要求 operator 自动完成 | 可作为 runbook/后续增强 |

## 2. P0 回归测试用例

P0 用例建议作为 PR / 发布前必须覆盖的核心回归，重点保护 quorum、安全升级、破坏性 recovery 动作和安装生成物一致性。

| 用例ID | 功能 | 测试目标 | 前置条件 | 测试步骤 | 期望结果 | 自动化建议 |
|---|---|---|---|---|---|---|
| HCP-P0-001 | 基础创建 | 验证 3 member EtcdCluster 可成功创建 | operator 已安装；StorageClass 可用；HCP 节点可调度 | 创建 `size: 3` EtcdCluster；等待 Pod/PVC/Service/PDB/STS 创建；查询 status | 3 Pod Ready；`phase=Ready`；`readyReplicas=3`；`memberCount=3`；`QuorumAvailable=True` | e2e |
| HCP-P0-002 | StatefulSet | 验证 STS 使用 Parallel 策略 | EtcdCluster 已创建 | 查询 StatefulSet `spec.podManagementPolicy` | 值为 `Parallel` | 单测 + e2e |
| HCP-P0-003 | Peer Service | 验证 peer discovery Service 正确 | EtcdCluster 已创建 | 查询 `<name>` Service | headless；`clusterIP=None`；`publishNotReadyAddresses=true` | 单测 + e2e |
| HCP-P0-004 | Client Service | 验证 client Service 只暴露 Ready Pod | EtcdCluster 已创建 | 查询 `<name>-client` Service 和 EndpointSlice | headless；2379 端口；`publishNotReadyAddresses=false`；endpoint 只包含 Ready Pod | 单测 + e2e |
| HCP-P0-005 | PDB | 验证 quorum 保护 | `size=3` 集群 Ready | 查询 PDB spec | `maxUnavailable=1`；selector 匹配 etcd Pod；`unhealthyPodEvictionPolicy=AlwaysAllow` | 单测 + e2e |
| HCP-P0-006 | PDB 驱逐保护 | 验证不能同时驱逐两个 member | 3 Pod Ready | eviction 第 1 个 Pod；其恢复前 eviction 第 2 个 Pod | 第二次 eviction 被 PDB 拒绝；quorum 保持可用 | e2e |
| HCP-P0-007 | HCP 节点调度 | 验证 Pod 只落 HCP worker | 节点带 `cpaas.io/hcp-management-node=true` | 创建带 nodeSelector 的 EtcdCluster；查询 Pod nodeName 和 node label | 所有 Pod 均落到 HCP worker，不落 master/control-plane | e2e |
| HCP-P0-008 | readyz 本地健康 | 验证 readyz 不依赖 quorum/linearizable check | 3 member Ready | 使 1 个非目标 member 异常；检查另外 2 个 Pod readiness | 另外 2 个健康 member 仍 Ready | slow e2e |
| HCP-P0-009 | learner readiness | 验证 learner promote 前不 Ready | 创建或扩容过程中存在 learner | 查询 Pod Ready 状态和 client endpoints；待 promote 后再次查询 | promote 前 NotReady 且不进 client endpoints；promote 后 Ready | e2e |
| HCP-P0-010 | TLS probe | 验证 probe 使用 client TLS Secret | TLS provider 可用 | 创建 TLS EtcdCluster；查询 StatefulSet volumes、volumeMounts、probe args | sidecar 挂载 `<cluster>-client-tls`；CA/cert/key 参数正确；readyz 成功 | 单测 + e2e |
| HCP-P0-011 | status 基础字段 | 验证 status 写回完整 | EtcdCluster Ready | 查询 EtcdCluster YAML；对比 etcdctl/member/pod 状态 | `phase`、`readyReplicas`、`memberCount`、`leaderID`、`members[]`、`observedGeneration` 正确 | e2e |
| HCP-P0-012 | conditions 正常态 | 验证核心 conditions | EtcdCluster Ready | 查询 `.status.conditions` | `EtcdClusterCreated=True`；`EtcdClusterReady=True`；`QuorumAvailable=True`；`SingleMemberRecoveryActive=False` | e2e |
| HCP-P0-013 | patch 版本升级 | 验证允许 patch upgrade | 当前 etcd 版本和目标 patch 镜像可用 | 修改 `spec.version` 到 patch 新版本；观察滚动升级 | 镜像更新；滚动完成；最终 Ready；quorum 持续可用 | e2e |
| HCP-P0-014 | 禁止 downgrade | 验证降级被阻止 | EtcdCluster Ready | 修改 `spec.version` 为低版本 | StatefulSet image 不变；condition/event 表示 upgrade blocked | 单测 + e2e |
| HCP-P0-015 | 禁止跨 major/minor 非法升级 | 验证 unsupported 版本被阻止 | EtcdCluster Ready | 修改为跨 major、跨不支持 minor 或非法字符串 | 不更新 StatefulSet；condition/event 说明原因 | 单测 + e2e |
| HCP-P0-016 | 升级中禁止 recovery | 防止滚动升级与 recovery 冲突 | `recovery.enabled=true` | 发起版本升级；升级过程中观察 recovery Job/PVC 删除 | 不创建 recovery Job；不删除 PVC/Pod | slow e2e |
| HCP-P0-017 | 扩容 1→3 | 验证 learner 串行扩容 | 初始 `size=1` Ready | 修改 `spec.size=3`；观察 member add/promote | 每次只处理一个 learner；最终 3 member Ready | e2e |
| HCP-P0-018 | scale 与 recovery 互斥 | 防止扩缩容中误恢复 | recovery enabled；scale 进行中 | scale 过程中制造短暂 Pod 异常 | 不创建 recovery Job；不删除 PVC | slow e2e |
| HCP-P0-019 | 单 member recovery 成功 | 验证单成员故障自动恢复主路径 | 3 member Ready；`recovery.enabled=true`；quorum 可用 | 使 1 个 member 数据损坏或 CrashLoop；等待超过 `gracePeriod` | 创建 recovery attempt；只恢复目标 member；最终 3 member Ready；`lastResult=Succeeded` | slow e2e |
| HCP-P0-020 | recovery 精确删除 | 验证破坏性动作只作用目标 ordinal | 触发单 member recovery | 记录 Pod/PVC delete events | 只删除 `<cluster>-<ordinal>` Pod 和 `etcd-data-<cluster>-<ordinal>` PVC；其他 ordinal 不受影响 | 单测 + e2e |
| HCP-P0-021 | recovery 幂等 | 验证重复 reconcile 不重复破坏 | recovery Job Running | 强制多次 reconcile；观察 Job 和 delete events | 不重复创建同 attempt Job；不重复删除 Pod/PVC | 单测 + e2e |
| HCP-P0-022 | 多 member 异常不恢复 | 防止自动恢复扩大故障 | 3 member 中 2 个异常 | 等待超过 gracePeriod；查询 Job/PVC/status | 不创建 recovery Job；不删 PVC；recovery Blocked 或 `QuorumAvailable=False` | 单测 + slow e2e |
| HCP-P0-023 | quorum lost 不恢复 | 验证 quorum 丢失进入人工 DR 路径 | 3 member 中 2 个不可用 | 观察 operator 行为 | 只告警/Blocked；不执行 remove/add；不删 PVC | 单测 + slow e2e |
| HCP-P0-024 | manifests 一致性 | 验证 CRD/RBAC/generated docs 不漂移 | 本地开发环境 | 执行 `make manifests generate`；检查 diff | 无非预期 diff；CRD/RBAC 包含新增字段/权限 | CI |
| HCP-P0-025 | 单元测试 | 验证核心逻辑不回退 | 本地/CI | 执行 controller、probe、etcdutils、api 相关 go test | 测试全部通过 | CI |
| HCP-P0-026 | RBAC | 验证 operator 权限完整 | operator SA 已安装 | `kubectl auth can-i` 检查 PDB、Job、Pod delete、PVC delete、DataStore 等权限 | 无 forbidden；recovery/job/service/pdb 操作权限完整 | 脚本化 |

## 3. P1 回归测试用例

P1 用例建议纳入每日 e2e 或发布前 slow e2e，覆盖更多异常路径、集成场景和可观测性。

| 用例ID | 功能 | 测试目标 | 前置条件 | 测试步骤 | 期望结果 | 自动化建议 |
|---|---|---|---|---|---|---|
| HCP-P1-001 | 调度字段透传 | 验证 nodeSelector/tolerations/affinity/topologySpread/priorityClass 下发 | EtcdCluster 配置完整 podTemplate | 查询 StatefulSet PodTemplate | 字段与 CR spec 一致；默认 priorityClass 符合设计 | 单测 + e2e |
| HCP-P1-002 | hostname anti-affinity | 验证 member 分布在不同节点 | 至少 3 个 HCP 节点 | 创建集群并查询 Pod nodeName | 3 个 member 分布到不同节点 | e2e |
| HCP-P1-003 | zone spread | 验证 2 zone/3 member 不 Pending | 2 zone 环境 | 配置 `ScheduleAnyway` zone spread；创建集群 | Pod 可调度，尽量跨 zone | e2e |
| HCP-P1-004 | startup probe | 验证慢启动不被误杀 | 可模拟慢启动 | 创建集群并观察重启次数 | startupProbe 给足窗口，无重启风暴 | e2e |
| HCP-P1-005 | TLS Secret 缺失 | 验证证书缺失可诊断且不误删 | TLS 集群 | 删除/阻断 client Secret | Pod 不误 Ready；status/event/log 可诊断；不触发 recovery 删除 PVC | e2e |
| HCP-P1-006 | TLS 证书错误 | 验证错误证书失败可恢复 | TLS 集群 | 替换为错误 CA/cert；再恢复正确 Secret | readyz/health 失败；恢复 Secret 后集群恢复 | 手工/e2e |
| HCP-P1-007 | condition semantic diff | 验证无 hot loop | 稳定 Ready 集群 | 不修改 spec，持续观察 status resourceVersion/reconcile 日志 | 无无意义 status patch | 单测 + e2e |
| HCP-P1-008 | observedGeneration | 验证 spec 更新后 status generation 推进 | EtcdCluster Ready | 修改非破坏性 spec；等待 reconcile | `observedGeneration == metadata.generation` | e2e |
| HCP-P1-009 | 相邻 minor 升级 | 验证允许相邻 minor upgrade | 多版本镜像可用 | 修改 `spec.version` 到相邻 minor | 滚动升级成功，最终 Ready | slow e2e |
| HCP-P1-010 | 节点滚动升级 | 验证 HCP managed 节点升级不丢 quorum | CAPI 环境，`nodeDrainTimeout=0` | 触发节点滚动/替换 | 任意时刻最多 1 member 不可用；quorum 持续可用 | 集成/slow e2e |
| HCP-P1-011 | drain 卡住保护 | 验证已有 NotReady 时不会 force delete 第二个 member | 1 member NotReady | 对另一个 member 所在节点 drain | PDB `ALLOWED DISRUPTIONS=0`；drain 卡住而非绕过 PDB | 集成/手工 |
| HCP-P1-012 | 缩容 3→1 | 验证成员安全移除 | 3 member Ready | 修改 `spec.size=1`；观察 member list/status | 多余 member 从 membership 移除；剩余集群 Ready | e2e |
| HCP-P1-013 | recovery disabled | 验证关闭自动恢复 | `recovery.enabled=false` | 制造单 member 异常并等待 | 不创建 Job；不删 PVC/Pod；只更新状态/告警 | 单测 + e2e |
| HCP-P1-014 | gracePeriod 去抖 | 验证短暂重启不触发恢复 | recovery enabled | 短暂重启一个 Pod，并在 gracePeriod 内恢复 | 不创建 recovery Job；不删除 PVC | e2e |
| HCP-P1-015 | NOSPACE | 验证 NOSPACE 不触发恢复 | 可模拟 NOSPACE alarm | 触发 NOSPACE；观察 status/recovery | recovery Blocked/告警；不删 PVC | 手工/e2e |
| HCP-P1-016 | CORRUPT/db 损坏 | 验证单 member 损坏可恢复 | 可注入单成员 db 损坏 | 注入 CORRUPT/db load failure | 满足 preflight 后执行 recovery，最终 Ready | 手工/slow e2e |
| HCP-P1-017 | recovery timeout | 验证超时状态 | 设置短 timeout | 触发 recovery 并阻塞完成 | 标记 Failed；不重复删除 | 单测 + e2e |
| HCP-P1-018 | recovery maxRetries | 验证最大重试后 Blocked | `maxRetries=1` | 让 recovery 持续失败 | 达最大次数后不再重试；状态 Blocked | 单测 + e2e |
| HCP-P1-019 | reset-member 空盘路径 | 验证空盘 remove/add | recovery 删除 PVC 后重建 | 查看 initContainer 日志和 member list | remove old member + add new peer；最终 Ready | slow e2e |
| HCP-P1-020 | reset-member 有数据 no-op | 验证正常重启不 remove/add | 数据盘已有 db | 重启 Pod，查看 reset-member 日志 | 检测到 db 存在，不执行 remove/add | e2e |
| HCP-P1-021 | DataStore 创建 | 验证 Ready 后发布 DataStore | DataStore CRD 已安装；CR 带 annotation | 创建 EtcdCluster 并等待 Ready | 创建 DataStore，endpoint 指向 client Service，TLS 指向 client Secret | 集成/e2e |
| HCP-P1-022 | DataStore NotReady gate | 验证 EtcdCluster 未 Ready 不创建 DataStore | 带 annotation，但集群 NotReady | 查询 DataStore 和 condition | 不创建 DataStore；`DataStoreReady=False`，reason 明确 | 单测 + e2e |
| HCP-P1-023 | DataStore CRD 缺失 | 验证无 CRD 时 operator 不崩溃 | 不安装 DataStore CRD | 带 annotation 创建 EtcdCluster | manager 正常；condition reason 为 `DataStoreCRDNotFound` | 单测 + e2e |
| HCP-P1-024 | etcdctl 观测一致性 | 验证 status 与 etcdctl 一致 | EtcdCluster Ready | 执行 endpoint health/status/member list/alarm list | leader、learner、healthy、alarm 与 status 一致 | 脚本化 |
| HCP-P1-025 | recovery 可观测性 | 验证状态、Job、日志可用于排障 | 触发 recovery | 查询 `status.recovery`、Job、Job Pod logs、reset-member logs | 可看到 target、attempt、lastResult、active condition 和日志 | e2e |

## 4. P2 回归测试用例

P2 用例主要用于手工演练、环境相关集成验证、后续增强能力和边界确认。

| 用例ID | 功能 | 测试目标 | 前置条件 | 测试步骤 | 期望结果 | 自动化建议 |
|---|---|---|---|---|---|---|
| HCP-P2-001 | DataStore annotation 移除 | 验证移除 annotation 后 condition 清理 | 曾启用 DataStore | 移除 `hcp.alauda.io/datastore` annotation | 不再维护或清理 `DataStoreReady` condition | 单测/e2e |
| HCP-P2-002 | DataStore 外部修改修复 | 验证 reconcile 可修复 spec drift | DataStore 已存在 | 手工修改 DataStore spec；触发 EtcdCluster reconcile | DataStore spec 被修复为期望值 | e2e |
| HCP-P2-003 | TLS 自动轮换边界 | 确认不把自动轮换当成本期能力 | TLS 集群 | 更新 Secret，不手动滚动 Pod | 不承诺自动无损轮换；行为可观测，不触发 recovery 误操作 | 手工 |
| HCP-P2-004 | Velero 资源备份 | 验证 DR 资源备份包含必需对象 | Velero 已安装 | 创建 Backup；检查包含 EtcdCluster、Secrets、PVC/PV、DataStore | 备份内容完整 | Runbook |
| HCP-P2-005 | etcd snapshot | 验证 snapshot 可生成和校验 | etcd client TLS 可用 | `etcdctl snapshot save`；校验 snapshot status | snapshot 成功且可校验 | 脚本/手工 |
| HCP-P2-006 | quorum lost DR | 验证人工 DR 顺序 | 有 Velero 备份和 snapshot | 破坏 quorum；先恢复资源和 TLS Secret；再恢复 etcd 数据 | 最终 EtcdCluster 恢复 Ready | 手工演练 |
| HCP-P2-007 | TLS Secret 缺失下 DR | 验证 DR 中缺 TLS Secret 可诊断 | 不完整恢复环境 | 只恢复 EtcdCluster，不恢复 Secret；再恢复 Secret | Secret 缺失时 Pod/readyz 失败且可诊断；恢复 Secret 后继续 | 手工 |
| HCP-P2-008 | TopoLVM/换机复用盘 | 验证换机后 member 带原数据 rejoin | Baremetal/TopoLVM 环境 | 替换节点并复用盘/IP/hostname | reset-member no-op；member 原数据 rejoin | 集成/手工 |
| HCP-P2-009 | reset-member 镜像依赖 | 验证 etcd 镜像包含 `/bin/sh` 和 `etcdctl` | 使用发布镜像 | 触发 initContainer 或 exec 检查 | shell 和 etcdctl 可用 | 镜像验收 |
| HCP-P2-010 | 事件文案 | 验证阻断场景 event 清晰 | 构造 version blocked/recovery blocked | 查询 Events | reason/message 明确，可用于排障 | 单测/e2e |
| HCP-P2-011 | 资源删除清理 | 验证删除 EtcdCluster 后资源清理策略 | EtcdCluster 已创建 | 删除 EtcdCluster；查询 STS/Service/PDB/Job/PVC/Secret/DataStore | 清理行为符合实现策略，无残留影响同名重建 | e2e |
| HCP-P2-012 | 同名重建 | 验证删除后可同名重建 | 旧资源已删除/清理 | 用同名创建 EtcdCluster | 新集群 Ready，无旧资源干扰 | e2e |

## 5. 建议回归分层

| 回归层级 | 建议覆盖 |
|---|---|
| PR 必跑 | CRD/RBAC 生成一致性、单元测试、Service/PDB/STS 渲染、版本 guard、status/condition 基础逻辑、recovery preflight/幂等/精确删除单测 |
| 每日 e2e | 基础创建、client Service、readyz、TLS probe、扩容、patch 升级、单 member recovery、DataStore gate |
| 发布前 slow e2e | 节点滚动升级、CAPI drain/PDB、相邻 minor 升级、CORRUPT/NOSPACE、recovery timeout/maxRetries、换机复用盘 |
| 手工/Runbook 演练 | Velero 资源恢复、etcd snapshot 恢复、quorum lost DR、TLS 异常恢复、真实 Baremetal/TopoLVM 换机 |

## 6. 非目标能力与测试边界

以下能力在设计中不是本期自动化验收目标，回归测试不应将其作为必须自动完成的通过条件。

| 能力 | 回归测试应如何处理 |
|---|---|
| 自动备份调度 | 只验证 runbook/snapshot/Velero 可执行，不要求 operator 自动备份 |
| quorum lost 自动恢复 | 明确验证“不自动恢复、不删 PVC”，应进入人工 DR |
| TLS 自动无损轮换 | 不作为通过标准；只验证异常可诊断、不误触发 recovery |
| 自动 defrag / NOSPACE 自动修复 | NOSPACE 应 Blocked/告警，不应自动重建 member |
| DataStore 通用强依赖 | 无 annotation 或无 CRD 时 operator 必须安全 no-op |
| Shared Nothing / Dedicated Request Serving | 属于更大 HCP 架构边界，不应作为 etcd operator 单独验收标准 |

## 7. 后续落地建议

1. 将 P0 中可单测的场景优先覆盖到 controller、probe、etcdutils 单元测试。
2. 将 P0 中依赖 Kubernetes API 但不依赖真实节点替换的场景纳入 envtest 或 Kind e2e。
3. 将节点滚动升级、Baremetal/TopoLVM 换机、Velero/snapshot DR 划为发布前 slow e2e 或手工演练。
4. 对 recovery 相关用例重点记录 Kubernetes delete event，确保只删除目标 ordinal 的 Pod/PVC。
5. 对 blocked 场景统一校验 condition reason、event reason 和 operator 日志，保证故障可诊断。
