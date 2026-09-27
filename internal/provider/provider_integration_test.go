package provider

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
)

func testProviderService(t *testing.T) (*db.Pool, *Service) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping provider integration test")
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
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return pool, New(pool, key, audit.New(pool))
}

func TestProviderSecretEncryptedMaskedAndSoftDeleted(t *testing.T) {
	pool, svc := testProviderService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, Input{Name: "test-provider", BaseURL: "http://127.0.0.1:19000/v1", Secret: "sk-super-secret"}, audit.Actor{Type: "admin", ID: 1, RequestID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if p.SecretMasked == "sk-super-secret" || p.SecretMasked == "" {
		t.Fatalf("secret was not masked: %q", p.SecretMasked)
	}
	var cipher string
	if err := pool.QueryRow(ctx, `SELECT secret_cipher FROM provider WHERE id=$1`, p.ID).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if cipher == "sk-super-secret" {
		t.Fatal("plaintext secret persisted")
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM audit_log WHERE action='provider.create'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "sk-super-secret") {
		t.Fatal("provider secret leaked into audit detail")
	}
	listed, _, err := svc.List(ctx, "", "", 20, 0)
	if err != nil || len(listed) != 1 || listed[0].SecretMasked != "" {
		t.Fatalf("safe list=%+v err=%v", listed, err)
	}
	plain, err := svc.Secret(ctx, p.ID)
	if err != nil || plain != "sk-super-secret" {
		t.Fatalf("decrypt=%q err=%v", plain, err)
	}
	if err := svc.SetStatus(ctx, p.ID, "disabled", audit.Actor{Type: "admin", ID: 1, RequestID: "status"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := svc.Get(ctx, p.ID)
	if err != nil || loaded.Status != "disabled" {
		t.Fatalf("get=%+v err=%v", loaded, err)
	}
	if err := svc.Delete(ctx, p.ID, audit.Actor{Type: "admin", ID: 1, RequestID: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, p.ID); err == nil {
		t.Fatal("soft-deleted provider should be hidden")
	}
	list, _, err := svc.List(ctx, "", "", 20, 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestProviderCannotDeleteWhileModelsExist(t *testing.T) {
	pool, svc := testProviderService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, Input{Name: "has-model", BaseURL: "http://x", Secret: "x"}, audit.Actor{Type: "admin", ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model(provider_id,name,model_key) VALUES($1,'m','m')`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, p.ID, audit.Actor{Type: "admin", ID: 1}); err == nil {
		t.Fatal("expected conflict deleting provider with model")
	}
}

func TestAdapterConnectionTestSanitizesAndTimesOut(t *testing.T) {
	// A syntactically invalid endpoint fails without echoing credentials.
	c := NewClient("http://[::1", "very-secret", "bearer", 10*time.Millisecond)
	if _, err := c.Test(context.Background()); err == nil || containsText(err.Error(), "very-secret") {
		t.Fatalf("expected sanitized error, got %v", err)
	}
}

func containsText(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexText(s, sub) >= 0
}
func indexText(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
