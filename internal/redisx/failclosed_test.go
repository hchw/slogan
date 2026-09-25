package redisx

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestUnavailableRedisFailsClosed documents the fail-closed contract that the
// API and gateway readiness probes rely on: an unreachable Redis must fail
// startup and, if it disappears later, must surface errors instead of silently
// allowing traffic without rate limits.
func TestUnavailableRedisFailsClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, "127.0.0.1:1", "", 0); err == nil {
		t.Fatal("startup must fail when Redis is unreachable")
	}

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR required for the runtime fail-closed check")
	}
	client, err := Open(ctx, addr, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Allow(ctx, "rl:test", 10, time.Minute); err != nil {
		t.Fatalf("healthy limiter should allow: %v", err)
	}
	_ = client.Close()
	// The backend disappearing at runtime must not silently bypass limits.
	if _, _, err := client.Allow(ctx, "rl:test", 10, time.Minute); err == nil {
		t.Fatal("rate limiting must fail closed when the store goes away")
	}
	if err := client.SetActiveVersion(ctx, 1); err == nil {
		t.Fatal("score snapshot writes must fail when the store goes away")
	}
}
