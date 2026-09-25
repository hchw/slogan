// Package auth handles user/admin identity, revocable opaque sessions, RBAC
// and API key authentication. Passwords use Argon2id; only token digests are
// stored.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/secure"
)

// Service provides identity and authorization operations.
type Service struct {
	pool        *db.Pool
	sessionTTL  time.Duration
	generateKey func() (secure.APIKey, error)
}

// New returns an auth service.
func New(pool *db.Pool, sessionTTL time.Duration) *Service {
	return &Service{pool: pool, sessionTTL: sessionTTL, generateKey: secure.NewAPIKey}
}

// User is an end-user account.
type User struct {
	ID           int64
	Email        string
	Nickname     string
	Status       string
	PasswordHash string
}

// Principal identifies an authenticated session owner.
type Principal struct {
	Type string // user | admin
	ID   int64
}

// RegisterUser creates an active user and a zero-balance quota account.
func (s *Service) RegisterUser(ctx context.Context, email, password, nickname string) (*User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, apperr.InvalidParam("invalid email address")
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return nil, apperr.Internal("failed to hash password").WithCause(err)
	}

	var u User
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO app_user (email, password_hash, nickname)
            VALUES ($1,$2,$3) RETURNING id, email, nickname, status`,
			email, hash, nickname).Scan(&u.ID, &u.Email, &u.Nickname, &u.Status)
		if err != nil {
			if isUniqueViolation(err) {
				return apperr.Duplicate("email already registered")
			}
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO quota_account (user_id) VALUES ($1)`, u.ID)
		return err
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("registration failed").WithCause(err)
	}
	return &u, nil
}

// AuthenticateUser verifies credentials and returns the user.
func (s *Service) AuthenticateUser(ctx context.Context, email, password string) (*User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, email, nickname, status, password_hash
        FROM app_user WHERE email=$1 AND deleted_at IS NULL`, email).
		Scan(&u.ID, &u.Email, &u.Nickname, &u.Status, &u.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Unauthenticated("invalid email or password")
	}
	if err != nil {
		return nil, apperr.Internal("login failed").WithCause(err)
	}
	if !secure.VerifyPassword(password, u.PasswordHash) {
		return nil, apperr.Unauthenticated("invalid email or password")
	}
	if u.Status != "active" {
		return nil, apperr.StateConflict("account is disabled")
	}
	return &u, nil
}

// UserByID loads an active user.
func (s *Service) UserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, email, nickname, status FROM app_user
        WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&u.ID, &u.Email, &u.Nickname, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("user not found")
	}
	if err != nil {
		return nil, apperr.Internal("load user failed").WithCause(err)
	}
	return &u, nil
}

// CreateUserSession issues a revocable session token for a user.
func (s *Service) CreateUserSession(ctx context.Context, userID int64) (string, time.Time, error) {
	return s.createSession(ctx, "user", userID)
}

// CreateAdminSession issues a revocable session token for an administrator.
func (s *Service) CreateAdminSession(ctx context.Context, adminID int64) (string, time.Time, error) {
	return s.createSession(ctx, "admin", adminID)
}

func (s *Service) createSession(ctx context.Context, ptype string, pid int64) (string, time.Time, error) {
	token, err := secure.RandomToken(32)
	if err != nil {
		return "", time.Time{}, apperr.Internal("token generation failed").WithCause(err)
	}
	expires := time.Now().UTC().Add(s.sessionTTL)
	_, err = s.pool.Exec(ctx, `INSERT INTO session (principal_type, principal_id, token_hash, expires_at)
        VALUES ($1,$2,$3,$4)`, ptype, pid, secure.HashToken(token), expires)
	if err != nil {
		return "", time.Time{}, apperr.Internal("session creation failed").WithCause(err)
	}
	return token, expires, nil
}

// ValidateSession resolves a session token to a principal, rejecting revoked or
// expired sessions and disabled principals.
func (s *Service) ValidateSession(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, apperr.Unauthenticated("missing credentials")
	}
	var (
		p       Principal
		expires time.Time
		revoked *time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT principal_type, principal_id, expires_at, revoked_at
        FROM session WHERE token_hash=$1`, secure.HashToken(token)).
		Scan(&p.Type, &p.ID, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Unauthenticated("invalid session")
	}
	if err != nil {
		return nil, apperr.Internal("session lookup failed").WithCause(err)
	}
	if revoked != nil {
		return nil, apperr.Unauthenticated("session revoked")
	}
	if time.Now().After(expires) {
		return nil, apperr.TokenExpired()
	}

	var status string
	switch p.Type {
	case "user":
		err = s.pool.QueryRow(ctx, `SELECT status FROM app_user WHERE id=$1 AND deleted_at IS NULL`, p.ID).Scan(&status)
	case "admin":
		err = s.pool.QueryRow(ctx, `SELECT status FROM admin_user WHERE id=$1 AND deleted_at IS NULL`, p.ID).Scan(&status)
	default:
		return nil, apperr.Unauthenticated("invalid session")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Unauthenticated("account not found")
	}
	if err != nil {
		return nil, apperr.Internal("session principal lookup failed").WithCause(err)
	}
	if status != "active" {
		return nil, apperr.Unauthenticated("account disabled")
	}
	return &p, nil
}

// RevokeSession marks a session revoked.
func (s *Service) RevokeSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE session SET revoked_at=now()
        WHERE token_hash=$1 AND revoked_at IS NULL`, secure.HashToken(token))
	return err
}

// RevokeAllForPrincipal revokes every session for an account.
func (s *Service) RevokeAllForPrincipal(ctx context.Context, ptype string, pid int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE session SET revoked_at=now()
        WHERE principal_type=$1 AND principal_id=$2 AND revoked_at IS NULL`, ptype, pid)
	return err
}

// ---- administrators and RBAC ----

// Admin is an administrator account with resolved permissions.
type Admin struct {
	ID          int64
	Username    string
	DisplayName string
	Status      string
	Permissions map[string]bool
}

// Has reports whether the admin holds a permission.
func (a *Admin) Has(perm string) bool {
	if a == nil {
		return false
	}
	return a.Permissions[perm]
}

// Bootstrap creates the first administrator. It fails if any administrator
// already exists, and never uses a compiled-in default credential.
func (s *Service) Bootstrap(ctx context.Context, username, password, displayName string) (*Admin, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, apperr.InvalidParam("username is required")
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return nil, apperr.Internal("failed to hash password").WithCause(err)
	}

	var adminID int64
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_user WHERE deleted_at IS NULL`).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return apperr.StateConflict("an administrator already exists; bootstrap is disabled")
		}
		if err := tx.QueryRow(ctx, `INSERT INTO admin_user (username, display_name, password_hash)
            VALUES ($1,$2,$3) RETURNING id`, username, displayName, hash).Scan(&adminID); err != nil {
			if isUniqueViolation(err) {
				return apperr.Duplicate("username already exists")
			}
			return err
		}
		var roleID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM role WHERE code='super_admin'`).Scan(&roleID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO admin_user_role (admin_user_id, role_id) VALUES ($1,$2)`, adminID, roleID)
		return err
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("bootstrap failed").WithCause(err)
	}
	return s.AdminByID(ctx, adminID)
}

// AuthenticateAdmin verifies admin credentials.
func (s *Service) AuthenticateAdmin(ctx context.Context, username, password string) (*Admin, error) {
	var (
		aid    int64
		status string
		hash   string
	)
	err := s.pool.QueryRow(ctx, `SELECT id, status, password_hash
        FROM admin_user WHERE username=$1 AND deleted_at IS NULL`, strings.TrimSpace(username)).
		Scan(&aid, &status, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Unauthenticated("invalid username or password")
	}
	if err != nil {
		return nil, apperr.Internal("admin login failed").WithCause(err)
	}
	if !secure.VerifyPassword(password, hash) {
		return nil, apperr.Unauthenticated("invalid username or password")
	}
	if status != "active" {
		return nil, apperr.StateConflict("account is disabled")
	}
	return s.AdminByID(ctx, aid)
}

// AdminByID loads an administrator with aggregated permissions.
func (s *Service) AdminByID(ctx context.Context, id int64) (*Admin, error) {
	a := &Admin{ID: id, Permissions: map[string]bool{}}
	err := s.pool.QueryRow(ctx, `SELECT username, display_name, status FROM admin_user
        WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&a.Username, &a.DisplayName, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("administrator not found")
	}
	if err != nil {
		return nil, apperr.Internal("admin lookup failed").WithCause(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT r.permissions FROM role r
        JOIN admin_user_role ur ON ur.role_id = r.id
        WHERE ur.admin_user_id=$1 AND r.deleted_at IS NULL`, id)
	if err != nil {
		return nil, apperr.Internal("permission lookup failed").WithCause(err)
	}
	defer rows.Close()
	for rows.Next() {
		var perms []string
		if err := rows.Scan(&perms); err != nil {
			return nil, apperr.Internal("permission scan failed").WithCause(err)
		}
		for _, p := range perms {
			a.Permissions[p] = true
		}
	}
	return a, rows.Err()
}

// ---- API keys ----

// APIKey is an authenticated key with its owning user's status.
type APIKey struct {
	ID              int64
	UserID          int64
	Name            string
	Prefix          string
	Status          string
	ExpiresAt       *time.Time
	AllowedModels   []string
	RateLimitPerMin int
	UserStatus      string
}

// Active reports whether the key and its owner are usable.
func (k *APIKey) Active() bool {
	if k == nil || k.Status != "active" || k.UserStatus != "active" {
		return false
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return false
	}
	return true
}

// AuthenticateAPIKey resolves a raw key, or returns an invalid-key error.
func (s *Service) AuthenticateAPIKey(ctx context.Context, raw string) (*APIKey, error) {
	if raw == "" {
		return nil, apperr.InvalidAPIKey()
	}
	var (
		k         APIKey
		expires   *time.Time
		allowed   []string
		rateLimit *int
	)
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.user_id, k.name, k.key_prefix, k.status,
        k.expires_at, COALESCE(k.allowed_models, '[]'::jsonb), k.rate_limit_per_min, u.status
        FROM api_key k JOIN app_user u ON u.id = k.user_id
        WHERE k.key_hash=$1 AND k.deleted_at IS NULL`, secure.HashToken(raw)).
		Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Status, &expires, &allowed, &rateLimit, &k.UserStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.InvalidAPIKey()
	}
	if err != nil {
		return nil, apperr.Internal("api key lookup failed").WithCause(err)
	}
	k.ExpiresAt = expires
	k.AllowedModels = allowed
	if rateLimit != nil {
		k.RateLimitPerMin = *rateLimit
	}
	if !k.Active() {
		return nil, apperr.InvalidAPIKey()
	}
	return &k, nil
}

// TouchAPIKey updates last_used_at (best effort).
func (s *Service) TouchAPIKey(ctx context.Context, id int64) {
	_, _ = s.pool.Exec(ctx, `UPDATE api_key SET last_used_at=now() WHERE id=$1`, id)
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func validatePassword(password string) error {
	if len(password) < 10 {
		return apperr.InvalidParam("password must be at least 10 characters")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}

// EnsureSeedRoles creates the built-in roles with their permission sets.
func (s *Service) EnsureSeedRoles(ctx context.Context) error {
	roles := []struct {
		code, name string
		perms      []string
	}{
		{"super_admin", "超级管理员", []string{
			"provider:create", "provider:update", "provider:delete", "provider:read",
			"model:create", "model:update", "model:read",
			"evaluation:refresh", "evaluation:publish", "evaluation:read",
			"user:read", "user:enable", "user:disable",
			"quota:adjust", "quota:read",
			"package:create", "package:read", "code:generate", "code:read",
			"policy:update", "policy:read",
			"usage:read", "audit:read", "log:read",
		}},
		{"model_admin", "模型管理员", []string{
			"provider:create", "provider:update", "provider:read",
			"model:create", "model:update", "model:read",
			"evaluation:refresh", "evaluation:read",
		}},
		{"ops_admin", "运营管理员", []string{
			"user:read", "user:enable", "user:disable",
			"quota:adjust", "quota:read",
			"package:create", "package:read", "code:generate", "code:read",
			"usage:read",
		}},
		{"auditor", "审计员", []string{"usage:read", "audit:read", "log:read", "quota:read"}},
	}
	for _, r := range roles {
		_, err := s.pool.Exec(ctx, `INSERT INTO role (code, name, permissions) VALUES ($1,$2,$3)
            ON CONFLICT (code) WHERE deleted_at IS NULL DO UPDATE SET permissions=EXCLUDED.permissions`,
			r.code, r.name, r.perms)
		if err != nil {
			return fmt.Errorf("auth: seed role %s: %w", r.code, err)
		}
	}
	return nil
}

// ---- API key management ----

// APIKeyView is the safe representation of a key (no secret).
type APIKeyView struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	KeyPrefix       string     `json:"keyPrefix"`
	Status          string     `json:"status"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt      *time.Time `json:"lastUsedAt,omitempty"`
	RateLimitPerMin int        `json:"rateLimitPerMin"`
	AllowedModels   []string   `json:"allowedModels,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

// CreateAPIKey issues a new key. The full secret is returned exactly once.
func (s *Service) CreateAPIKey(ctx context.Context, userID int64, name string, expiresAt *time.Time, rateLimit int, allowedModels []string) (*APIKeyView, string, error) {
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	if len(allowedModels) > 0 {
		var found int
		if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT model_key) FROM model
            WHERE model_key=ANY($1) AND deleted_at IS NULL`, allowedModels).Scan(&found); err != nil {
			return nil, "", apperr.Internal("failed to validate allowed models").WithCause(err)
		}
		if found != len(uniqueStrings(allowedModels)) {
			return nil, "", apperr.InvalidParam("allowedModels contains an unknown or deleted model")
		}
	}
	var allowedJSON any
	if len(allowedModels) > 0 {
		allowedJSON = allowedModels
	}
	var generated secure.APIKey
	var v *APIKeyView
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		generated, err = s.generateKey()
		if err != nil {
			return nil, "", apperr.Internal("failed to generate key").WithCause(err)
		}
		v = &APIKeyView{Name: name, KeyPrefix: generated.Prefix, Status: "active", ExpiresAt: expiresAt, RateLimitPerMin: rateLimit}
		err = s.pool.QueryRow(ctx, `INSERT INTO api_key (user_id, name, key_prefix, key_hash, expires_at, rate_limit_per_min, allowed_models)
            VALUES ($1,$2,$3,$4,$5,NULLIF($6,0),$7) RETURNING id, created_at`,
			userID, name, generated.Prefix, generated.Hash, expiresAt, rateLimit, allowedJSON).Scan(&v.ID, &v.CreatedAt)
		if err == nil {
			break
		}
		if !isUniqueViolation(err) {
			return nil, "", apperr.Internal("failed to create key").WithCause(err)
		}
	}
	if err != nil {
		return nil, "", apperr.Internal("could not generate a unique API key").WithCause(err)
	}
	v.CreatedAt = v.CreatedAt.UTC()
	if v.ExpiresAt != nil {
		t := v.ExpiresAt.UTC()
		v.ExpiresAt = &t
	}
	v.AllowedModels = append([]string(nil), allowedModels...)
	return v, generated.Full, nil
}

// ListAPIKeys lists a user's keys.
func (s *Service) ListAPIKeys(ctx context.Context, userID int64, limit, offset int) ([]APIKeyView, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, key_prefix, status, expires_at, last_used_at,
        COALESCE(rate_limit_per_min,0), COALESCE(allowed_models,'[]'::jsonb), created_at FROM api_key
        WHERE user_id=$1 AND deleted_at IS NULL ORDER BY id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list keys").WithCause(err)
	}
	defer rows.Close()
	var out []APIKeyView
	for rows.Next() {
		var v APIKeyView
		if err := rows.Scan(&v.ID, &v.Name, &v.KeyPrefix, &v.Status, &v.ExpiresAt, &v.LastUsedAt, &v.RateLimitPerMin, &v.AllowedModels, &v.CreatedAt); err != nil {
			return nil, 0, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		if v.ExpiresAt != nil {
			t := v.ExpiresAt.UTC()
			v.ExpiresAt = &t
		}
		if v.LastUsedAt != nil {
			t := v.LastUsedAt.UTC()
			v.LastUsedAt = &t
		}
		out = append(out, v)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM api_key WHERE user_id=$1 AND deleted_at IS NULL`, userID).Scan(&total)
	return out, total, rows.Err()
}

// SetAPIKeyStatus enables or disables one of a user's keys.
func (s *Service) SetAPIKeyStatus(ctx context.Context, userID, keyID int64, status string) error {
	if status != "active" && status != "disabled" {
		return apperr.InvalidParam("status must be active or disabled")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE api_key SET status=$3 WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`,
		keyID, userID, status)
	if err != nil {
		return apperr.Internal("failed to update key").WithCause(err)
	}
	if ct.RowsAffected() == 0 {
		return apperr.NotFound("key not found")
	}
	return nil
}

// RevokeAPIKey revokes and soft-deletes a user's key.
func (s *Service) RevokeAPIKey(ctx context.Context, userID, keyID int64) error {
	ct, err := s.pool.Exec(ctx, `UPDATE api_key SET status='revoked', deleted_at=now()
        WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`, keyID, userID)
	if err != nil {
		return apperr.Internal("failed to revoke key").WithCause(err)
	}
	if ct.RowsAffected() == 0 {
		return apperr.NotFound("key not found")
	}
	return nil
}

// SetUserStatus enables/disables a user and revokes sessions when disabled.
func (s *Service) SetUserStatus(ctx context.Context, userID int64, status string, actor audit.Actor) error {
	if status != "active" && status != "disabled" {
		return apperr.InvalidParam("status must be active or disabled")
	}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE app_user SET status=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, userID, status)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return apperr.NotFound("user not found")
		}
		if status == "disabled" {
			if _, err := tx.Exec(ctx, `UPDATE session SET revoked_at=now()
                WHERE principal_type='user' AND principal_id=$1 AND revoked_at IS NULL`, userID); err != nil {
				return err
			}
		}
		return s.auditWriteStatus(ctx, tx, actor, userID, status)
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return err
		}
		return apperr.Internal("failed to update user").WithCause(err)
	}
	return nil
}

func (s *Service) auditWriteStatus(ctx context.Context, tx pgx.Tx, actor audit.Actor, userID int64, status string) error {
	return audit.New(s.pool).WriteInTx(ctx, tx, audit.Entry{
		ActorType: actor.Type, ActorID: actor.ID, Action: "user.status", TargetType: "user",
		TargetID: fmt.Sprintf("%d", userID), RequestID: actor.RequestID, IP: actor.IP,
		Detail: map[string]any{"status": status},
	})
}

// UserView is an administrator-facing user summary.
type UserView struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"`
}

// ListUsers lists users for administrators.
func (s *Service) ListUsers(ctx context.Context, keyword, status string, limit, offset int) ([]UserView, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, email, nickname, status FROM app_user
        WHERE deleted_at IS NULL AND ($1='' OR email ILIKE '%'||$1||'%') AND ($2='' OR status=$2)
        ORDER BY id DESC LIMIT $3 OFFSET $4`, keyword, status, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list users").WithCause(err)
	}
	defer rows.Close()
	var out []UserView
	for rows.Next() {
		var v UserView
		if err := rows.Scan(&v.ID, &v.Email, &v.Nickname, &v.Status); err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE deleted_at IS NULL
        AND ($1='' OR email ILIKE '%'||$1||'%') AND ($2='' OR status=$2)`, keyword, status).Scan(&total)
	return out, total, rows.Err()
}
