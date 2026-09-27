# Laya 本地分类运行时设计

| 项目 | 内容 |
|---|---|
| 范围 | Laya 第三方 Python 决策模型的本地部署、运行、集成和运维 |
| 调用方 | Go 网关的 `LayaClient` |
| 运行方式 | 与 Go 网关同一主机/节点的私有 Python sidecar；多节点逐节点部署 |
| 外部依赖 | 首次准备权重时访问 Hugging Face；运行推理时不调用外部分类模型 |
| 关联 | `docs/product-requirements.md`、`docs/detailed-design.md`、`docs/design-openai-gateway.md` |

## 1. 目的与边界

Laya 指 [NandhaKishorM/laya](https://github.com/NandhaKishorM/laya) 项目提供的本地 typed-decision 分类运行时；它**不是**本项目 Go 网关的模型评分/排序引擎，也不是上游生成模型。

职责边界：

- **Laya 本地服务**：针对请求文本回答受约束的分类问题，返回分类标签及置信信息。
- **Go `RoutingService`**：将分类结果映射为版本化路由意图和需求，再执行硬约束过滤、评分快照排序、价格/可用性策略及最终选模。
- **评分刷新 worker**：后台调用已接入的供应商模型进行自评/互评；与 Laya 在线分类完全独立。
- **供应商模型**：只接收用户实际请求，不承担额外的分类器调用。

```text
客户端
  |
  v
Go Gateway -- 内网 HTTP --> 本节点 Laya sidecar
  |                             |
  |<-- typed choice/confidence -+
  |
  +--> Go RoutingService 使用已发布能力快照选择供应商模型
                                |
                                +--> Laya 故障/低置信度时使用通用路由
```

分类只辅助确定“请求大致属于什么任务”。Laya 不负责 API Key 鉴权、额度、权限、工具/模态硬约束、评分发布或高风险判断；这些仍由 Go 服务和确定性规则负责。Laya 输出不得直接指定供应商模型 ID、修改价格或绕过硬约束。

## 2. 上游组件与调用契约

上游 Laya 是 Python 包，要求 Python 3.10+；`laya-serve` 提供服务模式，仓库文档说明 HTTP 健康接口为 `GET /health`，Jev 兼容推理接口为 `POST /v1/systemone`。模型使用 `choice`、`score`、`noul` 等 typed-decision 类型。实现时使用专用 Go 适配器封装上游请求/响应，不让上游协议字段渗入网关公开 API。

### 2.1 Go 内部分类结果

Go 适配器将上游结果归一成内部结构：

```text
ClassificationResult
  intent: general | coding | reasoning | writing | translation |
          summarization | casual_chat | emotional_support
  confidence: 0..1
  classifier_version: pinned Laya 包/模型/问题集版本
  input_truncated: boolean
  duration_ms: integer
  fallback_reason: optional enum
```

Laya 只返回意图标签和置信信息。Go 侧持有版本化、可测试的映射表，将 `intent` 映射到路由能力阈值和排序偏好；该表不得由 Laya 动态生成。工具调用和图片等显式请求特征直接从 API 字段读取，优先于文本分类。高风险标签仍按 PRD 中的确定性规则产生，不由 Laya 单独判定。

### 2.2 分类输入

- 仅对 `model=auto` 且确实需要内容语义判断的请求调用 Laya；显式模型请求不调用。
- Go 从对话中构造独立的分类副本，优先保留最新用户消息和有限的必要上下文；剔除 API Key、认证头、供应商凭证、二进制附件及不参与分类的元数据。
- 为符合所选 checkpoint 上下文窗口，分类副本设可配置的最大长度；超长时优先保留最新用户消息并标记 `input_truncated`。原始请求不因分类而改写，仍按兼容契约转发给所选供应商。
- Laya 是本地推理，不向额外云端分类供应商发送用户文本。首次权重下载属于部署操作，权重准备完成后允许启用 Hugging Face 离线模式。
- Laya 和 Go 服务均不得默认记录分类原文；日志只记请求 ID、Laya 版本、耗时、标签、置信度、截断标志和 fallback 原因。

### 2.3 意图与路由映射

分类问题/选项必须与产品的路由意图维度版本化管理。初始分类集合覆盖代码、推理、写作、翻译、摘要、闲聊、情感支持及通用请求；实现阶段用代表性中文和多轮请求构造固定测试集。意图映射为能力约束/偏好后，Go 仍须按既定算法完成硬过滤与确定性评分。

- `confidence >= 0.60`：Go 可采用对应意图的版本化需求映射。
- `confidence < 0.60`、未知标签、响应校验失败或无结果：按 `general` 处理，不让分类结果成为排除候选模型的理由。
- Laya 不可用时使用同一 `general` 行为，并保持工具/模态、授权、上下文、可用性等确定性硬约束。
- 输出中的置信度不是正确率保证；发布前必须对固定的中文留出集测分类质量、各类别召回率、置信度校准和误分类案例。若未达到运营设定的验收门槛，关闭语义分类，仅保留结构规则和通用路由。

## 3. 部署拓扑与生命周期

### 3.1 拓扑

首版使用同一代码仓库、独立进程/容器部署：React 在 `web/`，Go API、Go Gateway、Go Worker 可分别运行；每个接收网关流量的节点配一个 Laya sidecar。Go Gateway 通过节点本地或仅容器私有网络访问 Laya，不经过公网或公共负载均衡器。

```text
Load Balancer
  +--> Gateway node A --> 127.0.0.1/private-network --> Laya sidecar A
  +--> Gateway node B --> 127.0.0.1/private-network --> Laya sidecar B
  `--> Gateway node N --> 127.0.0.1/private-network --> Laya sidecar N

所有节点使用相同的 Go 路由映射版本、Laya 包版本、checkpoint revision 和问题集版本。
```

逐节点部署避免新增跨节点分类网络依赖，并允许网关独立水平扩展。Laya 不保存账户、会话、路由快照或业务状态；节点间只需保持运行版本/配置一致。若将来改成共享 Laya 集群，须另行设计其负载均衡、隔离、容量与故障域，不作为首版默认拓扑。

### 3.2 安装和升级

- 用 Docker Compose（或等价受控容器编排）安装/启动/停止/查看状态；生产环境禁止无版本约束地 `pip install laya` 或跟随 upstream `main/latest`。
- 在部署锁文件中固定 Laya 发布版本或 Git commit、容器镜像 digest、checkpoint revision 和 Go 意图映射/问题集版本；升级先做兼容和质量测试，再逐节点滚动替换。保留上一组可回滚版本。
- 调研时上游仓库 `pyproject.toml` 声明版本为 `0.3.20`，运行要求 Python 3.10+；该版本仅为评估基线，实施时重新核实发布包、服务接口和权重 revision 后固定，不能把本文件中的版本号当作永远追随最新版。
- CPU 为默认兼容部署方式；NVIDIA GPU 为可选加速方式，需要兼容的驱动、容器工具链及 CUDA/PyTorch 组合。硬件选择由部署配置显式声明，不在运行时自动更换模型或静默切设备。
- 上游 CPU Docker quickstart 提到约 8 GB RAM、10 GB 可用磁盘作为试跑基线；这不是生产容量保证。上线前必须按选用 checkpoint、上下文长度、并发和硬件实测 CPU、RAM/VRAM、启动时间及 P95 推理延迟。

### 3.3 安装操作与配置

Laya 生命周期由交付包提供受控操作入口（例如 `laya-install`、`laya-status`、`laya-upgrade <pinned-version>`、`laya-rollback`）；命令实现归属后续代码变更，本设计固定其行为契约：

1. **安装**：检查 Docker/Compose、磁盘、内存和可选 GPU 驱动；加载锁定的 Laya 镜像/包与部署配置，创建私有网络和权重缓存卷。
2. **准备权重**：按锁定 checkpoint revision 下载权重，校验文件后落入持久缓存；网络受限时支持预先导入权重包，再以离线模式启动。
3. **启动**：读取 secret file，设置目标设备、模型集合、线程数、preload 和 API Key；等待权重加载及分类 smoke test 成功后才标记服务 ready。
4. **升级/回滚**：先启动新版本 sidecar，完成 health + 分类 smoke test，再逐网关节点切换；失败保持旧版并自动回滚，不覆盖旧 checkpoint 缓存。
5. **停止/卸载**：先将节点从网关负载均衡摘除并等待在途请求结束，再停止服务；默认保留权重缓存，只有显式清理操作才删除下载数据。

部署配置至少包含：`LAYA_DEVICE`（`cpu`/`cuda`）、固定的 `LAYA_MODEL`/`LAYA_MODELS`、`LAYA_AUTO_TASK`（需要 Router 自动选择 typed-decision checkpoint 时启用）、`LAYA_PRELOAD=1`、`LAYA_API_KEY_FILE`、`HF_HOME` 和可选 `HF_HUB_OFFLINE=1`。Go 与 Laya 使用同一 secret file 来源但各自按运行时读取；secret 不写入版本库。容器间走专属网络服务名，不依赖对宿主机或公网发布端口。

上游 `laya-serve` 支持 `/health`、`/v1/systemone`、`LAYA_PRELOAD`、`LAYA_MODELS` 和 `LAYA_API_KEY`；生产配置必须在锁定版本上做接口 smoke test。CPU 模式参考上游 HTTP quickstart，GPU 模式使用 NVIDIA Compose override；不要将上游 demo server 直接作为公网服务。

### 3.4 权重缓存和启动

- Hugging Face 权重放在独立持久卷，容器重建/版本升级不重复下载；该缓存可重建，但部署记录必须能复现 checkpoint revision。
- 初始化阶段先准备并校验权重，再启动/预加载 Laya 服务；生产启用 preload，避免首个用户请求承担模型加载延迟。中文请求的 Router/checkpoint 配置须显式包含 multilingual checkpoint；若需要 Router 自动选择 typed-decision checkpoint，按固定版本的上游配置启用相应选项并做集成测试。
- 生产启动前可预先下载权重，然后启用 Hugging Face offline mode；权重缺失或校验失败时服务报告未就绪，不得默默下载未固定版本。
- Compose 配置通过 secret file 注入 `LAYA_API_KEY`，绝不写入 Git、镜像层、命令行参数或日志。Go Gateway 使用同一部署 secret 访问 sidecar。

### 3.5 健康、就绪和多节点扩缩

- `GET /health` 用于进程存活检查；部署管理器另需以“模型已加载且推理 smoke test 通过”作为就绪条件。不能把进程已监听误当成权重可推理。
- Gateway 节点在自身 readiness 中报告 Laya 状态：健康可分类；Laya 未就绪/熔断时节点可降级提供通用路由，但必须标记 degraded、告警且不误报完整可用。负载均衡以 Go Gateway 健康决定是否接流量，分类器故障本身按下节回退，而非破坏用户请求。
- 新 Gateway 节点入池前先加载当前评分快照和匹配的 Laya/路由配置。评分发布的节点 ACK 屏障与 Laya 模型健康检查是两个独立条件。
- sidecar 更新按节点滚动：先启动新版本、加载模型、smoke test，通过后切换该节点 Gateway；失败则保留旧版并回滚。禁止在有请求时覆盖共享权重目录。

### 3.6 交付操作入口与质量门槛（实现对应）

交付目录 `deploy/laya/` 提供受控脚本，实现 3.3 的行为契约，不通过 Web 控制台暴露 shell：

| 入口 | 行为 |
| --- | --- |
| `preflight.sh [--gpu]` | 校验 Docker/Compose、secret file、CPU/内存/磁盘；`--gpu` 时校验 `nvidia-smi` 设备与 `nvidia` 容器运行时，缺失即明确失败，不静默回落到 CPU。 |
| `install.sh` | 干净主机安装：preflight → 构建固定版本镜像 → 启动 → 等待 checkpoint 就绪 → 认证 smoke → 记录发布版本与镜像 digest；权重来自持久 `hf_models` 卷，重复执行复用缓存。 |
| `status.sh` | 进程状态 + checkpoint 就绪。容器存活但 `model_loaded=false` 时报告**未就绪**，绝不报告 classifier-ready。 |
| `smoke.sh` | 带内部 Key 调用固定契约 `POST /v1/systemone`；仅在通过后才允许节点报告 classifier-ready。 |
| `upgrade.sh <version>` / `rollback.sh` | 升级前记录旧版本，就绪或 smoke 失败自动保留/回滚旧版，不覆盖旧 checkpoint 缓存。 |

Go 网关 `/readyz` 返回节点就绪、评分版本、`classifierEnabled`、`classifierReady`、`degraded` 与 `classifierReason`：分类器不可用时节点仍接流量（通用路由），但必须显式标记 degraded，不误报完整可用。

**质量门槛**：语义分类开关默认关闭（`LAYA_ENABLED=false`）。只有 `scripts/laya_holdout.py` 在固定中文留出集上产出达标报告（覆盖各类召回、macro-F1、置信度校准 ECE、p95 延迟与留出集 SHA-256）并记录阈值后，才允许启用；工具在未提供已记录阈值时以非零退出码结束，避免把“预期质量”写成代码默认值。

## 4. 超时、故障和安全回退

- Laya 调用设独立超时（首版建议 500 ms，按硬件压测可配置调整），不自动重试，避免在线请求延迟成倍增加。记录超时、熔断和恢复指标。
- 服务不可达、超时、HTTP 错误、JSON/schema 非法、标签不认识、置信度过低均退回 `general`；网关继续执行硬约束过滤和评分，不向用户暴露内部 Laya 故障，也不因本地分类服务故障直接返回 5xx。
- 在每次请求上固定分类结果、路由映射版本及评分版本，写入路由决策元数据，便于重放分析；不保存分类输入文本。
- Laya 服务只绑定 loopback 或专属私有网络；不得映射到公网。私有网络仍启用 Bearer API Key；远程跨主机时使用 TLS/mTLS 和网络 ACL。健康检查端点即使不鉴权也只能在内部网络可达。
- 分类文本仅进入本地 Laya 进程。只有用户请求最终选中的供应商会收到实际生成请求；Laya 本身不得持久化用户原文。

## 5. 管理与观测

Laya 安装、升级、启动、停止、权重准备和回滚属于交付/运维层责任，不开放给普通用户，也不由管理 Web 页面远程执行任意 shell 命令。管理端可展示各网关节点的 Laya 版本、checkpoint revision、健康/降级状态、最近探活和错误率；生命周期控制通过受控部署编排和审计化发布流程完成。

至少采集：分类调用数、成功/错误/超时数、fallback 数及原因、各意图分布、置信度分布、输入截断率、推理延迟分位数、加载耗时、当前包/模型/问题集版本、节点健康状态。指标和日志不得包含完整 Prompt、Key 或 secret。

## 6. 首版验收

1. 可由受控部署流程在干净主机安装并启动固定版本 Laya；重建容器后复用权重缓存，缺少权重/下载失败时明确报未就绪。
2. CPU-only 可运行；NVIDIA GPU 配置作为可选路径，设备不可用时按配置失败或降级，不静默改变版本/设备。
3. Go Gateway 只经私有接口调用 Laya；无效内部 Key、外部网络访问和未授权来源均不能调用分类接口。
4. 中文固定测试集覆盖所有初始意图；评估输出标签/schema、置信度与 Go 映射一致，质量门槛由上线验收记录固定。
5. 分类置信度低于 0.60、服务超时/不可用/坏响应均走 `general`，原请求仍能依确定性硬约束完成路由；工具、模态、权限、余额、高风险规则不会被分类器绕过。
6. 两个以上网关节点的 Laya 包版本、checkpoint revision、问题集和映射版本一致；新节点 readiness、滚动升级失败和旧版回滚均可验证。
7. 日志/审计中可按 request ID 查询分类标签、置信度、版本、fallback 原因和耗时，但检索不到原始分类文本或 secret。

## 7. 上游参考

- [Laya GitHub](https://github.com/NandhaKishorM/laya)
- [Laya Docker quickstart and HTTP serving](https://github.com/NandhaKishorM/laya/blob/main/docs/docker.md)
- 上游文档说明其 typed-decision checkpoint 的零样本效果需按目标任务验证；本产品不得将仓库演示性能或通用 benchmark 当作中文生产路由质量承诺。
