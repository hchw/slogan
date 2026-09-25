// Package model manages the model catalog: discovery, manual registration,
// metadata, lifecycle and the dual (upstream cost / user charge) rate card.
package model

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/provider"
)

// Status values for a model.
const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"
	StatusDisabled    = "disabled"
	StatusDiscovering = "discovering"
	StatusEvaluating  = "evaluating"
	StatusEvalFailed  = "eval_failed"
)

// Model is a catalog entry with its provider status.
type Model struct {
	ID                int64    `json:"id"`
	ProviderID        int64    `json:"providerId"`
	ProviderName      string   `json:"providerName,omitempty"`
	ProviderStatus    string   `json:"providerStatus,omitempty"`
	Name              string   `json:"name"`
	ModelKey          string   `json:"modelKey"`
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
	Source            string   `json:"source"`
	Status            string   `json:"status"`
}

// Input is the create/update payload.
type Input struct {
	ProviderID        int64    `json:"providerId"`
	Name              string   `json:"name"`
	ModelKey          string   `json:"modelKey"`
	ContextLength     int      `json:"contextLength"`
	InputModalities   []string `json:"inputModalities"`
	SupportsStream    bool     `json:"supportsStream"`
	SupportsTools     bool     `json:"supportsTools"`
	SupportsVision    bool     `json:"supportsVision"`
	InputPriceMicro   *int64   `json:"inputPriceMicro"`
	OutputPriceMicro  *int64   `json:"outputPriceMicro"`
	ChargeInputMicro  *int64   `json:"chargeInputMicro"`
	ChargeOutputMicro *int64   `json:"chargeOutputMicro"`
}

// Service provides model persistence and discovery.
type Service struct {
	pool     *db.Pool
	provider *provider.Service
	audit    *audit.Service
}

// New returns a model service.
func New(pool *db.Pool, prov *provider.Service, aud *audit.Service) *Service {
	return &Service{pool: pool, provider: prov, audit: aud}
}

const modelCols = `m.id, m.provider_id, p.name, p.status, m.name, m.model_key, COALESCE(m.context_length,0),
    m.input_modalities, m.supports_stream, m.supports_tools, m.supports_vision,
    m.input_price_micro, m.output_price_micro, m.charge_input_micro, m.charge_output_micro,
    m.price_version, m.source, m.status`

func scanModel(scan func(dest ...any) error) (*Model, error) {
	m := &Model{}
	err := scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.ProviderStatus, &m.Name, &m.ModelKey,
		&m.ContextLength, &m.InputModalities, &m.SupportsStream, &m.SupportsTools, &m.SupportsVision,
		&m.InputPriceMicro, &m.OutputPriceMicro, &m.ChargeInputMicro, &m.ChargeOutputMicro,
		&m.PriceVersion, &m.Source, &m.Status)
	if err != nil {
		return nil, err
	}
	if m.InputModalities == nil {
		m.InputModalities = []string{"text"}
	}
	return m, nil
}

// Create adds a model manually (or via discovery) after validating rates.
func (s *Service) Create(ctx context.Context, in Input, actor audit.Actor) (*Model, error) {
	if in.ProviderID == 0 || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.ModelKey) == "" {
		return nil, apperr.MissingParam("providerId, name and modelKey are required")
	}
	if err := validateInput(in); err != nil {
		return nil, err
	}
	if _, err := s.provider.Get(ctx, in.ProviderID); err != nil {
		return nil, err
	}
	modalities := in.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	var m *Model
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO model (provider_id, name, model_key, context_length,
            input_modalities, supports_stream, supports_tools, supports_vision,
            input_price_micro, output_price_micro, charge_input_micro, charge_output_micro, source)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'manual')
            RETURNING id`,
			in.ProviderID, in.Name, in.ModelKey, in.ContextLength, modalities,
			in.SupportsStream, in.SupportsTools, in.SupportsVision,
			deref(in.InputPriceMicro), deref(in.OutputPriceMicro),
			deref(in.ChargeInputMicro), deref(in.ChargeOutputMicro))
		var id int64
		if err := row.Scan(&id); err != nil {
			if strings.Contains(err.Error(), "23505") {
				return apperr.Duplicate("model already exists for this provider")
			}
			return err
		}
		var e error
		m, e = s.getTx(ctx, tx, id)
		if e != nil {
			return e
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "model.create",
			TargetType: "model", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to create model").WithCause(err)
	}
	return m, nil
}

// Update modifies model metadata and rates.
func (s *Service) Update(ctx context.Context, id int64, in Input, actor audit.Actor) (*Model, error) {
	if in.InputPriceMicro != nil && *in.InputPriceMicro < 0 {
		return nil, apperr.InvalidParam("inputPriceMicro must be non-negative")
	}
	if err := validateInput(in); err != nil {
		return nil, err
	}
	var m *Model
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE model SET
            name = COALESCE(NULLIF($2,''), name),
            model_key = COALESCE(NULLIF($3,''), model_key),
            context_length = COALESCE($4, context_length),
            input_modalities = COALESCE($5, input_modalities),
            supports_stream = $6, supports_tools = $7, supports_vision = $8,
            input_price_micro = COALESCE($9, input_price_micro),
            output_price_micro = COALESCE($10, output_price_micro),
            charge_input_micro = COALESCE($11, charge_input_micro),
            charge_output_micro = COALESCE($12, charge_output_micro),
            price_version = 'v' || to_char(now(),'YYYYMMDDHH24MISS'),
            updated_at = now()
            WHERE id=$1 AND deleted_at IS NULL`,
			id, in.Name, in.ModelKey, nullableInt(in.ContextLength), nullableModalities(in.InputModalities),
			in.SupportsStream, in.SupportsTools, in.SupportsVision,
			in.InputPriceMicro, in.OutputPriceMicro, in.ChargeInputMicro, in.ChargeOutputMicro)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("model not found")
		}
		m, err = s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "model.update",
			TargetType: "model", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to update model").WithCause(err)
	}
	return m, nil
}

// SetStatus changes a model lifecycle status.
func (s *Service) SetStatus(ctx context.Context, id int64, status string, actor audit.Actor) error {
	switch status {
	case StatusAvailable, StatusUnavailable, StatusDisabled:
	default:
		return apperr.InvalidParam("invalid model status")
	}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE model SET status=$2, updated_at=now()
            WHERE id=$1 AND deleted_at IS NULL`, id, status)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("model not found")
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "model.status",
			TargetType: "model", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"status": status},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return err
		}
		return apperr.Internal("failed to update model status").WithCause(err)
	}
	return nil
}

// Delete soft-deletes a model while preserving request/evaluation history.
func (s *Service) Delete(ctx context.Context, id int64, actor audit.Actor) error {
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE model SET deleted_at=now(), updated_at=now()
            WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("model not found")
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "model.delete",
			TargetType: "model", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return err
		}
		return apperr.Internal("failed to delete model").WithCause(err)
	}
	return nil
}

// Get loads a model by ID.
func (s *Service) Get(ctx context.Context, id int64) (*Model, error) {
	m, err := scanModel(s.pool.QueryRow(ctx, `SELECT `+modelCols+` FROM model m
        JOIN provider p ON p.id = m.provider_id
        WHERE m.id=$1 AND m.deleted_at IS NULL`, id).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("model not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to load model").WithCause(err)
	}
	return m, nil
}

func (s *Service) getTx(ctx context.Context, tx pgx.Tx, id int64) (*Model, error) {
	return scanModel(tx.QueryRow(ctx, `SELECT `+modelCols+` FROM model m
        JOIN provider p ON p.id = m.provider_id
        WHERE m.id=$1 AND m.deleted_at IS NULL`, id).Scan)
}

// List returns models with optional filters.
func (s *Service) List(ctx context.Context, providerID int64, status, keyword string, limit, offset int) ([]Model, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+modelCols+` FROM model m
        JOIN provider p ON p.id = m.provider_id
        WHERE m.deleted_at IS NULL
          AND ($1=0 OR m.provider_id=$1)
          AND ($2='' OR m.status=$2)
          AND ($3='' OR m.name ILIKE '%'||$3||'%' OR m.model_key ILIKE '%'||$3||'%')
        ORDER BY m.id DESC LIMIT $4 OFFSET $5`, providerID, status, keyword, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list models").WithCause(err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM model m WHERE m.deleted_at IS NULL
        AND ($1=0 OR m.provider_id=$1) AND ($2='' OR m.status=$2)
        AND ($3='' OR m.name ILIKE '%'||$3||'%' OR m.model_key ILIKE '%'||$3||'%')`,
		providerID, status, keyword).Scan(&total)
	return out, total, rows.Err()
}

// EligibleModels returns models usable for routing/key access: the model and
// its provider must be active and the key must permit the model.
func (s *Service) EligibleModels(ctx context.Context, allowed []string) ([]Model, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+modelCols+` FROM model m
        JOIN provider p ON p.id = m.provider_id
        WHERE m.deleted_at IS NULL AND m.status='available' AND p.status='enabled'
        ORDER BY m.id`)
	if err != nil {
		return nil, apperr.Internal("failed to load eligible models").WithCause(err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows.Scan)
		if err != nil {
			return nil, err
		}
		if len(allowed) > 0 && !contains(allowed, m.ModelKey) && !contains(allowed, m.Name) {
			continue
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ByKey resolves an enabled model by its model_key (or display name).
func (s *Service) ByKey(ctx context.Context, key string) (*Model, error) {
	m, err := scanModel(s.pool.QueryRow(ctx, `SELECT `+modelCols+` FROM model m
        JOIN provider p ON p.id = m.provider_id
        WHERE m.deleted_at IS NULL AND (m.model_key=$1 OR m.name=$1)
        ORDER BY m.id LIMIT 1`, key).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("model not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to resolve model").WithCause(err)
	}
	return m, nil
}

// DiscoveryResult summarizes a discovery run.
type DiscoveryResult struct {
	Discovered  int `json:"discovered"`
	Added       int `json:"added"`
	Updated     int `json:"updated"`
	Unavailable int `json:"unavailable"`
}

// Discover fetches the provider model list and upserts entries idempotently.
func (s *Service) Discover(ctx context.Context, providerID int64, actor audit.Actor) (*DiscoveryResult, error) {
	p, err := s.provider.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	secret, err := s.provider.Secret(ctx, providerID)
	if err != nil {
		return nil, err
	}
	client := provider.NewClient(p.BaseURL, secret, p.AuthType, 30e9)
	remote, err := client.ListModels(ctx)
	if err != nil {
		return nil, apperr.New(502, "50201", "provider_error", "", "provider discovery failed")
	}
	res := &DiscoveryResult{Discovered: len(remote)}
	seen := map[string]bool{}

	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		for _, rm := range remote {
			if rm.ID == "" {
				continue
			}
			seen[rm.ID] = true
			var existingID int64
			err := tx.QueryRow(ctx, `SELECT id FROM model WHERE provider_id=$1 AND model_key=$2 AND deleted_at IS NULL`,
				providerID, rm.ID).Scan(&existingID)
			if errors.Is(err, pgx.ErrNoRows) {
				if _, err := tx.Exec(ctx, `INSERT INTO model (provider_id, name, model_key, source)
                    VALUES ($1,$2,$3,'discovered')`, providerID, rm.ID, rm.ID); err != nil {
					return err
				}
				res.Added++
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE model SET name=$2,
                status=CASE WHEN status='unavailable' THEN 'available' ELSE status END,
                updated_at=now() WHERE id=$1`, existingID, rm.ID); err != nil {
				return err
			}
			res.Updated++
		}
		// Models no longer reported become unavailable (history preserved).
		ct, err := tx.Exec(ctx, `UPDATE model SET status='unavailable', updated_at=now()
            WHERE provider_id=$1 AND deleted_at IS NULL AND source='discovered'
              AND status='available' AND model_key <> ALL($2)`, providerID, keyList(seen))
		if err != nil {
			return err
		}
		res.Unavailable = int(ct.RowsAffected())
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "model.discover",
			TargetType: "provider", TargetID: itoa(providerID), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"added": res.Added, "updated": res.Updated, "unavailable": res.Unavailable},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("discovery failed").WithCause(err)
	}
	return res, nil
}

func keyList(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func validateInput(in Input) error {
	if in.ContextLength < 0 {
		return apperr.InvalidParam("contextLength must be non-negative")
	}
	for _, v := range []*int64{in.InputPriceMicro, in.OutputPriceMicro, in.ChargeInputMicro, in.ChargeOutputMicro} {
		if v != nil && *v < 0 {
			return apperr.InvalidParam("prices must be non-negative micro-yuan per token")
		}
	}
	for _, mod := range in.InputModalities {
		switch mod {
		case "text", "image", "audio":
		default:
			return apperr.InvalidParam("unsupported modality: " + mod)
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableModalities(v []string) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func itoa(v int64) string {
	if v == 0 {
		return ""
	}
	buf := [20]byte{}
	n := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		n--
		buf[n] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}
