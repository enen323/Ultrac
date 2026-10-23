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
//
// SourceEnv is deliberately absent. Environment variables are not a settings
// layer: they are what the settings layers are applied *to*. Modelling them as
// a layer is how "settings wrote the env but the env won" bugs happen.
type Source int

const (
	// SourceDefault carries built-in defaults. It has no file behind it.
	SourceDefault Source = iota
	SourceUser
	SourceProject
	SourceLocal
	SourceFlag
	SourcePolicy
)

func (s Source) String() string {
	switch s {
	case SourceDefault:
		return "default"
	case SourceUser:
		return "user"
	case SourceProject:
		return "project"
	case SourceLocal:
		return "local"
	case SourceFlag:
		return "flag"
	case SourcePolicy:
		return "policy"
	default:
		return fmt.Sprintf("source(%d)", int(s))
	}
}

// Settings is the shape of a settings file.
//
// Only Env and Compaction are typed. That is not laziness about scope — it is
// a statement about who owns the file. ~/.ultrac/settings.json is read by the
// TypeScript implementation too, and a struct with forty typed fields would
// silently drop the thirty-eight this version has never heard of on write. So
// unknown sections live in Extra, verbatim, and survive a read-modify-write
// cycle.
//
// A writer is only correct here because of Extra. Remove it and the first
// `/provider` run on a machine with hooks configured deletes the hooks block.
type Settings struct {
	Env map[string]string `json:"env,omitempty"`

	// Compaction is the typed compaction section. Nil means "no layer
	// defined one" — the engine-side defaults (compact.DefaultSettings)
	// apply at resolution time, not here, so a resolved view still reports
	// the truth about what the files say (docs/compaction-branch-plan.md
	// D6).
	Compaction *CompactionSettings `json:"compaction,omitempty"`

	// Extra holds every other top-level key, preserved byte-for-byte as JSON.
	// Marshalling merges Extra and Env into one object; see MarshalJSON.
	Extra map[string]json.RawMessage `json:"-"`
}

// CompactionSettings is the "compaction" section: the auto-compaction gate
// and the two token budgets (pi's CompactionSettings,
// core/compaction/compaction.ts:120-124).
//
// Every field is a pointer so an absent key stays absent: merging a project
// layer's {"keepRecentTokens": 1000} over a user layer's {"enabled": false}
// must keep both, and a value bool could not tell "explicitly disabled" from
// "not written". Resolution to concrete numbers (defaults 16384 / 20000 /
// enabled true) belongs to the read side, not to the file format.
type CompactionSettings struct {
	Enabled          *bool `json:"enabled,omitempty"`
	ReserveTokens    *int  `json:"reserveTokens,omitempty"`
	KeepRecentTokens *int  `json:"keepRecentTokens,omitempty"`
}

// compactionKey is the top-level key the typed section occupies. Env and
// compaction are the two carve-outs; everything else is Extra.
const compactionKey = "compaction"

// UnmarshalJSON splits a settings document into Env, Compaction and Extra.
//
// A malformed "env" block is not fatal: it lands in Extra instead, so the rest
// of the file still loads and the user sees a validation error rather than an
// empty configuration. Losing every hook because one env value was a number is
// the worse failure. The same rule governs "compaction": a section that does
// not decode into CompactionSettings (wrong shape, non-integer tokens) is
// parked in Extra byte-for-byte rather than dropped, so a write cannot destroy
// what the user was trying to fix.
func (s *Settings) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	s.Extra = make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if k == "env" || k == compactionKey {
			continue
		}
		s.Extra[k] = v
	}

	s.Env = nil
	envRaw, ok := raw["env"]
	if !ok {
		s.Compaction = decodeCompaction(raw[compactionKey], s)
		return nil
	}
	var env map[string]string
	if err := json.Unmarshal(envRaw, &env); err == nil {
		s.Env = env
		s.Compaction = decodeCompaction(raw[compactionKey], s)
		return nil
	}
	// Not a map[string]string. Keep it so a write cannot lose it.
	s.Extra["env"] = envRaw
	s.Compaction = decodeCompaction(raw[compactionKey], s)
	return nil
}

// decodeCompaction parses the "compaction" section into the typed field.
// A malformed section stays in Extra (parked by the caller's skip) and the
// typed field stays nil; the raw is moved back so the round-trip is lossless.
func decodeCompaction(raw json.RawMessage, s *Settings) *CompactionSettings {
	if len(raw) == 0 {
		return nil
	}
	var c CompactionSettings
	if err := json.Unmarshal(raw, &c); err != nil {
		if s.Extra != nil {
			s.Extra[compactionKey] = raw
		}
		return nil
	}
	return &c
}

// MarshalJSON reassembles Env, Compaction and Extra into one object.
//
// NOTE on the mixed value/pointer receivers across this type: it is deliberate,
// not an oversight. MarshalJSON must stay on a value receiver because
// write() in loader.go passes Settings by value -- with a pointer receiver the
// marshaller would not see the method, fall back to default serialization, and
// drop Extra (json:"-"), silently erasing the user's hooks and permissions.
// UnmarshalJSON must stay on a pointer or json.Unmarshal would quietly do
// nothing. Only Set mutates, and it is the one on a pointer. Don't "fix" the
// mix by flipping every method to pointers without also reworking the call
// sites that pass Settings by value.
//
// Extra is written first and Env second so the typed field always wins for the
// one key both can hold. Any "env" left in Extra by UnmarshalJSON is dropped
// here, which is correct: it was only parked there to survive until this call.
// The same applies to a "compaction" the typed field now carries — but only
// then: a malformed section (typed nil, parked raw) must survive the write.
func (s Settings) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(s.Extra)+2)
	for k, v := range s.Extra {
		if k == "env" || (k == compactionKey && s.Compaction != nil) {
			continue
		}
		out[k] = v
	}
	if len(s.Env) > 0 {
		env, err := json.Marshal(s.Env)
		if err != nil {
			return nil, err
		}
		out["env"] = env
	}
	if s.Compaction != nil {
		compaction, err := json.Marshal(s.Compaction)
		if err != nil {
			return nil, err
		}
		out[compactionKey] = compaction
	}

	// json.Marshal sorts map keys, which is what we want: the TypeScript
	// writer emits sorted keys too, so the two implementations produce
	// byte-identical files for identical input and git diffs stay quiet.
	return json.Marshal(out)
}

// Get returns a top-level setting as raw JSON, searching Env-aware Extra last.
func (s Settings) Get(key string) (json.RawMessage, bool) {
	if key == "env" {
		if s.Env == nil {
			return nil, false
		}
		b, err := json.Marshal(s.Env)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	if key == compactionKey && s.Compaction != nil {
		b, err := json.Marshal(s.Compaction)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	v, ok := s.Extra[key]
	return v, ok
}

// Set stores a top-level setting. A nil or empty raw value deletes the key,
// which is how "blank field means remove it" is expressed for non-env sections.
//
// The receiver is a pointer on purpose. Under a value receiver every
// assignment below lands on a copy, so the call site sees nothing change and
// no error is raised -- a silent no-op that only shows up as "my setting was
// saved but has no effect". See the mixed-receiver note on MarshalJSON for
// why the other methods on Settings stay on values.
func (s *Settings) Set(key string, raw json.RawMessage) {
	if key == "env" {
		if len(raw) == 0 {
			s.Env = nil
			return
		}
		var env map[string]string
		if err := json.Unmarshal(raw, &env); err != nil {
			return
		}
		s.Env = env
		return
	}
	if key == compactionKey {
		// The typed section and the parked-raw copy are one key: whichever
		// representation wins, the other must go, or MarshalJSON would
		// write the section twice under different shapes.
		delete(s.Extra, compactionKey)
		if len(raw) == 0 {
			s.Compaction = nil
			return
		}
		var c CompactionSettings
		if err := json.Unmarshal(raw, &c); err != nil {
			// Unparseable: preserve it verbatim rather than losing it.
			if s.Extra == nil {
				s.Extra = make(map[string]json.RawMessage)
			}
			s.Extra[compactionKey] = raw
			return
		}
		s.Compaction = &c
		return
	}
	if len(raw) == 0 {
		delete(s.Extra, key)
		return
	}
	if s.Extra == nil {
		s.Extra = make(map[string]json.RawMessage)
	}
	s.Extra[key] = raw
}

// Layer is one loaded source.
type Layer struct {
	Source Source
	// Path is the file the layer came from. Empty for defaults and flags,
	// which have no file behind them. It is reported to the user when they
	// ask where a value came from.
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

	// Layers returns each loaded source separately, for /config and for
	// "where did this value come from" answers.
	Layers(ctx context.Context, cwd string) ([]Layer, error)

	// Update applies mutate to one source and persists it. cwd locates the
	// project-scoped files; it is ignored by the user-level source.
	Update(ctx context.Context, src Source, cwd string, mutate func(*Settings) error) error

	// Invalidate drops the cached view of one source. Callers that write a
	// settings file behind the resolver's back need this.
	Invalidate(src Source)
}

// Error is a settings problem worth showing the user: a malformed file, a
// directory where a file should be. It is distinct from "the file is absent",
// which is not an error at all.
type Error struct {
	Path    string
	Op      string
	Err     error
	Warning bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("config: %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Warningf marks an Error as recoverable. The loader collects warnings instead
// of aborting: one bad file in the stack must not take down the other four.
func (e *Error) Warningf() *Error {
	e.Warning = true
	return e
}
