# OCP HCP 容灾调研

> 调研日期：2026-06-11 ｜ 范围：OpenShift Hosted Control Planes（HCP / HyperShift）的容灾机制，覆盖 Red Hat 官方与 HyperShift 上游文档。

## 目的

搞清楚 HCP 集群的容灾到底怎么做，回答四个问题：

1. 是定时自动备份，还是只有手动 runbook？
2. 控制平面 etcd 怎么备份和恢复？OADP / Velero 起什么作用？
3. etcd 或控制平面升级前，会不会自动先备份一次？
4. 备份存到哪里？

以及两个实际会碰到的问题：管很多 hosted cluster 时能不能批量备份；OADP 能不能备份并恢复 etcd 的 PV。

## 结论

**HCP 的容灾以手动 runbook 为主，平台没有内置的定时自动备份。** 有两条路：

- **手动 etcd 快照**（`etcdctl snapshot save`）——一致性强，适合单集群应急或迁移。
- **OADP / Velero**——OCP 4.17+ 起的官方 DR 路径，能批量、能叠加定时。

逐问回答：

| 问题 | 结论 |
|------|------|
| 1. 定时 vs 手册 | **手册为主**。无 HCP 原生定时；可用 OADP 的 `Schedule` CR（cron）自己加。4.13 仅手动，4.17+ 引入 OADP DR，4.19 有"自动化 OADP DR"页但仍靠手动触发，不是 cron。 |
| 2. etcd 机制 | etcd 是 3 副本 statefulset，每成员独立 PVC。**单成员故障 Operator 自愈；多成员/quorum 丢失才需从快照手动恢复。** OADP 是结构化备份路径（DPA + Backup CR）。 |
| 3. 升级前自动备份 | **文档没有记录这个行为。** 想保险得自己先备。 |
| 4. 存到哪 | **S3 兼容对象存储**（S3 / Azure / GCP / MinIO）。手动快照先落本地再传 S3；OADP 强制要求对象存储。 |
| 批量备份 | **能**。一个 OADP `Backup`/`Schedule` CR 用 `includedNamespaces` 就能覆盖多个 hosted cluster。手动 `etcdctl` 才是一个个来。 |
| OADP 备/恢复 etcd PV | **能。** etcd PVC 随 namespace 进备份，恢复时重建。但卷拷贝不等于一致性快照。 |

**三个要记住的坑：**

- **没有定时备份意味着 RPO 不可控**——quorum 丢失时只能恢复到最后一次快照，之后的数据全丢，而且文档连数据丢失告警都没有。
- **恢复必须保留 etcd 的 AES 加密 secret**，否则有快照也解不开。
- **OADP 备的是 etcd 的 PV 卷数据，不是事务一致的逻辑快照**；要一致性时间点镜像（比如升级前）还得用 `etcdctl`。

## 细节

### 定时 vs 手册

默认都是管理员手动触发，没有自动调度。上游文档对手动 etcd 路径直说 *"a fully manual process and requires API downtime"*；RH 4.17 的 OADP 流程也是 *"backing up ... by creating the Backup custom resource"*。

要定时，得自己叠加 OADP 的通用 `Schedule` CR（cron），官方说它和 HCP 备份流程兼容——但这是 OADP 的通用能力，不是 HCP 专属功能。

版本上：4.13 只有手动 etcd 快照；4.17+ OADP 成为一等 DR 路径；4.19 出现"自动化 OADP DR"页，但仍由手动 apply 的 `Backup` CR 触发，DPA 只是自动化了 reconciliation 的暂停/恢复，**不是 cron 定时**。上游还有个 Tech Preview 的原生 etcd 快照备份（`HCPEtcdBackup` feature gate），以 Job 跑 `etcdctl`，尚未 GA。

### etcd 备份与恢复

etcd 是 3 副本 statefulset，每个成员一个 PVC。恢复分两种情况：

- **单成员故障且 quorum 没丢**（需 `HighlyAvailable` + `managementType=Managed`）：Operator 自动恢复，管理员只要删掉坏的 pod 和 PVC，自动重建，**不用快照**。
- **多成员丢数据或 crashloop（quorum 丢失）**：原文 *"must be restored from a snapshot"*——必须暂停 reconciliation、缩容 statefulset、从快照就地恢复，期间控制平面停机。

手动快照是核心备份原语：`oc exec` 进运行中的 etcd pod 跑 `etcdctl snapshot save`（首选，从持久盘拷贝是备选但可能丢 WAL）。文档对**备份频率/时机没有任何建议**。

OADP/Velero 是另一条结构化路径：配 `DataProtectionApplication`(DPA) 后在 `openshift-adp` 起 velero + node-agent，用 Kopia + File System Backup 做数据快照，`hypershift-oadp-plugin` 负责暂停/恢复 reconciliation 等 HCP 专属编排。备份内容是 etcd + HCP 组件 + hosted cluster PVC 数据。

### 升级前是否自动备份

所有已核实来源都没提到 HCP 在升级前自动备 etcd。这点和独立 OCP 不同（后者 cluster-etcd-operator 某些场景会自动快照）。**实务上升级前应自己先手动快照或跑一次 OADP Backup。**

### 备份存哪里

- **手动快照**：先落 etcd 容器本地 `/var/lib/data/snapshot.db`，再上传 S3（如 `s3://${BUCKET}/${CLUSTER}-snapshot.db`，上传是"推荐但可选"）；控制平面另存本地 manifest。恢复时从 S3 读，预签名 URL 写进 HostedCluster 的 `restoreSnapshotURL`。
- **OADP**：必须用在线对象存储（S3 / Azure / GCP / MinIO），位置配在 DPA 的 `backupLocations`，不能仅本地。

### 批量备份多个集群

手动 `etcdctl` 路径是一集群一套 etcd，只能一个个来，定位是单集群应急。

OADP 可以批量：一个 `Backup` CR 用 `includedNamespaces` 列多个 hosted cluster 的 namespace（控制平面默认 `clusters-<name>`），或用 label selector，再配 `Schedule` CR 就是一个定时任务覆盖整队列。

注意：这套能力要自己组装，平台不替你建；`includedNamespaces` 是静态列表，新集群不会自动纳入，实务上用 GitOps（ArgoCD/Kustomize）模板化管理，做到"加集群即加备份"。

### OADP 能否备份并恢复 etcd PV

**能。** OADP Backup 的 `includedResources` 含 `pvc`、`pv`，etcd 的 PVC 随 HCP namespace 一起备份——CSI 存储走卷快照 + datamover，非 CSI 走 Kopia 文件级拷贝。恢复时 Velero 重建整个 namespace（含 etcd statefulset + 带数据的 PVC），这就是 4.17+ 的官方 HCP DR 路径。

两点要分清：

- **"OADP 不作为 etcd DR 方案"这句话针对的是独立 OCP 自己的 etcd**（那种应该用 cluster-etcd-operator），不是 HCP 托管集群的 etcd。对 HCP，OADP 恰恰是官方 etcd DR 路径，别混。
- **备 PV 不等于一致性逻辑快照**：对正在写入的 etcd 做文件级卷拷贝，理论上可能抓到不一致状态，这也是 `etcdctl snapshot save` 被列为首选的原因。HCP OADP 流程靠插件暂停 reconciliation 降低风险，但文档没写明备份前是否 scale-down/quiesce etcd。

**实务建议**：用 OADP `Schedule` + GitOps 做整队列定时 DR，关键变更（如升级）前再补一次手动 `etcdctl` 快照，两者并用。

## 附录：核实说明与引用

调研用多源扇出搜索 → 抓取 → 对抗式核实（每条 3 票，2/3 反驳才否决）→ 综合，共抓 15 源、提取 63 条声明、确认 19 条。注意：多个 `docs.redhat.com` 链接对自动抓取返回 403，相关结论靠精确短语搜索 + 上游镜像逐字印证，非每次直读 RH 实时页。"无定时/无升级前备份"是否定式结论（页面未提及），强度以此理解。

关键来源：

- RH 4.13 HCP 备份/恢复/DR — `docs.redhat.com/.../4.13/.../hcp-backup-restore-dr`
- RH 4.17 HA for HCP（OADP DR） — `docs.redhat.com/.../4.17/.../high-availability-for-hosted-control-planes`
- OCP 4.17 OADP DR 步骤 — `docs.openshift.com/.../4.17/.../hcp-disaster-recovery-oadp.html`
- 上游手动 etcd 快照 runbook — `hypershift.pages.dev/how-to/aws/etc-backup-restore/`
- 上游 etcd recovery — `hypershift.pages.dev/how-to/disaster-recovery/etcd-recovery/`
- 上游 OADP DR（含 includedResources/includedNamespaces） — `hypershift.pages.dev/how-to/disaster-recovery/backup-and-restore-oadp/` 与 `hypershift-docs.netlify.app/.../backup-and-restore-oadp-1-5/`
- OKD 4.19 自动化 OADP DR — `docs.okd.io/4.19/.../hcp-disaster-recovery-oadp-auto.html`
- 上游 Tech Preview Etcd Snapshot Backup（`HCPEtcdBackup`） — `hypershift.pages.dev/how-to/disaster-recovery/etcd-snapshot-backup/`

待确认：升级前是否自动备份、Tech Preview etcd 备份的 GA 与频率、`hypershift-oadp-plugin` 备 etcd 时是否做一致性 quiesce。
