package audit

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/db"
)

func TestAuditFailureRollsBackProtectedWrite(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
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

	svc := New(pool)
	err = pool.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO provider (name,base_url) VALUES ('atomic','http://local')`); err != nil {
			return err
		}
		return svc.WriteInTx(ctx, tx, Entry{ActorType: "admin", Action: strings.Repeat("x", 70), TargetType: "provider"})
	})
	if err == nil {
		t.Fatal("expected audit constraint failure")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider WHERE name='atomic'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("business write survived audit rollback: count=%d", count)
	}
}
