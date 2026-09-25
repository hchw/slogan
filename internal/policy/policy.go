// Package policy loads and updates the single routing policy row that governs
// deterministic ranking and evaluation limits.
package policy

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/db"
)

// Policy mirrors the routing_policy row.
type Policy struct {
	ID                      int64   `json:"-"`
	LowCapabilityBias       int     `json:"lowCapabilityBias"`
	MinPublishRatio         float64 `json:"minPublishRatio"`
	HighRiskForceQuality    bool    `json:"highRiskForceQuality"`
	EvalMaxTokens           int     `json:"evalMaxTokens"`
	EvalConcurrency         int     `json:"evalConcurrency"`
	EvalTimeoutSeconds      int     `json:"evalTimeoutSeconds"`
	ContentLogEnabled       bool    `json:"contentLogEnabled"`
	ContentLogRetentionDays int     `json:"contentLogRetentionDays"`
	Version                 string  `json:"version"`
}

// Service reads and writes the routing policy.
type Service struct {
	pool *db.Pool
}

// New returns a policy service.
func New(pool *db.Pool) *Service { return &Service{pool: pool} }

// Seed inserts the default policy row if none exists.
func (s *Service) Seed(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO routing_policy (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	return err
}

// Get returns the current policy.
func (s *Service) Get(ctx context.Context) (*Policy, error) {
	p := &Policy{}
	err := s.pool.QueryRow(ctx, `SELECT id, low_capability_bias, min_publish_ratio,
        high_risk_force_quality, eval_max_tokens, eval_concurrency, eval_timeout_seconds,
        content_log_enabled, content_log_retention_days, version
        FROM routing_policy ORDER BY id LIMIT 1`).Scan(
		&p.ID, &p.LowCapabilityBias, &p.MinPublishRatio, &p.HighRiskForceQuality,
		&p.EvalMaxTokens, &p.EvalConcurrency, &p.EvalTimeoutSeconds,
		&p.ContentLogEnabled, &p.ContentLogRetentionDays, &p.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.Seed(ctx); err != nil {
			return nil, err
		}
		return s.Get(ctx)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Update persists a new policy (single row, id=1) and bumps the version label.
func (s *Service) Update(ctx context.Context, q db.Execer, p *Policy) error {
	_, err := q.Exec(ctx, `UPDATE routing_policy SET
        low_capability_bias=$1, min_publish_ratio=$2, high_risk_force_quality=$3,
        eval_max_tokens=$4, eval_concurrency=$5, eval_timeout_seconds=$6,
        content_log_enabled=$7, content_log_retention_days=$8, version=$9, updated_at=now()
        WHERE id=(SELECT id FROM routing_policy ORDER BY id LIMIT 1)`,
		p.LowCapabilityBias, p.MinPublishRatio, p.HighRiskForceQuality,
		p.EvalMaxTokens, p.EvalConcurrency, p.EvalTimeoutSeconds,
		p.ContentLogEnabled, p.ContentLogRetentionDays, p.Version)
	return err
}
