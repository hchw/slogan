// Package redisx wraps Redis for rate limiting, score snapshot caching, the
// evaluation task queue and gateway node leases. Redis is never the source of
// truth for money or tasks; those live in PostgreSQL.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is a configured Redis client.
type Client struct {
	*redis.Client
}

// Open connects to Redis and verifies connectivity.
func Open(ctx context.Context, addr, password string, dbIndex int) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: dbIndex})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return &Client{Client: rdb}, nil
}

// Healthy reports whether Redis is reachable within the timeout.
func (c *Client) Healthy(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.Ping(cctx).Err()
}

// Allow implements a fixed-window limiter. On storage error it returns an
// error so callers can fail closed.
func (c *Client) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if limit <= 0 {
		return true, 0, nil
	}
	pipe := c.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, err
	}
	n := incr.Val()
	if n > int64(limit) {
		ttl, _ := c.TTL(ctx, key).Result()
		if ttl < 0 {
			ttl = window
		}
		return false, ttl, nil
	}
	return true, 0, nil
}

// SetSnapshot stores an immutable score snapshot for a version.
func (c *Client) SetSnapshot(ctx context.Context, versionID int64, payload []byte) error {
	return c.Set(ctx, snapshotKey(versionID), payload, 0).Err()
}

// GetSnapshot reads a previously stored score snapshot.
func (c *Client) GetSnapshot(ctx context.Context, versionID int64) ([]byte, error) {
	b, err := c.Get(ctx, snapshotKey(versionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return b, err
}

// SetActiveVersion records the currently active score version.
func (c *Client) SetActiveVersion(ctx context.Context, versionID int64) error {
	return c.Set(ctx, "routing:active_version", strconv.FormatInt(versionID, 10), 0).Err()
}

// ActiveVersion returns the active version ID, or 0 if unset.
func (c *Client) ActiveVersion(ctx context.Context) (int64, error) {
	s, err := c.Get(ctx, "routing:active_version").Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func snapshotKey(versionID int64) string {
	return "routing:scores:" + strconv.FormatInt(versionID, 10)
}

// ---- evaluation queue ----

// Enqueue adds a task to a Redis stream.
func (c *Client) Enqueue(ctx context.Context, stream string, values map[string]any) error {
	return c.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err()
}

// EnsureGroup creates a consumer group if missing (MKSTREAM).
func (c *Client) EnsureGroup(ctx context.Context, stream, group string) error {
	err := c.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && !stringsContains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Consume reads up to count messages for a consumer.
func (c *Client) Consume(ctx context.Context, stream, group, consumer string, count int64, block time.Duration) ([]redis.XMessage, error) {
	res, err := c.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}
	return res[0].Messages, nil
}

// Ack acknowledges a stream message.
func (c *Client) Ack(ctx context.Context, stream, group, id string) error {
	return c.XAck(ctx, stream, group, id).Err()
}

// ---- gateway node leases ----

// NodeState is the lease record for a gateway node.
type NodeState struct {
	ID                string
	Version           int64
	Ready             bool
	Drain             bool
	ClassifierReady   bool
	ClassifierVersion string
	ClassifierReason  string
	UpdatedAt         time.Time
}

// Heartbeat refreshes a gateway node lease.
func (c *Client) Heartbeat(ctx context.Context, nodeID string, version int64, ready, drain bool) error {
	key := "node:" + nodeID
	pipe := c.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"version":    strconv.FormatInt(version, 10),
		"ready":      strconv.FormatBool(ready),
		"drain":      strconv.FormatBool(drain),
		"updated_at": strconv.FormatInt(time.Now().Unix(), 10),
	})
	pipe.Expire(ctx, key, 30*time.Second)
	_, err := pipe.Exec(ctx)
	return err
}

// Nodes lists live gateway node leases.
func (c *Client) Nodes(ctx context.Context) (map[string]NodeState, error) {
	out := map[string]NodeState{}
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, "node:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			nodeID := trimNodePrefix(k)
			removed, err := c.IsRemoved(ctx, nodeID)
			if err != nil {
				return nil, err
			}
			if removed {
				continue
			}
			h, err := c.HGetAll(ctx, k).Result()
			if err != nil {
				return nil, err
			}
			v, _ := strconv.ParseInt(h["version"], 10, 64)
			ready, _ := strconv.ParseBool(h["ready"])
			drain, _ := strconv.ParseBool(h["drain"])
			// A missing classifier field means the node predates classifier
			// reporting; treat it as not degraded rather than as failed traffic.
			classifierReady := true
			if raw, ok := h["classifier_ready"]; ok {
				classifierReady, _ = strconv.ParseBool(raw)
			}
			updated := time.Time{}
			if ts, err := strconv.ParseInt(h["updated_at"], 10, 64); err == nil {
				updated = time.Unix(ts, 0).UTC()
			}
			out[nodeID] = NodeState{
				ID: nodeID, Version: v, Ready: ready, Drain: drain, UpdatedAt: updated,
				ClassifierReady: classifierReady, ClassifierVersion: h["classifier_version"], ClassifierReason: h["classifier_reason"],
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// ClassifierState is the minimal classifier health published by a gateway node.
type ClassifierState struct {
	Ready   bool
	Version string
	Reason  string
}

// HeartbeatClassifier records local classifier health without touching the
// node's traffic readiness: a degraded classifier must not look like a failed
// data-plane node, and must not look fully healthy either.
func (c *Client) HeartbeatClassifier(ctx context.Context, nodeID string, state ClassifierState) error {
	key := "node:" + nodeID
	pipe := c.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"classifier_ready":   strconv.FormatBool(state.Ready),
		"classifier_version": state.Version,
		"classifier_reason":  state.Reason,
		"updated_at":         strconv.FormatInt(time.Now().Unix(), 10),
	})
	pipe.Expire(ctx, key, 30*time.Second)
	_, err := pipe.Exec(ctx)
	return err
}

// MarkRemoved records a control-plane confirmation that a node has been
// removed from load-balancer traffic. The tombstone prevents its still-running
// process from re-registering a live lease until deployment explicitly clears it.
func (c *Client) MarkRemoved(ctx context.Context, nodeID string) error {
	return c.Set(ctx, "node-removed:"+nodeID, "1", 24*time.Hour).Err()
}

// IsRemoved reports whether deployment confirmed that the node is no longer
// eligible for traffic.
func (c *Client) IsRemoved(ctx context.Context, nodeID string) (bool, error) {
	n, err := c.Exists(ctx, "node-removed:"+nodeID).Result()
	return n > 0, err
}

// ClearRemoved explicitly permits a deployment to re-admit a previously removed node.
func (c *Client) ClearRemoved(ctx context.Context, nodeID string) error {
	return c.Del(ctx, "node-removed:"+nodeID).Err()
}

// ReleaseNode drops a node lease.
func (c *Client) ReleaseNode(ctx context.Context, nodeID string) error {
	return c.Del(ctx, "node:"+nodeID).Err()
}

func trimNodePrefix(k string) string {
	if len(k) > 5 && k[:5] == "node:" {
		return k[5:]
	}
	return k
}

func stringsContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
