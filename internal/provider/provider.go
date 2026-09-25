// Package provider manages upstream providers: encrypted credentials, masked
// display, connectivity testing and an OpenAI-compatible HTTP adapter.
package provider

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/secure"
)

// Provider is an upstream provider record (without its secret).
type Provider struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	BaseURL            string   `json:"baseUrl"`
	AuthType           string   `json:"authType"`
	Protocol           string   `json:"protocol"`
	Region             string   `json:"region"`
	ExtensionAllowlist []string `json:"extensionAllowlist"`
	Status             string   `json:"status"`
	SecretMasked       string   `json:"secretMasked,omitempty"`
}

// Input is the create/update payload.
type Input struct {
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	BaseURL            string   `json:"baseUrl"`
	AuthType           string   `json:"authType"`
	Secret             string   `json:"secret"`
	Protocol           string   `json:"protocol"`
	Region             string   `json:"region"`
	ExtensionAllowlist []string `json:"extensionAllowlist"`
}

// Service provides provider persistence.
type Service struct {
	pool  *db.Pool
	key   []byte
	audit *audit.Service
}

// New returns a provider service.
func New(pool *db.Pool, key []byte, aud *audit.Service) *Service {
	return &Service{pool: pool, key: key, audit: aud}
}

// Create stores a new provider with an encrypted secret.
func (s *Service) Create(ctx context.Context, in Input, actor audit.Actor) (*Provider, error) {
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.BaseURL) == "" || in.Secret == "" {
		return nil, apperr.MissingParam("name, baseUrl and secret are required")
	}
	if in.Type == "" {
		in.Type = "openai_compatible"
	}
	if in.AuthType == "" {
		in.AuthType = "bearer"
	}
	if in.Protocol == "" {
		in.Protocol = "openai"
	}
	if err := validateExtensionNames(in.ExtensionAllowlist); err != nil {
		return nil, err
	}
	if in.ExtensionAllowlist == nil {
		in.ExtensionAllowlist = []string{}
	}
	cipher, err := secure.Encrypt(s.key, []byte(in.Secret))
	if err != nil {
		return nil, apperr.Internal("failed to encrypt secret").WithCause(err)
	}

	p := &Provider{}
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO provider (name, type, base_url, auth_type, secret_cipher, extension_allowlist, protocol, region)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, name, type, base_url, auth_type, protocol, region, extension_allowlist, status`,
			in.Name, in.Type, in.BaseURL, in.AuthType, cipher, in.ExtensionAllowlist, in.Protocol, in.Region).
			Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthType, &p.Protocol, &p.Region, &p.ExtensionAllowlist, &p.Status)
		if err != nil {
			return err
		}
		p.SecretMasked = secure.Mask(in.Secret)
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "provider.create",
			TargetType: "provider", TargetID: itoa(p.ID), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"name": in.Name},
		})
	})
	if err != nil {
		return nil, apperr.Internal("failed to create provider").WithCause(err)
	}
	return p, nil
}

// Update modifies a provider. An empty secret leaves the stored secret intact.
func (s *Service) Update(ctx context.Context, id int64, in Input, actor audit.Actor) (*Provider, error) {
	if err := validateExtensionNames(in.ExtensionAllowlist); err != nil {
		return nil, err
	}
	var cipher *string
	if in.Secret != "" {
		c, err := secure.Encrypt(s.key, []byte(in.Secret))
		if err != nil {
			return nil, apperr.Internal("failed to encrypt secret").WithCause(err)
		}
		cipher = &c
	}
	var allowlist any
	if in.ExtensionAllowlist != nil {
		allowlist = in.ExtensionAllowlist
	}
	p := &Provider{}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE provider SET
              name = COALESCE(NULLIF($2,''), name),
              type = COALESCE(NULLIF($3,''), type),
              base_url = COALESCE(NULLIF($4,''), base_url),
              auth_type = COALESCE(NULLIF($5,''), auth_type),
              protocol = COALESCE(NULLIF($6,''), protocol),
              region = $7,
              secret_cipher = COALESCE($8, secret_cipher),
              extension_allowlist = COALESCE($9, extension_allowlist),
              updated_at = now()
            WHERE id=$1 AND deleted_at IS NULL
            RETURNING id, name, type, base_url, auth_type, protocol, region, extension_allowlist, status`,
			id, in.Name, in.Type, in.BaseURL, in.AuthType, in.Protocol, in.Region, cipher, allowlist).
			Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthType, &p.Protocol, &p.Region, &p.ExtensionAllowlist, &p.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("provider not found")
		}
		if err != nil {
			return err
		}
		if in.Secret != "" {
			p.SecretMasked = secure.Mask(in.Secret)
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "provider.update",
			TargetType: "provider", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to update provider").WithCause(err)
	}
	return p, nil
}

// Delete soft-deletes a provider that has no active models.
func (s *Service) Delete(ctx context.Context, id int64, actor audit.Actor) error {
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		var models int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM model WHERE provider_id=$1 AND deleted_at IS NULL`, id).Scan(&models); err != nil {
			return err
		}
		if models > 0 {
			return apperr.StateConflict("provider still has models; disable or delete them first")
		}
		ct, err := tx.Exec(ctx, `UPDATE provider SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("provider not found")
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "provider.delete",
			TargetType: "provider", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return err
		}
		return apperr.Internal("failed to delete provider").WithCause(err)
	}
	return nil
}

// List returns providers with paging.
func (s *Service) List(ctx context.Context, status, keyword string, limit, offset int) ([]Provider, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, type, base_url, auth_type, protocol, region, extension_allowlist, status
        FROM provider WHERE deleted_at IS NULL
          AND ($1='' OR status=$1)
          AND ($2='' OR name ILIKE '%'||$2||'%')
        ORDER BY id DESC LIMIT $3 OFFSET $4`, status, keyword, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list providers").WithCause(err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthType, &p.Protocol, &p.Region, &p.ExtensionAllowlist, &p.Status); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM provider WHERE deleted_at IS NULL
        AND ($1='' OR status=$1) AND ($2='' OR name ILIKE '%'||$2||'%')`, status, keyword).Scan(&total)
	return out, total, rows.Err()
}

// Get loads one provider.
func (s *Service) Get(ctx context.Context, id int64) (*Provider, error) {
	p := &Provider{}
	err := s.pool.QueryRow(ctx, `SELECT id, name, type, base_url, auth_type, protocol, region, extension_allowlist, status
        FROM provider WHERE id=$1 AND deleted_at IS NULL`, id).
		Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthType, &p.Protocol, &p.Region, &p.ExtensionAllowlist, &p.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("provider not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to load provider").WithCause(err)
	}
	return p, nil
}

// Secret decrypts and returns the provider credential for adapter use only.
func (s *Service) Secret(ctx context.Context, id int64) (string, error) {
	var cipher *string
	err := s.pool.QueryRow(ctx, `SELECT secret_cipher FROM provider WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&cipher)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperr.NotFound("provider not found")
	}
	if err != nil {
		return "", apperr.Internal("failed to load provider secret").WithCause(err)
	}
	if cipher == nil || *cipher == "" {
		return "", nil
	}
	plain, err := secure.Decrypt(s.key, *cipher)
	if err != nil {
		return "", apperr.Internal("failed to decrypt secret").WithCause(err)
	}
	return string(plain), nil
}

// SetStatus enables or disables a provider.
func (s *Service) SetStatus(ctx context.Context, id int64, status string, actor audit.Actor) error {
	if status != "enabled" && status != "disabled" {
		return apperr.InvalidParam("status must be enabled or disabled")
	}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE provider SET status=$2, updated_at=now()
            WHERE id=$1 AND deleted_at IS NULL`, id, status)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("provider not found")
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "provider.status",
			TargetType: "provider", TargetID: itoa(id), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"status": status},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return err
		}
		return apperr.Internal("failed to update provider status").WithCause(err)
	}
	return nil
}

func validateExtensionNames(names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || strings.TrimSpace(name) != name || seen[name] {
			return apperr.InvalidParam("extensionAllowlist names must be unique non-empty field names")
		}
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				return apperr.InvalidParam("extensionAllowlist contains an invalid field name")
			}
		}
		seen[name] = true
	}
	return nil
}

func itoa(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}
