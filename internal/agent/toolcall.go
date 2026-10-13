package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Tool is the type-erased view of an AgentTool that the runtime stores.
//
// Inputs arrive as JSON from the model and a registry has to hold tools whose
// input types differ, so the loop only ever sees this interface. Erase
// produces it from a typed AgentTool[In].
//
// The split mirrors pi's boundary: AgentTool<TParameters> is the authoring
// shape; the loop works with the untyped dispatch shape.
type Tool interface {
	// Name is the identifier the model calls.
	Name() string
	// Label is the human-readable display name.
	Label() string
	// Description is what the model is told the tool does.
	Description() string
	// Parameters is the tool's JSON Schema.
	Parameters() json.RawMessage
	// ExecutionMode is this tool's per-call mode override; the empty string
	// means "use the loop default".
	ExecutionMode() ToolExecutionMode

	// Prepare turns raw model arguments into the tool's validated input,
	// boxed as any. It runs the tool's PrepareArguments shim (if any) and
	// then validates against the tool's schema. It returns an error when
	// either step fails; the loop converts that into an isError tool result.
	Prepare(raw map[string]any) (any, error)

	// Run executes one call whose arguments Prepare already validated. The
	// boxed value is the tool's In. A returned error is a tool-level
	// failure the loop converts into an isError tool result.
	Run(ctx context.Context, toolCallID string, in any, onUpdate func(Result)) (Result, error)
}

// Erase adapts a typed AgentTool into the runtime Tool.
//
// The schema is compiled once here; a schema that fails to compile is not a
// registration panic but a per-call error, so one broken tool cannot take the
// process down at init.
func Erase[In any](t *AgentTool[In]) Tool {
	e := &erasedTool[In]{tool: t}
	if len(t.Parameters) > 0 {
		schema, err := compileSchema(t.Parameters)
		if err != nil {
			e.compileErr = fmt.Errorf("agent: tool %q: invalid parameters schema: %w", t.Name, err)
		} else {
			e.schema = schema
		}
	}
	return e
}

type erasedTool[In any] struct {
	tool       *AgentTool[In]
	schema     *jsonschema.Schema
	compileErr error
}

func (e *erasedTool[In]) Name() string                     { return e.tool.Name }
func (e *erasedTool[In]) Label() string                    { return e.tool.Label }
func (e *erasedTool[In]) Description() string              { return e.tool.Description }
func (e *erasedTool[In]) Parameters() json.RawMessage      { return e.tool.Parameters }
func (e *erasedTool[In]) ExecutionMode() ToolExecutionMode { return e.tool.ExecutionMode }

// Prepare runs the shim, decodes, and schema-validates, in pi's preflight
// order (packages/agent/src/agent-loop.ts): lookup → prepareArguments →
// schema validation → beforeToolCall.
//
// What gets validated depends on the shim, mirroring pi: with no shim, the
// raw arguments the model sent are validated, so unknown fields, wrong types
// and missing required keys are all caught. With a shim, the shim's output is
// validated — a typed Go struct cannot carry unknown fields, so the shim
// author owns the final shape (pi's contract for prepareArguments is the
// same: "must return an object that matches TParameters", types.ts:469-473).
func (e *erasedTool[In]) Prepare(raw map[string]any) (any, error) {
	if e.compileErr != nil {
		return nil, e.compileErr
	}
	if e.tool.PrepareArguments == nil {
		if e.schema != nil {
			if err := e.validate(raw); err != nil {
				return nil, err
			}
		}
		return e.decode(raw)
	}
	in, err := e.tool.PrepareArguments(raw)
	if err != nil {
		return nil, err
	}
	if e.schema != nil {
		if err := e.validate(in); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// validate checks a JSON-serializable value against the tool's schema.
func (e *erasedTool[In]) validate(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("agent: tool %s: cannot encode arguments: %w", e.tool.Name, err)
	}
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("agent: tool %s: cannot decode arguments: %w", e.tool.Name, err)
	}
	if err := e.schema.Validate(decoded); err != nil {
		return fmt.Errorf("agent: tool %s: invalid arguments: %w", e.tool.Name, err)
	}
	return nil
}

// decode turns raw model arguments into the tool's input type. There is no
// empty-map shortcut: an empty object must still go through validation
// upstream, where `required` rejects it.
func (e *erasedTool[In]) decode(raw map[string]any) (In, error) {
	var zero In
	encoded, err := json.Marshal(raw)
	if err != nil {
		return zero, fmt.Errorf("agent: tool %s: cannot encode arguments: %w", e.tool.Name, err)
	}
	var in In
	if err := json.Unmarshal(encoded, &in); err != nil {
		return zero, fmt.Errorf("agent: tool %s: invalid input: %w", e.tool.Name, err)
	}
	return in, nil
}

func (e *erasedTool[In]) Run(ctx context.Context, toolCallID string, in any, onUpdate func(Result)) (Result, error) {
	typed, ok := in.(In)
	if !ok {
		return Result{}, fmt.Errorf("agent: tool %s: prepared arguments have type %T, want %T", e.tool.Name, in, zeroOf[In]())
	}
	return e.tool.Execute(ctx, toolCallID, typed, onUpdate)
}

func zeroOf[In any]() In { var zero In; return zero }

// compileSchema compiles a JSON Schema document. Compilation is done once per
// tool, at Erase time, so per-call cost is validation only.
func compileSchema(doc json.RawMessage) (*jsonschema.Schema, error) {
	// v6's AddResource takes the decoded JSON value, not a reader.
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	const resource = "urn:ultrac:agent:tool-parameters"
	if err := compiler.AddResource(resource, value); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

// ValidateArguments checks raw arguments against a JSON Schema document. It
// is exported for callers that validate outside the tool-call path (tests,
// the probe command).
func ValidateArguments(schema json.RawMessage, args map[string]any) error {
	compiled, err := compileSchema(schema)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	return compiled.Validate(value)
}

// Registry is the set of tools available for one run, assembled from
// built-ins plus whatever the caller contributes. The loop only ever sees a
// Registry, so a plugin tool and a built-in are indistinguishable from its
// side.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Tool
}

// NewRegistry returns an empty Registry, optionally seeded with tools.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool)}
	for _, t := range tools {
		_ = r.Add(t)
	}
	return r
}

// Add registers a tool under its name. A collision is an error rather than a
// silent overwrite: the caller has to say which one wins.
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("agent: cannot register a nil tool")
	}
	if t.Name() == "" {
		return fmt.Errorf("agent: cannot register a tool with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[t.Name()]; dup {
		return fmt.Errorf("agent: tool %q is already registered", t.Name())
	}
	r.byName[t.Name()] = t
	return nil
}

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// All returns every registered tool, sorted by name.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, t)
	}
	sortTools(out)
	return out
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	return out
}

func sortTools(tools []Tool) {
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j].Name() < tools[j-1].Name(); j-- {
			tools[j], tools[j-1] = tools[j-1], tools[j]
		}
	}
}
