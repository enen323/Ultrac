// Package tool defines the tool-facing contract.
//
// # Two shapes
//
// Tool[In] is what an author implements: typed, so the compiler checks the
// call site against the schema the model is shown. Erased is what the Registry
// stores, because the input arrives as JSON from the model and a registry has
// to hold tools whose input types differ. Adapt bridges the two.
//
// # What is deliberately absent
//
// Permission policy. The engine injects a CanUseFunc into every call, so this
// package never imports the policy layer. That mirrors the TypeScript
// signature, which already took a canUseTool callback as its third argument —
// the coupling was avoided there first, and this keeps it that way.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// InterruptBehavior says what happens when the user sends a new message while
// this tool is running.
type InterruptBehavior string

const (
	// InterruptBlock lets the running tool finish; the new message waits.
	// This is the default when a tool does not say otherwise.
	InterruptBlock InterruptBehavior = "block"
	// InterruptCancel stops the tool and discards its result.
	InterruptCancel InterruptBehavior = "cancel"
)

// Decision is a permission verdict.
//
// It lives here rather than in the policy package to keep the dependency
// pointing one way: the engine owns policy, tools only consume the answer.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// PermissionResult is the verdict for one call.
type PermissionResult struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	// UpdatedInput lets policy rewrite a call before it runs, e.g. to strip a
	// flag. When set, the engine uses it in place of what the model sent.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

// CanUseFunc decides whether a call may proceed.
type CanUseFunc func(ctx context.Context, name string, input json.RawMessage) (PermissionResult, error)

// Result is what a tool hands back to the engine.
//
// IsError is not a Go error: a tool that fails is a normal turn event that the
// model reads and reacts to. Returning a Go error instead means "the engine
// could not run this at all", which is a different thing and is treated as
// such.
type Result struct {
	Content  string         `json:"content"`
	IsError  bool           `json:"isError,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Progress is partial output from a running tool.
type Progress struct {
	Message  string  `json:"message,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
}

// ProgressFunc receives progress. It may be nil; tools must check.
type ProgressFunc func(p Progress)

// DescriptionOptions is the ambient state a tool may consult when writing its
// own description.
type DescriptionOptions struct {
	IsNonInteractive bool
	Tools            []string
}

// SearchRead classifies a call so the frontend can collapse it.
type SearchRead struct {
	Search bool
	Read   bool
	List   bool
}

// UseContext is the engine-provided ambient state for one invocation.
type UseContext struct {
	SessionID  string
	TurnIndex  int
	WorkDir    string
	IsSubagent bool
}

// Tool is the typed authoring interface.
//
// The predicate methods carry the input because the answer can depend on it:
// `rm -rf /` and `ls` are both Bash, and only one of them is destructive.
type Tool[In any] interface {
	// Name is the identifier the model calls. It is also the wire name.
	Name() string
	// Aliases are accepted for lookups but are never advertised to the model.
	Aliases() []string
	// SearchHint is a short keyword phrase used when tools are discovered by
	// search rather than listed. Empty means the tool is always listed.
	SearchHint() string

	// Description is computed per call because it may embed input-dependent
	// detail, such as the current working directory or a listing.
	Description(ctx context.Context, in In, opts DescriptionOptions) (string, error)
	// InputSchema is a JSON Schema object. It must be non-nil.
	InputSchema() json.RawMessage
	// OutputSchema may return nil when the tool's output has no fixed shape.
	OutputSchema() json.RawMessage

	// Call runs the tool. A failure the model should see belongs in the
	// returned Result with IsError set; a returned error means the call never
	// ran to a verdict.
	Call(ctx context.Context, in In, uc *UseContext, canUse CanUseFunc, onProgress ProgressFunc) (*Result, error)

	// IsConcurrencySafe reports whether this call may run alongside others.
	// Tools that mutate shared state must return false.
	IsConcurrencySafe(in In) bool
	IsReadOnly(in In) bool
	// IsDestructive marks irreversible operations: delete, overwrite, send.
	IsDestructive(in In) bool
	InterruptBehavior() InterruptBehavior
	IsSearchOrRead(in In) SearchRead
	// IsOpenWorld marks tools that reach outside the workspace.
	IsOpenWorld(in In) bool
	RequiresUserInteraction() bool
}

// Erased is the type-erased view of a Tool that the Registry stores.
//
// Inputs are raw JSON because that is what the model produces; the adapter
// decodes them into the tool's own input type.
type Erased interface {
	Name() string
	Aliases() []string
	SearchHint() string

	Description(ctx context.Context, input json.RawMessage, opts DescriptionOptions) (string, error)
	InputSchema() json.RawMessage

	Call(ctx context.Context, input json.RawMessage, uc *UseContext, canUse CanUseFunc, onProgress ProgressFunc) (*Result, error)

	IsConcurrencySafe(input json.RawMessage) bool
	IsReadOnly(input json.RawMessage) bool
	IsDestructive(input json.RawMessage) bool
	InterruptBehavior() InterruptBehavior
	IsSearchOrRead(input json.RawMessage) SearchRead
	IsOpenWorld(input json.RawMessage) bool
	RequiresUserInteraction() bool
}

// Adapt erases a typed Tool into the shape the Registry stores.
func Adapt[In any](t Tool[In]) Erased { return adapter[In]{inner: t} }

type adapter[In any] struct{ inner Tool[In] }

func (a adapter[In]) Name() string                 { return a.inner.Name() }
func (a adapter[In]) Aliases() []string            { return a.inner.Aliases() }
func (a adapter[In]) SearchHint() string           { return a.inner.SearchHint() }
func (a adapter[In]) InputSchema() json.RawMessage { return a.inner.InputSchema() }
func (a adapter[In]) InterruptBehavior() InterruptBehavior {
	return a.inner.InterruptBehavior()
}
func (a adapter[In]) RequiresUserInteraction() bool { return a.inner.RequiresUserInteraction() }

func (a adapter[In]) Description(ctx context.Context, raw json.RawMessage, opts DescriptionOptions) (string, error) {
	in, err := decode[In](raw)
	if err != nil {
		return "", fmt.Errorf("tool %s: invalid input: %w", a.inner.Name(), err)
	}
	return a.inner.Description(ctx, in, opts)
}

func (a adapter[In]) Call(ctx context.Context, raw json.RawMessage, uc *UseContext, canUse CanUseFunc, onProgress ProgressFunc) (*Result, error) {
	in, err := decode[In](raw)
	if err != nil {
		return nil, fmt.Errorf("tool %s: invalid input: %w", a.inner.Name(), err)
	}
	return a.inner.Call(ctx, in, uc, canUse, onProgress)
}

func (a adapter[In]) IsSearchOrRead(raw json.RawMessage) SearchRead {
	in, err := decode[In](raw)
	if err != nil {
		return SearchRead{}
	}
	return a.inner.IsSearchOrRead(in)
}

// Predicate defaults are the cautious answer for input the adapter could not
// decode: assume the call is not safe to parallelise, is not a read, may be
// destructive, and reaches outside the workspace. Getting one of these wrong
// in the permissive direction is how a sandbox silently stops sandboxing, so
// the undecodable case never grants a permission.
func (a adapter[In]) IsConcurrencySafe(raw json.RawMessage) bool {
	return a.boolFlag(raw, a.inner.IsConcurrencySafe, false)
}

func (a adapter[In]) IsReadOnly(raw json.RawMessage) bool {
	return a.boolFlag(raw, a.inner.IsReadOnly, false)
}

func (a adapter[In]) IsDestructive(raw json.RawMessage) bool {
	return a.boolFlag(raw, a.inner.IsDestructive, true)
}

func (a adapter[In]) IsOpenWorld(raw json.RawMessage) bool {
	return a.boolFlag(raw, a.inner.IsOpenWorld, true)
}

func (a adapter[In]) boolFlag(raw json.RawMessage, fn func(In) bool, undecodable bool) bool {
	in, err := decode[In](raw)
	if err != nil {
		return undecodable
	}
	return fn(in)
}

func decode[In any](raw json.RawMessage) (In, error) {
	var in In
	if len(raw) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err
	}
	return in, nil
}

// Registry is the set of tools available for one run.
//
// It is assembled per run from enabled built-ins plus whatever plugins
// contributed. The engine only ever sees a Registry, so a plugin tool and a
// built-in are indistinguishable from the caller's side.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Erased
}

// NewRegistry returns an empty Registry, optionally seeded with tools.
func NewRegistry(tools ...Erased) *Registry {
	r := &Registry{byName: make(map[string]Erased)}
	for _, t := range tools {
		_ = r.Add(t)
	}
	return r
}

// Add registers a tool under its name and aliases.
//
// A name collision is an error rather than a silent overwrite: with built-ins
// and plugin-provided tools landing in the same map, the caller has to say
// which one wins instead of discovering it by outcome.
func (r *Registry) Add(t Erased) error {
	if t == nil {
		return errors.New("tool: cannot register a nil tool")
	}
	name := t.Name()
	if name == "" {
		return errors.New("tool: cannot register a tool with an empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, n := range names(t) {
		if _, dup := r.byName[n]; dup {
			return fmt.Errorf("tool: %q is already registered", n)
		}
	}
	for _, n := range names(t) {
		r.byName[n] = t
	}
	return nil
}

// Lookup finds a tool by name or alias.
func (r *Registry) Lookup(name string) (Erased, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// All returns each registered tool once, sorted by name. Aliases resolve to
// the same tool and are therefore not repeated.
func (r *Registry) All() []Erased {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{}, len(r.byName))
	out := make([]Erased, 0, len(r.byName))
	for _, t := range r.byName {
		if _, dup := seen[t.Name()]; dup {
			continue
		}
		seen[t.Name()] = struct{}{}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns the primary names of every registered tool, sorted.
func (r *Registry) Names() []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	return out
}

// Len is the number of distinct tools.
func (r *Registry) Len() int { return len(r.All()) }

// Filter returns a new Registry holding the tools keep accepts. Names are
// already unique in the receiver, so Add cannot fail here.
func (r *Registry) Filter(keep func(Erased) bool) *Registry {
	out := NewRegistry()
	for _, t := range r.All() {
		if keep(t) {
			_ = out.Add(t)
		}
	}
	return out
}

func names(t Erased) []string {
	out := make([]string, 0, 1+len(t.Aliases()))
	out = append(out, t.Name())
	for _, a := range t.Aliases() {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

var (
	builtinMu sync.RWMutex
	builtins  []Erased
)

// RegisterBuiltin lets a built-in tool self-register from its package init(),
// so cmd/ultrac only needs a blank import to make it available. Duplicate
// names panic for the same reason provider.Register does.
func RegisterBuiltin(t Erased) {
	if t == nil {
		panic("tool: RegisterBuiltin called with a nil tool")
	}
	if t.Name() == "" {
		panic("tool: RegisterBuiltin called with an empty name")
	}

	builtinMu.Lock()
	defer builtinMu.Unlock()
	for _, existing := range builtins {
		if existing.Name() == t.Name() {
			panic("tool: duplicate built-in " + t.Name())
		}
	}
	builtins = append(builtins, t)
}

// Builtins returns the process-wide built-in set, sorted by name.
func Builtins() []Erased {
	builtinMu.RLock()
	defer builtinMu.RUnlock()

	out := make([]Erased, len(builtins))
	copy(out, builtins)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// BuiltinNames lists the built-in tool names, sorted.
func BuiltinNames() []string {
	all := Builtins()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	return out
}
