// Package routing implements deterministic model selection: hard-constraint
// filtering, capability/cost/latency/stability scoring and tie-breaking.
package routing

import (
	"sort"

	"github.com/hchw/slogan/internal/apperr"
)

// Candidate is a model that has already passed hard-constraint filtering.
type Candidate struct {
	ModelID          int64
	ModelKey         string
	Capability       float64 // arithmetic mean of required dimension scores
	CostMicro        float64 // representative per-token cost
	HasCost          bool
	LatencyMs        float64
	HasLatency       bool
	Stability        float64 // recent success ratio
	StabilitySamples int
}

// Weights are the scoring weights.
type Weights struct {
	Capability float64
	Cost       float64
	Latency    float64
	Stability  float64
}

// WeightsFor computes weights from the low-capability-bias value (0..100) and
// the high-risk quality-first flag.
func WeightsFor(bias int, highRiskQuality bool) Weights {
	if highRiskQuality {
		return Weights{Capability: 0.70, Cost: 0.10, Latency: 0.10, Stability: 0.10}
	}
	b := float64(bias) / 100.0
	if b < 0 {
		b = 0
	}
	if b > 1 {
		b = 1
	}
	return Weights{
		Capability: 0.70 - 0.50*b,
		Cost:       0.10 + 0.60*b,
		Latency:    0.10,
		Stability:  0.10,
	}
}

// Scored is a ranked candidate.
type Scored struct {
	ModelID          int64
	ModelKey         string
	Score            float64
	Capability       float64
	Cost             float64
	Latency          float64
	Stability        float64
	StabilitySamples int
}

// Tier is the stability prior when fewer than 20 samples are available.
const stabilitySampleFloor = 20
const stabilityPrior = 0.5

// Rank scores and sorts candidates deterministically. It returns an error when
// there are no candidates.
func Rank(cands []Candidate, w Weights) ([]Scored, error) {
	if len(cands) == 0 {
		return nil, apperr.ModelUnavailable()
	}
	costScore := reverseNormalize(cands, func(c Candidate) (float64, bool) { return c.CostMicro, c.HasCost })
	latScore := reverseNormalize(cands, func(c Candidate) (float64, bool) { return c.LatencyMs, c.HasLatency })

	out := make([]Scored, 0, len(cands))
	for i, c := range cands {
		stability := c.Stability
		if c.StabilitySamples < stabilitySampleFloor {
			stability = stabilityPrior
		}
		score := w.Capability*c.Capability + w.Cost*costScore[i] + w.Latency*latScore[i] + w.Stability*stability
		out = append(out, Scored{
			ModelID:          c.ModelID,
			ModelKey:         c.ModelKey,
			Score:            score,
			Capability:       c.Capability,
			Cost:             costScore[i],
			Latency:          latScore[i],
			Stability:        stability,
			StabilitySamples: c.StabilitySamples,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Stability != b.Stability {
			return a.Stability > b.Stability
		}
		if a.Cost != b.Cost {
			return a.Cost > b.Cost // higher normalized cost score = lower cost
		}
		return a.ModelKey < b.ModelKey
	})
	return out, nil
}

// reverseNormalize returns, for each candidate, a value in [0,1] where the
// lowest raw value scores 1. Missing values score 0. When all present values
// are equal, every present candidate scores 1.
func reverseNormalize(cands []Candidate, get func(Candidate) (float64, bool)) []float64 {
	out := make([]float64, len(cands))
	min, max := 0.0, 0.0
	have := false
	for _, c := range cands {
		v, ok := get(c)
		if !ok {
			continue
		}
		if !have {
			min, max, have = v, v, true
			continue
		}
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if !have {
		return out
	}
	for i, c := range cands {
		v, ok := get(c)
		if !ok {
			out[i] = 0
			continue
		}
		if max == min {
			out[i] = 1
			continue
		}
		out[i] = (max - v) / (max - min)
	}
	return out
}
