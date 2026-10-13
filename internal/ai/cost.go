package ai

// Cost accounting for one request at catalog prices.
//
// Ported from pi's calculateCost (packages/ai/src/models.ts:1206-1226). Rates
// are per million tokens; the highest matching tier above the request's input
// tokens applies, and Anthropic's 1h cache writes bill at twice the base input
// rate — which is why CacheWrite1h is a pointer on Usage: a provider that does
// not report the TTL breakdown must not be billed as if every cache write were
// long-lived.

// CalculateCost fills u.Cost from the model's rates and the token counts.
//
// It mutates u in place and returns it, mirroring pi's signature. The input
// total includes cache reads and writes, because that is the volume the tiers
// are keyed on.
func CalculateCost(model Model, u *Usage) Cost {
	if u == nil {
		return Cost{}
	}
	inputTokens := u.Input + u.CacheRead + u.CacheWrite
	rates := model.Cost
	matchedThreshold := -1
	for _, tier := range model.Cost.Tiers {
		if inputTokens > tier.InputTokensAbove && tier.InputTokensAbove > matchedThreshold {
			rates = ModelCost{
				Input:      tier.Input,
				Output:     tier.Output,
				CacheRead:  tier.CacheRead,
				CacheWrite: tier.CacheWrite,
			}
			matchedThreshold = tier.InputTokensAbove
		}
	}

	longWrite := 0
	if u.CacheWrite1h != nil {
		longWrite = *u.CacheWrite1h
	}
	shortWrite := u.CacheWrite - longWrite
	u.Cost = Cost{
		Input:      (rates.Input / 1e6) * float64(u.Input),
		Output:     (rates.Output / 1e6) * float64(u.Output),
		CacheRead:  (rates.CacheRead / 1e6) * float64(u.CacheRead),
		CacheWrite: (rates.CacheWrite*float64(shortWrite) + rates.Input*2*float64(longWrite)) / 1e6,
	}
	u.Cost.Total = u.Cost.Input + u.Cost.Output + u.Cost.CacheRead + u.Cost.CacheWrite
	return u.Cost
}

// SumTokens recomputes the derived total the way the Anthropic adapter does:
// providers that report component counts but no total.
func SumTokens(u *Usage) {
	if u == nil {
		return
	}
	u.TotalTokens = u.Input + u.Output + u.CacheRead + u.CacheWrite
}
