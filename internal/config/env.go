package config

import (
	"os"
	"strings"
)

// EnvResult reports what an Apply* call did.
//
// The counts are not decoration. "I set the base URL and nothing happened" is
// the single most common report about this system, and the answer is almost
// always in Skipped: the value was filtered by host management or by the safe
// allowlist. Surfacing that at the call site turns a mystery into a line of
// output.
type EnvResult struct {
	Applied []string
	Skipped []SkipReason
}

// SkipReason explains why one key from a settings source was not applied.
type SkipReason struct {
	Key    string
	Source Source
	Why    SkipKind
}

// SkipKind classifies a skip.
type SkipKind string

const (
	// SkipHostManaged means the host owns inference routing, so provider keys
	// are stripped from every source.
	SkipHostManaged SkipKind = "host-managed"
	// SkipNotSafe means the key is not in the safe allowlist, which is the
	// gate project-scoped sources pass through.
	SkipNotSafe SkipKind = "not-in-safe-list"
	// SkipSSHTunnel means the key is a placeholder that the remote launcher
	// owns for the life of a forwarded socket.
	SkipSSHTunnel SkipKind = "ssh-tunnel"
	// SkipCCDHost means the desktop host set the key in the spawn environment
	// and settings must not override it.
	SkipCCDHost SkipKind = "desktop-host"
	// SkipEmpty means the value was the empty string.
	SkipEmpty SkipKind = "empty"
)

// FilterEnv applies every strip rule that guards a settings-sourced env block
// and returns the result plus the reasons anything was dropped.
//
// The rules compose, and the order is not arbitrary:
//
//  1. SSH tunnel placeholders — only when ANTHROPIC_UNIX_SOCKET is set. The
//     remote launcher puts placeholder credentials in place so the SDK has
//     something to send; settings must not clobber them or the tunnel breaks.
//  2. Host-managed provider vars — only when the host flag is truthy.
//  3. Desktop-host spawn keys — only in claude-desktop mode.
//
// Each rule is conditional on process.env, never on the settings block. That is
// what makes the host flag un-disableable from settings: the check runs before
// settings are consulted.
func FilterEnv(src Source, env map[string]string) (map[string]string, []SkipReason) {
	out := make(map[string]string, len(env))
	var skips []SkipReason

	sshTunnel := os.Getenv("ANTHROPIC_UNIX_SOCKET") != ""
	hostManaged := EnvTruthy(os.Getenv(EnvProviderManagedByHost))
	ccdKeys := ccdSpawnEnvKeys()

	for k, v := range env {
		if sshTunnel && isSSHTunnelVar(k) {
			skips = append(skips, SkipReason{Key: k, Source: src, Why: SkipSSHTunnel})
			continue
		}
		if hostManaged && IsProviderManagedEnvVar(k) {
			skips = append(skips, SkipReason{Key: k, Source: src, Why: SkipHostManaged})
			continue
		}
		if _, clash := ccdKeys[k]; clash {
			skips = append(skips, SkipReason{Key: k, Source: src, Why: SkipCCDHost})
			continue
		}
		out[k] = v
	}
	return out, skips
}

// sshTunnelVars are the keys `claude ssh` sets up as placeholders. Stripping
// them from settings is what stops a project's settings.json from replacing
// the tunnel's credentials with the user's own.
var sshTunnelVars = newSet(
	"ANTHROPIC_UNIX_SOCKET",
	EnvBaseURL,
	EnvAPIKey,
	EnvAuthToken,
	"CLAUDE_CODE_OAUTH_TOKEN",
)

func isSSHTunnelVar(key string) bool { return sshTunnelVars.has(upperASCII(key)) }

// ccdSpawnEnvSnapshot is the set of keys present before any settings were
// applied, captured once when running under claude-desktop.
//
// The snapshot is the point: settings must not override variables the desktop
// host set to orchestrate the subprocess (an OTEL_LOGS_EXPORTER=console would
// corrupt its stdio JSON-RPC transport). Keys added later by user settings are
// not in the snapshot, so a mid-session settings.json edit still applies.
var ccdSpawnEnvSnapshot map[string]struct{}

func ccdSpawnEnvKeys() map[string]struct{} {
	if os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "claude-desktop" {
		return nil
	}
	if ccdSpawnEnvSnapshot != nil {
		return ccdSpawnEnvSnapshot
	}
	snap := make(map[string]struct{})
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			snap[k] = struct{}{}
		}
	}
	ccdSpawnEnvSnapshot = snap
	return snap
}

// ResetCCDEnvSnapshot clears the captured spawn environment. Tests need this;
// production never calls it, because re-snapshotting after settings have been
// applied would let those settings protect themselves.
func ResetCCDEnvSnapshot() { ccdSpawnEnvSnapshot = nil }

// ApplyPhase says how much of a source's env block may be applied.
type ApplyPhase int

const (
	// PhaseBeforeTrust applies trusted sources in full and project-scoped
	// sources only through the safe allowlist. This is the pre-trust-dialog
	// phase, where a project file has not yet been vouched for.
	PhaseBeforeTrust ApplyPhase = iota
	// PhaseAfterTrust applies everything. Reached only once the workspace has
	// been trusted, which is what makes LD_PRELOAD from a project file
	// acceptable: the user said yes to this directory.
	PhaseAfterTrust
)

// ApplyToEnv writes the resolved env blocks into process.env and reports what
// happened.
//
// It returns a count of assignments rather than mutating a map, because
// process.env is global and a test needs to run several scenarios in one
// process. setenv is injected for that reason too — os.Setenv is not
// undoable, so a test that used it directly would leak into the next case.
func ApplyToEnv(layers []Layer, phase ApplyPhase, setenv func(string, string) error) (EnvResult, error) {
	var res EnvResult

	for _, layer := range layers {
		if len(layer.Settings.Env) == 0 {
			continue
		}
		filtered, skips := FilterEnv(layer.Source, layer.Settings.Env)

		full := phase == PhaseAfterTrust || Trusted(layer.Source)
		for k, v := range filtered {
			if !full && !IsSafeEnvVar(k) {
				res.Skipped = append(res.Skipped, SkipReason{Key: k, Source: layer.Source, Why: SkipNotSafe})
				continue
			}
			if v == "" {
				res.Skipped = append(res.Skipped, SkipReason{Key: k, Source: layer.Source, Why: SkipEmpty})
				continue
			}
			if err := setenv(k, v); err != nil {
				return res, err
			}
			res.Applied = append(res.Applied, k+" (from "+layer.Source.String()+")")
		}
		res.Skipped = append(res.Skipped, skips...)
	}
	return res, nil
}

// EffectiveEnv merges the env blocks of the given layers, lowest precedence
// first, and applies the host-managed suppression.
//
// Use this for "what env would this configuration produce", for diagnostics and
// for provider construction. Use ApplyToEnv to actually write process.env —
// this function does not model the safe allowlist, because a merged view has
// no notion of which source a key came from, and that distinction is the whole
// point of the allowlist.
func EffectiveEnv(layers []Layer) map[string]string {
	out := make(map[string]string)

	for _, layer := range layers {
		for k, v := range layer.Settings.Env {
			out[k] = v
		}
	}

	hostManaged := EnvTruthy(os.Getenv(EnvProviderManagedByHost))
	if hostManaged {
		for k := range out {
			if IsProviderManagedEnvVar(k) {
				delete(out, k)
			}
		}
	}
	return out
}

// IsTruthy reads a settings env value the way the TypeScript side reads a
// boolean environment variable: unset, "0", "false", "no" and "off" are all
// false. Anything else is true.
func IsTruthy(v string) bool { return EnvTruthy(v) }

// EnvTruthy reports whether an environment-variable string means "on".
//
// The accepted set is exactly ['1','true','yes','on'], lower-cased and
// trimmed. It is narrower than Go's strconv.ParseBool on purpose: ParseBool
// accepts "T", "TRUE" and "1" but also "t", which nobody sets deliberately, and
// it rejects "on", which somebody does. The TypeScript side uses this list, so
// a value that disables a feature there must disable it here too.
func EnvTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
