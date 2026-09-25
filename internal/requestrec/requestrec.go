// Package requestrec owns the request record: its lifecycle state machine,
// price snapshots and idempotency association.
package requestrec

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/db"
)

// Lifecycle statuses.
const (
	StatusReserved          = "reserved"
	StatusUpstreamPending   = "upstream_pending"
	StatusSuccess           = "success"
	StatusUpstreamError     = "upstream_error"
	StatusGatewayTimeout    = "gateway_timeout"
	StatusStreamBroken      = "stream_broken"
	StatusClientDisconnect  = "client_disconnected"
	StatusQuotaRejected     = "quota_rejected"
	StatusModelUnavailable  = "model_unavailable"
	StatusInvalidRequest    = "invalid_request"
	StatusStreamUnsupported = "stream_unsupported"
)

// Usage sources.
const (
	UsageProvider = "provider"
	UsagePartial  = "provider_partial"
	UsageNone     = "none"
)

// Record is a persisted request lifecycle row.
type Record struct {
	RequestID         string
	UserID            int64
	APIKeyID          int64
	RequestedModel    string
	ModelID           int64
	Status            string
	ErrorCode         string
	IdempotencyKey    string
	RequestDigest     string
	Intent            string
	ScoreVersionID    int64
	PolicyVersion     string
	ClassifierVersion string
	CostInputMicro    int64
	CostOutputMicro   int64
	ChargeInputMicro  int64
	ChargeOutputMicro int64
	EstimatedInput    int
	ReservedMicro     int64
	UsageSource       string
	InputTokens       int64
	OutputTokens      int64
	CostMicro         int64
	ChargeMicro       int64
	LatencyMs         int
	CreatedAt         time.Time
	FinishedAt        *time.Time
}

// New carries the fields needed to open a request record.
type New struct {
	RequestID         string
	UserID            int64
	APIKeyID          int64
	RequestedModel    string
	ModelID           int64
	Status            string
	IdempotencyKey    string
	RequestDigest     string
	Intent            string
	ScoreVersionID    int64
	PolicyVersion     string
	ClassifierVersion string
	CostInputMicro    int64
	CostOutputMicro   int64
	ChargeInputMicro  int64
	ChargeOutputMicro int64
	EstimatedInput    int
	ReservedMicro     int64
}

// IsTerminal reports whether a status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusSuccess, StatusUpstreamError, StatusGatewayTimeout, StatusStreamBroken,
		StatusClientDisconnect, StatusQuotaRejected, StatusModelUnavailable,
		StatusInvalidRequest, StatusStreamUnsupported:
		return true
	}
	return false
}

// Service provides request record persistence.
type Service struct {
	pool *db.Pool
}

// NewService returns a request record service.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// ErrIdempotencyConflict indicates the key is bound to a different digest.
var ErrIdempotencyConflict = errors.New("idempotency key bound to a different request")

// Begin inserts a new request record within tx. It returns an existing record
// when the idempotency key already exists.
func (s *Service) Begin(ctx context.Context, tx pgx.Tx, n New) (*Record, bool, error) {
	if n.Status == "" {
		n.Status = StatusReserved
	}
	if n.RequestedModel == "" {
		n.RequestedModel = "auto"
	}
	var idemKey any
	if n.IdempotencyKey != "" {
		idemKey = n.IdempotencyKey
	}
	var digest any
	if n.RequestDigest != "" {
		digest = n.RequestDigest
	}
	var insertedID string
	err := tx.QueryRow(ctx, `INSERT INTO request_record
        (request_id, user_id, api_key_id, requested_model, model_id, status, idempotency_key, request_digest,
         intent, score_version_id, policy_version, classifier_version,
         cost_input_micro, cost_output_micro, charge_input_micro, charge_output_micro,
         estimated_input_tokens, reserved_micro)
        VALUES ($1,$2,$3,$4,NULLIF($5,0),$6,$7,$8,NULLIF($9,''),NULLIF($10,0),NULLIF($11,''),NULLIF($12,''),
                $13,$14,$15,$16,$17,$18)
        ON CONFLICT DO NOTHING RETURNING request_id`,
		n.RequestID, nullableID(n.UserID), nullableID(n.APIKeyID), n.RequestedModel, n.ModelID, n.Status,
		idemKey, digest, n.Intent, n.ScoreVersionID, n.PolicyVersion, n.ClassifierVersion,
		n.CostInputMicro, n.CostOutputMicro, n.ChargeInputMicro, n.ChargeOutputMicro,
		n.EstimatedInput, n.ReservedMicro).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing *Record
		var lookupErr error
		if n.IdempotencyKey != "" {
			existing, lookupErr = s.scan(tx.QueryRow(ctx, recordSelect+
				` WHERE user_id=$1 AND idempotency_key=$2`, n.UserID, n.IdempotencyKey).Scan)
		} else {
			existing, lookupErr = s.getTx(ctx, tx, n.RequestID)
		}
		if lookupErr != nil {
			return nil, false, apperr.Internal("idempotency conflict could not be resolved").WithCause(lookupErr)
		}
		return existing, true, nil
	}
	if err != nil {
		return nil, false, apperr.Internal("failed to create request record").WithCause(err)
	}
	rec, err := s.getTx(ctx, tx, n.RequestID)
	if err != nil {
		return nil, false, err
	}
	return rec, false, nil
}

// ByIdempotency finds an existing request by user-scoped idempotency key.
func (s *Service) ByIdempotency(ctx context.Context, userID int64, key string) (*Record, error) {
	rec, err := s.scan(s.pool.QueryRow(ctx, recordSelect+` WHERE user_id=$1 AND idempotency_key=$2`, userID, key).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("no request for idempotency key")
	}
	if err != nil {
		return nil, apperr.Internal("idempotency lookup failed").WithCause(err)
	}
	return rec, nil
}

// SetUpstreamPending marks the request as awaiting the provider.
func (s *Service) SetUpstreamPending(ctx context.Context, tx pgx.Tx, requestID string) error {
	_, err := tx.Exec(ctx, `UPDATE request_record SET status=$2 WHERE request_id=$1 AND status=$3`,
		requestID, StatusUpstreamPending, StatusReserved)
	return err
}

// Finalize writes a terminal status and usage/cost snapshot fields.
type Finalize struct {
	RequestID    string
	Status       string
	ErrorCode    string
	UsageSource  string
	InputTokens  int64
	OutputTokens int64
	CostMicro    int64
	ChargeMicro  int64
	LatencyMs    int
}

// Finalize sets the terminal state, idempotently (no-op if already terminal).
func (s *Service) Finalize(ctx context.Context, tx pgx.Tx, f Finalize) error {
	if !IsTerminal(f.Status) {
		return apperr.InvalidParam("final request status must be terminal")
	}
	_, err := tx.Exec(ctx, `UPDATE request_record SET
        status=$2, error_code=NULLIF($3,''), usage_source=NULLIF($4,''),
        input_tokens=$5, output_tokens=$6, cost_micro=$7, charge_micro=$8, latency_ms=$9, finished_at=now()
        WHERE request_id=$1 AND finished_at IS NULL`,
		f.RequestID, f.Status, f.ErrorCode, f.UsageSource, f.InputTokens, f.OutputTokens, f.CostMicro, f.ChargeMicro, f.LatencyMs)
	return err
}

// Get loads a request record by ID.
func (s *Service) Get(ctx context.Context, requestID string) (*Record, error) {
	rec, err := s.scan(s.pool.QueryRow(ctx, recordSelect+` WHERE request_id=$1`, requestID).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("request not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to load request").WithCause(err)
	}
	return rec, nil
}

// ListByUser lists a user's requests.
func (s *Service) ListByUser(ctx context.Context, userID int64, modelID int64, limit, offset int) ([]Record, int64, error) {
	rows, err := s.pool.Query(ctx, recordSelect+`
        WHERE user_id=$1 AND ($2=0 OR model_id=$2)
        ORDER BY id DESC LIMIT $3 OFFSET $4`, userID, modelID, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list requests").WithCause(err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec, err := s.scan(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *rec)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM request_record WHERE user_id=$1 AND ($2=0 OR model_id=$2)`,
		userID, modelID).Scan(&total)
	return out, total, rows.Err()
}

const recordSelect = `SELECT request_id, COALESCE(user_id,0), COALESCE(api_key_id,0),
    COALESCE(requested_model,''), COALESCE(model_id,0), status, COALESCE(error_code,''),
    COALESCE(idempotency_key,''), COALESCE(request_digest,''), COALESCE(intent,''),
    COALESCE(score_version_id,0), COALESCE(policy_version,''), COALESCE(classifier_version,''),
    cost_input_micro, cost_output_micro, charge_input_micro, charge_output_micro,
    estimated_input_tokens, reserved_micro, COALESCE(usage_source,''),
    input_tokens, output_tokens, cost_micro, charge_micro, latency_ms, created_at, finished_at
    FROM request_record`

func (s *Service) getTx(ctx context.Context, tx pgx.Tx, requestID string) (*Record, error) {
	return s.scan(tx.QueryRow(ctx, recordSelect+` WHERE request_id=$1`, requestID).Scan)
}

func (s *Service) scan(scan func(dest ...any) error) (*Record, error) {
	r := &Record{}
	err := scan(&r.RequestID, &r.UserID, &r.APIKeyID, &r.RequestedModel, &r.ModelID, &r.Status, &r.ErrorCode,
		&r.IdempotencyKey, &r.RequestDigest, &r.Intent, &r.ScoreVersionID, &r.PolicyVersion, &r.ClassifierVersion,
		&r.CostInputMicro, &r.CostOutputMicro, &r.ChargeInputMicro, &r.ChargeOutputMicro,
		&r.EstimatedInput, &r.ReservedMicro, &r.UsageSource,
		&r.InputTokens, &r.OutputTokens, &r.CostMicro, &r.ChargeMicro, &r.LatencyMs, &r.CreatedAt, &r.FinishedAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	if r.FinishedAt != nil {
		t := r.FinishedAt.UTC()
		r.FinishedAt = &t
	}
	return r, nil
}

func nullableID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func isUnique(err error) bool {
	s := err.Error()
	return indexOf(s, "23505") >= 0 || indexOf(s, "duplicate key") >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
