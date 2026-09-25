package redisx

import (
	"context"
	"os"
	"testing"
	"time"
)

func testRedis(t *testing.T) *Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Redis integration test")
	}
	c, err := Open(context.Background(), addr, "", 0)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.FlushDB(context.Background()).Err()
	return c
}

func TestAllowFixedWindowAndFailClosedErrorContract(t *testing.T) {
	c := testRedis(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		ok, _, err := c.Allow(ctx, "test:limit", 2, time.Minute)
		if err != nil || !ok {
			t.Fatalf("request %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, retry, err := c.Allow(ctx, "test:limit", 2, time.Minute)
	if err != nil || ok || retry <= 0 {
		t.Fatalf("limit: ok=%v retry=%v err=%v", ok, retry, err)
	}
}

func TestSnapshotAndActiveVersion(t *testing.T) {
	c := testRedis(t)
	ctx := context.Background()
	if err := c.SetSnapshot(ctx, 42, []byte(`{"m":{}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSnapshot(ctx, 42)
	if err != nil || string(got) != `{"m":{}}` {
		t.Fatalf("snapshot=%s err=%v", got, err)
	}
	if err := c.SetActiveVersion(ctx, 42); err != nil {
		t.Fatal(err)
	}
	id, err := c.ActiveVersion(ctx)
	if err != nil || id != 42 {
		t.Fatalf("active=%d err=%v", id, err)
	}
}

func TestNodeLeaseAndStreamQueue(t *testing.T) {
	c := testRedis(t)
	ctx := context.Background()
	if err := c.Heartbeat(ctx, "gw-test", 7, true, false); err != nil {
		t.Fatal(err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || nodes["gw-test"].Version != 7 || !nodes["gw-test"].Ready {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	if err := c.EnsureGroup(ctx, "test:stream", "g"); err != nil {
		t.Fatal(err)
	}
	if err := c.Enqueue(ctx, "test:stream", map[string]any{"taskId": "1"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Consume(ctx, "test:stream", "g", "c1", 1, 100*time.Millisecond)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs=%v err=%v", msgs, err)
	}
	if err := c.Ack(ctx, "test:stream", "g", msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkRemoved(ctx, "gw-test"); err != nil {
		t.Fatal(err)
	}
	nodes, err = c.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := nodes["gw-test"]; exists {
		t.Fatal("confirmed removed node must leave the traffic-serving set")
	}
	removed, err := c.IsRemoved(ctx, "gw-test")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if err := c.ClearRemoved(ctx, "gw-test"); err != nil {
		t.Fatal(err)
	}
}
