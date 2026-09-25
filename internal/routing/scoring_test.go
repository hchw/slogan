package routing

import (
	"testing"
)

func almost(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestWeightsFor(t *testing.T) {
	w := WeightsFor(0, false)
	if !almost(w.Capability, 0.70) || !almost(w.Cost, 0.10) {
		t.Fatalf("bias 0 weights: %+v", w)
	}
	w = WeightsFor(100, false)
	if !almost(w.Capability, 0.20) || !almost(w.Cost, 0.70) {
		t.Fatalf("bias 100 weights: %+v", w)
	}
	w = WeightsFor(50, true)
	if !almost(w.Capability, 0.70) || !almost(w.Cost, 0.10) {
		t.Fatalf("high risk quality weights: %+v", w)
	}
}

func TestRankMissingOptionalDimensions(t *testing.T) {
	cands := []Candidate{
		{ModelID: 1, ModelKey: "a", Capability: 0.9, HasCost: false, StabilitySamples: 100, Stability: 0.99},
		{ModelID: 2, ModelKey: "b", Capability: 0.9, CostMicro: 10, HasCost: true, StabilitySamples: 100, Stability: 0.99},
	}
	ranked, err := Rank(cands, WeightsFor(0, false))
	if err != nil {
		t.Fatal(err)
	}
	// Candidate b has a real cost score (1) while a scores 0 for missing cost.
	if ranked[0].ModelID != 2 {
		t.Fatalf("expected model 2 to win, got %d", ranked[0].ModelID)
	}
}

func TestRankTieBreak(t *testing.T) {
	cands := []Candidate{
		{ModelID: 1, ModelKey: "zeta", Capability: 0.5, CostMicro: 10, HasCost: true, Stability: 0.8, StabilitySamples: 100},
		{ModelID: 2, ModelKey: "alpha", Capability: 0.5, CostMicro: 10, HasCost: true, Stability: 0.8, StabilitySamples: 100},
	}
	ranked, err := Rank(cands, WeightsFor(50, false))
	if err != nil {
		t.Fatal(err)
	}
	if ranked[0].ModelKey != "alpha" {
		t.Fatalf("expected lexicographically smaller model to win ties, got %s", ranked[0].ModelKey)
	}
}

func TestRankCostBiasPrefersCheaper(t *testing.T) {
	cands := []Candidate{
		{ModelID: 1, ModelKey: "strong", Capability: 1.0, CostMicro: 100, HasCost: true, StabilitySamples: 100, Stability: 0.99},
		{ModelID: 2, ModelKey: "cheap", Capability: 0.8, CostMicro: 1, HasCost: true, StabilitySamples: 100, Stability: 0.99},
	}
	qualityFirst, _ := Rank(cands, WeightsFor(0, false))
	if qualityFirst[0].ModelKey != "strong" {
		t.Fatalf("quality-first should pick strong, got %s", qualityFirst[0].ModelKey)
	}
	costFirst, _ := Rank(cands, WeightsFor(100, false))
	if costFirst[0].ModelKey != "cheap" {
		t.Fatalf("cost-first should pick cheap, got %s", costFirst[0].ModelKey)
	}
}

func TestRankEmpty(t *testing.T) {
	if _, err := Rank(nil, WeightsFor(50, false)); err == nil {
		t.Fatal("expected error for empty candidate set")
	}
}

func TestStabilityPrior(t *testing.T) {
	cands := []Candidate{{ModelID: 1, ModelKey: "a", Capability: 1, HasCost: true, CostMicro: 1, Stability: 0.0, StabilitySamples: 3}}
	ranked, _ := Rank(cands, WeightsFor(0, false))
	if !almost(ranked[0].Stability, 0.5) {
		t.Fatalf("expected stability prior 0.5 for small samples, got %v", ranked[0].Stability)
	}
}

func TestHighRisk(t *testing.T) {
	if !HighRisk("请帮我分析这段代码的 SQL 注入 漏洞") {
		t.Fatal("expected security rule to match")
	}
	if !HighRisk("drop table users migration") {
		t.Fatal("expected destructive db rule to match")
	}
	if HighRisk("今天天气不错，推荐一部电影") {
		t.Fatal("did not expect a match")
	}
}

func TestCapabilityMean(t *testing.T) {
	dims := map[string]float64{"coding": 0.9, "reasoning": 0.7}
	req := map[string]float64{"coding": 0.75, "reasoning": 0.65}
	v, ok := capabilityMean(dims, req)
	if !ok || !almost(v, 0.8) {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := capabilityMean(dims, map[string]float64{"coding": 0.95}); ok {
		t.Fatal("expected below-threshold to be ineligible")
	}
	if _, ok := capabilityMean(dims, map[string]float64{"vision": 0.1}); ok {
		t.Fatal("expected missing dimension to be ineligible")
	}
}
