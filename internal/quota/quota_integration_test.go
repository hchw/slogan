package quota

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/secure"
)

func setup(t *testing.T) (*db.Pool, *Service, int64) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
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
		t.Fatalf("reset: %v", err)
	}
	if _, err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO app_user (email, password_hash) VALUES ('a@b.com','x') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO quota_account (user_id, balance_micro) VALUES ($1, 1000)`, userID); err != nil {
		t.Fatalf("account: %v", err)
	}
	return pool, New(pool, audit.New(pool)), userID
}

func TestReserveConcurrency(t *testing.T) {
	pool, svc, userID := setup(t)
	ctx := context.Background()

	const n = 20
	const amount = int64(100) // balance 1000 => exactly 10 should succeed
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rid := "req-" + itoa(int64(i))
			err := pool.WithTx(ctx, func(tx pgx.Tx) error {
				return svc.Reserve(ctx, tx, userID, rid, amount)
			})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if success != 10 {
		t.Fatalf("expected exactly 10 successful reservations, got %d", success)
	}
	acct, _ := svc.Account(ctx, userID)
	if acct.ReservedMicro != 1000 || acct.AvailableMicro != 0 {
		t.Fatalf("unexpected account after reservations: %+v", acct)
	}
}

func TestSettleIdempotentAndRelease(t *testing.T) {
	pool, svc, userID := setup(t)
	ctx := context.Background()

	rid := "req-settle"
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return svc.Reserve(ctx, tx, userID, rid, 500)
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Settle twice with the same request: only one charge.
	var first, second int64
	_ = pool.WithTx(ctx, func(tx pgx.Tx) error {
		s, err := svc.Settle(ctx, tx, userID, rid, 500, 300)
		if err != nil {
			return err
		}
		first = s.ChargeMicro
		return nil
	})
	_ = pool.WithTx(ctx, func(tx pgx.Tx) error {
		s, err := svc.Settle(ctx, tx, userID, rid, 500, 300)
		if err != nil {
			return err
		}
		second = s.ChargeMicro
		return nil
	})
	if first != 300 || second != 300 {
		t.Fatalf("expected idempotent charge 300, got first=%d second=%d", first, second)
	}
	acct, _ := svc.Account(ctx, userID)
	if acct.BalanceMicro != 700 || acct.ReservedMicro != 0 {
		t.Fatalf("unexpected account after settle: %+v", acct)
	}

	var ledgerCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE ref_id=$1 AND type='consume'`, rid).Scan(&ledgerCount)
	if ledgerCount != 1 {
		t.Fatalf("expected exactly one consume ledger entry, got %d", ledgerCount)
	}
}

func TestSettleWithoutUsageReleases(t *testing.T) {
	pool, svc, userID := setup(t)
	ctx := context.Background()
	rid := "req-release"
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return svc.Reserve(ctx, tx, userID, rid, 400)
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	var charged bool
	_ = pool.WithTx(ctx, func(tx pgx.Tx) error {
		s, err := svc.Settle(ctx, tx, userID, rid, 400, 0)
		if err != nil {
			return err
		}
		charged = s.Charged
		return nil
	})
	if charged {
		t.Fatal("expected no charge when there is no trusted usage")
	}
	acct, _ := svc.Account(ctx, userID)
	if acct.BalanceMicro != 1000 || acct.ReservedMicro != 0 {
		t.Fatalf("expected full release, got %+v", acct)
	}
}

func TestRedeemConcurrentExactlyOnce(t *testing.T) {
	pool, svc, userID := setup(t)
	ctx := context.Background()

	var pkgID, batchID int64
	if err := pool.QueryRow(ctx, `INSERT INTO quota_package (name, face_value_micro) VALUES ('p', 5000) RETURNING id`).Scan(&pkgID); err != nil {
		t.Fatalf("package: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO redemption_code_batch (package_id, quantity) VALUES ($1,1) RETURNING id`, pkgID).Scan(&batchID); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO redemption_code (batch_id, package_id, code_hash, code_prefix) VALUES ($1, $2, $3, 'QZ-TEST')`, batchID, pkgID, secure.HashToken("QZ-TESTCODE")); err != nil {
		t.Fatalf("code: %v", err)
	}

	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := svc.Redeem(ctx, userID, "QZ-TESTCODE"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("expected exactly one successful redemption, got %d", ok)
	}
	acct, _ := svc.Account(ctx, userID)
	if acct.BalanceMicro != 6000 {
		t.Fatalf("expected balance 6000 after grant, got %d", acct.BalanceMicro)
	}
	var grants int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE type='grant'`).Scan(&grants)
	if grants != 1 {
		t.Fatalf("expected exactly one grant ledger entry, got %d", grants)
	}
}

func TestCodeBatchGenerationIsUniqueAndQueryable(t *testing.T) {
	pool, svc, _ := setup(t)
	ctx := context.Background()
	var packageID int64
	if err := pool.QueryRow(ctx, `INSERT INTO quota_package (name, face_value_micro) VALUES ('bulk',2500) RETURNING id`).Scan(&packageID); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	batch, err := svc.GenerateCodes(ctx, audit.Actor{Type: "system", RequestID: "codes"}, packageID, 50, &expires)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Quantity != 50 || len(batch.Codes) != 50 {
		t.Fatalf("batch=%+v", batch)
	}
	seen := map[string]bool{}
	for _, code := range batch.Codes {
		if seen[code] {
			t.Fatalf("duplicate generated code %s", code)
		}
		seen[code] = true
	}
	list, total, err := svc.ListCodes(ctx, batch.BatchID, "unused", 100, 0)
	if err != nil || total != 50 || len(list) != 50 {
		t.Fatalf("list=%d total=%d err=%v", len(list), total, err)
	}
	// Soft-deleting a code releases its partial unique key for a controlled
	// rebuild without physically deleting audit/history rows.
	hash := secure.HashToken(batch.Codes[0])
	if _, err := pool.Exec(ctx, `UPDATE redemption_code SET deleted_at=now() WHERE code_hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	var nextBatch int64
	if err := pool.QueryRow(ctx, `INSERT INTO redemption_code_batch(package_id,quantity) VALUES($1,1) RETURNING id`, packageID).Scan(&nextBatch); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO redemption_code(batch_id,package_id,code_hash,code_prefix) VALUES($1,$2,$3,'QZ-REBUILD')`, nextBatch, packageID, hash); err != nil {
		t.Fatalf("rebuild after soft-delete: %v", err)
	}
}

func TestRedeemExpired(t *testing.T) {
	pool, svc, userID := setup(t)
	ctx := context.Background()
	var pkgID, batchID int64
	_ = pool.QueryRow(ctx, `INSERT INTO quota_package (name, face_value_micro) VALUES ('p', 100) RETURNING id`).Scan(&pkgID)
	_ = pool.QueryRow(ctx, `INSERT INTO redemption_code_batch (package_id, quantity) VALUES ($1,1) RETURNING id`, pkgID).Scan(&batchID)
	if _, err := pool.Exec(ctx, `INSERT INTO redemption_code (batch_id, package_id, code_hash, code_prefix, expires_at)
        VALUES ($1, $2, $3, 'QZ-EXP', now() - interval '1 hour')`, batchID, pkgID, secure.HashToken("QZ-EXPIRED")); err != nil {
		t.Fatalf("code: %v", err)
	}
	if _, _, err := svc.Redeem(ctx, userID, "QZ-EXPIRED"); err == nil {
		t.Fatal("expected expired code to be rejected")
	}
	acct, _ := svc.Account(ctx, userID)
	if acct.BalanceMicro != 1000 {
		t.Fatalf("balance must be unchanged, got %d", acct.BalanceMicro)
	}
}
