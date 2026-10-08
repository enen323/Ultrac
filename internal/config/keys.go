// Package config resolves settings from layered sources.
//
// # Why the key names matter
//
// Every constant below is a name the original TypeScript implementation read.
// They are not free to rename: ~/.ultrac/settings.json already contains them,
// and during the migration both implementations read the same file. A rename
// here would look like a clean-up and behave like a configuration outage.
//
// # Layering
//
// Part of the infrastructure layer. Everything above may import it; it imports
// nothing from ultrac.
package config

// Provider and model keys. These are what ~/.ultrac/settings.json carries in
// its "env" block today.
const (
	EnvBaseURL        = "ANTHROPIC_BASE_URL"
	EnvAuthToken      = "ANTHROPIC_AUTH_TOKEN"
	EnvAPIKey         = "ANTHROPIC_API_KEY"
	EnvModel          = "ANTHROPIC_MODEL"
	EnvSmallFastModel = "ANTHROPIC_SMALL_FAST_MODEL"
)

// EnvProviderManagedByHost, when set, makes the provider-related environment
// variables disappear from every settings source, not just one.
//
// This is preserved deliberately. The TypeScript implementation does the same,
// and a rewrite that quietly dropped the behaviour would reintroduce the exact
// failure it exists to explain: "I wrote the config and nothing took effect".
// /provider in the TypeScript CLI prints a warning about it for that reason.
const EnvProviderManagedByHost = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"

// EnvGitBashPath is required on Windows. Without it the TypeScript side hard
// exits while probing for a Git Bash install, because the probe cannot fall
// back to anything useful.
const EnvGitBashPath = "CLAUDE_CODE_GIT_BASH_PATH"

// ProviderEnvKeys are the keys suppressed when EnvProviderManagedByHost is set.
var ProviderEnvKeys = []string{
	EnvBaseURL,
	EnvAuthToken,
	EnvAPIKey,
	EnvModel,
	EnvSmallFastModel,
}

// EnvHomeOverride relocates the settings directory, for tests and for a
// portable install that must not touch the user profile.
const EnvHomeOverride = "ULTRAC_HOME"
