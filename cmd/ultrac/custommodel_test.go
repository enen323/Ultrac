// custommodel_test.go — the add_custom_model persistence regression
// (custom-model-plan.md P6/G6, re-pointed at settings by
// settings-unify-plan.md P3).
//
// What it pins down is the premise that can break silently ("it works on my
// machine"): the declaration AND its credential land in the USER layer's
// settings, and the startup path re-reads them. The test therefore builds a
// second harness from the same files with nothing shared — new resolver, new
// snapshot, new closures — and asserts the model is there and its key resolves.
// A write to the project layer, or a snapshot that stops reloading, turns this
// red.
//
// It also witnesses the in-process half (D4): the live harness sees the new
// model and credential without a restart, because reloadConfig swapped the
// pointer the per-request resolver reads.

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/models"
	"ultrac/internal/rpc"
)

// customSpec is the dialog's payload for a DeepSeek-compatible endpoint:
// what the P5 acceptance fills in, minus the real key.
func customSpec() rpc.CustomModelSpec {
	return rpc.CustomModelSpec{
		Provider:      "api.example.com",
		BaseURL:       "https://api.example.com/v1",
		APIKey:        "sk-custom-key",
		API:           string(ai.APIOpenAICompletions),
		ModelID:       "example-chat",
		ContextWindow: 128000,
		MaxTokens:     8192,
	}
}

// providersOnDisk reads the providers section of the user settings file — the
// one place a declaration can live since settings-unify-plan.md.
func providersOnDisk(t *testing.T, home string) map[string]models.ProviderConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var s config.Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return s.Providers
}

func TestAddCustomModelPersistsAcrossRestart(t *testing.T) {
	env := newPrintEnv(t)
	spec := customSpec()

	// -- write, through the real host ------------------------------------
	h1, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	host := newRPCHost(h1, context.Background())
	result, err := host.AddCustomModel(spec)
	if err != nil {
		t.Fatalf("AddCustomModel: %v", err)
	}

	// The response is the fresh catalog (the picker patches from it, no
	// second round trip) plus the declared providers.
	var added ai.Model
	found := false
	for _, m := range result.Models {
		if m.Provider == ai.ProviderId(spec.Provider) && m.ID == spec.ModelID {
			added, found = m, true
			break
		}
	}
	if !found {
		t.Fatalf("result.models has no %s/%s", spec.Provider, spec.ModelID)
	}
	if added.BaseURL != spec.BaseURL || added.API != ai.Api(spec.API) {
		t.Errorf("added model = %+v, want the spec's endpoint and dialect", added)
	}
	if added.ContextWindow != spec.ContextWindow || added.MaxTokens != spec.MaxTokens {
		t.Errorf("limits = (%d, %d), want (%d, %d)",
			added.ContextWindow, added.MaxTokens, spec.ContextWindow, spec.MaxTokens)
	}
	var info *rpc.CustomModelProviderInfo
	for i := range result.Providers {
		if result.Providers[i].ID == spec.Provider {
			info = &result.Providers[i]
		}
	}
	if info == nil {
		t.Fatalf("result.providers has no %q: %+v", spec.Provider, result.Providers)
	}
	if info.BaseURL != spec.BaseURL ||
		info.API != spec.API || len(info.ModelIDs) != 1 || info.ModelIDs[0] != spec.ModelID {
		t.Errorf("provider info = %+v, want the declaration", *info)
	}

	// -- the in-process half (D4) -----------------------------------------
	if !containsModel(h1.availableModels(), added) {
		t.Error("the live harness must offer the new model without a restart")
	}
	if got := h1.credentialFor(spec.Provider); got != spec.APIKey {
		t.Errorf("credentialFor(%q) = %q, want the key just written", spec.Provider, got)
	}

	// -- the file: one place, user layer ----------------------------------
	//
	// The declaration and the key are one entry now. The old split (key in
	// auth.json, URL in models.json) is what let a key drift away from the
	// endpoint it belongs to, so the separation is asserted AGAINST here.
	providers := providersOnDisk(t, env.home)
	entry, ok := providers[spec.Provider]
	if !ok {
		t.Fatalf("no %q provider in settings.json", spec.Provider)
	}
	if entry.BaseURL != spec.BaseURL || entry.API != spec.API {
		t.Errorf("entry = %+v, want the spec's endpoint and dialect", entry)
	}
	if entry.APIKey != spec.APIKey {
		t.Errorf("entry.apiKey = %q, want the written key in the same entry", entry.APIKey)
	}
	if len(entry.Models) != 1 || entry.Models[0].ID != spec.ModelID {
		t.Errorf("entry.models = %+v, want just %q", entry.Models, spec.ModelID)
	}
	// The legacy files must not be created by this flow any more.
	for _, legacy := range []string{"models.json", "auth.json"} {
		if fileExists(filepath.Join(env.home, legacy)) {
			t.Errorf("%s was written: the unify migration made it obsolete", legacy)
		}
	}
	// The settings file keeps whatever else it had (the fixture's own
	// provider), proving the write goes through the preserving marshal rather
	// than rewriting the document from the struct alone.
	raw := readFile(t, filepath.Join(env.home, "settings.json"))
	if !strings.Contains(raw, `"deepseek"`) {
		t.Errorf("settings.json lost the pre-existing provider on write:\n%s", raw)
	}

	// -- the restart: nothing shared, everything re-read ------------------
	h2, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("second buildHarness exit = %d, want 0", code)
	}
	if !containsModel(h2.availableModels(), added) {
		t.Error("a fresh harness must offer the model written before it")
	}
	if got := h2.credentialFor(spec.Provider); got != spec.APIKey {
		t.Errorf("after restart credentialFor(%q) = %q, want the written key", spec.Provider, got)
	}
}

// TestAddCustomModelUpsertsSameProvider — a second add for the same provider id
// extends it rather than duplicating it: the merge rule at load time is an
// upsert by model id, so the file must be one too. The endpoint and dialect
// already declared win (the user may have tuned them by hand), and a re-saved
// model keeps its display name.
func TestAddCustomModelUpsertsSameProvider(t *testing.T) {
	env := newPrintEnv(t)
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	host := newRPCHost(h, context.Background())

	first := customSpec()
	first.ModelName = "Example Chat"
	if _, err := host.AddCustomModel(first); err != nil {
		t.Fatalf("first AddCustomModel: %v", err)
	}
	second := customSpec()
	second.ModelID = "example-reasoner"
	second.ModelName = "Example Reasoner"
	if _, err := host.AddCustomModel(second); err != nil {
		t.Fatalf("second AddCustomModel: %v", err)
	}
	// A third save of the first model, without a name: the display name
	// already on file survives.
	again := customSpec()
	if _, err := host.AddCustomModel(again); err != nil {
		t.Fatalf("third AddCustomModel: %v", err)
	}

	entry, ok := providersOnDisk(t, env.home)[first.Provider]
	if !ok {
		t.Fatalf("provider %q missing from settings.json", first.Provider)
	}
	if len(entry.Models) != 2 {
		t.Fatalf("models = %d, want 2 (an upsert, not an append)", len(entry.Models))
	}
	byID := map[string]string{}
	for _, m := range entry.Models {
		byID[m.ID] = m.Name
	}
	if byID["example-chat"] != "Example Chat" {
		t.Errorf("example-chat name = %q, want the preserved display name", byID["example-chat"])
	}
	if byID["example-reasoner"] != "Example Reasoner" {
		t.Errorf("example-reasoner name = %q, want the given name", byID["example-reasoner"])
	}

	// Both are selectable and both resolve the one key.
	for _, m := range h.availableModels() {
		if m.Provider != ai.ProviderId(first.Provider) {
			continue
		}
		if m.ID != "example-chat" && m.ID != "example-reasoner" {
			t.Errorf("unexpected model %q", m.ID)
		}
	}
	if got := h.credentialFor(first.Provider); got != first.APIKey {
		t.Errorf("credentialFor(%q) = %q, want %q", first.Provider, got, first.APIKey)
	}
}

// TestAddCustomModelNormalizesBaseURL — a pasted full endpoint is trimmed to
// API-root level, because the adapters append their own path
// (settings-unify-plan.md D10). Without this the request goes to
// /chat/completions/chat/completions and 404s against a perfectly good key.
func TestAddCustomModelNormalizesBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.stepfun.com/step_plan/v1/chat/completions": "https://api.stepfun.com/step_plan/v1",
		"https://api.deepseek.com/anthropic/v1/messages":        "https://api.deepseek.com/anthropic",
		"https://api.deepseek.com/anthropic/":                   "https://api.deepseek.com/anthropic",
		"https://api.openai.com/v1":                             "https://api.openai.com/v1",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAddCustomModelRejectsBadSpec — the host's own entry point refuses a
// malformed spec before touching the file. The protocol layer checks the same
// rules; this is what keeps a direct caller from writing half a provider.
func TestAddCustomModelRejectsBadSpec(t *testing.T) {
	env := newPrintEnv(t)
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	host := newRPCHost(h, context.Background())

	bad := customSpec()
	bad.BaseURL = "ftp://example.com"
	if _, err := host.AddCustomModel(bad); err == nil {
		t.Fatal("a non-http baseUrl must be refused")
	}
	bad = customSpec()
	bad.ModelID = ""
	if _, err := host.AddCustomModel(bad); err == nil {
		t.Fatal("an empty modelId must be refused")
	}
	// Nothing reached the disk: the refused provider is absent, and the
	// fixture's own provider is untouched.
	onDisk := providersOnDisk(t, env.home)
	if _, ok := onDisk[customSpec().Provider]; ok {
		t.Fatalf("a refused spec wrote providers: %+v", onDisk)
	}
}

// TestAddCustomModelThinkingLevels — the dialog's checkboxes land as a
// support SET, not a severity ramp (custom-model-thinking-limits-plan.md
// D1): "only high" must offer high without low or medium, and "low + high"
// must not grow a medium in between. Every case is checked twice — on the
// live harness (D4 hot reload) and on a harness built from the file alone
// (the restart path) — plus once on the bytes, because an ABSENT level
// counts as SUPPORTED (ai.SupportedThinkingLevels) and only the explicit
// nulls keep the picker honest (D5).
func TestAddCustomModelThinkingLevels(t *testing.T) {
	cases := []struct {
		name   string
		levels []string
		want   []ai.ModelThinkingLevel
	}{
		{"only high", []string{"high"}, []ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkHigh}},
		{"only low", []string{"low"}, []ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkLow}},
		{"low and high", []string{"low", "high"}, []ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkLow, ai.ModelThinkHigh}},
		{"all three", []string{"low", "medium", "high"}, []ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkLow, ai.ModelThinkMedium, ai.ModelThinkHigh}},
		{"duplicate collapses", []string{"low", "low", "high"}, []ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkLow, ai.ModelThinkHigh}},
		{"none", nil, []ai.ModelThinkingLevel{ai.ModelThinkOff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPrintEnv(t)
			h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
			if code != 0 {
				t.Fatalf("buildHarness exit = %d, want 0", code)
			}
			spec := customSpec()
			spec.ThinkingLevels = tc.levels
			if _, err := newRPCHost(h, context.Background()).AddCustomModel(spec); err != nil {
				t.Fatalf("AddCustomModel: %v", err)
			}

			if got := ai.SupportedThinkingLevels(catalogModel(t, h, spec)); !slices.Equal(got, tc.want) {
				t.Errorf("live levels = %v, want %v", got, tc.want)
			}
			h2, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
			if code != 0 {
				t.Fatalf("second buildHarness exit = %d, want 0", code)
			}
			if got := ai.SupportedThinkingLevels(catalogModel(t, h2, spec)); !slices.Equal(got, tc.want) {
				t.Errorf("levels after restart = %v, want %v", got, tc.want)
			}

			def := modelDefinitionOnDisk(t, env.home, spec)
			if len(tc.levels) == 0 {
				// A non-reasoning declaration must stay byte-identical to
				// what this flow wrote before the levels existed (D2).
				if def.Reasoning != nil || def.ThinkingLevelMap != nil {
					t.Errorf("definition = %+v, want neither field for a non-reasoning model", def)
				}
				if raw := readFile(t, filepath.Join(env.home, "settings.json")); strings.Contains(raw, `"reasoning"`) ||
					strings.Contains(raw, `"thinkingLevelMap"`) {
					t.Errorf("settings.json grew a reasoning block nobody asked for:\n%s", raw)
				}
				return
			}
			if def.Reasoning == nil || !*def.Reasoning {
				t.Fatalf("reasoning = %v, want true once any level is ticked", def.Reasoning)
			}
			ticked := map[ai.ModelThinkingLevel]bool{}
			for _, level := range tc.levels {
				ticked[ai.ModelThinkingLevel(level)] = true
			}
			for _, level := range []ai.ModelThinkingLevel{
				ai.ModelThinkMinimal, ai.ModelThinkLow, ai.ModelThinkMedium,
				ai.ModelThinkHigh, ai.ModelThinkXHigh, ai.ModelThinkMax,
			} {
				value, present := def.ThinkingLevelMap[level]
				if !present {
					t.Errorf("thinkingLevelMap[%s] is absent — an absent level counts as supported", level)
					continue
				}
				if ticked[level] {
					if value == nil || *value != string(level) {
						t.Errorf("thinkingLevelMap[%s] = %v, want the identity mapping", level, value)
					}
					continue
				}
				if value != nil {
					t.Errorf("thinkingLevelMap[%s] = %q, want nil for an unticked level", level, *value)
				}
			}
		})
	}
}

// TestAddCustomModelThinkingLevelsCleared — re-saving with nothing ticked
// takes the capability away again. The upsert replaces the definition
// wholesale (D8), so "omitted" means "not a reasoning model" rather than
// "keep what was there"; without this there would be no way back through
// the dialog.
func TestAddCustomModelThinkingLevelsCleared(t *testing.T) {
	env := newPrintEnv(t)
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	host := newRPCHost(h, context.Background())

	reasoning := customSpec()
	reasoning.ThinkingLevels = []string{"high"}
	if _, err := host.AddCustomModel(reasoning); err != nil {
		t.Fatalf("AddCustomModel(high): %v", err)
	}
	if got := ai.SupportedThinkingLevels(catalogModel(t, h, reasoning)); !slices.Equal(got,
		[]ai.ModelThinkingLevel{ai.ModelThinkOff, ai.ModelThinkHigh}) {
		t.Fatalf("levels = %v, want the declared high", got)
	}

	if _, err := host.AddCustomModel(customSpec()); err != nil {
		t.Fatalf("AddCustomModel(none): %v", err)
	}
	if got := ai.SupportedThinkingLevels(catalogModel(t, h, reasoning)); !slices.Equal(got,
		[]ai.ModelThinkingLevel{ai.ModelThinkOff}) {
		t.Errorf("levels after clearing = %v, want off only", got)
	}
	if def := modelDefinitionOnDisk(t, env.home, reasoning); def.Reasoning != nil || def.ThinkingLevelMap != nil {
		t.Errorf("definition = %+v, want the reasoning fields gone again", def)
	}
}

// catalogModel finds the model a spec declared in the harness's catalog.
func catalogModel(t *testing.T, h *harness, spec rpc.CustomModelSpec) ai.Model {
	t.Helper()
	for _, m := range h.availableModels() {
		if m.Provider == ai.ProviderId(spec.Provider) && m.ID == spec.ModelID {
			return m
		}
	}
	t.Fatalf("catalog has no %s/%s", spec.Provider, spec.ModelID)
	return ai.Model{}
}

// modelDefinitionOnDisk reads the declaration back from the user settings
// file — the authority, not the in-memory snapshot.
func modelDefinitionOnDisk(t *testing.T, home string, spec rpc.CustomModelSpec) models.ModelDefinition {
	t.Helper()
	entry, ok := providersOnDisk(t, home)[spec.Provider]
	if !ok {
		t.Fatalf("no %q provider in settings.json", spec.Provider)
	}
	for _, def := range entry.Models {
		if def.ID == spec.ModelID {
			return def
		}
	}
	t.Fatalf("provider %q has no model %q", spec.Provider, spec.ModelID)
	return models.ModelDefinition{}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
