# 千丝傀智（Slogan）

> 一线分两路，双傀各承长

![千丝傀智（Slogan）](web/public/slogan.png)

千丝傀智（Slogan）是一个多模型 AI 网关：为用户提供 OpenAI 兼容调用入口，为运营人员提供供应商、模型、评分与额度控制台。

## 架构

- **Go API**：`/user/api/v1` 与 `/admin/api/v1`，管理会话、RBAC、模型目录和额度运营。
- **Go Gateway**：`/v1/models`、`/v1/chat/completions`，鉴权、硬约束路由、预扣、流式/非流式转发与 usage 结算。
- **Go Worker**：评分刷新与 settlement-pending 恢复。
- **本地 Laya Python sidecar**：只为 `model=auto` 分类意图；Go 自己过滤和选模。默认 `LAYA_ENABLED=false`，中文留出集达标后才能开启。
- **PostgreSQL**：身份、目录、账务、请求和评分的事实来源；**Redis**：限流、队列、快照与 Gateway 节点 lease。
- **React + TypeScript**：用户与管理工作台位于 `web/`，使用不同的 session namespace。

详细产品和部署边界见 [`docs/`](docs/) 及 OpenSpec 变更 `implement-multi-model-ai-gateway`。

## 文档与运维入口

| 文档 | 内容 |
| --- | --- |
| [`docs/product-requirements.md`](docs/product-requirements.md) | 产品需求与边界 |
| [`docs/detailed-design.md`](docs/detailed-design.md) | 详细设计（计费、路由、发布、安全） |
| [`docs/frontend-design.md`](docs/frontend-design.md) | 用户端/管理端交互与状态 |
| [`docs/design-laya-runtime.md`](docs/design-laya-runtime.md) | Laya 安装、就绪、故障回退与质量门槛 |
| [`docs/runbook.md`](docs/runbook.md) | 生产运行手册：迁移、secret、滚动、摘流、备份恢复、回滚、演练 |
| [`docs/observability-alerts.md`](docs/observability-alerts.md) | 指标清单与告警建议 |
| [`docs/deployment-baseline.md`](docs/deployment-baseline.md) | 目标机器容量与分类质量基线记录模板 |

常用命令：`make help`。`make test` 跑 Go 单测+集成测试（需 `TEST_DATABASE_URL`/`TEST_REDIS_ADDR`），`make web-test` 跑前端构建与组件测试，`make compose-check` 静态校验部署编排，`make e2e` 跑端到端流程，`make smoke` 校验 Laya 前置条件与 sidecar。

## 一键本地全栈

```sh
make compose-check                  # 静态校验编排（不需要跑起来）
make dev-infra                      # 仅 PostgreSQL + Redis
make dev-up                         # Go API/Gateway/Worker + PostgreSQL + Redis + 静态 web
# 需要本地假上游时（禁用于生产）：
cd deploy && docker compose --profile mock up -d mock-provider
```

`--profile mock` 提供一个 OpenAI 兼容的假上游（`fake-chat`），只用于本地联调与端到端验证：它会伪造 usage 数字。冒烟流程：

```sh
API_URL=http://127.0.0.1:8080 GATEWAY_URL=http://127.0.0.1:8081 \
ADMIN_USER=admin ADMIN_PASSWORD=... \
PROVIDER_BASE_URL=http://mock-provider:9000/v1 MODEL_KEY=fake-chat \
./scripts/e2e_smoke.sh
```

## 本地开发

要求 Go 1.26+、Node 22+、Docker Compose。

1. 准备环境：

   ```sh
   cp .env.example .env
   mkdir -p deploy/secrets
   # 生产使用随机 32-byte hex key 和独立 Laya secret；不要提交 secrets。
   printf '%s\n' '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f' > deploy/secrets/provider_secret_key.txt
   printf '%s\n' 'replace-me' > deploy/secrets/laya_api_key.txt
   make dev-infra
   ```

2. 在当前 shell 加载 `.env`，然后运行 API、Gateway、Worker：

   ```sh
   set -a; . ./.env; set +a
   go run ./cmd/api
   go run ./cmd/gateway
   go run ./cmd/worker
   ```

   服务启动时应用版本化 migration；检查 `/healthz` 与 `/readyz`。

3. 单独创建初始管理员（只能成功一次）：

   ```sh
   set -a; . ./.env; set +a
   ADMIN_USERNAME=admin go run ./cmd/api --bootstrap-admin
   ```

   命令从 stdin 提示输入密码；密码不得放进 shell 参数。随后打开 `/admin/login`。

4. 前端：

   ```sh
   make web-install
   make web-dev
   ```

   Vite 默认地址为 `http://localhost:5173`。部署时经同源 Nginx 转发 `/user/api/v1`、`/admin/api/v1` 与 `/v1`。

5. 测试与构建：

   ```sh
   go test ./...
   go vet ./...
   go build ./...
   npm --prefix web test
   npm --prefix web run build
   ```

PostgreSQL 集成测试使用 `TEST_DATABASE_URL`；Redis 集成测试使用 `TEST_REDIS_ADDR`。未设置时相应测试会 skip。

## Docker Compose

```sh
cd deploy
docker compose up -d postgres redis
# Go API/Gateway/Worker + React/Nginx：
docker compose --profile app up -d --build
# 另加本地 Laya（需准备固定权重、secret 和合适硬件）：
docker compose --profile app --profile laya up -d --build
```

Compose secret 文件在 `deploy/secrets/`，已加入 ignore，严禁提交真实密钥。Laya 版本基线、权重 cache、health、升级与回滚说明见 [`deploy/laya/README.md`](deploy/laya/README.md)。生产部署前必须重核上游版本/服务协议、使用不可变镜像 digest，并完成中文留出集评测。多节点评分发布在所有流量节点 ACK 前保持 pending；负载均衡必须将 draining 节点摘流并确认。

## 首版边界

统一余额以整数微元计量，兑换后不单独过期、不区分模型；兑换码可有兑换截止。上游成本价与用户售价分开按 request snapshot 结算；没有可信 usage 时释放预扣、不按估算收费。首版仅 `models` 和 `chat/completions`，包含 streaming 与 tool calling；不含组织额度、模型范围余额桶、embeddings、多模态输入或多模型合答。
