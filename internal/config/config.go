package config

import (
	"context"
	"encoding/json"
	"fmt"
)

// Source is one layer of the settings stack.
//
// The numeric order is the precedence order: a later source overrides an
// earlier one. The sequence is fixed because it is observable behaviour, not
// an implementation detail — a project file that silently outranked a command
// line flag would be a bug nobody could reproduce.
type Source int

const (
	SourceDefault Source = iota
	SourceUser
	SourceProject
	SourceFlag
	SourceEnv
)

func (s Source) String() string {
	switch s {
	case SourceDefault:
		return "default"
	case SourceUser:
		return "user"
	case SourceProject:
		return "project"
	case SourceFlag:
		return "flag"
	case SourceEnv:
		return "env"
	default:
		return fmt.Sprintf("source(%d)", int(s))
	}
}

// Settings is the shape of a settings file.
//
// Only Env is modelled so far, because that is the only section verified in
// ~/.ultrac/settings.json. When other sections are ported they get typed
// fields here.
//
// Extra is required before this package is allowed to write a settings file:
// the file is shared with the TypeScript implementation, and a writer that
// drops sections it does not model would delete the other side's
// configuration. Until a round-trip that preserves unknown keys exists,
// reads may proceed and writes must not.
type Settings struct {
	Env map[string]string `json:"env,omitempty"`

	// Extra holds sections this version does not model, preserved verbatim so
	// a write cannot lose them.
	Extra map[string]json.RawMessage `json:"-"`
}

// Layer is one loaded source.
type Layer struct {
	Source Source
	// Path is the file the layer came from. Empty for defaults, flags and the
	// environment, which have no file behind them. It is reported to the user
	// when they ask where a value came from.
	Path     string
	Settings Settings
}

// Resolver reads and updates the settings stack.
//
// Update is expressed as a mutation rather than a whole-file write so the
// resolver can own the read-modify-write cycle, including cache invalidation.
// Getting that wrong is how a change lands on disk and is still invisible to
// the process that made it.
type Resolver interface {
	// Get resolves the stack for a working directory.
	Get(ctx context.Context, cwd string) (*Settings, error)

	// Update applies mutate to one source and persists it.
	Update(ctx context.Context, src Source, mutate func(*Settings) error) error

	// Invalidate drops the cached view of one source. Callers that write a
	// settings file behind the resolver's back need this.
	Invalidate(src Source)
}

// EffectiveEnv merges the env blocks of the given layers, lowest precedence
// first, and applies the host-managed suppression.
//
// The suppression is applied here rather than at each call site so that no
// caller can forget it, and so that the rule has exactly one test.
func EffectiveEnv(layers []Layer) map[string]string {
	out := make(map[string]string)

	for _, layer := range layers {
		for k, v := range layer.Settings.Env {
			out[k] = v
		}
	}

	if isTruthy(out[EnvProviderManagedByHost]) {
		for _, k := range ProviderEnvKeys {
			delete(out, k)
		}
	}
	return out
}

// isTruthy matches the TypeScript side's reading of a boolean environment
// variable, where an unset variable and "0"/"false" are both false.
func isTruthy(v string) bool {
	switch v {
	case "", "0", "false", "FALSE", "False", "no", "off":
		return false
	default:
		return true
	}
}
