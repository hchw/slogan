// Package bootstrap wires the shared service graph used by the API, Gateway and
// Worker processes.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/evaluation"
	"github.com/hchw/slogan/internal/metrics"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/redisx"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/usage"
)

// Bundle holds the constructed services.
type Bundle struct {
	Config   *config.Config
	DB       *db.Pool
	Redis    *redisx.Client
	Audit    *audit.Service
	Auth     *auth.Service
	Provider *provider.Service
	Model    *model.Service
	Quota    *quota.Service
	Request  *requestrec.Service
	Usage    *usage.Service
	Policy   *policy.Service
	Eval     *evaluation.Service
	Metrics  *metrics.Registry
}

// Build opens infrastructure, runs migrations, seeds roles/policy and
// constructs services.
func Build(ctx context.Context, cfg *config.Config) (*Bundle, error) {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if _, err := db.Migrate(ctx, pool, nil); err != nil {
		return nil, err
	}
	rdb, err := redisx.Open(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		return nil, err
	}

	aud := audit.New(pool)
	authSvc := auth.New(pool, cfg.SessionTTL)
	if err := authSvc.EnsureSeedRoles(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: seed roles: %w", err)
	}
	prov := provider.New(pool, cfg.ProviderSecretKey, aud)
	models := model.New(pool, prov, aud)
	q := quota.New(pool, aud)
	reqs := requestrec.NewService(pool)
	usg := usage.New(pool)
	pol := policy.New(pool)
	if err := pol.Seed(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: seed policy: %w", err)
	}
	ev := evaluation.New(pool, rdb, models, prov, pol, aud, cfg.Evaluation)
	ev.SetMembershipAdapter(evaluation.NewHTTPMembershipAdapter(cfg.LBMembershipURL, cfg.LBMembershipToken, cfg.ScoreAckTimeout))
	ev.SetAckTimeout(cfg.ScoreAckTimeout)

	// Metrics are composed from the operational sources plus in-process
	// counters; no label carries user identifiers, prompts or secrets.
	registry := metrics.NewRegistry(metrics.NewCounters(), metrics.NewPostgresCollector(pool, rdb))

	return &Bundle{
		Config: cfg, DB: pool, Redis: rdb, Audit: aud, Auth: authSvc, Provider: prov,
		Model: models, Quota: q, Request: reqs, Usage: usg, Policy: pol, Eval: ev, Metrics: registry,
	}, nil
}

// Close releases resources.
func (b *Bundle) Close() {
	if b.Redis != nil {
		_ = b.Redis.Close()
	}
	if b.DB != nil {
		b.DB.Close()
	}
}
