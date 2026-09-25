# 指标与告警

`GET /metrics` 暴露 Prometheus 文本格式（需 `Authorization: Bearer $METRICS_TOKEN`）。未配置令牌时端点直接拒绝，避免误配把内部指标暴露出去。`api`/`gateway`/`worker` 各自可按 `METRICS_ADDR` 打开内部抓取监听，抓取地址不应发布到公网。

## 1. 指标清单

| 指标 | 类型 | 含义 | 主要标签 |
| --- | --- | --- | --- |
| `slogan_gateway_requests_total` | counter | 进程内已完成结算的请求数 | `status`（`success`/`stream_broken`/`client_disconnected`/`upstream_error`/`invalid_request_error`/`model_unavailable`/`quota_rejected`） |
| `slogan_gateway_usage_source_total` | counter | 结算所用 usage 来源 | `source`（`provider`/`none`） |
| `slogan_gateway_requests_24h` | gauge | 近 24h 请求数（含未结算） | `status` |
| `slogan_gateway_tokens_total` | gauge | 近 7 天结算 Token | `direction`（`input`/`output`） |
| `slogan_gateway_settled_requests_7d` | gauge | 近 7 天结算请求数 | — |
| `slogan_gateway_cost_micro_7d` | gauge | 近 7 天上游成本（微元） | — |
| `slogan_gateway_charge_micro_7d` | gauge | 近 7 天用户扣费（微元） | — |
| `slogan_gateway_reservations_open` | gauge | 未结算/未释放的预扣笔数 | — |
| `slogan_gateway_reserved_micro` | gauge | 当前冻结金额（微元） | — |
| `slogan_gateway_reservations_released_total` | counter | 无可信 usage 而释放的预扣笔数 | — |
| `slogan_gateway_settlements_total` | counter | 实际扣费结算笔数 | — |
| `slogan_evaluation_tasks` | gauge | 评分刷新任务数 | `status`（`running`/`completed`/`failed`） |
| `slogan_evaluation_cost_micro_total` | counter | 评估产生的上游成本（微元），仅成本统计、不计入用户扣费 | — |
| `slogan_gateway_nodes_serving` | gauge | 正在接流量的网关节点数 | — |
| `slogan_gateway_nodes_classifier_degraded` | gauge | 接流量但分类器未就绪的节点数 | — |
| `slogan_gateway_nodes_score_version_stale` | gauge | 未加载当前评分版本的节点数 | — |
| `slogan_gateway_score_version_active` | gauge | 当前已发布评分版本 ID（0 表示未发布） | — |
| `slogan_laya_classify_total` | counter | 分类调用数 | `outcome`（`classified`/`fallback`） |
| `slogan_laya_fallback_total` | counter | 分类回退数 | `reason`（`disabled`/`unavailable`/`timeout`/`bad_response`/`unknown_label`/`low_confidence`/`circuit_open`） |
| `slogan_laya_classify_duration_ms_sum` / `_count` | counter | 分类延迟求和与计数（用于均值/分位近似） | — |

标签全部为有界枚举：**不含用户 ID、请求 ID、prompt、API Key、provider secret**。

## 2. 建议告警

| 告警 | 条件（示例） | 严重度 | 处置 |
| --- | --- | --- | --- |
| 数据面不可用 | `/readyz` 5xx 持续 1 分钟 | P1 | 查 `db unavailable`/`redis unavailable`；依赖恢复后自动回到 200 |
| 上游错误率升高 | `rate(slogan_gateway_requests_total{status="upstream_error"}[5m]) / rate(slogan_gateway_requests_total[5m]) > 0.05` | P1 | 检查 provider 状态与密钥；必要时摘除该 provider 的模型 |
| 断流率升高 | `status="stream_broken"` 占比 > 1% | P2 | 查上游稳定性与超时；断流只发 SSE error，不重试 |
| 扣费/成本背离 | `slogan_gateway_cost_micro_7d / slogan_gateway_charge_micro_7d` 偏离定价预期 | P2 | 核对价格配置与模型售价 |
| 预扣长期未释放 | `slogan_gateway_reservations_open > 0` 持续 15 分钟且 `settlements` 不增长 | P2 | 查 worker 结算恢复；确认 `usage_event` 是否缺失 |
| 评分发布未完成 | `slogan_gateway_nodes_score_version_stale > 0` 持续 10 分钟 | P2 | 查节点健康；配置 `LB_MEMBERSHIP_URL` 完成摘流确认 |
| 分类器降级 | `slogan_gateway_nodes_classifier_degraded > 0` | P3 | 请求仍可完成（通用意图）；查 Laya checkpoint 与熔断恢复 |
| 分类回退激增 | `rate(slogan_laya_fallback_total[5m]) > 0` 且 reason=`timeout`/`unavailable` | P3 | 查 sidecar 健康与 500ms 超时；不得因此重启网关 |
| 评估任务失败 | `slogan_evaluation_tasks{status="failed"} > 0` | P3 | 查看任务明细中的失败原因与重试失败项 |

## 3. 日志

- 请求日志只包含 `request_id`、状态、模型、Token、成本/扣费与延迟；不包含 prompt 与凭据。
- 供应商密钥、API Key 明文、Laya 分类输入文本永不写日志；敏感字段在写出前统一 redact。
- 审计日志写入与业务同一事务，包含操作者、动作、目标、原因、结果与 `request_id`。
