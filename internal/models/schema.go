// Package models loads the user's models.json and merges it over the
// built-in model catalog, so a provider can be added without recompiling
// (docs/开发计划/新功能计划/provider-config-plan.md).
//
// # Provenance
//
// A port of pi's provider-configuration layer:
//
//   - packages/coding-agent/src/core/model-config.ts — the schema and the
//     load semantics (ENOENT → empty; parse/schema failure → an error the
//     caller downgrades to a warning).
//   - packages/coding-agent/src/core/provider-composer.ts — the merge rules
//     (applyModelsJson / applyModelOverride), ported rule-for-rule in
//     merge.go.
//
// Two deviations, both decided in the plan (§7 D4/D6): pi's `compat` and
// `authHeader` provider fields are not in the schema (unknown fields are
// rejected with a path), and pi's `oauth` field is accepted but ignored —
// an oauth credential cannot authenticate yet.
//
// # Layering
//
// This package imports internal/ai (for the Model type and the built-in
// catalog) and nothing else from ultrac. The CLI layer loads the files;
// this package decodes, validates and merges them.
package models

import (
	"encoding/json"

	"ultrac/internal/ai"
)

// Config is one decoded models.json document.
//
// The providers map is keyed by provider id exactly as the file spells it;
// "my-proxy" in the file is "my-proxy" on the --model flag.
type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

// ProviderConfig is one provider entry. Every field is optional at the
// schema level; an entry that sets none of them is a merge-time error
// (pi's applyModelsJson empty-entry rule, ported in merge.go).
type ProviderConfig struct {
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	// APIKey is a literal credential (plan D5/D6: no $VAR, no !command —
	// write the key, use the key).
	APIKey string `json:"apiKey,omitempty"`
	// API is the default dialect for this provider's models[] entries.
	API string `json:"api,omitempty"`
	// OAuth is accepted and ignored (plan D4): a user copying pi's
	// models.json keeps their oauth block without an error, but oauth
	// authentication does not work yet. Distinct from auth.json's
	// type:"oauth", which the credential chain rejects explicitly.
	OAuth   json.RawMessage   `json:"oauth,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Models are upserted onto the catalog: an entry with a matching id is
	// replaced, anything else is appended.
	Models []ModelDefinition `json:"models,omitempty"`
	// ModelOverrides patch existing entries field by field, keyed by model
	// id; an id with no matching entry is a no-op (pi's rule).
	ModelOverrides map[string]ModelOverride `json:"modelOverrides,omitempty"`
}

// ModelDefinition is one models[] entry. Required: id. Everything else
// falls back to the provider entry, then to the entry being replaced, then
// to the built-in defaults (pi's modelFromJson chain).
type ModelDefinition struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	API       string `json:"api,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	Reasoning *bool  `json:"reasoning,omitempty"`
	// ThinkingLevelMap marks per-level support; a nil value means the
	// level is unsupported (ai.ThinkingLevelMap).
	ThinkingLevelMap ai.ThinkingLevelMap     `json:"thinkingLevelMap,omitempty"`
	Input            []ai.ModelInputModality `json:"input,omitempty"`
	InputLimits      *ai.ModelInputLimits    `json:"inputLimits,omitempty"`
	// Cost is a full cost block; every rate is required when present.
	Cost                          *ModelCost                       `json:"cost,omitempty"`
	PromptCache                   ai.ModelPromptCache              `json:"promptCache,omitempty"`
	ContextWindow                 *int                             `json:"contextWindow,omitempty"`
	MaxTokens                     *int                             `json:"maxTokens,omitempty"`
	SamplingParams                ai.SamplingParams                `json:"samplingParams,omitempty"`
	SamplingParamsByThinkingLevel ai.SamplingParamsByThinkingLevel `json:"samplingParamsByThinkingLevel,omitempty"`
	// Headers are sent with every request for this model, merged over the
	// provider's headers.
	Headers map[string]string `json:"headers,omitempty"`
}

// ModelOverride is one modelOverrides entry: every field optional, merged
// field-by-field onto the existing catalog entry (pi's applyModelOverride).
// There is deliberately no id — the map key is the id.
type ModelOverride struct {
	Name             string                  `json:"name,omitempty"`
	Reasoning        *bool                   `json:"reasoning,omitempty"`
	ThinkingLevelMap ai.ThinkingLevelMap     `json:"thinkingLevelMap,omitempty"`
	Input            []ai.ModelInputModality `json:"input,omitempty"`
	InputLimits      *ai.ModelInputLimits    `json:"inputLimits,omitempty"`
	// Cost is a partial merge: a nil field keeps the entry's value.
	Cost                          *ModelCostOverride               `json:"cost,omitempty"`
	PromptCache                   ai.ModelPromptCache              `json:"promptCache,omitempty"`
	ContextWindow                 *int                             `json:"contextWindow,omitempty"`
	MaxTokens                     *int                             `json:"maxTokens,omitempty"`
	SamplingParams                ai.SamplingParams                `json:"samplingParams,omitempty"`
	SamplingParamsByThinkingLevel ai.SamplingParamsByThinkingLevel `json:"samplingParamsByThinkingLevel,omitempty"`
	Headers                       map[string]string                `json:"headers,omitempty"`
}

// ModelCost is the full pricing block. Mirrors ai.ModelCost; it exists as
// its own type so the schema can require all four rates in models[] while
// modelOverrides accepts a partial one.
type ModelCost struct {
	Input      float64            `json:"input"`
	Output     float64            `json:"output"`
	CacheRead  float64            `json:"cacheRead"`
	CacheWrite float64            `json:"cacheWrite"`
	Tiers      []ai.ModelCostTier `json:"tiers,omitempty"`
}

// ModelCostOverride is the partial pricing block modelOverrides accepts.
type ModelCostOverride struct {
	Input      *float64           `json:"input,omitempty"`
	Output     *float64           `json:"output,omitempty"`
	CacheRead  *float64           `json:"cacheRead,omitempty"`
	CacheWrite *float64           `json:"cacheWrite,omitempty"`
	Tiers      []ai.ModelCostTier `json:"tiers,omitempty"`
}
