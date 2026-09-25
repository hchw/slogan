package requestrec

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/db"
)

func setupRequestRecords(t *testing.T) (*db.Pool, int64) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping request record integration test")
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
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO app_user(email,password_hash) VALUES('req@example.test','x') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	return pool, uid
}

func TestRequestLifecycleTerminalStateAndIdempotentBegin(t *testing.T) {
	pool, userID := setupRequestRecords(t)
	ctx := context.Background()
	svc := NewService(pool)
	n := New{RequestID: "req-state", UserID: userID, RequestedModel: "model", Status: StatusReserved, IdempotencyKey: "idem-state", RequestDigest: "digest"}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error { _, _, err := svc.Begin(ctx, tx, n); return err }); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error { return svc.SetUpstreamPending(ctx, tx, n.RequestID) }); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return svc.Finalize(ctx, tx, Finalize{RequestID: n.RequestID, Status: StatusSuccess})
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return svc.Finalize(ctx, tx, Finalize{RequestID: n.RequestID, Status: StatusUpstreamError, ErrorCode: "late"})
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.Get(ctx, n.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusSuccess || rec.ErrorCode != "" {
		t.Fatalf("terminal state changed: %+v", rec)
	}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return svc.Finalize(ctx, tx, Finalize{RequestID: "x", Status: StatusUpstreamPending})
	}); err == nil {
		t.Fatal("nonterminal final status accepted")
	}
	var duplicate *Record
	var existed bool
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		duplicate, existed, err = svc.Begin(ctx, tx, New{RequestID: "other-request-id", UserID: userID, IdempotencyKey: "idem-state", RequestDigest: "digest"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !existed || duplicate == nil || duplicate.RequestID != n.RequestID {
		t.Fatalf("duplicate begin was not resolved: exists=%v rec=%+v", existed, duplicate)
	}
}
