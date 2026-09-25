package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/auth"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/httpx"
	"github.com/hchw/slogan/internal/laya"
	"github.com/hchw/slogan/internal/metrics"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
	"github.com/hchw/slogan/internal/provider"
	"github.com/hchw/slogan/internal/quota"
	"github.com/hchw/slogan/internal/redisx"
	"github.com/hchw/slogan/internal/requestrec"
	"github.com/hchw/slogan/internal/routing"
	"github.com/hchw/slogan/internal/usage"
)

// Service is the OpenAI-compatible gateway data plane.
type Service struct {
	pool     *db.Pool
	cfg      *config.Config
	auth     *auth.Service
	models   *model.Service
	provider *provider.Service
	quota    *quota.Service
	reqs     *requestrec.Service
	usg      *usage.Service
	router   *routing.Router
	laya     *laya.Client
	rdb      *redisx.Client
	policy   *policy.Service
	metrics  *metrics.Counters
}

// New builds the gateway service.
func New(pool *db.Pool, cfg *config.Config, authSvc *auth.Service, models *model.Service,
	prov *provider.Service, q *quota.Service, reqs *requestrec.Service, usg *usage.Service,
	router *routing.Router, lc *laya.Client, rdb *redisx.Client, pol *policy.Service) *Service {
	return &Service{pool: pool, cfg: cfg, auth: authSvc, models: models, provider: prov, quota: q,
		reqs: reqs, usg: usg, router: router, laya: lc, rdb: rdb, policy: pol}
}

// Handler returns the gateway mux.
// SetMetrics installs request-outcome counters. Labels stay bounded (status
// and usage-source names) and never include user ids, prompts or credentials.
func (s *Service) SetMetrics(counters *metrics.Counters) {
	s.metrics = counters
	if counters == nil {
		return
	}
	counters.Help("slogan_gateway_requests_total", "Gateway requests by terminal status.")
	counters.Help("slogan_gateway_usage_source_total", "Gateway requests by usage source.")
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return httpx.RequestID(mux)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func (s *Service) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteOpenAIError(w, r, apperr.InvalidRequest("", "method not allowed"))
		return
	}
	ctx := r.Context()
	key, err := s.auth.AuthenticateAPIKey(ctx, bearer(r))
	if err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}
	models, err := s.models.EligibleModels(ctx, key.AllowedModels)
	if err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]item, 0, len(models)+1)
	data = append(data, item{ID: "auto", Object: "model", Created: time.Now().Unix(), OwnedBy: "slogan"})
	for _, m := range models {
		data = append(data, item{ID: m.ModelKey, Object: "model", Created: time.Now().Unix(), OwnedBy: m.ProviderName})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Service) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteOpenAIError(w, r, apperr.InvalidRequest("", "method not allowed"))
		return
	}
	ctx := r.Context()
	start := time.Now()

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpx.WriteOpenAIError(w, r, apperr.InvalidRequest("", "failed to read request body"))
		return
	}
	req, err := ParseRequest(raw)
	if err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}

	key, err := s.auth.AuthenticateAPIKey(ctx, bearer(r))
	if err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}
	if err := s.checkRateLimits(ctx, key); err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}
	if req.HasMaxTok && req.MaxTokens > s.cfg.RequestMaxOutputTokens {
		httpx.WriteOpenAIError(w, r, apperr.InvalidRequest("max_tokens", "max_tokens exceeds the platform limit"))
		return
	}

	requestID := httpx.RequestIDFrom(ctx)
	digest := CanonicalDigest(req.Body)
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))

	// Idempotency resolution.
	if idemKey != "" {
		if err := s.resolveIdempotency(ctx, key.UserID, idemKey, digest); err != nil {
			httpx.WriteOpenAIError(w, r, err)
			return
		}
	}

	// Select the model.
	selected, decision, err := s.selectModel(ctx, req, key)
	if err != nil {
		s.recordNonBillable(ctx, requestID, key, req.Model, err)
		httpx.WriteOpenAIError(w, r, err)
		return
	}
	if len(req.Extensions) > 0 {
		p, err := s.provider.Get(ctx, selected.ProviderID)
		if err != nil {
			httpx.WriteOpenAIError(w, r, err)
			return
		}
		if err := ValidateExtensions(req, p.ExtensionAllowlist); err != nil {
			s.recordNonBillable(ctx, requestID, key, req.Model, err)
			httpx.WriteOpenAIError(w, r, err)
			return
		}
	}
	if req.Stream && !selected.SupportsStream {
		s.recordNonBillable(ctx, requestID, key, req.Model, apperr.InvalidRequest("stream", "model does not support streaming"))
		httpx.WriteOpenAIError(w, r, apperr.InvalidRequest("stream", "model does not support streaming"))
		return
	}
	if err := s.checkModelProviderRateLimits(ctx, selected); err != nil {
		httpx.WriteOpenAIError(w, r, err)
		return
	}

	// Reserve quota. Use the model-specific tokenizer when registered, falling
	// back to the bounded conservative estimator; estimates are never billed.
	estInput, err := EstimateInputTokensForModel(req, selected.ModelKey)
	if err != nil {
		invalid := apperr.InvalidRequest("messages", "request is too large to estimate safely")
		s.recordNonBillable(ctx, requestID, key, req.Model, invalid)
		httpx.WriteOpenAIError(w, r, invalid)
		return
	}
	maxOut := requestedMaxOutput(req, s.cfg.RequestMaxOutputTokens)
	chargeIn, chargeOut, costIn, costOut := selected.ChargeInputMicro, selected.ChargeOutputMicro,
		selected.InputPriceMicro, selected.OutputPriceMicro
	reserveAmount, err := ComputeReservation(estInput, maxOut, chargeIn, chargeOut)
	if err != nil {
		invalid := apperr.InvalidRequest("max_tokens", "reservation amount exceeds supported integer range")
		s.recordNonBillable(ctx, requestID, key, req.Model, invalid)
		httpx.WriteOpenAIError(w, r, invalid)
		return
	}
	if s.cfg.MaxReservationMicro > 0 && reserveAmount > s.cfg.MaxReservationMicro {
		err := apperr.InsufficientQuota()
		s.recordNonBillable(ctx, requestID, key, req.Model, err)
		httpx.WriteOpenAIError(w, r, err)
		return
	}

	rec := requestrec.New{
		RequestID: requestID, UserID: key.UserID, APIKeyID: key.ID, RequestedModel: req.Model,
		ModelID: selected.ID, Status: requestrec.StatusReserved, IdempotencyKey: idemKey, RequestDigest: digest,
		Intent: decision.Intent, ScoreVersionID: decision.ScoreVersionID, PolicyVersion: decision.PolicyVersion,
		ClassifierVersion: decision.ClassifierVersion,
		CostInputMicro:    costIn, CostOutputMicro: costOut, ChargeInputMicro: chargeIn, ChargeOutputMicro: chargeOut,
		EstimatedInput: estInput, ReservedMicro: reserveAmount,
	}
	var existing *requestrec.Record
	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		r0, existed, err := s.reqs.Begin(ctx, tx, rec)
		if err != nil {
			return err
		}
		if existed {
			existing = r0
			return nil
		}
		if err := s.quota.Reserve(ctx, tx, key.UserID, requestID, reserveAmount); err != nil {
			return err
		}
		if decision.Model != nil {
			candidatesJSON, _ := json.Marshal(decision.Candidates)
			policyJSON, _ := json.Marshal(map[string]any{
				"version":           decision.PolicyVersion,
				"weights":           decision.Weights,
				"classifierVersion": decision.ClassifierVersion,
			})
			if _, err := tx.Exec(ctx, `INSERT INTO route_decision
                (request_id, intent, selected_model_id, score_version_id, candidates, policy_snapshot)
                VALUES ($1,$2,$3,NULLIF($4,0),$5,$6)`,
				requestID, decision.Intent, selected.ID, decision.ScoreVersionID, candidatesJSON, policyJSON); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if existing == nil {
			httpx.WriteOpenAIError(w, r, err)
			return
		}
	}
	if existing != nil {
		// A concurrent or prior request owns this idempotency key.
		if existing.RequestDigest != "" && existing.RequestDigest != digest {
			httpx.WriteOpenAIError(w, r, apperr.IdempotencyConflict())
			return
		}
		if !requestrec.IsTerminal(existing.Status) {
			httpx.WriteOpenAIError(w, r, apperr.RequestInProgress(existing.RequestID))
			return
		}
		httpx.WriteOpenAIError(w, r, apperr.IdempotencyReplay())
		return
	}

	s.auth.TouchAPIKey(ctx, key.ID)
	s.dispatch(w, r, dispatchInput{
		req: req, key: key, selected: selected, record: rec, start: start,
	})
}

type dispatchInput struct {
	req      *Messages
	key      *auth.APIKey
	selected *model.Model
	record   requestrec.New
	start    time.Time
}

// selectModel applies explicit-model validation or auto routing.
func (s *Service) selectModel(ctx context.Context, req *Messages, key *auth.APIKey) (*model.Model, *routing.Decision, error) {
	if req.Model != "auto" {
		m, err := s.models.ByKey(ctx, req.Model)
		if err != nil {
			return nil, nil, apperr.ModelNotFound()
		}
		if m.Status != model.StatusAvailable || m.ProviderStatus != "enabled" {
			return nil, nil, apperr.ModelNotFound()
		}
		if len(key.AllowedModels) > 0 && !contains(key.AllowedModels, m.ModelKey) && !contains(key.AllowedModels, m.Name) {
			return nil, nil, apperr.ModelNotFound()
		}
		return m, &routing.Decision{Model: m, Intent: "explicit"}, nil
	}

	intent := "general"
	classifierVersion := ""
	text := req.LastUserText()
	if res := s.laya.Classify(ctx, text); res.Intent != "" {
		intent = res.Intent
		classifierVersion = res.ClassifierVersion
	}
	reqs := routing.Requirements{
		AllowedModels:        key.AllowedModels,
		Intent:               intent,
		RequiredCapabilities: routing.IntentToCapabilities(intent),
		EstimatedInputTokens: EstimateInputTokens(req) + requestedMaxOutput(req, s.cfg.RequestMaxOutputTokens),
		NeedsTools:           req.ToolNames > 0,
		NeedsVision:          req.NeedsVision,
		NeedsStream:          req.Stream,
		Text:                 text,
	}
	dec, err := s.router.Select(ctx, reqs)
	if err != nil {
		return nil, nil, err
	}
	dec.ClassifierVersion = classifierVersion
	return dec.Model, dec, nil
}

func (s *Service) checkRateLimits(ctx context.Context, key *auth.APIKey) error {
	type check struct {
		key   string
		limit int
	}
	checks := []check{
		{key: "rl:global", limit: s.cfg.RateLimits.GlobalPerMin},
		{key: "rl:user:" + itoa(key.UserID), limit: s.cfg.RateLimits.UserPerMin},
	}
	keyLimit := s.cfg.RateLimits.KeyPerMin
	if key.RateLimitPerMin > 0 {
		keyLimit = key.RateLimitPerMin
	}
	checks = append(checks, check{key: "rl:key:" + itoa(key.ID), limit: keyLimit})
	for _, c := range checks {
		if c.limit <= 0 {
			continue
		}
		ok, retry, err := s.rdb.Allow(ctx, c.key, c.limit, time.Minute)
		if err != nil {
			return apperr.RateLimitStoreUnavailable() // fail closed
		}
		if !ok {
			return apperr.RateLimitedAfter(int(math.Ceil(retry.Seconds())))
		}
	}
	return nil
}

func (s *Service) checkModelProviderRateLimits(ctx context.Context, m *model.Model) error {
	checks := []struct {
		key   string
		limit int
	}{
		{key: "rl:model:" + itoa(m.ID), limit: s.cfg.RateLimits.ModelPerMin},
		{key: "rl:provider:" + itoa(m.ProviderID), limit: s.cfg.RateLimits.ProviderPerMin},
	}
	for _, c := range checks {
		if c.limit <= 0 {
			continue
		}
		ok, retry, err := s.rdb.Allow(ctx, c.key, c.limit, time.Minute)
		if err != nil {
			return apperr.RateLimitStoreUnavailable()
		}
		if !ok {
			return apperr.RateLimitedAfter(int(math.Ceil(retry.Seconds())))
		}
	}
	return nil
}

func (s *Service) resolveIdempotency(ctx context.Context, userID int64, idemKey, digest string) error {
	existing, err := s.reqs.ByIdempotency(ctx, userID, idemKey)
	if err != nil {
		if httpx.IsNotFound(err) {
			return nil
		}
		return err
	}
	if existing.RequestDigest != "" && existing.RequestDigest != digest {
		return apperr.IdempotencyConflict()
	}
	if !requestrec.IsTerminal(existing.Status) {
		return apperr.RequestInProgress(existing.RequestID)
	}
	return apperr.IdempotencyReplay()
}

// recordNonBillable writes a minimal terminal request record without charging.
func (s *Service) recordNonBillable(ctx context.Context, requestID string, key *auth.APIKey, requestedModel string, cause error) {
	status := requestrec.StatusInvalidRequest
	if e, ok := apperr.Is(cause); ok {
		switch e.Code {
		case "model_unavailable":
			status = requestrec.StatusModelUnavailable
		case "insufficient_quota":
			status = requestrec.StatusQuotaRejected
		}
	}
	_, _ = s.pool.Exec(ctx, `INSERT INTO request_record
        (request_id, user_id, api_key_id, requested_model, status, error_code, finished_at)
        VALUES ($1,$2,$3,$4,$5,$6,now())
        ON CONFLICT (request_id) DO NOTHING`,
		requestID, key.UserID, key.ID, requestedModel, status, codeOf(cause))
}

func codeOf(err error) string {
	if e, ok := apperr.Is(err); ok {
		return e.Code
	}
	return ""
}

func requestedMaxOutput(req *Messages, platformMax int) int {
	if !req.HasMaxTok || req.MaxTokens == 0 {
		return platformMax
	}
	return req.MaxTokens
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

var errClientDisconnected = errors.New("client disconnected")

// settleRecord finalises billing and usage for a completed or failed request.
func (s *Service) settleRecord(ctx context.Context, rec requestrec.New, usg *Usage, status, errCode string, latency time.Duration) {
	settleCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		settleCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	var (
		costMicro, chargeMicro int64
		usageSource            = requestrec.UsageNone
		inTokens, outTokens    int64
	)
	if usg != nil && usg.Valid() {
		inTokens, outTokens = usg.PromptTokens, usg.CompletionTokens
		costMicro = inTokens*rec.CostInputMicro + outTokens*rec.CostOutputMicro
		chargeMicro = inTokens*rec.ChargeInputMicro + outTokens*rec.ChargeOutputMicro
		usageSource = requestrec.UsageProvider
	}
	err := s.pool.WithTx(settleCtx, func(tx pgx.Tx) error {
		settlement, err := s.quota.Settle(settleCtx, tx, rec.UserID, rec.RequestID, rec.ReservedMicro, chargeMicro)
		if err != nil {
			return err
		}
		// The account may have competing reservations if actual usage exceeded
		// the conservative estimate. Record the amount actually debited.
		chargeMicro = settlement.ChargeMicro
		if err := s.reqs.Finalize(settleCtx, tx, requestrec.Finalize{
			RequestID: rec.RequestID, Status: status, ErrorCode: errCode, UsageSource: usageSource,
			InputTokens: inTokens, OutputTokens: outTokens, CostMicro: costMicro, ChargeMicro: chargeMicro,
			LatencyMs: int(latency.Milliseconds()),
		}); err != nil {
			return err
		}
		if usg != nil && usg.Valid() && rec.ModelID != 0 {
			var providerID int64
			_ = tx.QueryRow(settleCtx, `SELECT provider_id FROM model WHERE id=$1`, rec.ModelID).Scan(&providerID)
			if err := s.usg.Record(settleCtx, tx, rec.RequestID, rec.UserID, rec.ModelID, providerID, inTokens, outTokens, costMicro, chargeMicro, time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("request settlement failed; retaining pending usage snapshot", "request_id", rec.RequestID, "error", err)
		// Preserve the validated usage snapshot so a worker can retry settlement.
		_, persistErr := s.pool.Exec(context.Background(), `UPDATE request_record SET status='settlement_pending',
            error_code=NULLIF($2,''), usage_source=$3, input_tokens=$4, output_tokens=$5,
            cost_micro=$6, charge_micro=$7, latency_ms=$8
            WHERE request_id=$1 AND finished_at IS NULL`, rec.RequestID, errCode, usageSource,
			inTokens, outTokens, costMicro, chargeMicro, int(latency.Milliseconds()))
		if persistErr != nil {
			slog.Error("failed to persist pending settlement", "request_id", rec.RequestID, "error", persistErr)
			return
		}
		return
	}
	if s.metrics != nil {
		s.metrics.Inc("slogan_gateway_requests_total", map[string]string{"status": status})
		s.metrics.Inc("slogan_gateway_usage_source_total", map[string]string{"source": usageSource})
	}
}
