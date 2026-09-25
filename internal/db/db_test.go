package db

import (
	"context"
	"os"
	"testing"
)

// testPool connects to TEST_DATABASE_URL and resets the public schema so each
// test run starts clean. Tests skip when the variable is unset.
func testPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire schema lock connection: %v", err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatalf("acquire schema lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`)
		lock.Release()
	})
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return pool
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	applied, err := Migrate(ctx, pool, nil)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("expected at least one migration to apply")
	}
	applied2, err := Migrate(ctx, pool, nil)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied2) != 0 {
		t.Fatalf("expected no migrations on rerun, got %v", applied2)
	}

	// Spot-check a couple of tables exist.
	for _, table := range []string{"app_user", "quota_ledger", "request_record", "score_version"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
			t.Fatalf("regclass %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %s to exist", table)
		}
	}
}

func TestPartialUniqueIndexAllowsRecreateAfterSoftDelete(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider (name, base_url) VALUES ('p','http://x')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider (name, base_url) VALUES ('p2','http://y')`); err != nil {
		t.Fatalf("insert second provider: %v", err)
	}
	// Soft delete the first model and confirm the business key can be reused.
	if _, err := pool.Exec(ctx, `INSERT INTO model (provider_id, name, model_key) VALUES (1,'m','k')`); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model SET deleted_at=now() WHERE model_key='k'`); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model (provider_id, name, model_key) VALUES (1,'m2','k')`); err != nil {
		t.Fatalf("reuse business key after soft delete: %v", err)
	}
}

func TestUniquePublishedScoreVersion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO score_version (status) VALUES ('published')`); err != nil {
		t.Fatalf("first published insert: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO score_version (status) VALUES ('published')`); err == nil {
		t.Fatal("expected a unique violation for a second published version")
	}
}
