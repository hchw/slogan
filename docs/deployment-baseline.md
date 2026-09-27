# 部署基线与门槛（Deployment Baseline）

本文件记录**目标部署机**的实测容量与质量基线。原则：

- 门槛是测量结果，不是代码里的默认值；未测量即视为未达标。
- 未达标时保持语义分类关闭（`LAYA_ENABLED=false`），指定模型调用不受影响。
- 报告需与机器规格、镜像 digest、评分版本一起归档，可复现。

## 1. 需要记录的机器规格

| 项 | 记录值 |
| --- | --- |
| 主机 / 云实例类型 | |
| CPU（型号、物理核、是否独享） | |
| 内存 | |
| 磁盘（类型、可用空间、是否本地 SSD） | |
| GPU（型号、显存、驱动版本、CUDA/cuDNN；无则填“无”） | |
| 容器运行时（Docker/Compose 版本） | |
| PostgreSQL / Redis 部署形态（同机/独立、规格） | |
| Laya 运行设备（`cpu` / `cuda`） | |
| 镜像 digest（api/gateway/worker/web） | |
| Laya 镜像 digest + 包版本 + checkpoint revision | |

## 2. 数据面基线（`scripts/load_probe.py`）

```sh
GATEWAY_URL=http://127.0.0.1:8081 API_KEY=$API_KEY \
python3 scripts/load_probe.py --model <model-key> --requests 200 --concurrency 8 \
  --max-p95-ms <记录阈值> --max-error-rate <记录阈值> \
  --output docs/reports/baseline-$(date +%F).json
```

脚本输出 `successRate`、`p50/p95/p99/max` 延迟与 `sampleErrors`；未提供阈值时以非零退出码结束。建议至少覆盖：

- 并发档位：1 / 4 / 8 / 16（记录每档 p95 与错误率）
- 流式（SSE）与非流式分别测量（流式看首字节与整包完成时间）
- 长上下文与短请求各一组，避免只测单一形态

记录表：

| 并发 | 形态 | 请求数 | 成功率 | p50 | p95 | p99 | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 非流式 | | | | | | |
| 8 | 非流式 | | | | | | |
| 8 | 流式 | | | | | | |

## 3. 分类质量基线（`scripts/laya_holdout.py`）

```sh
python3 scripts/laya_holdout.py <固定中文留出集> --base-url http://127.0.0.1:8000 \
  --api-key "$(cat deploy/secrets/laya_api_key.txt)" \
  --min-macro-f1 <记录阈值> --min-per-class-recall <记录阈值> \
  --max-p95-latency-ms 500 --max-ece <记录阈值> \
  --output docs/reports/laya-holdout-$(date +%F).json
```

记录：macro-F1、各类召回（尤其少数类）、置信度校准 ECE、p95 延迟、留出集 SHA-256、checkpoint/问题集 revision。

| 指标 | 记录值 | 阈值 | 是否达标 |
| --- | --- | --- | --- |
| macro-F1 | | | |
| 各类召回（最低一类） | | | |
| ECE | | | |
| p95 延迟 | | ≤ 500 ms（默认超时） | |
| 长文本截断率 | | | |

**启用条件**：以上全部达标后，才允许把 `LAYA_ENABLED=true`。若 Laya 未就绪，节点继续以通用意图路由并上报 `degraded`，用户请求不得因此失败。

## 4. 资源占用观测

| 项 | 记录值 |
| --- | --- |
| `api` 常驻内存 / CPU | |
| `gateway` 常驻内存 / 单请求峰值 CPU | |
| `worker` 内存（评估并发 5 时） | |
| Laya 加载耗时 / 常驻内存 / 分类 p95 | |
| PostgreSQL 磁盘增长（每 10 万请求） | |
| Redis 内存（限流键 + 快照 + 心跳） | |

## 5. 验收结论

| 结论 | 勾选 |
| --- | --- |
| 数据面 p95/错误率达标 | [ ] |
| 分类质量与延迟达标（未达标则保持关闭） | [ ] |
| 资源占用可接受（含 1.5 倍峰值余量） | [ ] |
| 备份/恢复与回滚演练通过（见 `docs/runbook.md` 第 6 节） | [ ] |

结论填写人 / 日期 / 报告路径：
