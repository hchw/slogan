// Package usage aggregates validated token usage, user charges and upstream
// cost for reporting.
package usage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/db"
)

// Service aggregates and reports usage.
type Service struct {
	pool *db.Pool
}

// New returns a usage service.
func New(pool *db.Pool) *Service { return &Service{pool: pool} }

// Record upserts the hourly usage aggregate for a settled request.
func (s *Service) Record(ctx context.Context, tx pgx.Tx, requestID string, userID, modelID, providerID, input, output, cost, charge int64, at time.Time) error {
	ct, err := tx.Exec(ctx, `INSERT INTO usage_event
        (request_id, user_id, model_id, provider_id, input_tokens, output_tokens, cost_micro, charge_micro, occurred_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (request_id) DO NOTHING`,
		requestID, userID, modelID, providerID, input, output, cost, charge, at)
	if err != nil {
		return apperr.Internal("failed to record usage event").WithCause(err)
	}
	if ct.RowsAffected() == 0 {
		return nil // settlement replay: do not increment aggregate again
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_record
        (user_id, model_id, provider_id, request_count, input_tokens, output_tokens, cost_micro, charge_micro, stat_date, stat_hour)
        VALUES ($1,$2,$3,1,$4,$5,$6,$7,$8::date,EXTRACT(HOUR FROM $9::timestamptz)::smallint)
        ON CONFLICT (user_id, model_id, stat_date, stat_hour) DO UPDATE SET
            request_count = usage_record.request_count + 1,
            input_tokens  = usage_record.input_tokens + EXCLUDED.input_tokens,
            output_tokens = usage_record.output_tokens + EXCLUDED.output_tokens,
            cost_micro    = usage_record.cost_micro + EXCLUDED.cost_micro,
            charge_micro  = usage_record.charge_micro + EXCLUDED.charge_micro`,
		userID, modelID, providerID, input, output, cost, charge, at, at)
	if err != nil {
		return apperr.Internal("failed to record usage").WithCause(err)
	}
	return nil
}

// Summary is a user's usage over a period.
type Summary struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	ChargeMicro  int64 `json:"chargeMicro"`
}

// UserSummary aggregates a user's usage between from and to.
func (s *Service) UserSummary(ctx context.Context, userID int64, from, to time.Time) (*Summary, error) {
	sum := &Summary{}
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(request_count),0), COALESCE(sum(input_tokens),0),
        COALESCE(sum(output_tokens),0), COALESCE(sum(charge_micro),0)
        FROM usage_record WHERE user_id=$1 AND stat_date BETWEEN $2::date AND $3::date`,
		userID, from, to).Scan(&sum.Requests, &sum.InputTokens, &sum.OutputTokens, &sum.ChargeMicro)
	if err != nil {
		return nil, apperr.Internal("failed to load usage summary").WithCause(err)
	}
	return sum, nil
}

// ModelUsageRow is per-model aggregate usage.
type ModelUsageRow struct {
	ModelID      int64   `json:"modelId"`
	ModelName    string  `json:"modelName"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostMicro    int64   `json:"costMicro"`
	ChargeMicro  int64   `json:"chargeMicro"`
	AvgLatencyMs float64 `json:"avgLatencyMs"`
	SuccessRate  float64 `json:"successRate"`
}

// ModelUsage returns per-model aggregates for administrators.
func (s *Service) ModelUsage(ctx context.Context, from, to time.Time, providerID, modelID int64) ([]ModelUsageRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.model_id, COALESCE(m.name,''),
        COALESCE(sum(u.request_count),0), COALESCE(sum(u.input_tokens),0), COALESCE(sum(u.output_tokens),0),
        COALESCE(sum(u.cost_micro),0), COALESCE(sum(u.charge_micro),0)
        FROM usage_record u LEFT JOIN model m ON m.id = u.model_id
        WHERE u.stat_date BETWEEN $1::date AND $2::date
          AND ($3=0 OR m.provider_id=$3) AND ($4=0 OR u.model_id=$4)
        GROUP BY u.model_id, m.name ORDER BY u.model_id`, from, to, providerID, modelID)
	if err != nil {
		return nil, apperr.Internal("failed to load model usage").WithCause(err)
	}
	defer rows.Close()
	var out []ModelUsageRow
	for rows.Next() {
		var r ModelUsageRow
		if err := rows.Scan(&r.ModelID, &r.ModelName, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.CostMicro, &r.ChargeMicro); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserUsageRow is per-user aggregate usage.
type UserUsageRow struct {
	UserID       int64 `json:"userId"`
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	ChargeMicro  int64 `json:"chargeMicro"`
}

// UserUsage returns per-user aggregates for administrators.
func (s *Service) UserUsage(ctx context.Context, from, to time.Time, userID int64) ([]UserUsageRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, COALESCE(sum(request_count),0),
        COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0), COALESCE(sum(charge_micro),0)
        FROM usage_record WHERE stat_date BETWEEN $1::date AND $2::date AND ($3=0 OR user_id=$3)
        GROUP BY user_id ORDER BY user_id`, from, to, userID)
	if err != nil {
		return nil, apperr.Internal("failed to load user usage").WithCause(err)
	}
	defer rows.Close()
	var out []UserUsageRow
	for rows.Next() {
		var r UserUsageRow
		if err := rows.Scan(&r.UserID, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.ChargeMicro); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
