package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/redisx"
)

// RunNode maintains this gateway node's lease and keeps the active score
// snapshot loaded. The node reports the version it has actually loaded so the
// control plane can enforce the publication ACK barrier.
func (s *Service) RunNode(ctx context.Context, nodeID string) {
	loaded := int64(0)
	load := func() {
		if removed, err := s.rdb.IsRemoved(ctx, nodeID); err == nil && removed {
			_ = s.rdb.ReleaseNode(ctx, nodeID)
			return
		}
		versionID := s.publishedVersion(ctx)
		if versionID != 0 && versionID != loaded {
			if err := s.cacheSnapshot(ctx, versionID); err == nil {
				loaded = versionID
			}
		}
		// The drain flag is a function of ACK state, not a sticky operator flag:
		// a node behind the published version is draining and not ready, and it
		// clears itself once the snapshot is loaded. Traffic removal by the load
		// balancer is handled separately through the membership adapter.
		draining := versionID != 0 && loaded != versionID
		_ = s.rdb.Heartbeat(ctx, nodeID, loaded, !draining, draining)
		// Classifier state is reported separately so a degraded classifier never
		// looks like a traffic-serving failure, and never looks fully healthy.
		classifier := s.ClassifierState(ctx)
		_ = s.rdb.HeartbeatClassifier(ctx, nodeID, redisx.ClassifierState{Ready: classifier.Ready, Version: classifier.Version, Reason: classifier.Reason})
	}
	load()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = s.rdb.ReleaseNode(context.Background(), nodeID)
			return
		case <-ticker.C:
			load()
		}
	}
}

// ClassifierState is the local classifier health reported to the control plane.
type ClassifierState struct {
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ClassifierState reports this node's semantic classifier health. A disabled
// classifier is not degraded; an enabled but unready one is.
func (s *Service) ClassifierState(ctx context.Context) ClassifierState {
	state := ClassifierState{Enabled: s.laya.Enabled(), Ready: true}
	if !state.Enabled {
		return state
	}
	ready := s.laya.Ready(ctx)
	state.Ready = ready.ClassifierReady
	state.Version = ready.Version
	state.Reason = ready.Reason
	return state
}

func (s *Service) publishedVersion(ctx context.Context) int64 {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM score_version WHERE status='published' LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	return id
}

func (s *Service) cacheSnapshot(ctx context.Context, versionID int64) error {
	if b, err := s.rdb.GetSnapshot(ctx, versionID); err == nil && b != nil {
		return s.rdb.SetActiveVersion(ctx, versionID)
	}
	rows, err := s.pool.Query(ctx, `SELECT model_id, dimension, score FROM model_capability WHERE score_version_id=$1`, versionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	caps := map[int64]map[string]float64{}
	for rows.Next() {
		var modelID int64
		var dim string
		var score float64
		if err := rows.Scan(&modelID, &dim, &score); err != nil {
			return err
		}
		if caps[modelID] == nil {
			caps[modelID] = map[string]float64{}
		}
		caps[modelID][dim] = score
	}
	if err := rows.Err(); err != nil {
		return err
	}
	payload, _ := json.Marshal(caps)
	if err := s.rdb.SetSnapshot(ctx, versionID, payload); err != nil {
		return err
	}
	return s.rdb.SetActiveVersion(ctx, versionID)
}
