package routing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/apperr"
	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/model"
	"github.com/hchw/slogan/internal/policy"
)

// IntentRequirementsVersion identifies the versioned intent -> capability map.
const IntentRequirementsVersion = "v1"

// CapabilityProvider exposes the active score version and its capabilities.
type CapabilityProvider interface {
	ActiveVersion(ctx context.Context) (int64, error)
	Capabilities(ctx context.Context, versionID int64) (map[int64]map[string]float64, error)
}

// IntentToCapabilities maps an intent label to required capability thresholds.
// The mapping is maintained and versioned in Go; the classifier never supplies
// thresholds.
func IntentToCapabilities(intent string) map[string]float64 {
	switch intent {
	case "coding":
		return map[string]float64{"coding": 0.75, "reasoning": 0.65}
	case "reasoning":
		return map[string]float64{"reasoning": 0.75}
	case "writing":
		return map[string]float64{"writing": 0.70}
	case "translation":
		return map[string]float64{"translation": 0.70}
	case "summarization":
		return map[string]float64{"summarization": 0.70}
	case "casual_chat":
		return map[string]float64{"casual_chat": 0.60}
	case "emotional_support":
		return map[string]float64{"emotional_support": 0.60}
	default: // general
		return map[string]float64{}
	}
}

// Requirements describes a routing request after authentication.
type Requirements struct {
	AllowedModels        []string
	Intent               string
	RequiredCapabilities map[string]float64
	EstimatedInputTokens int
	NeedsTools           bool
	NeedsVision          bool
	NeedsStream          bool
	Text                 string
}

// Decision is the routing outcome.
type Decision struct {
	Model             *model.Model
	Intent            string
	ClassifierVersion string
	ScoreVersionID    int64
	PolicyVersion     string
	Candidates        []Scored
	Weights           Weights
}

// Router selects models deterministically.
type Router struct {
	models *model.Service
	policy *policy.Service
	caps   CapabilityProvider
	pool   *db.Pool
	now    func() time.Time
}

// NewRouter builds a router.
func NewRouter(models *model.Service, pol *policy.Service, caps CapabilityProvider, pool *db.Pool) *Router {
	return &Router{models: models, policy: pol, caps: caps, pool: pool, now: time.Now}
}

// Select filters eligible models and ranks them.
func (r *Router) Select(ctx context.Context, req Requirements) (*Decision, error) {
	pol, err := r.policy.Get(ctx)
	if err != nil {
		return nil, err
	}

	all, err := r.models.EligibleModels(ctx, req.AllowedModels)
	if err != nil {
		return nil, err
	}

	// Hard filter: context, tools, vision.
	filtered := make([]model.Model, 0, len(all))
	for _, m := range all {
		if m.ContextLength > 0 && req.EstimatedInputTokens > m.ContextLength {
			continue
		}
		if req.NeedsTools && !m.SupportsTools {
			continue
		}
		if req.NeedsVision && !m.SupportsVision {
			continue
		}
		if req.NeedsStream && !m.SupportsStream {
			continue
		}
		filtered = append(filtered, m)
	}
	if len(filtered) == 0 {
		return nil, apperr.ModelUnavailable()
	}

	versionID, err := r.caps.ActiveVersion(ctx)
	if err != nil {
		return nil, err
	}
	var capMap map[int64]map[string]float64
	if versionID > 0 {
		capMap, err = r.caps.Capabilities(ctx, versionID)
		if err != nil {
			return nil, err
		}
	}

	stability, samples, latencies := r.stability(ctx, filtered)

	candidates := make([]Candidate, 0, len(filtered))
	for _, m := range filtered {
		dims := capMap[m.ID]
		if dims == nil {
			continue // no historical valid score => never auto-route a new model
		}
		if len(req.RequiredCapabilities) > 0 {
			sum, ok := capabilityMean(dims, req.RequiredCapabilities)
			if !ok {
				continue
			}
			candidates = append(candidates, r.candidate(m, sum, stability, samples, latencies))
			continue
		}
		// No required capabilities (general intent): use overall mean if known.
		sum, ok := overallMean(dims)
		if !ok {
			sum = 0
		}
		candidates = append(candidates, r.candidate(m, sum, stability, samples, latencies))
	}
	if len(candidates) == 0 {
		return nil, apperr.ModelUnavailable()
	}

	highRisk := HighRisk(req.Text)
	w := WeightsFor(pol.LowCapabilityBias, highRisk && pol.HighRiskForceQuality)
	ranked, err := Rank(candidates, w)
	if err != nil {
		return nil, err
	}

	selectedID := ranked[0].ModelID
	var selected *model.Model
	for i := range filtered {
		if filtered[i].ID == selectedID {
			selected = &filtered[i]
			break
		}
	}
	if selected == nil {
		return nil, apperr.ModelUnavailable()
	}
	return &Decision{
		Model:          selected,
		Intent:         req.Intent,
		ScoreVersionID: versionID,
		PolicyVersion:  pol.Version,
		Candidates:     ranked,
		Weights:        w,
	}, nil
}

func (r *Router) candidate(m model.Model, capability float64, stability map[int64]float64, samples map[int64]int, latencies map[int64]float64) Candidate {
	latency, hasLatency := latencies[m.ID]
	c := Candidate{
		ModelID:          m.ID,
		ModelKey:         m.ModelKey,
		Capability:       capability,
		CostMicro:        float64(m.InputPriceMicro),
		HasCost:          m.InputPriceMicro > 0,
		LatencyMs:        latency,
		HasLatency:       hasLatency,
		Stability:        stability[m.ID],
		StabilitySamples: samples[m.ID],
	}
	return c
}

func (r *Router) stability(ctx context.Context, models []model.Model) (map[int64]float64, map[int64]int, map[int64]float64) {
	out := map[int64]float64{}
	samples := map[int64]int{}
	latencies := map[int64]float64{}
	if len(models) == 0 {
		return out, samples, latencies
	}
	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	rows, err := r.pool.Query(ctx, `SELECT model_id,
        count(*) AS total,
        count(*) FILTER (WHERE status='success') AS ok,
        avg(latency_ms) FILTER (WHERE latency_ms > 0) AS avg_latency
        FROM request_record
        WHERE model_id = ANY($1) AND created_at > now() - interval '7 days'
        GROUP BY model_id`, ids)
	if err != nil {
		return out, samples, latencies
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var total, ok int
		var avgLatency *float64
		if err := rows.Scan(&id, &total, &ok, &avgLatency); err != nil {
			continue
		}
		samples[id] = total
		if total > 0 {
			out[id] = float64(ok) / float64(total)
		}
		if avgLatency != nil {
			latencies[id] = *avgLatency
		}
	}
	return out, samples, latencies
}

func capabilityMean(dims, required map[string]float64) (float64, bool) {
	var sum float64
	n := 0
	for dim, threshold := range required {
		v, ok := dims[dim]
		if !ok {
			return 0, false
		}
		if v < threshold {
			return 0, false // below required threshold => ineligible
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, true
	}
	return sum / float64(n), true
}

func overallMean(dims map[string]float64) (float64, bool) {
	if len(dims) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range dims {
		sum += v
	}
	return sum / float64(len(dims)), true
}

// CapabilitiesFromRows is a helper for capability providers.
func CapabilitiesFromRows(rows pgx.Rows) (map[int64]map[string]float64, error) {
	out := map[int64]map[string]float64{}
	for rows.Next() {
		var modelID int64
		var dimension string
		var score float64
		if err := rows.Scan(&modelID, &dimension, &score); err != nil {
			return nil, err
		}
		if out[modelID] == nil {
			out[modelID] = map[string]float64{}
		}
		out[modelID][dimension] = score
	}
	return out, rows.Err()
}
