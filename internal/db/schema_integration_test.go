package db

import (
	"context"
	"testing"
)

func TestForeignKeysAndSoftDeleteBusinessKeyReuse(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model (provider_id, name, model_key) VALUES (999999,'x','x')`); err == nil {
		t.Fatal("expected model provider foreign key to reject missing provider")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_user (email,password_hash) VALUES ('reuse@example.test','hash')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_user SET deleted_at=now() WHERE email='reuse@example.test'`); err != nil {
		t.Fatalf("soft-delete user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_user (email,password_hash) VALUES ('reuse@example.test','hash2')`); err != nil {
		t.Fatalf("reuse soft-deleted email: %v", err)
	}
}
