// Package audit records administrative actions. High-risk operations must
// write audit rows in the same transaction as the business change.
package audit

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hchw/slogan/internal/db"
)

// Entry describes one audit record. Secrets and request bodies are never
// stored here.
type Entry struct {
	ActorType  string // admin | user | system
	ActorID    int64
	Action     string
	TargetType string
	TargetID   string
	Reason     string
	Result     string
	RequestID  string
	IP         string
	Detail     map[string]any
}

// Service writes audit records.
type Service struct {
	pool *db.Pool
}

// Actor identifies who performed an audited action.
type Actor struct {
	Type      string // admin | user | system
	ID        int64
	IP        string
	RequestID string
}

// SystemActor is used for background/automated actions.
func SystemActor(requestID string) Actor { return Actor{Type: "system", RequestID: requestID} }

// New returns an audit service.
func New(pool *db.Pool) *Service { return &Service{pool: pool} }

// TxWriter is satisfied by pgx.Tx and *db.Pool for shared use.
type TxWriter interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// WriteInTx records an audit entry using the provided transaction/executor so
// it commits atomically with the protected operation.
func (s *Service) WriteInTx(ctx context.Context, q TxWriter, e Entry) error {
	if e.ActorType == "" {
		e.ActorType = "system"
	}
	if e.Result == "" {
		e.Result = "success"
	}
	var detail []byte
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_log
        (actor_type, actor_id, action, target_type, target_id, reason, result, request_id, detail, ip)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ActorType, nullableInt(e.ActorID), e.Action, e.TargetType, e.TargetID,
		e.Reason, e.Result, e.RequestID, detail, e.IP)
	return err
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
