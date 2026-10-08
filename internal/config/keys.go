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

// EnvProviderManagedByHost, when set, makes the provider-routing variables
// disappear from every settings source, not just one.
//
// This is preserved deliberately. The TypeScript implementation does the same,
// and a rewrite that quietly dropped the behaviour would reintroduce the exact
// failure it exists to explain: "I wrote the config and nothing took effect".
// /provider in the TypeScript CLI prints a warning about it for that reason.
//
// The important subtlety: the flag reads process.env, not the merged settings.
// A settings file cannot unset it, because the check happens before settings
// are consulted. That is not an oversight to fix, it is the guarantee.
const EnvProviderManagedByHost = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"

// EnvGitBashPath is required on Windows. Without it the TypeScript side hard
// exits while probing for a Git Bash install, because the probe cannot fall
// back to anything useful.
const EnvGitBashPath = "CLAUDE_CODE_GIT_BASH_PATH"

// EnvConfigDir relocates the user configuration directory. Tests set it to a
// temp directory; a portable install may use it to avoid touching the profile.
const EnvConfigDir = "ULTRAC_CONFIG_DIR"

// EnvManagedSettingsPath overrides the managed settings directory. The
// TypeScript side only honours it for internal users; here it exists so tests
// can exercise the policy layer without admin rights.
const EnvManagedSettingsPath = "CLAUDE_CODE_MANAGED_SETTINGS_PATH"

// providerManagedEnvVars are the variables that control inference routing:
// which provider, which endpoint, which model IDs.
//
// Ported from src/utils/managedEnvConstants.ts. The set is a verbatim copy
// because a missing entry means a variable that should have been suppressed
// gets through, and the symptom (a user's settings redirecting requests away
// from the host's provider) gives no hint which key was responsible.
var providerManagedEnvVars = newSet(
	// The flag itself — settings can't unset it once the host set it.
	EnvProviderManagedByHost,

	// Provider selection.
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",

	// Endpoint config.
	EnvBaseURL,
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_FOUNDRY_RESOURCE",
	"ANTHROPIC_VERTEX_PROJECT_ID",

	// Region routing. The per-model VERTEX_REGION_CLAUDE_* family is matched
	// by prefix below rather than listed, so a model release does not require
	// editing this file.
	"CLOUD_ML_REGION",

	// Auth.
	EnvAPIKey,
	EnvAuthToken,
	"CLAUDE_CODE_OAUTH_TOKEN",
	"AWS_BEARER_TOKEN_BEDROCK",
	"ANTHROPIC_FOUNDRY_API_KEY",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",

	// Model defaults. These are provider-specific ID formats, so letting a
	// settings file set them would route to a model the host never provisioned.
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES",
	EnvSmallFastModel,
	"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
	"CLAUDE_CODE_SUBAGENT_MODEL",
)

// providerManagedEnvPrefixes are matched in addition to the exact set. Prefix
// matching exists so per-model Vertex region overrides scale with model
// releases instead of drifting out of this file every few months.
var providerManagedEnvPrefixes = []string{
	"VERTEX_REGION_CLAUDE_",
}

// ProviderEnvKeys is the exact-match set, exported for diagnostics. The
// prefix-matched families are deliberately absent: they cannot be enumerated.
func ProviderEnvKeys() []string {
	out := make([]string, 0, len(providerManagedEnvVars))
	for k := range providerManagedEnvVars {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// IsProviderManagedEnvVar reports whether key controls inference routing.
//
// Matching is case-insensitive on both paths, because Windows environment
// variables are case-insensitive and a settings file written on Linux can be
// read on Windows after a repo sync. A case-sensitive check would let
// anthropic_base_url through on one platform and not the other.
func IsProviderManagedEnvVar(key string) bool {
	upper := upperASCII(key)
	if providerManagedEnvVars.has(upper) {
		return true
	}
	for _, p := range providerManagedEnvPrefixes {
		if hasPrefixASCII(upper, p) {
			return true
		}
	}
	return false
}

// safeEnvVars may be applied from project-scoped settings.
//
// This allowlist is the source of truth for "safe". It is the boundary that
// keeps a cloned repository from redirecting traffic, so the omissions matter
// more than the inclusions. Deliberately NOT here:
//
//   - ANTHROPIC_BASE_URL and the Bedrock/Vertex/Foundry equivalents — these
//     redirect every request to an attacker-controlled server.
//   - The proxy variables (HTTP_PROXY, HTTPS_PROXY, NO_PROXY and lowercase)
//     — same redirection, one layer down.
//   - The OTLP endpoint variables — same, into someone else's collector.
//   - NODE_TLS_REJECT_UNAUTHORIZED and NODE_EXTRA_CA_CERTS — these disable or
//     widen certificate validation, which turns the redirect above into a
//     silent one.
//   - ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, AWS_BEARER_TOKEN_BEDROCK — a
//     project file that sets credentials is reading them, not providing them.
//
// Ported from src/utils/managedEnvConstants.ts.
var safeEnvVars = newSet(
	"ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_CUSTOM_MODEL_OPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES",
	"ANTHROPIC_FOUNDRY_API_KEY",
	EnvModel,
	"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
	EnvSmallFastModel,
	"AWS_DEFAULT_REGION",
	"AWS_PROFILE",
	"AWS_REGION",
	"BASH_DEFAULT_TIMEOUT_MS",
	"BASH_MAX_OUTPUT_LENGTH",
	"BASH_MAX_TIMEOUT_MS",
	"CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR",
	"CLAUDE_CODE_API_KEY_HELPER_TTL_MS",
	"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	"CLAUDE_CODE_DISABLE_TERMINAL_TITLE",
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
	"CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL",
	"CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_SUBAGENT_MODEL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_VERTEX",
	"DISABLE_AUTOUPDATER",
	"DISABLE_BUG_COMMAND",
	"DISABLE_COST_WARNINGS",
	"DISABLE_ERROR_REPORTING",
	"DISABLE_FEEDBACK_COMMAND",
	"DISABLE_TELEMETRY",
	"ENABLE_TOOL_SEARCH",
	"MAX_MCP_OUTPUT_TOKENS",
	"MAX_THINKING_TOKENS",
	"MCP_TIMEOUT",
	"MCP_TOOL_TIMEOUT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_LOG_TOOL_DETAILS",
	"OTEL_LOG_USER_PROMPTS",
	"OTEL_LOGS_EXPORT_INTERVAL",
	"OTEL_LOGS_EXPORTER",
	"OTEL_METRIC_EXPORT_INTERVAL",
	"OTEL_METRICS_EXPORTER",
	"OTEL_METRICS_INCLUDE_ACCOUNT_UUID",
	"OTEL_METRICS_INCLUDE_SESSION_ID",
	"OTEL_METRICS_INCLUDE_VERSION",
	"OTEL_RESOURCE_ATTRIBUTES",
	"USE_BUILTIN_RIPGREP",
	"VERTEX_REGION_CLAUDE_3_5_HAIKU",
	"VERTEX_REGION_CLAUDE_3_5_SONNET",
	"VERTEX_REGION_CLAUDE_3_7_SONNET",
	"VERTEX_REGION_CLAUDE_4_0_OPUS",
	"VERTEX_REGION_CLAUDE_4_0_SONNET",
	"VERTEX_REGION_CLAUDE_4_1_OPUS",
	"VERTEX_REGION_CLAUDE_4_5_SONNET",
	"VERTEX_REGION_CLAUDE_4_6_SONNET",
	"VERTEX_REGION_CLAUDE_HAIKU_4_5",
)

// IsSafeEnvVar reports whether key may be applied from project settings.
func IsSafeEnvVar(key string) bool {
	return safeEnvVars.has(upperASCII(key))
}

// DangerousShellSettings name commands that execute arbitrary code from a
// settings file. They are rejected even in user settings, because
// ~/.ultrac/settings.json is itself a thing a compromised process can write.
var DangerousShellSettings = []string{
	"apiKeyHelper",
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
	"otelHeadersHelper",
	"statusLine",
}

// IsDangerousShellSetting reports whether key runs arbitrary code.
func IsDangerousShellSetting(key string) bool {
	for _, k := range DangerousShellSettings {
		if k == key {
			return true
		}
	}
	return false
}

// set is a tiny uppercase-normalised string set. Go's maps.Set is generic over
// comparable types and cannot be indexed by a transformed key, so this exists
// to keep the case-insensitive lookup in one place.
type set map[string]struct{}

func newSet(keys ...string) set {
	s := make(set, len(keys))
	for _, k := range keys {
		s[upperASCII(k)] = struct{}{}
	}
	return s
}

func (s set) has(upperKey string) bool {
	_, ok := s[upperKey]
	return ok
}

// upperASCII upper-cases ASCII letters only, leaving UTF-8 bytes intact.
// strings.ToUpper would also fold non-ASCII runes, which can change a byte
// length and corrupt a path-like key.
func upperASCII(s string) string {
	hasLower := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			hasLower = true
			break
		}
	}
	if !hasLower {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func hasPrefixASCII(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
