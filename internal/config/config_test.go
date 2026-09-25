package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadRequiresDatabaseAndKey(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": "", "PROVIDER_SECRET_KEY": ""})
	if _, err := Load(); err == nil {
		t.Fatal("expected error when required config is missing")
	}
}

func TestLoadRejectsBadKey(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL":        "postgres://example",
		"PROVIDER_SECRET_KEY": "not-hex",
	})
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-hex secret key")
	}
}

func TestLoadReadsSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	// 32 bytes hex = 64 hex chars.
	hexKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if err := os.WriteFile(keyPath, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setEnv(t, map[string]string{
		"DATABASE_URL":             "postgres://example",
		"PROVIDER_SECRET_KEY":      "",
		"PROVIDER_SECRET_KEY_FILE": keyPath,
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.ProviderSecretKey) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(cfg.ProviderSecretKey))
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL":             "postgres://example",
		"PROVIDER_SECRET_KEY":      "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		"PROVIDER_SECRET_KEY_FILE": "",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Evaluation.MaxTokens != 2000 || cfg.Evaluation.Concurrency != 5 {
		t.Fatalf("unexpected eval defaults: %+v", cfg.Evaluation)
	}
	if cfg.RateLimits.UserPerMin != 600 {
		t.Fatalf("unexpected rate limit default: %d", cfg.RateLimits.UserPerMin)
	}
	if cfg.Laya.Confidence != 0.60 {
		t.Fatalf("unexpected laya confidence default: %v", cfg.Laya.Confidence)
	}
}
