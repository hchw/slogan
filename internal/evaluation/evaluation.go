// Package evaluation runs score refreshes, aggregates capabilities, publishes
// immutable score versions and enforces the multi-node activation barrier.
package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/redisx"
)

// Dimensions scored by the evaluation template.
var Dimensions = []string{
	"coding", "reasoning", "writing", "translation", "summarization",
	"casual_chat", "emotional_support", "vision", "tool_calling", "structured_output",
}

// StreamName is the Redis stream carrying evaluation tasks.
const StreamName = "eval:tasks"

// Service runs evaluation and owns score versions.
type Service struct {
	pool       *db.Pool
	rdb        *redisx.Client
	models     *model.Service
	provider   *provider.Service
	policy     *policy.Service
	audit      *audit.Service
	cfg        config.EvaluationConfig
	membership MembershipAdapter
	ackTimeout time.Duration
}

// New builds an evaluation service.
func New(pool *db.Pool, rdb *redisx.Client, models *model.Service, prov *provider.Service,
	pol *policy.Service, aud *audit.Service, cfg config.EvaluationConfig) *Service {
	return &Service{pool: pool, rdb: rdb, models: models, provider: prov, policy: pol, audit: aud, cfg: cfg, membership: noMembershipAdapter{}, ackTimeout: 15 * time.Second}
}

// SetMembershipAdapter installs the deployment-specific drain/removal checker.
func (s *Service) SetMembershipAdapter(adapter MembershipAdapter) {
	if adapter == nil {
		s.membership = noMembershipAdapter{}
		return
	}
	s.membership = adapter
}

// SetAckTimeout overrides the score activation ACK timeout (primarily useful in tests).
func (s *Service) SetAckTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.ackTimeout = timeout
	}
}

// NodeInfo is the control-plane view of one gateway node: which score version
// it has actually loaded, whether it is taking traffic, and its classifier
// health. A degraded classifier is reported explicitly instead of being folded
// into traffic readiness.
type NodeInfo struct {
	ID                string     `json:"id"`
	ScoreVersionID    int64      `json:"scoreVersionId"`
	Ready             bool       `json:"ready"`
	Drain             bool       `json:"drain"`
	ClassifierReady   bool       `json:"classifierReady"`
	ClassifierVersion string     `json:"classifierVersion,omitempty"`
	ClassifierReason  string     `json:"classifierReason,omitempty"`
	Degraded          bool       `json:"degraded"`
	LastSeen          *time.Time `json:"lastSeen,omitempty"`
}

// Nodes lists live gateway node leases for the admin console.
func (s *Service) Nodes(ctx context.Context) ([]NodeInfo, error) {
	states, err := s.rdb.Nodes(ctx)
	if err != nil {
		return nil, apperr.Internal("failed to read node leases").WithCause(err)
	}
	out := make([]NodeInfo, 0, len(states))
	for _, st := range states {
		info := NodeInfo{
			ID: st.ID, ScoreVersionID: st.Version, Ready: st.Ready, Drain: st.Drain,
			ClassifierReady: st.ClassifierReady, ClassifierVersion: st.ClassifierVersion,
			ClassifierReason: st.ClassifierReason, Degraded: !st.ClassifierReady,
		}
		if !st.UpdatedAt.IsZero() {
			seen := st.UpdatedAt
			info.LastSeen = &seen
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ActiveVersion returns the published score version ID (0 if none).
func (s *Service) ActiveVersion(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM score_version WHERE status='published' LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Capabilities loads the effective capability matrix for a score version: the
// models scored by this refresh plus, for any participant the refresh could not
// score, the most recent historical score for that model. Historical fallback
// keeps an existing model routable and is never counted as this refresh's new
// score coverage. Models with no historical score are intentionally omitted so
// they are never auto-routed on fabricated ability.
func (s *Service) Capabilities(ctx context.Context, versionID int64) (map[int64]map[string]float64, error) {
	fresh, err := s.NewScores(ctx, versionID)
	if err != nil {
		return nil, err
	}
	fallback, err := s.historicalScores(ctx, versionID)
	if err != nil {
		return nil, err
	}
	for modelID, dims := range fallback {
		if _, ok := fresh[modelID]; ok {
			continue
		}
		fresh[modelID] = dims
	}
	return fresh, nil
}

// NewScores loads only the capability rows produced by this refresh.
func (s *Service) NewScores(ctx context.Context, versionID int64) (map[int64]map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `SELECT model_id, dimension, score FROM model_capability WHERE score_version_id=$1`, versionID)
	if err != nil {
		return nil, err
	}
	return scanScores(rows)
}

// historicalScores returns, for each participant this version failed to score,
// the newest earlier score for that model.
func (s *Service) historicalScores(ctx context.Context, versionID int64) (map[int64]map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (mc.model_id, mc.dimension) mc.model_id, mc.dimension, mc.score
        FROM model_capability mc
        JOIN refresh_task t ON t.score_version_id=$1
        JOIN refresh_task_item i ON i.task_id=t.id AND i.model_id=mc.model_id
        WHERE mc.score_version_id < $1
          AND NOT EXISTS (SELECT 1 FROM model_capability n WHERE n.score_version_id=$1 AND n.model_id=mc.model_id)
        ORDER BY mc.model_id, mc.dimension, mc.score_version_id DESC`, versionID)
	if err != nil {
		return nil, err
	}
	return scanScores(rows)
}

// FallbackModels lists participants served by historical scores in a version.
func (s *Service) FallbackModels(ctx context.Context, versionID int64) ([]int64, error) {
	scores, err := s.historicalScores(ctx, versionID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(scores))
	for id := range scores {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func scanScores(rows pgx.Rows) (map[int64]map[string]float64, error) {
	defer rows.Close()
	out := map[int64]map[string]float64{}
	for rows.Next() {
		var modelID int64
		var dim string
		var score float64
		if err := rows.Scan(&modelID, &dim, &score); err != nil {
			return nil, err
		}
		if out[modelID] == nil {
			out[modelID] = map[string]float64{}
		}
		out[modelID][dim] = score
	}
	return out, rows.Err()
}

// FrozenModel is the provider/model metadata captured at refresh start. Secret
// material is deliberately excluded; current secret lookup remains controlled
// by the provider credential service.
type FrozenModel struct {
	ID                int64    `json:"modelId"`
	Name              string   `json:"name"`
	ModelKey          string   `json:"modelKey"`
	ProviderID        int64    `json:"providerId"`
	ProviderName      string   `json:"providerName"`
	ProviderBaseURL   string   `json:"providerBaseUrl"`
	ProviderAuthType  string   `json:"providerAuthType"`
	ProviderProtocol  string   `json:"providerProtocol"`
	ContextLength     int      `json:"contextLength"`
	InputModalities   []string `json:"inputModalities"`
	SupportsStream    bool     `json:"supportsStream"`
	SupportsTools     bool     `json:"supportsTools"`
	SupportsVision    bool     `json:"supportsVision"`
	InputPriceMicro   int64    `json:"inputPriceMicro"`
	OutputPriceMicro  int64    `json:"outputPriceMicro"`
	ChargeInputMicro  int64    `json:"chargeInputMicro"`
	ChargeOutputMicro int64    `json:"chargeOutputMicro"`
	PriceVersion      string   `json:"priceVersion"`
}

// Task is a refresh task.
type Task struct {
	ID             int64  `json:"taskId"`
	ScoreVersionID int64  `json:"scoreVersionId"`
	Status         string `json:"status"`
	Total          int    `json:"total"`
	Succeeded      int    `json:"succeeded"`
	Failed         int    `json:"failed"`
	Pending        int    `json:"pending"`
	CostMicro      int64  `json:"costMicro"`
}

// StartRefresh freezes the enabled participant set and enqueues items.
func (s *Service) StartRefresh(ctx context.Context, actor audit.Actor) (*Task, error) {
	var running int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM refresh_task WHERE status='running'`).Scan(&running); err != nil {
		return nil, apperr.Internal("failed to check running refresh").WithCause(err)
	}
	if running > 0 {
		return nil, apperr.New(409, "40903", "conflict", "", "a refresh is already running")
	}
	if s.cfg.MaxRefreshesPerDay > 0 {
		var recent int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM refresh_task WHERE created_at > now() - interval '24 hours'`).Scan(&recent); err != nil {
			return nil, apperr.Internal("failed to check refresh frequency").WithCause(err)
		}
		if recent >= s.cfg.MaxRefreshesPerDay {
			return nil, apperr.StateConflict("daily refresh limit reached")
		}
	}

	participants, err := s.models.EligibleModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(participants) == 0 {
		return nil, apperr.StateConflict("no enabled models to evaluate")
	}
	// Capability participants are those with a known provider; all eligible
	// models in the first release are participants.
	frozen := make([]FrozenModel, 0, len(participants))
	for _, m := range participants {
		p, err := s.provider.Get(ctx, m.ProviderID)
		if err != nil {
			return nil, err
		}
		frozen = append(frozen, FrozenModel{
			ID: m.ID, Name: m.Name, ModelKey: m.ModelKey, ProviderID: m.ProviderID, ProviderName: p.Name,
			ProviderBaseURL: p.BaseURL, ProviderAuthType: p.AuthType, ProviderProtocol: p.Protocol,
			ContextLength: m.ContextLength, InputModalities: m.InputModalities, SupportsStream: m.SupportsStream,
			SupportsTools: m.SupportsTools, SupportsVision: m.SupportsVision, InputPriceMicro: m.InputPriceMicro,
			OutputPriceMicro: m.OutputPriceMicro, ChargeInputMicro: m.ChargeInputMicro, ChargeOutputMicro: m.ChargeOutputMicro,
			PriceVersion: m.PriceVersion,
		})
	}
	frozenJSON, _ := json.Marshal(frozen)

	task := &Task{Status: "running", Total: len(participants)}
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO score_version (status, total_models, template_version, rule_version)
            VALUES ('building',$1,'v1','v1') RETURNING id`, len(participants)).Scan(&task.ScoreVersionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO refresh_task (score_version_id, status, total, succeeded, failed, frozen_participants, created_by)
            VALUES ($1,'running',$2,0,0,$3,NULLIF($4,0)) RETURNING id`,
			task.ScoreVersionID, len(participants), frozenJSON, actor.ID).Scan(&task.ID); err != nil {
			return err
		}
		for _, m := range participants {
			if _, err := tx.Exec(ctx, `INSERT INTO refresh_task_item (task_id, model_id, status)
                VALUES ($1,$2,'pending')`, task.ID, m.ID); err != nil {
				return err
			}
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "evaluation.refresh",
			TargetType: "refresh_task", TargetID: itoa(task.ID), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"participants": len(participants)},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to start refresh").WithCause(err)
	}

	// Enqueue items (best effort; the worker also recovers from the DB).
	_ = s.rdb.EnsureGroup(ctx, StreamName, "evaluators")
	for _, m := range participants {
		_ = s.rdb.Enqueue(ctx, StreamName, map[string]any{
			"taskId":  fmt.Sprintf("%d", task.ID),
			"modelId": fmt.Sprintf("%d", m.ID),
		})
	}
	return task, nil
}

// TaskStatus returns progress for a task.
func (s *Service) TaskStatus(ctx context.Context, taskID int64) (*Task, error) {
	t := &Task{ID: taskID}
	err := s.pool.QueryRow(ctx, `SELECT score_version_id, status, total, succeeded, failed, cost_micro
        FROM refresh_task WHERE id=$1`, taskID).
		Scan(&t.ScoreVersionID, &t.Status, &t.Total, &t.Succeeded, &t.Failed, &t.CostMicro)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("refresh task not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to load task").WithCause(err)
	}
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM refresh_task_item WHERE task_id=$1 AND status='pending'`, taskID).Scan(&t.Pending)
	return t, nil
}

// TaskItem is the per-model refresh outcome evidence for a task.
type TaskItem struct {
	ModelID      int64      `json:"modelId"`
	ModelKey     string     `json:"modelKey"`
	Status       string     `json:"status"`
	Error        string     `json:"errorMessage,omitempty"`
	OutputTokens int        `json:"outputTokens"`
	CostMicro    int64      `json:"costMicro"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}

// TaskItems lists per-model outcomes so the console can show why a candidate
// met or missed the publish threshold.
func (s *Service) TaskItems(ctx context.Context, taskID int64) ([]TaskItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.model_id, m.model_key, i.status, coalesce(i.error_message,''),
            i.output_tokens, i.cost_micro, i.started_at, i.finished_at
        FROM refresh_task_item i JOIN model m ON m.id=i.model_id
        WHERE i.task_id=$1 ORDER BY i.model_id`, taskID)
	if err != nil {
		return nil, apperr.Internal("failed to list task items").WithCause(err)
	}
	defer rows.Close()
	out := []TaskItem{}
	for rows.Next() {
		var it TaskItem
		if err := rows.Scan(&it.ModelID, &it.ModelKey, &it.Status, &it.Error, &it.OutputTokens, &it.CostMicro, &it.StartedAt, &it.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// LatestTasks returns recent refresh tasks for worker metrics and admin progress.
func (s *Service) LatestTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT id, score_version_id, status, total, succeeded, failed, cost_micro
        FROM refresh_task ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, apperr.Internal("failed to list refresh tasks").WithCause(err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ScoreVersionID, &t.Status, &t.Total, &t.Succeeded, &t.Failed, &t.CostMicro); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RetryFailedItems re-arms timeout items of a task so the worker can retry them.
func (s *Service) RetryFailedItems(ctx context.Context, taskID int64) (int, error) {
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM refresh_task WHERE id=$1`, taskID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, apperr.NotFound("refresh task not found")
		}
		return 0, apperr.Internal("failed to load task").WithCause(err)
	}
	if status == "running" {
		return 0, apperr.StateConflict("task is still running")
	}
	rows, err := s.pool.Query(ctx, `UPDATE refresh_task_item SET status='pending', error_message=NULL
        WHERE task_id=$1 AND status='timeout' RETURNING model_id`, taskID)
	if err != nil {
		return 0, apperr.Internal("failed to re-arm items").WithCause(err)
	}
	var modelIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			modelIDs = append(modelIDs, id)
		}
	}
	rows.Close()
	if len(modelIDs) == 0 {
		return 0, nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE refresh_task SET status='running', finished_at=NULL, failed=GREATEST(failed-$2,0) WHERE id=$1`, taskID, len(modelIDs)); err != nil {
		return 0, apperr.Internal("failed to reopen task").WithCause(err)
	}
	_ = s.rdb.EnsureGroup(ctx, StreamName, "evaluators")
	for _, id := range modelIDs {
		_ = s.rdb.Enqueue(ctx, StreamName, map[string]any{"taskId": itoa(taskID), "modelId": itoa(id)})
	}
	return len(modelIDs), nil
}

// ScoreVersion describes a score version.
type ScoreVersion struct {
	ID             int64      `json:"id"`
	Status         string     `json:"status"`
	SuccessRatio   float64    `json:"successRatio"`
	TotalModels    int        `json:"totalModels"`
	ValidModels    int        `json:"validModels"`
	CreatedAt      time.Time  `json:"createdAt"`
	PublishedAt    *time.Time `json:"publishedAt,omitempty"`
	FallbackModels int        `json:"fallbackModels"`
}

// ListVersions lists score versions.
func (s *Service) ListVersions(ctx context.Context) ([]ScoreVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT v.id, v.status, v.success_ratio, v.total_models, v.valid_models, v.created_at, v.published_at,
        (SELECT count(DISTINCT mc.model_id) FROM model_capability mc
            JOIN refresh_task t ON t.score_version_id=v.id
            JOIN refresh_task_item i ON i.task_id=t.id AND i.model_id=mc.model_id
            WHERE mc.score_version_id < v.id
              AND NOT EXISTS (SELECT 1 FROM model_capability n WHERE n.score_version_id=v.id AND n.model_id=mc.model_id)) AS fallback_models
        FROM score_version v ORDER BY v.id DESC LIMIT 100`)
	if err != nil {
		return nil, apperr.Internal("failed to list versions").WithCause(err)
	}
	defer rows.Close()
	var out []ScoreVersion
	for rows.Next() {
		var v ScoreVersion
		if err := rows.Scan(&v.ID, &v.Status, &v.SuccessRatio, &v.TotalModels, &v.ValidModels, &v.CreatedAt, &v.PublishedAt, &v.FallbackModels); err != nil {
			return nil, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		if v.PublishedAt != nil {
			t := v.PublishedAt.UTC()
			v.PublishedAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Publish activates a candidate version after validating the frozen-task
// success threshold and then enforces the multi-node activation barrier.
func (s *Service) Publish(ctx context.Context, versionID int64, reason string, actor audit.Actor) (string, error) {
	pol, err := s.policy.Get(ctx)
	if err != nil {
		return "", err
	}
	var status string
	var ratio float64
	err = s.pool.QueryRow(ctx, `SELECT status, success_ratio FROM score_version WHERE id=$1`, versionID).Scan(&status, &ratio)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperr.NotFound("score version not found")
	}
	if err != nil {
		return "", apperr.Internal("failed to load score version").WithCause(err)
	}
	if status != "building" && status != "published" {
		return "", apperr.New(409, "40905", "conflict", "", "score version is not publishable")
	}
	if ratio < pol.MinPublishRatio {
		return "", apperr.New(409, "40905", "conflict", "", "score version did not reach the publish threshold")
	}
	return s.activateVersion(ctx, versionID, reason, actor, "evaluation.publish")
}

// Rollback reactivates a retained immutable version through the same ACK
// barrier as normal publication.
func (s *Service) Rollback(ctx context.Context, versionID int64, reason string, actor audit.Actor) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM score_version WHERE id=$1`, versionID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperr.NotFound("score version not found")
	}
	if err != nil {
		return "", apperr.Internal("failed to load score version").WithCause(err)
	}
	if status != "superseded" && status != "published" {
		return "", apperr.New(409, "40905", "conflict", "", "only a retained score version can be rolled back")
	}
	return s.activateVersion(ctx, versionID, reason, actor, "evaluation.rollback")
}

func (s *Service) activateVersion(ctx context.Context, versionID int64, reason string, actor audit.Actor, action string) (string, error) {
	caps, err := s.Capabilities(ctx, versionID)
	if err != nil {
		return "", err
	}
	snapshot, _ := json.Marshal(caps)
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		var current int64
		err := tx.QueryRow(ctx, `SELECT id FROM score_version WHERE status='published' LIMIT 1 FOR UPDATE`).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current == versionID {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE score_version SET status='superseded' WHERE status='published' AND id<>$1`, versionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE score_version SET status='published',published_at=COALESCE(published_at,now()) WHERE id=$1`, versionID); err != nil {
			return err
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{ActorType: actor.Type, ActorID: actor.ID, Action: action, TargetType: "score_version", TargetID: itoa(versionID), Reason: reason, RequestID: actor.RequestID, IP: actor.IP})
	})
	if err != nil {
		return "", apperr.Internal("failed to activate score version").WithCause(err)
	}
	if err := s.rdb.SetSnapshot(ctx, versionID, snapshot); err != nil {
		return "", apperr.Internal("failed to cache snapshot").WithCause(err)
	}
	if err := s.rdb.SetActiveVersion(ctx, versionID); err != nil {
		return "", apperr.Internal("failed to set active version").WithCause(err)
	}
	ok, err := s.awaitActorAck(ctx, versionID)
	if err != nil {
		return "pending", err
	}
	if !ok {
		return "pending", nil
	}
	return "published", nil
}

func (s *Service) awaitActorAck(ctx context.Context, versionID int64) (bool, error) {
	deadline := time.Now().Add(s.ackTimeout)
	for {
		nodes, err := s.rdb.Nodes(ctx)
		if err != nil {
			return false, apperr.Internal("failed to read node leases").WithCause(err)
		}
		allAcked := true
		any := false
		for _, n := range nodes {
			any = true
			if n.Version != versionID {
				allAcked = false
				// Remove readiness from nodes that have not loaded the snapshot.
				// They remain in the barrier until they ACK or their lease disappears
				// after the deployment environment confirms removal from traffic.
				_ = s.rdb.Heartbeat(ctx, n.ID, n.Version, false, true)
			}
		}
		if !any || allAcked {
			return true, nil
		}
		if time.Now().After(deadline) {
			// Drain mismatching nodes through the configured membership adapter.
			// Only an explicit removal confirmation permits exclusion from the
			// barrier; otherwise the version remains pending.
			adapter := s.membership
			if adapter == nil {
				adapter = noMembershipAdapter{}
			}
			for _, n := range nodes {
				if n.Version == versionID {
					continue
				}
				removed, drainErr := adapter.DrainAndConfirm(ctx, n.ID)
				if drainErr != nil {
					return false, drainErr
				}
				if removed {
					if err := s.rdb.MarkRemoved(ctx, n.ID); err != nil {
						return false, err
					}
					_ = s.rdb.ReleaseNode(ctx, n.ID)
				}
			}
			remaining, listErr := s.rdb.Nodes(ctx)
			if listErr != nil {
				return false, listErr
			}
			for _, n := range remaining {
				if n.Version != versionID {
					return false, nil
				}
			}
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

// parse task/model ids from stream values.
func parseInt(v any) int64 {
	switch t := v.(type) {
	case string:
		var n int64
		fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
