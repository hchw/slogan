package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/usage"
)

// TestSettlementRecoveryIsIdempotent covers the crash-recovery path: a request
// that produced trusted usage but could not settle is retried by the worker
// without double charging.
func TestSettlementRecoveryIsIdempotent(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL required")
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
	authSvc := auth.New(pool, 24*time.Hour)
	user, err := authSvc.RegisterUser(ctx, "recovery@example.test", "RecoverySecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	q := quota.New(pool, aud)
	usg := usage.New(pool)
	system := audit.Actor{Type: "system", RequestID: "recovery-test"}
	if _, err := q.Adjust(ctx, system, user.ID, 1_000_000, "recovery seed", ""); err != nil {
		t.Fatal(err)
	}
	prov := provider.New(pool, make([]byte, 32), aud)
	created, err := prov.Create(ctx, provider.Input{Name: "recovery-provider", BaseURL: "https://example.invalid/v1", Secret: "s"}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	models := model.New(pool, prov, aud)
	m, err := models.Create(ctx, model.Input{ProviderID: created.ID, Name: "recovery-model", ModelKey: "recovery-model", ContextLength: 1024, InputModalities: []string{"text"}}, audit.Actor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "req-recovery-1"
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return q.Reserve(ctx, tx, user.ID, requestID, 50_000)
	}); err != nil {
		t.Fatal(err)
	}
	// A crash after the provider answered but before settlement committed.
	if _, err := pool.Exec(ctx, `INSERT INTO request_record
        (request_id, user_id, model_id, status, reserved_micro, usage_source, input_tokens, output_tokens, cost_micro, charge_micro)
        VALUES ($1,$2,$3,'settlement_pending',50000,'provider',10,5,1000,3000)`, requestID, user.ID, m.ID); err != nil {
		t.Fatal(err)
	}

	if err := recoverOnce(ctx, pool, q, usg); err != nil {
		t.Fatal(err)
	}
	acct, err := q.Account(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.ReservedMicro != 0 || acct.BalanceMicro != 1_000_000-3_000 {
		t.Fatalf("account after recovery=%+v", acct)
	}
	var status string
	var charge int64
	if err := pool.QueryRow(ctx, `SELECT status, charge_micro FROM request_record WHERE request_id=$1`, requestID).Scan(&status, &charge); err != nil {
		t.Fatal(err)
	}
	if status != "success" || charge != 3_000 {
		t.Fatalf("record after recovery status=%s charge=%d", status, charge)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_event WHERE request_id=$1`, requestID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("usage events=%d", events)
	}

	// Retrying the recovery must not charge twice.
	if err := recoverOnce(ctx, pool, q, usg); err != nil {
		t.Fatal(err)
	}
	again, err := q.Account(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.BalanceMicro != acct.BalanceMicro || again.ReservedMicro != acct.ReservedMicro {
		t.Fatalf("second recovery changed the ledger: %+v -> %+v", acct, again)
	}
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE ref_id=$1`, requestID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 2 { // reserve + consume
		t.Fatalf("ledger entries for request=%d", ledgerCount)
	}
}

// TestRecoveryReleasesUntrustedUsage asserts a pending record without trusted
// usage releases the reservation instead of estimating a charge.
func TestRecoveryReleasesUntrustedUsage(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL required")
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
	authSvc := auth.New(pool, 24*time.Hour)
	user, err := authSvc.RegisterUser(ctx, "recovery2@example.test", "RecoverySecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	q := quota.New(pool, aud)
	system := audit.Actor{Type: "system", RequestID: "recovery-test"}
	if _, err := q.Adjust(ctx, system, user.ID, 1_000_000, "recovery seed", ""); err != nil {
		t.Fatal(err)
	}
	const requestID = "req-recovery-2"
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error { return q.Reserve(ctx, tx, user.ID, requestID, 50_000) }); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO request_record
        (request_id, user_id, status, error_code, reserved_micro, usage_source, input_tokens, output_tokens, cost_micro, charge_micro)
        VALUES ($1,$2,'settlement_pending','upstream_error',50000,'none',0,0,0,0)`, requestID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := recoverOnce(ctx, pool, q, usage.New(pool)); err != nil {
		t.Fatal(err)
	}
	acct, err := q.Account(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.BalanceMicro != 1_000_000 || acct.ReservedMicro != 0 {
		t.Fatalf("untrusted usage must release the reservation: %+v", acct)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM request_record WHERE request_id=$1`, requestID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "upstream_error" {
		t.Fatalf("status after release=%s", status)
	}
}
