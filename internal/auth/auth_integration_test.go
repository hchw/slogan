package auth

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/secure"
)

func testAuthService(t *testing.T) (*db.Pool, *Service) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping auth integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire schema lock: %v", err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(742991884)`); err != nil {
		lock.Release()
		t.Fatalf("schema lock: %v", err)
	}
	t.Cleanup(func() { _, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(742991884)`); lock.Release() })
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := New(pool, 24*time.Hour)
	if err := svc.EnsureSeedRoles(ctx); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	return pool, svc
}

func TestRegisterValidatesAndCreatesQuotaAccountOnce(t *testing.T) {
	pool, svc := testAuthService(t)
	ctx := context.Background()
	if _, err := svc.RegisterUser(ctx, "bad", "short", ""); err == nil {
		t.Fatal("expected invalid registration")
	}
	if _, err := svc.RegisterUser(ctx, "user@example.test", "short", ""); err == nil {
		t.Fatal("expected weak password rejection")
	}
	u, err := svc.RegisterUser(ctx, "User@Example.test", "UserSecurePass123!", "Dev")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if u.Email != "user@example.test" {
		t.Fatalf("email not normalized: %s", u.Email)
	}
	if _, err := svc.RegisterUser(ctx, "user@example.test", "UserSecurePass123!", "Again"); err == nil {
		t.Fatal("expected duplicate email error")
	}
	var accounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quota_account WHERE user_id=$1`, u.ID).Scan(&accounts); err != nil || accounts != 1 {
		t.Fatalf("quota account count=%d err=%v", accounts, err)
	}
}

func TestSessionsRevokeAndDisabledPrincipal(t *testing.T) {
	_, svc := testAuthService(t)
	ctx := context.Background()
	u, err := svc.RegisterUser(ctx, "session@example.test", "UserSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.CreateUserSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.ValidateSession(ctx, token)
	if err != nil || p.ID != u.ID || p.Type != "user" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	if err := svc.RevokeSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateSession(ctx, token); err == nil {
		t.Fatal("revoked session must fail")
	}

	expired, _, err := svc.CreateUserSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx, `UPDATE session SET expires_at=now()-interval '1 minute' WHERE token_hash=$1`, secure.HashToken(expired)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateSession(ctx, expired); err == nil {
		t.Fatal("expired session must fail")
	}

	token2, _, _ := svc.CreateUserSession(ctx, u.ID)
	if err := svc.SetUserStatus(ctx, u.ID, "disabled", structActor()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateSession(ctx, token2); err == nil {
		t.Fatal("disabled user session must fail")
	}
}

func TestAdminBootstrapIsOneTimeAndSeedsRBAC(t *testing.T) {
	_, svc := testAuthService(t)
	ctx := context.Background()
	admin, err := svc.Bootstrap(ctx, "root", "AdminSecurePass123!", "Root")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !admin.Has("provider:create") || !admin.Has("evaluation:publish") {
		t.Fatalf("permissions not assigned: %+v", admin.Permissions)
	}
	if _, err := svc.Bootstrap(ctx, "second", "AdminSecurePass123!", "Second"); err == nil {
		t.Fatal("bootstrap must be one-time")
	}
	loggedIn, err := svc.AuthenticateAdmin(ctx, "root", "AdminSecurePass123!")
	if err != nil || loggedIn.ID != admin.ID {
		t.Fatalf("login: %+v %v", loggedIn, err)
	}
	if _, err := svc.AuthenticateAdmin(ctx, "root", "wrong password"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestAPIKeyStoredAsVerifierAndRevocable(t *testing.T) {
	pool, svc := testAuthService(t)
	ctx := context.Background()
	u, err := svc.RegisterUser(ctx, "key@example.test", "UserSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	var providerID int64
	if err := pool.QueryRow(ctx, `INSERT INTO provider(name,base_url) VALUES('scope','http://local') RETURNING id`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model(provider_id,name,model_key) VALUES($1,'Scoped','scoped-model')`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateAPIKey(ctx, u.ID, "invalid-scope", nil, 0, []string{"missing-model"}); err == nil {
		t.Fatal("unknown allowed model accepted")
	}
	view, raw, err := svc.CreateAPIKey(ctx, u.ID, "test", nil, 50, []string{"scoped-model"})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || view.KeyPrefix == raw {
		t.Fatal("expected one-time secret and distinct prefix")
	}
	key, err := svc.AuthenticateAPIKey(ctx, raw)
	if err != nil || key.ID != view.ID {
		t.Fatalf("authenticate: %+v %v", key, err)
	}
	if len(key.AllowedModels) != 1 || key.AllowedModels[0] != "scoped-model" {
		t.Fatalf("allowed models=%v", key.AllowedModels)
	}
	listedKeys, _, err := svc.ListAPIKeys(ctx, u.ID, 20, 0)
	if err != nil || len(listedKeys) != 1 || len(listedKeys[0].AllowedModels) != 1 {
		t.Fatalf("safe key list=%+v err=%v", listedKeys, err)
	}
	listJSON, _ := json.Marshal(listedKeys)
	if strings.Contains(string(listJSON), raw) {
		t.Fatal("key list returned the one-time secret")
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM api_key WHERE id=$1`, view.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw {
		t.Fatal("raw API key was stored")
	}
	// Force one collision with an existing verifier; creation must retry.
	unique, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	svc.generateKey = func() (secure.APIKey, error) {
		attempts++
		if attempts == 1 {
			return secure.APIKey{Full: raw, Prefix: view.KeyPrefix, Hash: secure.HashToken(raw)}, nil
		}
		return unique, nil
	}
	otherForRetry, err := svc.RegisterUser(ctx, "retry@example.test", "RetrySecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	_, retriedRaw, err := svc.CreateAPIKey(ctx, otherForRetry.ID, "retry", nil, 0, nil)
	if err != nil || retriedRaw != unique.Full || attempts != 2 {
		t.Fatalf("collision retry raw=%q attempts=%d err=%v", retriedRaw, attempts, err)
	}
	svc.generateKey = secure.NewAPIKey
	other, err := svc.RegisterUser(ctx, "other-key@example.test", "OtherSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAPIKeyStatus(ctx, other.ID, view.ID, "disabled"); err == nil {
		t.Fatal("another user must not control this key")
	}
	otherKeys, _, err := svc.ListAPIKeys(ctx, other.ID, 20, 0)
	if err != nil || len(otherKeys) != 0 {
		t.Fatalf("cross-user key list=%v err=%v", otherKeys, err)
	}
	if err := svc.SetAPIKeyStatus(ctx, u.ID, view.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateAPIKey(ctx, raw); err == nil {
		t.Fatal("disabled key accepted")
	}
	if err := svc.SetAPIKeyStatus(ctx, u.ID, view.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeAPIKey(ctx, u.ID, view.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateAPIKey(ctx, raw); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func structActor() audit.Actor { return audit.Actor{Type: "admin", ID: 1, RequestID: "test-request"} }
