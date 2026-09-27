# 生产运行手册（Runbook）

面向交付/运维。所有命令都在仓库根目录执行，`deploy/` 为部署根。首版为模块化单体：`api`（控制面）、`gateway`（数据面）、`worker`（后台任务）、`web`（静态前端）、`postgres`、`redis`，可选 `laya`（本地分类 sidecar）。

> 原则：数据库是事实来源，Redis 只承载限流、评分快照、节点心跳与任务队列。任何 Redis 丢失都不会改变账本，只是让节点重新加载快照；限流在 Redis 不可用时 fail-closed。

## 0. 发布前的固定信息

发布记录（release manifest）至少包含：

- 版本号、镜像 digest（`api`/`gateway`/`worker`/`web` 同一个 backend 镜像 + web 镜像）
- 数据库迁移版本（`migrations/` 的 checksum）
- 路由策略版本、评分版本 ID（发布后）
- Laya 包版本、镜像 digest、checkpoint revision、意图问题集 revision（启用语义分类时）

## 1. 首次部署

```sh
cd deploy
# 1) 准备 secret（绝不入库；deploy/.gitignore 已忽略 secrets/）
install -m 600 /dev/null secrets/provider_secret_key.txt      # 64 hex 字符（32 字节 AES-256 密钥）
openssl rand -hex 32 > secrets/provider_secret_key.txt
openssl rand -hex 32 > secrets/lb_membership_token.txt        # 可选：负载均衡控制面令牌
openssl rand -hex 32 > secrets/metrics_token.txt              # 抓取 /metrics 的令牌
openssl rand -hex 24 > secrets/laya_api_key.txt               # Laya 内部调用凭据

# 2) 基础设施
docker compose up -d postgres redis

# 3) 迁移（独立步骤，先于服务启动）
docker compose --profile app run --rm api --migrate-only

# 4) 引导第一个管理员（密码从标准输入或 ADMIN_PASSWORD_FILE 读取，不落历史）
docker compose --profile app run --rm -e ADMIN_PASSWORD_FILE=/run/secrets/… api --bootstrap-admin

# 5) 启动服务与静态前端
docker compose --profile app up -d --build

# 6) 健康检查
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:8081/healthz
curl -fsS http://127.0.0.1:8081/readyz
```

冒烟验证：`make e2e`（注册 → 发 Key → 兑换 → 指定模型调用 → reserve/settle → auto 分类 → 评分发布 → 多节点 ACK → 回滚）。

## 2. 配置项

| 变量 | 作用 |
| --- | --- |
| `DATABASE_URL` / `REDIS_ADDR` | 事实来源与缓存/队列 |
| `PROVIDER_SECRET_KEY_FILE` | provider 密钥加密密钥（AES-256-GCM），轮换需重加密流程 |
| `METRICS_ADDR` / `METRICS_TOKEN_FILE` | 内部抓取监听；未配置令牌时 `/metrics` 直接拒绝（fail-closed） |
| `LB_MEMBERSHIP_URL` / `LB_MEMBERSHIP_TOKEN_FILE` | 评分发布时摘流确认；未配置时不允许把未 ACK 节点移出屏障 |
| `SCORE_ACK_TIMEOUT` | 发布 ACK 屏障等待上限（默认 15s） |
| `LAYA_ENABLED` / `LAYA_BASE_URL` / `LAYA_API_KEY_FILE` | 语义分类开关与本地 sidecar；默认关闭 |
| `REQUESTS_PER_MIN_*` / `RATE_LIMIT_*` | 全局/用户/Key/Provider/模型限流 |

## 3. 日常运维

### 3.1 变更模型目录

1. 管理端添加/更新 Provider，先点“测试连通”。
2. “发现模型”后逐个启用；新模型未评分不会进入 auto 路由，但可被显式指定调用（`model` 字段）。
3. 修改价格只影响之后的请求；请求开始即固定价格快照，在途请求按旧价结算。

### 3.2 评分刷新与发布

1. 管理端“评估与评分” → “刷新评分”。刷新只产生候选版本，不改变线上路由。
2. 关注任务明细：每个模型的结果与评估成本，失败项可“重试失败项”。
3. 覆盖率（分子=本次通过校验的新评分模型，分母=刷新开始时冻结的启用参与模型）达到策略阈值才可发布；低于阈值必须拒绝。
4. 发布时确认对话框会显示历史评分回退数量（沿用旧分、不计入本次覆盖率）。
5. 发布后观察节点健康页：状态为 `pending` 表示仍有接流量节点未 ACK，**不得**对外报告发布成功；节点全部 ACK 后才转为 `published`。

### 3.3 节点滚动与摘流

1. 新节点启动后会先加载当前评分快照，再在 LB 中报告 ready；未加载完成不得接流量。
2. 摘流：把节点从 LB 摘除，等待在途请求结束（`/readyz` 会因 `drain` 返回 503）。
3. 评分发布屏障遇到未 ACK 节点时：先将该节点标记 draining，等待 `LB_MEMBERSHIP_URL` 返回“已移除”，确认后才把它移出屏障；未配置确认通道时发布保持 `pending`。
4. 回滚：管理端对保留版本执行“回滚到此版本”，同样经过 ACK 屏障，并强制填写原因（写入审计）。

### 3.4 Laya 分类器

```sh
cd deploy && ./laya/preflight.sh          # CPU 基线；--gpu 时校验驱动与 container runtime
./laya/install.sh                         # 干净主机安装（含 smoke）
./laya/status.sh                          # 进程 + checkpoint 就绪；进程存活 ≠ 可分类
./laya/smoke.sh                           # 认证 /v1/systemone 冒烟
./laya/upgrade.sh 0.3.20                  # 失败自动保留旧版并回滚
```

语义分类默认关闭。只有当 `scripts/laya_holdout.py` 在固定中文留出集上产出达标报告（各类召回、macro-F1、ECE、p95）并记录阈值后，才允许把 `LAYA_ENABLED` 置为 true；分类器不可用时节点继续提供通用路由并标记 degraded。

### 3.5 观测与告警

- `GET /metrics`（需 `Authorization: Bearer $METRICS_TOKEN`）：请求状态、Token、cost/charge、reserve/settle、评分任务与成本、节点接流量/评分版本/分类器降级。
- 告警阈值与处置见 `docs/observability-alerts.md`。
- 指标与日志不得包含 prompt、API Key、provider secret 或用户标识；日志敏感字段在写出行之前 redact。

### 3.6 备份与恢复

```sh
# 备份
docker compose exec -T postgres pg_dump -U slogan -Fc slogan > backup-$(date +%F).dump
cp deploy/secrets/provider_secret_key.txt backup-provider-secret-$(date +%F).txt   # 单独保管

# 恢复（新实例）
docker compose up -d postgres
docker compose exec -T postgres pg_restore -U slogan -d slogan --clean --if-exists < backup.dump
docker compose --profile app up -d --build
```

恢复后核对：`quota_account.balance_micro` 与 `quota_ledger` 汇总一致；`request_record` 中 `settlement_pending` 会在 worker 恢复流程中被重试结算；评分版本与 Redis 快照会在节点心跳后重新加载。

Redis 数据可丢弃：限流计数与快照都会重建。若整个 Redis 丢失，节点会在下一次心跳（≤10s）重新注册。

## 4. 故障处置

| 现象 | 处置 |
| --- | --- |
| `/readyz` 返回 503 且 body 为 `db unavailable` / `redis unavailable` | 先恢复依赖；数据面此时 fail-closed（拒绝新请求并保留已完成的结算状态） |
| `/readyz` 中 `degraded=true` | 分类器不可用：请求仍以通用意图完成，按告警跟进 Laya；不要因此重启网关 |
| 请求 `503 model_unavailable` | 无满足硬约束的候选（无已发布评分 / 权限 / 上下文 / 工具 / 模态）。检查评分是否已发布 |
| 请求 `409 idempotency_replay` / `idempotency_in_progress` | 客户端重复提交同一 `Idempotency-Key`；按契约返回原结果或提示等待 |
| 余额扣减与上游不一致 | 只按校验通过的 provider usage 结算；无可信 usage 会释放预扣（`release` 账本）。核对 `usage_event` 与 `quota_ledger` |
| 发布一直 `pending` | 有节点未 ACK：确认节点健康页，或配置 `LB_MEMBERSHIP_URL` 后重试 |
| 迁移 checksum drift 报错 | 该库曾应用过被修改的迁移。绿地可用全新库；生产需按变更流程回滚代码并补齐新迁移文件 |

## 5. 回滚

1. 应用回滚：切回上一镜像 digest（数据面与控制面同镜像），`docker compose --profile app up -d`。
2. 数据兼容：迁移为前向；回滚应用不删除已新增表/列（`usage_event`、`allowed_models`、`code_hash` 等均为新增）。
3. 评分回滚：管理端对保留版本执行回滚（强制原因 + 审计 + ACK 屏障）。
4. provider 加密密钥轮换：新增密钥→后台任务重加密→切换 `PROVIDER_SECRET_KEY_FILE`，不要直接覆盖。

## 6. 演练（dry-run）检查清单

在预发环境完整走一遍并留档：

- [ ] 从 clean clone 执行 `make compose-check` 与第 1 节步骤，服务健康
- [ ] `make e2e` 全流程通过
- [ ] 停掉 Redis：`/readyz` 与限流返回 503/fail-closed，重启后自动恢复
- [ ] 制造一个未 ACK 节点：发布保持 `pending`，配好摘流确认后可完成
- [ ] `./laya/status.sh` 在 checkpoint 未加载时明确报告未就绪
- [ ] pg_dump/pg_restore 演练并核对账本一致性
- [ ] 回滚上一镜像 digest 并确认健康
