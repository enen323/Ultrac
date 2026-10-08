// Package config resolves settings from layered sources.
//
// # Why the key names matter
//
// Every constant in this package is a name the original TypeScript
// implementation read. They are not free to rename: ~/.ultrac/settings.json
// already contains them, and during the migration both implementations read
// the same file. A rename here would look like a clean-up and behave like a
// configuration outage.
//
// # Layering
//
// Part of the infrastructure layer. Everything above may import it; it imports
// nothing from ultrac.
//
// # Precedence
//
// Lowest to highest:
//
//	plugin base < user < project < local < flag(--settings) < policy(managed)
//
// This order is observable behaviour, not an implementation detail: a project
// file that silently outranked an enterprise policy would be a bug nobody
// could reproduce. Note the two ends. Plugin settings sit below every file
// source, and policy sits above all of them — an admin's managed-settings.json
// is not a suggestion.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// DirName is the per-project configuration directory name, relative to a
// working directory. It is ".ultrac" because this fork renamed the whole
// claude -> ultrac prefix; see docs/config-loading.md §7.
//
// The managed directory below is the deliberate exception and keeps its
// original name — an enterprise deployment contract, not ours to change.
const DirName = ".ultrac"

// GlobalFileName is the user-global config file, ~/.ultrac.json.
//
// It is a different file from userSettings (~/.ultrac/settings.json) and
// carries a different shape: mcpServers, oauth, and per-project entries. The
// TypeScript side reads both, and conflating them is a mistake worth naming.
const GlobalFileName = ".ultrac.json"

// LegacyGlobalFileName is the pre-1.0 global config. It still wins when
// present, matching getGlobalUltracFile() in src/utils/env.ts.
const LegacyGlobalFileName = ".config.json"

// ManagedDir returns the platform's managed settings directory.
//
// The name keeps "ClaudeCode" on every platform, including Windows. This is
// intentional and load-bearing: enterprises point their MDM at these paths
// today, and renaming them would silently drop every managed policy in the
// field. The one escape hatch is CLAUDE_CODE_MANAGED_SETTINGS_PATH, which the
// TypeScript side only honours for internal users; it exists here for tests,
// which need to exercise the policy layer without admin rights.
func ManagedDir() string {
	if p := os.Getenv(EnvManagedSettingsPath); p != "" {
		return p
	}
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		// SystemDrive is normally "C:", but it is unset in some environments
		// (a Git Bash shell that has not imported it, a stripped service
		// account). Joining onto it blindly would produce "C:Program
		// Files\...", which is drive-relative rather than absolute — a path
		// that resolves against the current directory on that drive and so
		// silently points at the wrong place.
		drive := os.Getenv("SystemDrive")
		if drive == "" {
			drive = `C:`
		}
		return filepath.Join(drive+string(filepath.Separator), "Program Files", "ClaudeCode")
	default:
		return "/etc/claude-code"
	}
}

// ManagedDropInDir is managed-settings.d/, merged on top of
// managed-settings.json in lexical order. The systemd/sudoers drop-in
// convention: the base file gives defaults, drop-ins specialise.
func ManagedDropInDir() string {
	return filepath.Join(ManagedDir(), "managed-settings.d")
}

// ManagedSettingsFile is the base managed settings file.
func ManagedSettingsFile() string {
	return filepath.Join(ManagedDir(), "managed-settings.json")
}

// ConfigHome returns the user-level configuration directory.
//
// ULTRAC_CONFIG_DIR wins when set; otherwise ~/.ultrac. Tests set it to a temp
// dir, which is why this is a function and not a package-level constant — a
// constant would be captured before the test could set the variable.
func ConfigHome() string {
	if dir := os.Getenv(EnvConfigDir); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory is unusual but not fatal: fall back to the
		// relative path so a read fails with ENOENT rather than the whole
		// resolver refusing to start.
		return DirName
	}
	return filepath.Join(home, DirName)
}

// GlobalFile returns the path of the user-global config file.
//
// The legacy file wins when it exists, because that is what the TypeScript
// implementation does; picking the other one would read a different, stale
// config for anyone who upgraded across the rename.
func GlobalFile() string {
	home := ConfigHome()
	legacy := filepath.Join(home, LegacyGlobalFileName)
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return filepath.Join(home, GlobalFileName)
}

// SourceFile returns the settings file backing one source, or "" when the
// source has no single file behind it.
//
// The empty cases are not oversights. policySettings aggregates four origins
// (remote, MDM, managed files, HKCU) and is resolved by PolicyLayer, not here.
// flagSettings depends on the --settings argument, which is carried on the
// Resolver rather than read from the environment.
func SourceFile(src Source, cwd string) string {
	switch src {
	case SourceUser:
		return filepath.Join(ConfigHome(), "settings.json")
	case SourceProject:
		return filepath.Join(cwd, DirName, "settings.json")
	case SourceLocal:
		return filepath.Join(cwd, DirName, "settings.local.json")
	case SourcePolicy:
		return ManagedSettingsFile()
	case SourceDefault, SourceFlag:
		return ""
	}
	return ""
}

// RelativeSourceFile is the project-relative settings path, used for messages
// and for the gitignore entry written after a local-settings update.
func RelativeSourceFile(src Source) string {
	switch src {
	case SourceProject:
		return filepath.Join(DirName, "settings.json")
	case SourceLocal:
		return filepath.Join(DirName, "settings.local.json")
	}
	return ""
}

// Trusted reports whether a source's full env block may be applied before the
// trust dialog is answered.
//
// userSettings, flagSettings and policySettings are trusted; projectSettings
// and localSettings are not. The reason is concrete: project settings live
// inside a directory that can be cloned from someone else, so an
// ANTHROPIC_BASE_URL sitting in .ultrac/settings.json is an exfiltration
// vector aimed at whoever clones the repo. Enterprise policy is trusted for
// the opposite reason — it is admin-controlled and cannot be overridden.
func Trusted(src Source) bool {
	switch src {
	case SourceUser, SourceFlag, SourcePolicy:
		return true
	default:
		return false
	}
}

// Editable reports whether Update may write to this source. Policy and flag
// settings are read-only by contract: an enterprise policy that a user could
// rewrite would not be a policy.
func Editable(src Source) bool {
	switch src {
	case SourceUser, SourceProject, SourceLocal:
		return true
	default:
		return false
	}
}

// AllSources is the merge order, lowest precedence first.
//
// Callers must iterate it in order. Get() does; anyone assembling layers by
// hand and sorting them by the enum value gets the same answer, because the
// enum is declared in this order — but relying on that is how the order rots.
func AllSources() []Source {
	return []Source{
		SourceUser, SourceProject, SourceLocal, SourceFlag, SourcePolicy,
	}
}

// DisplayName is the short label shown to users in /config and /status.
func DisplayName(src Source) string {
	switch src {
	case SourceUser:
		return "User"
	case SourceProject:
		return "Project"
	case SourceLocal:
		return "Local"
	case SourceFlag:
		return "Flag"
	case SourcePolicy:
		return "Managed"
	default:
		return "Built-in"
	}
}

// LongDisplayName is the prose form, e.g. in "written to enterprise managed
// settings" sentences.
func LongDisplayName(src Source) string {
	switch src {
	case SourceUser:
		return "user settings"
	case SourceProject:
		return "shared project settings"
	case SourceLocal:
		return "project local settings"
	case SourceFlag:
		return "command line arguments"
	case SourcePolicy:
		return "enterprise managed settings"
	default:
		return "built-in defaults"
	}
}

// enabledSources is the --setting-sources gate. Nil means "no gate", i.e. all
// three user-settable sources load. Policy and flag are added unconditionally by
// EnabledSources, matching getEnabledSettingSources(): a user must not be able
// to switch off the settings their administrator installed.
var enabledSources []Source

// SetEnabledSources applies the --setting-sources flag. Names are the short
// forms the flag accepts; an empty slice restores the default.
func SetEnabledSources(names []string) error {
	if len(names) == 0 {
		enabledSources = nil
		return nil
	}
	out := make([]Source, 0, len(names))
	for _, n := range names {
		switch strings.TrimSpace(n) {
		case "user":
			out = append(out, SourceUser)
		case "project":
			out = append(out, SourceProject)
		case "local":
			out = append(out, SourceLocal)
		default:
			return &UnknownSourceError{Name: strings.TrimSpace(n)}
		}
	}
	enabledSources = out
	return nil
}

// UnknownSourceError reports a --setting-sources value that is not one of
// user, project, local.
type UnknownSourceError struct{ Name string }

func (e *UnknownSourceError) Error() string {
	return "invalid setting source: " + e.Name + ". Valid options are: user, project, local"
}

// EnabledSources returns the sources to load, in merge order.
//
// With no gate set this is every source: user, project, local, then the two
// that cannot be switched off, flag and policy.
//
// Duplicates from the gate are dropped while keeping merge order, so passing
// "user,user" does not read the user file twice.
func EnabledSources() []Source {
	gated := enabledSources
	if len(gated) == 0 {
		gated = []Source{SourceUser, SourceProject, SourceLocal}
	}

	seen := make(map[Source]bool, 5)
	out := make([]Source, 0, 5)
	for _, s := range slices.Concat(gated, []Source{SourceFlag, SourcePolicy}) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
