package ai

import (
	"math"
	"testing"
)

func costModel() Model {
	return Model{
		ID: "priced", API: APIAnthropicMessages, Provider: ProviderAnthropic,
		Cost: ModelCost{Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
	}
}

func TestCalculateCostBasic(t *testing.T) {
	u := &Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 0, CacheWrite: 0}
	c := CalculateCost(costModel(), u)
	// 1M in at $3 + 1M out at $15.
	if math.Abs(c.Total-18.0) > 1e-9 {
		t.Errorf("total = %v, want 18", c.Total)
	}
	if math.Abs(c.Input-3.0) > 1e-9 || math.Abs(c.Output-15.0) > 1e-9 {
		t.Errorf("components = %+v", c)
	}
}

func TestCalculateCostLongCacheWriteBillsDouble(t *testing.T) {
	// Anthropic charges 2x the base input rate for 1h cache writes
	// (pi: models.ts:1217-1223).
	long := 1_000_000
	u := &Usage{CacheWrite: long, CacheWrite1h: &long}
	c := CalculateCost(costModel(), u)
	// 1M long writes: 2 * $3 = $6, not $3.75.
	if math.Abs(c.CacheWrite-6.0) > 1e-9 {
		t.Errorf("cacheWrite = %v, want 6 (2x base input for the 1h tier)", c.CacheWrite)
	}
}

func TestCalculateCostTierThreshold(t *testing.T) {
	m := costModel()
	m.Cost.Tiers = []ModelCostTier{{
		InputTokensAbove: 200_000,
		Input:            1.5, Output: 7.5, CacheRead: 0.15, CacheWrite: 1.875,
	}}
	// Below the threshold the base rates apply.
	small := &Usage{Input: 100_000, Output: 100_000}
	if c := CalculateCost(m, small); math.Abs(c.Total-(0.3+1.5)) > 1e-9 {
		t.Errorf("below-tier total = %v", c.Total)
	}
	// Above it the tier rates apply; the input total includes cache counts.
	big := &Usage{Input: 250_000, Output: 100_000}
	if c := CalculateCost(m, big); math.Abs(c.Input-0.375) > 1e-9 {
		t.Errorf("above-tier input = %v, want 0.375 (1.5/M * 250k)", c.Input)
	}
}

func TestCalculateCostNilUsageIsSafe(t *testing.T) {
	if c := CalculateCost(costModel(), nil); c != (Cost{}) {
		t.Errorf("nil usage produced %+v", c)
	}
}

func TestSumTokens(t *testing.T) {
	u := &Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 3}
	SumTokens(u)
	if u.TotalTokens != 20 {
		t.Errorf("total = %d, want 20", u.TotalTokens)
	}
	SumTokens(nil) // must not panic
}
