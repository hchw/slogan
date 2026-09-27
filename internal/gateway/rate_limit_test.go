package gateway

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/redisx"
)

func TestMultiLevelLimiterReturnsRetryAfterAndFailsClosed(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	rdb, err := redisx.Open(context.Background(), addr, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	_ = rdb.FlushDB(context.Background()).Err()
	svc := &Service{cfg: &config.Config{RateLimits: config.RateLimitConfig{GlobalPerMin: 1}}, rdb: rdb}
	key := &auth.APIKey{ID: 1, UserID: 2}
	if err := svc.checkRateLimits(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	err = svc.checkRateLimits(context.Background(), key)
	app, ok := apperr.Is(err)
	if !ok || app.HTTP != 429 || app.Code != "rate_limit_exceeded" || app.RetryAfter <= 0 {
		t.Fatalf("limit error=%v", err)
	}

	dead := redisx.Client{Client: redis.NewClient(&redis.Options{Addr: net.JoinHostPort("127.0.0.1", "1"), DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, WriteTimeout: 10 * time.Millisecond})}
	defer dead.Close()
	closed := &Service{cfg: &config.Config{RateLimits: config.RateLimitConfig{GlobalPerMin: 1}}, rdb: &dead}
	err = closed.checkRateLimits(context.Background(), key)
	app, ok = apperr.Is(err)
	if !ok || app.HTTP != 503 {
		t.Fatalf("redis failure did not fail closed: %v", err)
	}
}

func TestModelAndProviderRateLimitsAreApplied(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	rdb, err := redisx.Open(context.Background(), addr, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	_ = rdb.FlushDB(context.Background()).Err()
	svc := &Service{cfg: &config.Config{RateLimits: config.RateLimitConfig{ModelPerMin: 1, ProviderPerMin: 1}}, rdb: rdb}
	m := &model.Model{ID: 9, ProviderID: 8}
	if err := svc.checkModelProviderRateLimits(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := svc.checkModelProviderRateLimits(context.Background(), m); err == nil {
		t.Fatal("second model/provider request should be limited")
	}
}
