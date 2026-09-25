package metrics

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/redisx"
)

func TestPostgresCollectorReportsBillingAndNodeMetrics(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if url == "" || redisAddr == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_ADDR required")
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
	rdb, err := redisx.Open(ctx, redisAddr, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	_ = rdb.FlushDB(ctx).Err()

	aud := audit.New(pool)
	authSvc := auth.New(pool, 24*time.Hour)
	user, err := authSvc.RegisterUser(ctx, "metrics@example.test", "MetricsSecurePass123!", "")
	if err != nil {
		t.Fatal(err)
	}
	q := quota.New(pool, aud)
	system := audit.Actor{Type: "system", RequestID: "metrics-test"}
	if _, err := q.Adjust(ctx, system, user.ID, 5_000_000, "metrics seed", ""); err != nil {
		t.Fatal(err)
	}
	// One settled request, one reservation still open.
	if _, err := pool.Exec(ctx, `INSERT INTO request_record (request_id, user_id, status, usage_source, input_tokens, output_tokens, cost_micro, charge_micro, finished_at)
        VALUES ('req-metrics-1',$1,'success','provider',100,40,3000,9000,now())`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTx(ctx, func(tx pgx.Tx) error {
		return q.Reserve(ctx, tx, user.ID, "req-metrics-2", 5000)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO usage_record (stat_date, user_id, model_id, provider_id, request_count, input_tokens, output_tokens, cost_micro, charge_micro)
        VALUES (current_date,$1,0,0,1,100,40,3000,9000)`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Heartbeat(ctx, "gw-ready", 7, true, false); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HeartbeatClassifier(ctx, "gw-ready", redisx.ClassifierState{Ready: false, Reason: "checkpoint_not_ready"}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Heartbeat(ctx, "gw-stale", 3, true, false); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HeartbeatClassifier(ctx, "gw-stale", redisx.ClassifierState{Ready: true}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SetActiveVersion(ctx, 7); err != nil {
		t.Fatal(err)
	}

	samples, err := NewPostgresCollector(pool, rdb).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	labels := map[string]string{}
	for _, s := range samples {
		key := s.Name
		for k, v := range s.Labels {
			key += "|" + k + "=" + v
		}
		values[key] = s.Value
		labels[key] = s.Name
	}
	for key, want := range map[string]float64{
		"slogan_gateway_requests_24h|status=success":   1,
		"slogan_gateway_tokens_total|direction=input":  100,
		"slogan_gateway_tokens_total|direction=output": 40,
		"slogan_gateway_cost_micro_7d":                 3000,
		"slogan_gateway_charge_micro_7d":               9000,
		"slogan_gateway_reservations_open":             1,
		"slogan_gateway_reserved_micro":                5000,
		"slogan_gateway_nodes_serving":                 2,
		"slogan_gateway_nodes_classifier_degraded":     1,
		"slogan_gateway_nodes_score_version_stale":     1,
		"slogan_gateway_score_version_active":          7,
		"slogan_gateway_settlements_total":             0,
	} {
		if values[key] != want {
			t.Fatalf("metric %s = %v want %v (all=%v)", key, values[key], want, values)
		}
	}
	// No label may carry a user identifier or free text.
	for _, s := range samples {
		for k, v := range s.Labels {
			if k == "user_id" || k == "user" || k == "prompt" || strings.Contains(v, "metrics@example.test") {
				t.Fatalf("metric %s leaks identifying label %s=%s", s.Name, k, v)
			}
		}
	}
}
