package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// echoInput is the input type of the fake tool below. It exists so the test
// exercises the generic path end to end: the registry only ever sees raw JSON,
// and decoding into a concrete struct is the adapter's job.
type echoInput struct {
	Value string `json:"value"`
}

// fakeTool is a minimal Tool[echoInput]. Every predicate answers from the
// decoded input, which is the point: an erased call must still see its input.
type fakeTool struct {
	aliases  []string
	lastCall echoInput
	calls    int
}

func (f *fakeTool) Name() string       { return "echo" }
func (f *fakeTool) Aliases() []string  { return f.aliases }
func (f *fakeTool) SearchHint() string { return "echo a value" }
func (f *fakeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
}
func (f *fakeTool) OutputSchema() json.RawMessage { return nil }

func (f *fakeTool) Description(_ context.Context, in echoInput, _ DescriptionOptions) (string, error) {
	return "echo " + in.Value, nil
}

func (f *fakeTool) Call(_ context.Context, in echoInput, _ *UseContext, _ CanUseFunc, onProgress ProgressFunc) (*Result, error) {
	f.calls++
	f.lastCall = in
	if onProgress != nil {
		onProgress(Progress{Message: "halfway", Fraction: 0.5})
	}
	if in.Value == "" {
		return &Result{Content: "value is required", IsError: true}, nil
	}
	return &Result{Content: in.Value}, nil
}

// Predicates read the input so the test can prove the adapter decoded it
// rather than passing an empty struct through.
func (f *fakeTool) IsConcurrencySafe(in echoInput) bool  { return in.Value != "unsafe" }
func (f *fakeTool) IsReadOnly(in echoInput) bool         { return in.Value != "write" }
func (f *fakeTool) IsDestructive(in echoInput) bool      { return in.Value == "delete" }
func (f *fakeTool) IsOpenWorld(in echoInput) bool        { return in.Value == "net" }
func (f *fakeTool) InterruptBehavior() InterruptBehavior { return InterruptBlock }
func (f *fakeTool) RequiresUserInteraction() bool        { return false }
func (f *fakeTool) IsSearchOrRead(in echoInput) SearchRead {
	return SearchRead{Read: in.Value == "read"}
}

func TestAdaptDecodesInput(t *testing.T) {
	inner := &fakeTool{}
	var erased Erased = Adapt[echoInput](inner)

	if got := erased.Name(); got != "echo" {
		t.Fatalf("Name() = %q, want %q", got, "echo")
	}

	desc, err := erased.Description(context.Background(), json.RawMessage(`{"value":"hi"}`), DescriptionOptions{})
	if err != nil {
		t.Fatalf("Description returned error: %v", err)
	}
	if desc != "echo hi" {
		t.Errorf("Description() = %q, want %q — the adapter did not decode the input", desc, "echo hi")
	}

	var progressCalls int
	res, err := erased.Call(context.Background(), json.RawMessage(`{"value":"hi"}`), &UseContext{}, nil, func(Progress) {
		progressCalls++
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if res.Content != "hi" {
		t.Errorf("Call content = %q, want %q", res.Content, "hi")
	}
	if progressCalls != 1 {
		t.Errorf("progress callback fired %d times, want 1", progressCalls)
	}
	if inner.lastCall.Value != "hi" {
		t.Errorf("tool saw input %+v, want Value=hi", inner.lastCall)
	}
}

func TestAdaptPredicatesReadInput(t *testing.T) {
	erased := Adapt[echoInput](&fakeTool{})

	cases := []struct {
		value string
		check func() bool
		want  bool
		what  string
	}{
		{"safe", func() bool { return erased.IsConcurrencySafe(json.RawMessage(`{"value":"safe"}`)) }, true, "IsConcurrencySafe"},
		{"unsafe", func() bool { return erased.IsConcurrencySafe(json.RawMessage(`{"value":"unsafe"}`)) }, false, "IsConcurrencySafe"},
		{"read", func() bool { return erased.IsReadOnly(json.RawMessage(`{"value":"read"}`)) }, true, "IsReadOnly"},
		{"write", func() bool { return erased.IsReadOnly(json.RawMessage(`{"value":"write"}`)) }, false, "IsReadOnly"},
		{"net", func() bool { return erased.IsOpenWorld(json.RawMessage(`{"value":"net"}`)) }, true, "IsOpenWorld"},
		{"delete", func() bool { return erased.IsDestructive(json.RawMessage(`{"value":"delete"}`)) }, true, "IsDestructive"},
	}
	for _, tc := range cases {
		if got := tc.check(); got != tc.want {
			t.Errorf("%s with value %q = %v, want %v", tc.what, tc.value, got, tc.want)
		}
	}
}

// Undecodable input must never widen a permission. These defaults are the
// difference between a sandbox and a comment.
func TestAdaptUndecodableInputIsCautious(t *testing.T) {
	erased := Adapt[echoInput](&fakeTool{})
	bad := json.RawMessage(`{"value":123}`)

	if erased.IsReadOnly(bad) {
		t.Error("IsReadOnly = true for undecodable input; must default to false")
	}
	if erased.IsConcurrencySafe(bad) {
		t.Error("IsConcurrencySafe = true for undecodable input; must default to false")
	}
	if !erased.IsDestructive(bad) {
		t.Error("IsDestructive = false for undecodable input; must default to true")
	}
	if !erased.IsOpenWorld(bad) {
		t.Error("IsOpenWorld = false for undecodable input; must default to true")
	}
	if erased.IsSearchOrRead(bad).Read {
		t.Error("IsSearchOrRead reports read for undecodable input; must report nothing")
	}
}

// A tool that fails is a normal turn event: it returns a Result with IsError
// set and a nil error, so the model reads it and can correct itself.
func TestToolLevelFailureIsNotAGoError(t *testing.T) {
	erased := Adapt[echoInput](&fakeTool{})

	res, err := erased.Call(context.Background(), json.RawMessage(`{"value":""}`), &UseContext{}, nil, nil)
	if err != nil {
		t.Fatalf("a tool-level failure must not surface as a Go error, got %v", err)
	}
	if !res.IsError {
		t.Error("Result.IsError = false, want true")
	}

	if _, err := erased.Call(context.Background(), json.RawMessage(`{"value":123}`), &UseContext{}, nil, nil); err == nil {
		t.Error("malformed JSON must surface as a Go error — the tool never ran to a verdict")
	}
}

func TestRegistryLookupByNameAndAlias(t *testing.T) {
	inner := &fakeTool{aliases: []string{"repeat"}}
	reg := NewRegistry(Adapt[echoInput](inner))

	if _, ok := reg.Lookup("echo"); !ok {
		t.Error("lookup by name failed")
	}
	if _, ok := reg.Lookup("repeat"); !ok {
		t.Error("lookup by alias failed")
	}
	if _, ok := reg.Lookup("missing"); ok {
		t.Error("lookup of an unregistered name succeeded")
	}
	if got := reg.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 — an alias must not count as a second tool", got)
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	reg := NewRegistry(Adapt[echoInput](&fakeTool{}))

	err := reg.Add(Adapt[echoInput](&fakeTool{}))
	if err == nil {
		t.Fatal("registering a duplicate name succeeded; want an error")
	}
	if reg.Len() != 1 {
		t.Errorf("a rejected Add changed the registry: Len() = %d, want 1", reg.Len())
	}
}

func TestRegistryFilter(t *testing.T) {
	readOnly := &fakeTool{aliases: nil}
	reg := NewRegistry(Adapt[echoInput](readOnly))

	filtered := reg.Filter(func(Erased) bool { return false })
	if filtered.Len() != 0 {
		t.Errorf("filtered registry Len() = %d, want 0", filtered.Len())
	}
	if reg.Len() != 1 {
		t.Errorf("Filter mutated the receiver: Len() = %d, want 1", reg.Len())
	}
}
