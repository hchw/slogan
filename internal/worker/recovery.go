package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/usage"
)

// RunSettlementRecovery retries durable usage snapshots whose quota settlement
// could not be committed at response time. Request IDs and usage_event uniqueness
// make retries safe.
func RunSettlementRecovery(ctx context.Context, pool *db.Pool, q *quota.Service, usg *usage.Service) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := recoverOnce(ctx, pool, q, usg); err != nil {
			slog.Error("settlement recovery batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type pendingSettlement struct {
	RequestID    string
	UserID       int64
	ModelID      int64
	Reserved     int64
	UsageSource  string
	InputTokens  int64
	OutputTokens int64
	CostMicro    int64
	ChargeMicro  int64
	LatencyMs    int
	ErrorCode    string
}

func recoverOnce(ctx context.Context, pool *db.Pool, q *quota.Service, usg *usage.Service) error {
	rows, err := pool.Query(ctx, `SELECT request_id, COALESCE(user_id,0), COALESCE(model_id,0), reserved_micro,
        COALESCE(usage_source,''), input_tokens, output_tokens, cost_micro, charge_micro, latency_ms, COALESCE(error_code,'')
        FROM request_record WHERE status='settlement_pending' AND finished_at IS NULL
        ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	var pending []pendingSettlement
	for rows.Next() {
		var p pendingSettlement
		if err := rows.Scan(&p.RequestID, &p.UserID, &p.ModelID, &p.Reserved, &p.UsageSource,
			&p.InputTokens, &p.OutputTokens, &p.CostMicro, &p.ChargeMicro, &p.LatencyMs, &p.ErrorCode); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, p := range pending {
		if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
			settlement, err := q.Settle(ctx, tx, p.UserID, p.RequestID, p.Reserved, p.ChargeMicro)
			if err != nil {
				return err
			}
			status := finalStatus(p.ErrorCode)
			ct, err := tx.Exec(ctx, `UPDATE request_record SET status=$2, charge_micro=$3, finished_at=now()
                WHERE request_id=$1 AND status='settlement_pending' AND finished_at IS NULL`,
				p.RequestID, status, settlement.ChargeMicro)
			if err != nil {
				return err
			}
			if ct.RowsAffected() == 0 {
				return nil
			}
			if p.UsageSource != "none" && p.ModelID != 0 {
				var providerID int64
				if err := tx.QueryRow(ctx, `SELECT provider_id FROM model WHERE id=$1`, p.ModelID).Scan(&providerID); err != nil {
					return err
				}
				return usg.Record(ctx, tx, p.RequestID, p.UserID, p.ModelID, providerID,
					p.InputTokens, p.OutputTokens, p.CostMicro, settlement.ChargeMicro, time.Now())
			}
			return nil
		}); err != nil {
			slog.Error("settlement recovery failed", "request_id", p.RequestID, "error", err)
		}
	}
	return nil
}

func finalStatus(code string) string {
	switch code {
	case "":
		return "success"
	case "upstream_timeout":
		return "gateway_timeout"
	case "client_disconnected":
		return "client_disconnected"
	case "stream_error":
		return "stream_broken"
	default:
		return "upstream_error"
	}
}
