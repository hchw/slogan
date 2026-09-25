package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/provider"
)

func setupModel(t *testing.T) (*db.Pool, *provider.Service, *Service) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping model integration test")
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
	if _, err = lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`); lock.Release() })
	if _, err = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	aud := audit.New(pool)
	prov := provider.New(pool, key, aud)
	return pool, prov, New(pool, prov, aud)
}

func TestModelCreateValidateUpdateStatusDeleteAndReuse(t *testing.T) {
	pool, prov, svc := setupModel(t)
	ctx := context.Background()
	actor := audit.Actor{Type: "admin", ID: 1, RequestID: "model-test"}
	p, err := prov.Create(ctx, provider.Input{Name: "p", BaseURL: "http://localhost", Secret: "s"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	negative := int64(-1)
	if _, err := svc.Create(ctx, Input{ProviderID: p.ID, Name: "bad", ModelKey: "bad", InputPriceMicro: &negative}, actor); err == nil {
		t.Fatal("negative price accepted")
	}
	in, out, chargeIn, chargeOut := int64(10), int64(20), int64(30), int64(40)
	m, err := svc.Create(ctx, Input{ProviderID: p.ID, Name: "Model", ModelKey: "model-x", ContextLength: 4096, InputModalities: []string{"text"}, SupportsStream: true, InputPriceMicro: &in, OutputPriceMicro: &out, ChargeInputMicro: &chargeIn, ChargeOutputMicro: &chargeOut}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if m.InputPriceMicro != 10 || m.ChargeOutputMicro != 40 || m.Status != StatusAvailable {
		t.Fatalf("model=%+v", m)
	}
	newPrice := int64(11)
	updated, err := svc.Update(ctx, m.ID, Input{InputPriceMicro: &newPrice}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InputPriceMicro != 11 {
		t.Fatalf("price not updated: %d", updated.InputPriceMicro)
	}
	if err := svc.SetStatus(ctx, m.ID, StatusDisabled, actor); err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.Get(ctx, m.ID)
	if err != nil || disabled.Status != StatusDisabled {
		t.Fatalf("disabled=%+v err=%v", disabled, err)
	}
	if err := svc.Delete(ctx, m.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, m.ID); err == nil {
		t.Fatal("soft-deleted model should not be visible")
	}
	recreated, err := svc.Create(ctx, Input{ProviderID: p.ID, Name: "Model New", ModelKey: "model-x"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if recreated.ID == m.ID {
		t.Fatal("expected new row after soft-delete reuse")
	}
	if _, err := pool.Exec(ctx, `UPDATE model SET deleted_at=now() WHERE id=$1`, recreated.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryIsIdempotentAndMarksMissingUnavailable(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	_, prov, svc := setupModel(t)
	ctx := context.Background()
	actor := audit.Actor{Type: "admin", ID: 1, RequestID: "discover-test"}
	p, err := prov.Create(ctx, provider.Input{Name: "discover", BaseURL: srv.URL + "/v1", Secret: "test-secret"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	models = []string{"alpha", "beta"}
	first, err := svc.Discover(ctx, p.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if first.Discovered != 2 || first.Added != 2 {
		t.Fatalf("first=%+v", first)
	}
	second, err := svc.Discover(ctx, p.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 2 {
		t.Fatalf("second=%+v", second)
	}
	models = []string{"alpha"}
	third, err := svc.Discover(ctx, p.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if third.Unavailable != 1 {
		t.Fatalf("third=%+v", third)
	}
	list, _, err := svc.List(ctx, p.ID, "", "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, m := range list {
		states[m.ModelKey] = m.Status
	}
	if states["alpha"] != "available" || states["beta"] != "unavailable" {
		t.Fatalf("states=%v", states)
	}
}
