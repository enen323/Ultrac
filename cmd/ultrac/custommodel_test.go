// custommodel_test.go — the add_custom_model persistence regression
// (custom-model-plan.md P6, G6): the real "write → restart → read back"
// path, no mocks.
//
// What it pins down is D8's two premises, either of which can be broken
// silently ("it works on my machine"): both files are written at the
// USER layer (ConfigHome), and the startup path re-reads them. The test
// therefore builds a second harness from the same files with nothing
// shared — new resolver, new snapshot, new closures — and asserts the
// model is there and its key resolves. A write to the project layer, or
// a snapshot that stops reloading, turns this red.
//
// It also witnesses the in-process half (D4): the live harness sees the
// new model and credential without a restart, because reloadModels
// swapped the pointer the per-request resolver reads.

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/config/auth"
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
	if len(result.Providers) != 1 {
		t.Fatalf("result.providers = %+v, want exactly the declared provider", result.Providers)
	}
	info := result.Providers[0]
	if info.ID != spec.Provider || info.BaseURL != spec.BaseURL ||
		info.API != spec.API || len(info.ModelIDs) != 1 || info.ModelIDs[0] != spec.ModelID {
		t.Errorf("provider info = %+v, want the declaration", info)
	}

	// -- the in-process half (D4) -----------------------------------------
	if !containsModel(h1.availableModels(), added) {
		t.Error("the live harness must offer the new model without a restart")
	}
	if got := h1.credentialFor(spec.Provider); got != spec.APIKey {
		t.Errorf("credentialFor(%q) = %q, want the key just written", spec.Provider, got)
	}

	// -- the files, at the user layer (D8) --------------------------------
	authPath := filepath.Join(env.home, "auth.json")
	modelsPath := filepath.Join(env.home, "models.json")
	if !fileExists(modelsPath) || !fileExists(authPath) {
		t.Fatalf("both files must exist under %s", env.home)
	}
	// D1: the key is in auth.json and nowhere in models.json.
	stack := auth.LoadStack([]string{authPath}, func(err error) { t.Errorf("auth: %v", err) })
	if got := stack.APIKey(spec.Provider); got != spec.APIKey {
		t.Errorf("auth.json key = %q, want %q", got, spec.APIKey)
	}
	if strings.Contains(readFile(t, modelsPath), spec.APIKey) {
		t.Error("models.json must not carry the apiKey (D1)")
	}
	// The declaration round-trips through the struct writer.
	cfg, err := models.Load(modelsPath)
	if err != nil {
		t.Fatalf("models.Load: %v", err)
	}
	entry, ok := cfg.Providers[spec.Provider]
	if !ok || entry.BaseURL != spec.BaseURL || entry.API != spec.API || len(entry.Models) != 1 {
		t.Fatalf("models.json provider = %+v, want the declaration", entry)
	}
	if entry.Models[0].ID != spec.ModelID {
		t.Errorf("models.json model = %q, want %q", entry.Models[0].ID, spec.ModelID)
	}

	// -- the restart: nothing shared, everything re-read ------------------
	h2, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("second buildHarness exit = %d, want 0", code)
	}
	if !containsModel(h2.availableModels(), added) {
		t.Error("a fresh harness must offer the model written before it (D8)")
	}
	if got := h2.credentialFor(spec.Provider); got != spec.APIKey {
		t.Errorf("after restart credentialFor(%q) = %q, want the written key", spec.Provider, got)
	}
}

// TestAddCustomModelUpsertsSameProvider — a second add for the same
// provider id extends it rather than duplicating it: the merge rule at
// load time is an upsert by model id, so the file must be one too. The
// endpoint and dialect already declared win (the user may have tuned
// them by hand), and a re-saved model keeps its display name.
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

	cfg, err := models.Load(filepath.Join(env.home, "models.json"))
	if err != nil {
		t.Fatalf("models.Load: %v", err)
	}
	entry, ok := cfg.Providers[first.Provider]
	if !ok {
		t.Fatalf("provider %q missing from models.json", first.Provider)
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

// TestAddCustomModelRejectsBadSpec — the host's own entry point refuses a
// malformed spec before touching either file. The protocol layer checks
// the same rules; this is what keeps a direct caller (this package's
// tests, a future CLI command) from writing half a provider.
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
	// Nothing reached the disk: no orphan credential, no half declaration.
	if fileExists(filepath.Join(env.home, "auth.json")) || fileExists(filepath.Join(env.home, "models.json")) {
		t.Fatal("a refused spec must not write either file")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
