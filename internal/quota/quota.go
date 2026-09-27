// Package quota implements the unified micro-yuan balance, the immutable
// ledger, atomic reservations, idempotent settlement, administrator
// adjustments, redemption packages and codes.
package quota

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/secure"
)

// Ledger event types.
const (
	TypeGrant   = "grant"
	TypeConsume = "consume"
	TypeRelease = "release"
	TypeAdjust  = "adjust"
	TypeReserve = "reserve"
)

// RefTypeRequest is the ledger reference type for gateway requests.
const RefTypeRequest = "request"

// Service provides quota operations.
type Service struct {
	pool  *db.Pool
	audit *audit.Service
}

// New returns a quota service.
func New(pool *db.Pool, aud *audit.Service) *Service {
	return &Service{pool: pool, audit: aud}
}

// Account is a user's quota state.
type Account struct {
	UserID         int64 `json:"userId"`
	BalanceMicro   int64 `json:"balanceMicro"`
	ReservedMicro  int64 `json:"reservedMicro"`
	AvailableMicro int64 `json:"availableMicro"`
}

// LedgerEntry is one immutable ledger row.
type LedgerEntry struct {
	ID                int64     `json:"id"`
	Type              string    `json:"type"`
	AmountMicro       int64     `json:"amountMicro"`
	BalanceAfterMicro int64     `json:"balanceAfterMicro"`
	RefType           string    `json:"refType,omitempty"`
	RefID             string    `json:"refId,omitempty"`
	Remark            string    `json:"remark,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
}

// Account returns the current quota state.
func (s *Service) Account(ctx context.Context, userID int64) (*Account, error) {
	a := &Account{UserID: userID}
	err := s.pool.QueryRow(ctx, `SELECT balance_micro, reserved_micro FROM quota_account WHERE user_id=$1`, userID).
		Scan(&a.BalanceMicro, &a.ReservedMicro)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("quota account not found")
	}
	if err != nil {
		return nil, apperr.Internal("failed to load account").WithCause(err)
	}
	a.AvailableMicro = a.BalanceMicro - a.ReservedMicro
	return a, nil
}

// Ledger lists ledger entries for a user.
func (s *Service) Ledger(ctx context.Context, userID int64, from, to *time.Time, limit, offset int) ([]LedgerEntry, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, type, amount_micro, balance_after_micro, COALESCE(ref_type,''),
        COALESCE(ref_id,''), remark, created_at FROM quota_ledger
        WHERE user_id=$1 AND ($2::timestamptz IS NULL OR created_at >= $2)
          AND ($3::timestamptz IS NULL OR created_at <= $3)
        ORDER BY id DESC LIMIT $4 OFFSET $5`, userID, from, to, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to load ledger").WithCause(err)
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.Type, &e.AmountMicro, &e.BalanceAfterMicro, &e.RefType, &e.RefID, &e.Remark, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE user_id=$1
        AND ($2::timestamptz IS NULL OR created_at >= $2) AND ($3::timestamptz IS NULL OR created_at <= $3)`,
		userID, from, to).Scan(&total)
	return out, total, rows.Err()
}

// Reserve atomically freezes amountMicro for a request inside tx. It is
// idempotent per request ID and fails closed on insufficient balance.
func (s *Service) Reserve(ctx context.Context, tx pgx.Tx, userID int64, requestID string, amountMicro int64) error {
	if amountMicro < 0 {
		return apperr.InvalidParam("reservation amount must be non-negative")
	}
	balance, reserved, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return err
	}
	if amountMicro == 0 {
		return nil
	}
	if balance-reserved < amountMicro {
		return apperr.InsufficientQuota()
	}
	ct, err := tx.Exec(ctx, `INSERT INTO quota_ledger (user_id, type, amount_micro, balance_after_micro, ref_type, ref_id, remark)
        VALUES ($1,'reserve',$2,$3,$4,$5,'request reservation')
        ON CONFLICT (ref_type, ref_id) WHERE type='reserve' DO NOTHING`,
		userID, amountMicro, balance, RefTypeRequest, requestID)
	if err != nil {
		return apperr.Internal("failed to reserve quota").WithCause(err)
	}
	if ct.RowsAffected() == 0 {
		return nil // already reserved for this request
	}
	if _, err := tx.Exec(ctx, `UPDATE quota_account SET reserved_micro = reserved_micro + $2,
        version = version + 1, updated_at = now() WHERE user_id=$1`, userID, amountMicro); err != nil {
		return apperr.Internal("failed to update reserved balance").WithCause(err)
	}
	return nil
}

// Settlement is the outcome of Settle.
type Settlement struct {
	Charged     bool
	ChargeMicro int64
}

// Settle finalises a request in tx: it releases the reservation and applies at
// most one final ledger effect. When chargeMicro <= 0 the reservation is
// released without charge. It is idempotent per request ID.
func (s *Service) Settle(ctx context.Context, tx pgx.Tx, userID int64, requestID string, reservedMicro, chargeMicro int64) (*Settlement, error) {
	if reservedMicro < 0 || chargeMicro < 0 {
		return nil, apperr.InvalidParam("settlement amounts must be non-negative")
	}
	balance, reservedNow, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return nil, err
	}

	// Already settled?
	var existingType string
	var existingAmount int64
	err = tx.QueryRow(ctx, `SELECT type, amount_micro FROM quota_ledger
        WHERE ref_type=$1 AND ref_id=$2 AND type IN ('consume','release') LIMIT 1`,
		RefTypeRequest, requestID).Scan(&existingType, &existingAmount)
	if err == nil {
		return &Settlement{Charged: existingType == TypeConsume, ChargeMicro: -existingAmount}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Internal("failed to check settlement").WithCause(err)
	}

	release := reservedMicro
	if release > reservedNow {
		release = reservedNow
	}
	if release > 0 {
		if _, err := tx.Exec(ctx, `UPDATE quota_account SET reserved_micro = reserved_micro - $2,
            version = version + 1, updated_at = now() WHERE user_id=$1`, userID, release); err != nil {
			return nil, apperr.Internal("failed to release reserved balance").WithCause(err)
		}
	}

	if chargeMicro > 0 {
		otherReserved := reservedNow - release
		maxCharge := balance - otherReserved
		if maxCharge < 0 {
			maxCharge = 0
		}
		if chargeMicro > maxCharge {
			chargeMicro = maxCharge // preserve funds reserved by other requests
		}
		if _, err := tx.Exec(ctx, `INSERT INTO quota_ledger (user_id, type, amount_micro, balance_after_micro, ref_type, ref_id, remark)
            VALUES ($1,'consume',$2,$3,$4,$5,'request consumption')`,
			userID, -chargeMicro, balance-chargeMicro, RefTypeRequest, requestID); err != nil {
			return nil, apperr.Internal("failed to record consumption").WithCause(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE quota_account SET balance_micro = balance_micro - $2,
            version = version + 1, updated_at = now() WHERE user_id=$1`, userID, chargeMicro); err != nil {
			return nil, apperr.Internal("failed to deduct balance").WithCause(err)
		}
		return &Settlement{Charged: true, ChargeMicro: chargeMicro}, nil
	}

	if _, err := tx.Exec(ctx, `INSERT INTO quota_ledger (user_id, type, amount_micro, balance_after_micro, ref_type, ref_id, remark)
        VALUES ($1,'release',0,$2,$3,$4,'reservation released (no billable usage)')`,
		userID, balance, RefTypeRequest, requestID); err != nil {
		return nil, apperr.Internal("failed to record release").WithCause(err)
	}
	return &Settlement{Charged: false, ChargeMicro: 0}, nil
}

// Redeem grants quota for a valid code exactly once.
func (s *Service) Redeem(ctx context.Context, userID int64, code string) (grantedMicro, balance int64, err error) {
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		var (
			codeID    int64
			packageID int64
			status    string
			expires   *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT id, package_id, status, expires_at FROM redemption_code
            WHERE code_hash=$1 AND deleted_at IS NULL FOR UPDATE`, secure.HashToken(code)).Scan(&codeID, &packageID, &status, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("redemption code not found")
		}
		if err != nil {
			return err
		}
		if status != "unused" {
			return apperr.New(409, "40904", "conflict", "", "redemption code already used")
		}
		if expires != nil && time.Now().After(*expires) {
			return apperr.StateConflict("redemption code expired")
		}
		var face int64
		if err := tx.QueryRow(ctx, `SELECT face_value_micro FROM quota_package WHERE id=$1 AND deleted_at IS NULL`, packageID).Scan(&face); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("package not found")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE redemption_code SET status='used', redeemed_by=$2, redeemed_at=now()
            WHERE id=$1`, codeID, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quota_account SET balance_micro = balance_micro + $2,
            version = version + 1, updated_at = now() WHERE user_id=$1`, userID, face); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT balance_micro FROM quota_account WHERE user_id=$1`, userID).Scan(&balance); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO quota_ledger (user_id, type, amount_micro, balance_after_micro, ref_type, ref_id, remark)
            VALUES ($1,'grant',$2,$3,'redemption_code',$4,'redeem package')`,
			userID, face, balance, secure.Digest([]byte(code)))
		if err != nil {
			return err
		}
		grantedMicro = face
		return nil
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return 0, 0, err
		}
		return 0, 0, apperr.Internal("redemption failed").WithCause(err)
	}
	return grantedMicro, balance, nil
}

// Adjust applies an administrator quota adjustment with optional idempotency.
func (s *Service) Adjust(ctx context.Context, actor audit.Actor, userID, amountMicro int64, reason, idemKey string) (int64, error) {
	if reason == "" {
		return 0, apperr.MissingParam("reason is required")
	}
	var balance int64
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		if idemKey != "" {
			var priorUser, priorAmount int64
			err := tx.QueryRow(ctx, `SELECT user_id, amount_micro FROM quota_ledger
                WHERE ref_type='adjust' AND ref_id=$1 AND type='adjust'`, idemKey).Scan(&priorUser, &priorAmount)
			if err == nil {
				if priorUser != userID || priorAmount != amountMicro {
					return apperr.StateConflict("idempotency key reused with a different adjustment")
				}
				return tx.QueryRow(ctx, `SELECT balance_micro FROM quota_account WHERE user_id=$1`, userID).Scan(&balance)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		b, reserved, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		_ = reserved
		if b+amountMicro < 0 {
			return apperr.StateConflict("adjustment would make balance negative")
		}
		if _, err := tx.Exec(ctx, `UPDATE quota_account SET balance_micro = balance_micro + $2,
            version = version + 1, updated_at = now() WHERE user_id=$1`, userID, amountMicro); err != nil {
			return err
		}
		balance = b + amountMicro
		refID := idemKey
		if _, err := tx.Exec(ctx, `INSERT INTO quota_ledger (user_id, type, amount_micro, balance_after_micro, ref_type, ref_id, remark)
            VALUES ($1,'adjust',$2,$3,'adjust',NULLIF($4,''),$5)`,
			userID, amountMicro, balance, refID, reason); err != nil {
			return err
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "quota.adjust",
			TargetType: "user", TargetID: itoa(userID), Reason: reason, RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"amountMicro": amountMicro},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return 0, err
		}
		return 0, apperr.Internal("adjustment failed").WithCause(err)
	}
	return balance, nil
}

func lockAccount(ctx context.Context, tx pgx.Tx, userID int64) (balance, reserved int64, err error) {
	err = tx.QueryRow(ctx, `SELECT balance_micro, reserved_micro FROM quota_account WHERE user_id=$1 FOR UPDATE`, userID).
		Scan(&balance, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, apperr.NotFound("quota account not found")
	}
	return balance, reserved, err
}

// ---- packages and codes ----

// Package is a redemption package.
type Package struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	FaceValueMicro int64     `json:"faceValueMicro"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
}

// CreatePackage creates a quota package.
func (s *Service) CreatePackage(ctx context.Context, actor audit.Actor, name string, faceValueMicro int64) (*Package, error) {
	if name == "" {
		return nil, apperr.MissingParam("name is required")
	}
	if faceValueMicro <= 0 {
		return nil, apperr.InvalidParam("faceValueMicro must be positive")
	}
	p := &Package{}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO quota_package (name, face_value_micro)
            VALUES ($1,$2) RETURNING id, name, face_value_micro, status, created_at`,
			name, faceValueMicro).Scan(&p.ID, &p.Name, &p.FaceValueMicro, &p.Status, &p.CreatedAt); err != nil {
			return err
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "package.create",
			TargetType: "package", TargetID: itoa(p.ID), RequestID: actor.RequestID, IP: actor.IP,
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to create package").WithCause(err)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}

// ListPackages returns packages with paging.
func (s *Service) ListPackages(ctx context.Context, limit, offset int) ([]Package, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, face_value_micro, status, created_at FROM quota_package
        WHERE deleted_at IS NULL ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list packages").WithCause(err)
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		var p Package
		if err := rows.Scan(&p.ID, &p.Name, &p.FaceValueMicro, &p.Status, &p.CreatedAt); err != nil {
			return nil, 0, err
		}
		p.CreatedAt = p.CreatedAt.UTC()
		out = append(out, p)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM quota_package WHERE deleted_at IS NULL`).Scan(&total)
	return out, total, rows.Err()
}

// CodeBatch is the result of generating codes.
type CodeBatch struct {
	BatchID  int64    `json:"batchId"`
	Quantity int      `json:"quantity"`
	Codes    []string `json:"codes"`
}

// Code is a redemption code row.
type Code struct {
	ID         int64      `json:"id"`
	BatchID    int64      `json:"batchId"`
	PackageID  int64      `json:"packageId"`
	Code       string     `json:"code,omitempty"`
	Status     string     `json:"status"`
	RedeemedAt *time.Time `json:"redeemedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

// GenerateCodes creates a batch of unique codes for a package.
func (s *Service) GenerateCodes(ctx context.Context, actor audit.Actor, packageID int64, quantity int, expiresAt *time.Time) (*CodeBatch, error) {
	if quantity <= 0 || quantity > 10000 {
		return nil, apperr.InvalidParam("quantity must be between 1 and 10000")
	}
	if _, err := s.packageExists(ctx, packageID); err != nil {
		return nil, err
	}
	batch := &CodeBatch{Quantity: quantity, Codes: make([]string, 0, quantity)}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO redemption_code_batch (package_id, quantity, created_by)
            VALUES ($1,$2,NULLIF($3,0)) RETURNING id`, packageID, quantity, actor.ID).Scan(&batch.BatchID); err != nil {
			return err
		}
		for i := 0; i < quantity; i++ {
			code, err := uniqueCode(ctx, tx, packageID, batch.BatchID, expiresAt)
			if err != nil {
				return err
			}
			batch.Codes = append(batch.Codes, code)
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: actor.Type, ActorID: actor.ID, Action: "code.generate",
			TargetType: "package", TargetID: itoa(packageID), RequestID: actor.RequestID, IP: actor.IP,
			Detail: map[string]any{"quantity": quantity, "batchId": batch.BatchID},
		})
	})
	if err != nil {
		if _, ok := apperr.Is(err); ok {
			return nil, err
		}
		return nil, apperr.Internal("failed to generate codes").WithCause(err)
	}
	return batch, nil
}

func uniqueCode(ctx context.Context, tx pgx.Tx, packageID, batchID int64, expiresAt *time.Time) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := secure.RandomToken(9)
		if err != nil {
			return "", err
		}
		code := "QZ-" + raw
		prefix := code
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		_, err = tx.Exec(ctx, `INSERT INTO redemption_code (batch_id, package_id, code_hash, code_prefix, expires_at)
            VALUES ($1,$2,$3,$4,$5)`, batchID, packageID, secure.HashToken(code), prefix, expiresAt)
		if err == nil {
			return code, nil
		}
		if !isUnique(err) {
			return "", err
		}
	}
	return "", apperr.Internal("failed to generate a unique code")
}

// ListCodes lists codes for a batch.
func (s *Service) ListCodes(ctx context.Context, batchID int64, status string, limit, offset int) ([]Code, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, batch_id, package_id, status, redeemed_at, expires_at
        FROM redemption_code WHERE deleted_at IS NULL AND ($1=0 OR batch_id=$1) AND ($2='' OR status=$2)
        ORDER BY id DESC LIMIT $3 OFFSET $4`, batchID, status, limit, offset)
	if err != nil {
		return nil, 0, apperr.Internal("failed to list codes").WithCause(err)
	}
	defer rows.Close()
	var out []Code
	for rows.Next() {
		var c Code
		if err := rows.Scan(&c.ID, &c.BatchID, &c.PackageID, &c.Status, &c.RedeemedAt, &c.ExpiresAt); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	var total int64
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM redemption_code WHERE deleted_at IS NULL
        AND ($1=0 OR batch_id=$1) AND ($2='' OR status=$2)`, batchID, status).Scan(&total)
	return out, total, rows.Err()
}

func (s *Service) packageExists(ctx context.Context, id int64) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT true FROM quota_package WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, apperr.NotFound("package not found")
	}
	return exists, err
}

func isUnique(err error) bool {
	return err != nil && (contains(err.Error(), "23505") || contains(err.Error(), "duplicate key"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(v int64) string {
	if v == 0 {
		return ""
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
