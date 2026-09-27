package routing

import (
	"context"
	"os"
	"testing"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
)

type staticCapabilities struct {
	version int64
	values  map[int64]map[string]float64
}

func (s staticCapabilities) ActiveVersion(context.Context) (int64, error) { return s.version, nil }
func (s staticCapabilities) Capabilities(context.Context, int64) (map[int64]map[string]float64, error) {
	return s.values, nil
}

func TestRouterAppliesHardFiltersBeforeCostPreference(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping routing integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`); lock.Release() })
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	aud := audit.New(pool)
	prov := provider.New(pool, make([]byte, 32), aud)
	models := model.New(pool, prov, aud)
	pol := policy.New(pool)
	if err := pol.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := prov.Create(ctx, provider.Input{Name: "router", BaseURL: "http://local", Secret: "s"}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	priceA, priceB := int64(100), int64(10)
	a, err := models.Create(ctx, model.Input{ProviderID: p.ID, Name: "A", ModelKey: "model-a", ContextLength: 1024, InputModalities: []string{"text"}, SupportsStream: true, SupportsTools: false, InputPriceMicro: &priceA}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := models.Create(ctx, model.Input{ProviderID: p.ID, Name: "B", ModelKey: "model-b", ContextLength: 8192, InputModalities: []string{"text"}, SupportsStream: true, SupportsTools: true, InputPriceMicro: &priceB}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := models.Create(ctx, model.Input{ProviderID: p.ID, Name: "C", ModelKey: "model-c", ContextLength: 8192, InputModalities: []string{"text"}, SupportsTools: true}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.SetStatus(ctx, c.ID, model.StatusDisabled, audit.Actor{Type: "system"}); err != nil {
		t.Fatal(err)
	}
	caps := staticCapabilities{version: 7, values: map[int64]map[string]float64{
		a.ID: {"coding": .90, "reasoning": .90},
		b.ID: {"coding": .80, "reasoning": .60},
		c.ID: {"coding": 1, "reasoning": 1},
	}}
	router := NewRouter(models, pol, caps, pool)
	req := Requirements{Intent: "coding", RequiredCapabilities: map[string]float64{"coding": .80, "reasoning": .60}, EstimatedInputTokens: 100, Text: "analyze code"}
	if _, err := pool.Exec(ctx, `UPDATE routing_policy SET low_capability_bias=0`); err != nil {
		t.Fatal(err)
	}
	decision, err := router.Select(ctx, req)
	if err != nil || decision.Model.ID != a.ID {
		t.Fatalf("quality-first selection=%+v err=%v", decision, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE routing_policy SET low_capability_bias=100`); err != nil {
		t.Fatal(err)
	}
	decision, err = router.Select(ctx, req)
	if err != nil || decision.Model.ID != b.ID {
		t.Fatalf("cost-first selection=%+v err=%v", decision, err)
	}

	req.AllowedModels = []string{"model-a"}
	decision, err = router.Select(ctx, req)
	if err != nil || decision.Model.ID != a.ID {
		t.Fatalf("key scope selection=%+v err=%v", decision, err)
	}
	req.AllowedModels = nil
	req.NeedsTools = true
	decision, err = router.Select(ctx, req)
	if err != nil || decision.Model.ID != b.ID {
		t.Fatalf("tool hard filter selection=%+v err=%v", decision, err)
	}
	req.AllowedModels = []string{"model-a"}
	if _, err := router.Select(ctx, req); err == nil {
		t.Fatal("must not relax tools constraint when no candidate")
	}
	req.AllowedModels = nil
	req.NeedsTools = false
	req.EstimatedInputTokens = 2000
	decision, err = router.Select(ctx, req)
	if err != nil || decision.Model.ID != b.ID {
		t.Fatalf("context hard filter selection=%+v err=%v", decision, err)
	}
}
