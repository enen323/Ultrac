package models

// merge.go — applyModelsJson ported rule-for-rule from pi
// (packages/coding-agent/src/core/provider-composer.ts:300-339 and
// :177-208), plus the snapshot the rest of the CLI queries.
//
// # The rules, and where they come from
//
// For each provider entry, in file order (ConfigHome first, project
// second — the project entry wins by arriving later):
//
//  1. an entry that sets no meaningful field is an error (pi's
//     empty-entry rule). Unlike pi — which throws while composing the
//     provider — the failure is recorded per provider and that provider
//     simply stops resolving; the built-in catalog and every other
//     provider keep working.
//  2. baseUrl overrides the baseUrl of every existing entry of that
//     provider.
//  3. models[] entries upsert: a matching id replaces the whole entry
//     (pi builds a fresh model from the definition — cost, window and
//     limits are NOT inherited), anything else appends.
//  4. modelOverrides patch existing entries field by field, after the
//     upserts, so an override can target a model models[] just added.
//  5. the dialect of an existing entry is never touched — that is
//     resolveModel's job (the --api flag and the ANTHROPIC_* convention
//     decide which dialect of a two-dialect provider is in play).
//
// Two pi branches are deliberately absent: the `oauth === "radius"`
// baseUrl carve-out and the `oauth && !baseUrl` check — oauth is ignored
// outright here (plan D4), and the compat deep-merge — compat is not in
// the schema (§4.2).

import (
	"errors"
	"fmt"
	"sort"

	"ultrac/internal/ai"
)

// Snapshot is the built-in catalog with the user's models.json files
// merged over it. It is immutable once built: every accessor returns
// copies, so a caller mutating what it got back cannot corrupt the view
// the next lookup sees (Go's answer to pi's deepFreeze).
type Snapshot struct {
	models    []ai.Model
	providers map[ai.ProviderId]ProviderConfig
	problems  map[ai.ProviderId][]error
}

// Build merges configs over the built-in catalog, lowest precedence
// first. Later configs override earlier ones per provider id (project
// over ConfigHome); within one config, entries apply in sorted-id order
// so the result does not depend on Go's map iteration order.
func Build(cfgs ...Config) *Snapshot {
	s := &Snapshot{
		models:    ai.Catalog(),
		providers: make(map[ai.ProviderId]ProviderConfig),
		problems:  make(map[ai.ProviderId][]error),
	}
	for _, cfg := range cfgs {
		for _, id := range sortedKeys(cfg.Providers) {
			provider := ai.ProviderId(id)
			entry := cfg.Providers[id]
			if isEmptyEntry(entry) {
				s.problems[provider] = append(s.problems[provider], fmt.Errorf(
					"provider %q: must specify \"baseUrl\", \"headers\", \"modelOverrides\", \"apiKey\", \"oauth\", \"api\", or \"models\"", id))
				continue
			}
			// A later level replaces the earlier entry wholesale, so a
			// broken ConfigHome entry is healed by a good project one.
			delete(s.problems, provider)
			s.providers[provider] = entry
			s.apply(provider, entry)
		}
	}
	return s
}

// apply runs rules 2-4 for one provider entry.
func (s *Snapshot) apply(provider ai.ProviderId, entry ProviderConfig) {
	// Rule 2: baseUrl overrides every existing entry of this provider.
	if entry.BaseURL != "" {
		for i, m := range s.models {
			if m.Provider == provider {
				m.BaseURL = entry.BaseURL
				s.models[i] = m
			}
		}
	}

	// Rule 3: models[] upserts.
	for _, def := range entry.Models {
		existing := s.existing(provider, def.ID)
		m, err := modelFromJSON(provider, def, entry, existing)
		if err != nil {
			s.problems[provider] = append(s.problems[provider], err)
			continue
		}
		if idx := s.indexOf(provider, def.ID); idx >= 0 {
			s.models[idx] = m
		} else {
			s.models = append(s.models, m)
		}
	}

	// Rule 4: modelOverrides, after the upserts.
	for _, id := range sortedKeys(entry.ModelOverrides) {
		idx := s.indexOf(provider, id)
		if idx < 0 {
			continue // no matching entry: a no-op, exactly as pi
		}
		s.models[idx] = applyModelOverride(s.models[idx], entry.ModelOverrides[id])
	}
}

// existing returns the current entry for (provider, id), or nil. It only
// supplies the api/baseUrl fallbacks pi's findModelDefaults provides.
func (s *Snapshot) existing(provider ai.ProviderId, id string) *ai.Model {
	idx := s.indexOf(provider, id)
	if idx < 0 {
		return nil
	}
	m := s.models[idx]
	return &m
}

func (s *Snapshot) indexOf(provider ai.ProviderId, id string) int {
	for i, m := range s.models {
		if m.Provider == provider && m.ID == id {
			return i
		}
	}
	return -1
}

// modelFromJSON builds a fresh catalog entry from a models[] definition
// (pi: modelFromJson, provider-composer.ts:210-249). The api chain is
// definition → provider → replaced entry; the baseUrl chain is
// definition → provider → replaced entry. A definition that replaces an
// entry does not inherit anything else: cost, window and limits come from
// the definition or the built-in defaults.
func modelFromJSON(provider ai.ProviderId, def ModelDefinition, entry ProviderConfig, existing *ai.Model) (ai.Model, error) {
	api := firstNonEmpty(def.API, entry.API)
	if api == "" && existing != nil {
		api = string(existing.API)
	}
	if api == "" {
		//lint:ignore ST1005 pi's message, ported verbatim (provider-composer.ts:219)
		return ai.Model{}, fmt.Errorf("provider %q, model %q: no \"api\" specified. Set at provider or model level.", provider, def.ID)
	}
	baseURL := firstNonEmpty(def.BaseURL, entry.BaseURL)
	if baseURL == "" && existing != nil {
		baseURL = existing.BaseURL
	}
	if baseURL == "" {
		//lint:ignore ST1005 pi's message, ported verbatim (provider-composer.ts:223)
		return ai.Model{}, fmt.Errorf("provider %q: \"baseUrl\" is required when defining custom models.", provider)
	}
	if def.ContextWindow != nil && *def.ContextWindow <= 0 {
		return ai.Model{}, fmt.Errorf("provider %q, model %q: invalid contextWindow", provider, def.ID)
	}
	if def.MaxTokens != nil && *def.MaxTokens <= 0 {
		return ai.Model{}, fmt.Errorf("provider %q, model %q: invalid maxTokens", provider, def.ID)
	}
	m := ai.Model{
		ID:          def.ID,
		Name:        firstNonEmpty(def.Name, def.ID),
		API:         ai.Api(api),
		Provider:    provider,
		BaseURL:     baseURL,
		Reasoning:   def.Reasoning != nil && *def.Reasoning,
		Input:       def.Input,
		InputLimits: def.InputLimits,
		Cost:        costOf(def.Cost),
		PromptCache: def.PromptCache,
		// pi's defaults (modelFromJson): 128000 window, 16384 cap.
		ContextWindow:                 derefInt(def.ContextWindow, 128000),
		MaxTokens:                     derefInt(def.MaxTokens, 16384),
		SamplingParams:                def.SamplingParams,
		SamplingParamsByThinkingLevel: def.SamplingParamsByThinkingLevel,
		ThinkingLevelMap:              def.ThinkingLevelMap,
	}
	if len(m.Input) == 0 {
		m.Input = []ai.ModelInputModality{ai.ModalityText}
	}
	// Provider headers are the base, model headers win per key — the same
	// precedence pi's resolveCompatibilityRequestConfig computes at
	// request time, baked in here so the adapters can read one field.
	m.Headers = mergeHeaders(entry.Headers, def.Headers)
	return m, nil
}

// applyModelOverride patches an existing entry field by field (pi:
// applyModelOverride, provider-composer.ts:177-208). baseUrl, api and id
// are deliberately not overridable — an override that could repoint an
// entry at another endpoint would make "which URL did it pick" a
// per-override mystery.
func applyModelOverride(m ai.Model, o ModelOverride) ai.Model {
	out := m
	if o.Name != "" {
		out.Name = o.Name
	}
	if o.Reasoning != nil {
		out.Reasoning = *o.Reasoning
	}
	if len(o.ThinkingLevelMap) > 0 {
		out.ThinkingLevelMap = mergeMaps(m.ThinkingLevelMap, o.ThinkingLevelMap)
	}
	if len(o.Input) > 0 {
		out.Input = o.Input
	}
	out.InputLimits = mergeInputLimits(m.InputLimits, o.InputLimits)
	if o.Cost != nil {
		out.Cost = ai.ModelCost{
			Input:      derefFloat(o.Cost.Input, m.Cost.Input),
			Output:     derefFloat(o.Cost.Output, m.Cost.Output),
			CacheRead:  derefFloat(o.Cost.CacheRead, m.Cost.CacheRead),
			CacheWrite: derefFloat(o.Cost.CacheWrite, m.Cost.CacheWrite),
			Tiers:      o.Cost.Tiers,
		}
		if len(o.Cost.Tiers) == 0 {
			out.Cost.Tiers = m.Cost.Tiers
		}
	}
	if len(o.PromptCache) > 0 {
		out.PromptCache = mergeMaps(m.PromptCache, o.PromptCache)
	}
	if o.ContextWindow != nil {
		out.ContextWindow = *o.ContextWindow
	}
	if o.MaxTokens != nil {
		out.MaxTokens = *o.MaxTokens
	}
	if len(o.SamplingParams) > 0 {
		out.SamplingParams = mergeMaps(m.SamplingParams, o.SamplingParams)
	}
	out.SamplingParamsByThinkingLevel = mergeSamplingByLevel(m.SamplingParamsByThinkingLevel, o.SamplingParamsByThinkingLevel)
	out.Headers = mergeHeaders(m.Headers, o.Headers)
	return out
}

// mergeInputLimits is pi's mergeInputLimits: a spread with the images
// sub-object (and its resize profile) merged one level deeper.
func mergeInputLimits(base, override *ai.ModelInputLimits) *ai.ModelInputLimits {
	if override == nil {
		return base
	}
	out := &ai.ModelInputLimits{}
	if base != nil {
		*out = *base
	}
	if override.MaxRequestBytes != nil {
		out.MaxRequestBytes = override.MaxRequestBytes
	}
	if override.Images == nil {
		return out
	}
	images := &ai.ModelImageInputLimits{}
	if out.Images != nil {
		*images = *out.Images
	}
	if override.Images.MaxPerMessage != nil {
		images.MaxPerMessage = override.Images.MaxPerMessage
	}
	if override.Images.MaxPerRequest != nil {
		images.MaxPerRequest = override.Images.MaxPerRequest
	}
	if override.Images.Resize != nil {
		resize := &ai.ModelImageResizeOptions{}
		if images.Resize != nil {
			*resize = *images.Resize
		}
		if override.Images.Resize.MaxWidth != nil {
			resize.MaxWidth = override.Images.Resize.MaxWidth
		}
		if override.Images.Resize.MaxHeight != nil {
			resize.MaxHeight = override.Images.Resize.MaxHeight
		}
		if override.Images.Resize.MaxBytes != nil {
			resize.MaxBytes = override.Images.Resize.MaxBytes
		}
		if override.Images.Resize.JPEGQuality != nil {
			resize.JPEGQuality = override.Images.Resize.JPEGQuality
		}
		images.Resize = resize
	}
	out.Images = images
	return out
}

// mergeSamplingByLevel is pi's mergeSamplingParamsByThinkingLevel: the
// per-level parameter objects merge, they do not replace.
func mergeSamplingByLevel(base, override ai.SamplingParamsByThinkingLevel) ai.SamplingParamsByThinkingLevel {
	if len(override) == 0 {
		return base
	}
	merged := make(ai.SamplingParamsByThinkingLevel, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for level, params := range override {
		if params == nil {
			merged[level] = nil
			continue
		}
		merged[level] = mergeMaps(base[level], params)
	}
	return merged
}

// isEmptyEntry is pi's applyModelsJson empty check (minus compat and
// authHeader, which this schema does not have). A name-only entry counts
// as empty, exactly as in pi.
func isEmptyEntry(e ProviderConfig) bool {
	return len(e.Models) == 0 &&
		e.BaseURL == "" &&
		len(e.Headers) == 0 &&
		len(e.ModelOverrides) == 0 &&
		e.APIKey == "" &&
		!hasOAuth(e) &&
		e.API == ""
}

// hasOAuth reports whether the entry carries a non-null oauth field.
// JSON null decodes into a json.RawMessage holding "null", which is
// absence for every practical purpose (pi's `!config.oauth`).
func hasOAuth(e ProviderConfig) bool {
	if len(e.OAuth) == 0 {
		return false
	}
	return string(e.OAuth) != "null"
}

// ---------------------------------------------------------------------------
// Snapshot accessors
// ---------------------------------------------------------------------------

// Catalog returns the merged catalog in declaration order (built-in
// entries keep their relative order; appended models trail).
func (s *Snapshot) Catalog() []ai.Model {
	out := make([]ai.Model, len(s.models))
	for i, m := range s.models {
		out[i] = copyModel(m)
	}
	return out
}

// Lookup finds an entry by provider and model id. The bool follows
// ai.Lookup's contract: false with a nil model means "send the id as-is".
func (s *Snapshot) Lookup(provider ai.ProviderId, id string) (ai.Model, bool) {
	if idx := s.indexOf(provider, id); idx >= 0 {
		return copyModel(s.models[idx]), true
	}
	return ai.Model{}, false
}

// DefaultModel returns the first entry for a provider — the
// declaration-order preference, as ai.DefaultModel defines it.
func (s *Snapshot) DefaultModel(provider ai.ProviderId) (ai.Model, bool) {
	for _, m := range s.models {
		if m.Provider == provider {
			return copyModel(m), true
		}
	}
	return ai.Model{}, false
}

// Providers lists every provider with at least one entry, built-in and
// user-declared, in lexical order.
func (s *Snapshot) Providers() []ai.ProviderId {
	seen := make(map[ai.ProviderId]struct{}, len(s.models))
	var out []ai.ProviderId
	for _, m := range s.models {
		if _, dup := seen[m.Provider]; dup {
			continue
		}
		seen[m.Provider] = struct{}{}
		out = append(out, m.Provider)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ProviderConfig returns the winning models.json entry for a provider
// (the project level when both declare it). The bool reports whether any
// models.json file declared it; built-in providers return false.
func (s *Snapshot) ProviderConfig(provider ai.ProviderId) (ProviderConfig, bool) {
	cfg, ok := s.providers[provider]
	return cfg, ok
}

// Problem returns the structural failures recorded for a provider, or
// nil. A provider with a problem does not resolve — that is pi's
// per-provider composition failure, kept per provider so one broken
// entry cannot take the built-in catalog down with it.
func (s *Snapshot) Problem(provider ai.ProviderId) error {
	errs := s.problems[provider]
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// Problems returns every recorded failure, ordered by provider id. The
// CLI prints these as warnings at startup. Each failure already names
// its provider (the messages are pi's, ported verbatim), so this adds
// no prefix of its own — a second one would read "provider "x":
// provider "x": …".
func (s *Snapshot) Problems() []error {
	ids := make([]ai.ProviderId, 0, len(s.problems))
	for id, errs := range s.problems {
		if len(errs) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]error, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Problem(id))
	}
	return out
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// copyModel clones the reference fields so the snapshot stays immutable
// no matter what a caller does with the copy it got.
func copyModel(m ai.Model) ai.Model {
	out := m
	out.Input = append([]ai.ModelInputModality(nil), m.Input...)
	out.Headers = cloneMap(m.Headers)
	out.ThinkingLevelMap = cloneMap(m.ThinkingLevelMap)
	out.PromptCache = cloneMap(m.PromptCache)
	out.SamplingParams = cloneMap(m.SamplingParams)
	out.SamplingParamsByThinkingLevel = cloneMap(m.SamplingParamsByThinkingLevel)
	if m.InputLimits != nil {
		limits := *m.InputLimits
		if m.InputLimits.Images != nil {
			images := *m.InputLimits.Images
			if m.InputLimits.Images.Resize != nil {
				resize := *m.InputLimits.Images.Resize
				images.Resize = &resize
			}
			limits.Images = &images
		}
		out.InputLimits = &limits
	}
	if len(m.Cost.Tiers) > 0 {
		out.Cost.Tiers = append([]ai.ModelCostTier(nil), m.Cost.Tiers...)
	}
	return out
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if len(m) == 0 {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeMaps overlays override onto base without mutating either.
func mergeMaps[K comparable, V any](base, override map[K]V) map[K]V {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[K]V, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// mergeHeaders is the string-map specialisation of mergeMaps, kept for
// readability at the call sites.
func mergeHeaders(base, override map[string]string) map[string]string {
	return mergeMaps(base, override)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func derefFloat(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// costOf converts a decoded cost block, defaulting to zero rates (pi:
// `definition.cost ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }`).
func costOf(c *ModelCost) ai.ModelCost {
	if c == nil {
		return ai.ModelCost{}
	}
	return ai.ModelCost{
		Input:      c.Input,
		Output:     c.Output,
		CacheRead:  c.CacheRead,
		CacheWrite: c.CacheWrite,
		Tiers:      c.Tiers,
	}
}
