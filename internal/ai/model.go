package ai

// Model catalog types.
//
// pi: packages/ai/src/types.ts:819-894 (BaseModel / Model) and
// packages/ai/src/providers/model-schema.ts (the enum and sub-record types).
// Only the chat Model is modelled: image and classifier models are explicit
// non-goals (docs/pi-core-harness-plan.md §2.2).

// Api is a provider API dialect. Known values are listed as constants; any
// other string is accepted, mirroring pi's `KnownApi | (string & {})`.
//
// pi: packages/ai/src/types.ts:61-73.
type Api string

const (
	APIOpenAICompletions     Api = "openai-completions"
	APIMistralConversations  Api = "mistral-conversations"
	APIOpenAIResponses       Api = "openai-responses"
	APIAzureOpenAIResponses  Api = "azure-openai-responses"
	APIOpenAICodexResponses  Api = "openai-codex-responses"
	APIAnthropicMessages     Api = "anthropic-messages"
	APIBedrockConverseStream Api = "bedrock-converse-stream"
	APIGoogleGenerativeAI    Api = "google-generative-ai"
	APIGoogleVertex          Api = "google-vertex"
	APIPiMessages            Api = "pi-messages"
)

// ProviderId identifies a provider. Known values are constants; any other
// string is accepted, mirroring pi's `KnownProvider | string`.
//
// pi: packages/ai/src/types.ts:87-130.
type ProviderId string

const (
	ProviderAmazonBedrock           ProviderId = "amazon-bedrock"
	ProviderAntLing                 ProviderId = "ant-ling"
	ProviderAnthropic               ProviderId = "anthropic"
	ProviderGoogle                  ProviderId = "google"
	ProviderGoogleVertex            ProviderId = "google-vertex"
	ProviderOpenAI                  ProviderId = "openai"
	ProviderAzure                   ProviderId = "azure"
	ProviderOpenAICodex             ProviderId = "openai-codex"
	ProviderRadius                  ProviderId = "radius"
	ProviderTypesafe                ProviderId = "typesafe"
	ProviderNvidia                  ProviderId = "nvidia"
	ProviderDeepseek                ProviderId = "deepseek"
	ProviderGithubCopilot           ProviderId = "github-copilot"
	ProviderXAI                     ProviderId = "xai"
	ProviderGroq                    ProviderId = "groq"
	ProviderCerebras                ProviderId = "cerebras"
	ProviderOpenRouter              ProviderId = "openrouter"
	ProviderVercelAIGateway         ProviderId = "vercel-ai-gateway"
	ProviderZai                     ProviderId = "zai"
	ProviderZaiCodingCN             ProviderId = "zai-coding-cn"
	ProviderMistral                 ProviderId = "mistral"
	ProviderMinimax                 ProviderId = "minimax"
	ProviderMinimaxCN               ProviderId = "minimax-cn"
	ProviderMoonshotAI              ProviderId = "moonshotai"
	ProviderMoonshotAICN            ProviderId = "moonshotai-cn"
	ProviderHuggingface             ProviderId = "huggingface"
	ProviderFireworks               ProviderId = "fireworks"
	ProviderTogether                ProviderId = "together"
	ProviderBaseten                 ProviderId = "baseten"
	ProviderOpencode                ProviderId = "opencode"
	ProviderOpencodeGo              ProviderId = "opencode-go"
	ProviderKimiCoding              ProviderId = "kimi-coding"
	ProviderMeta                    ProviderId = "meta"
	ProviderCloudflareWorkersAI     ProviderId = "cloudflare-workers-ai"
	ProviderCloudflareAIGateway     ProviderId = "cloudflare-ai-gateway"
	ProviderQwenTokenPlan           ProviderId = "qwen-token-plan"
	ProviderQwenTokenPlanCN         ProviderId = "qwen-token-plan-cn"
	ProviderQwenTokenPlanIndividual ProviderId = "qwen-token-plan-individual"
	ProviderXiaomi                  ProviderId = "xiaomi"
	ProviderXiaomiTokenPlanCN       ProviderId = "xiaomi-token-plan-cn"
	ProviderXiaomiTokenPlanAMS      ProviderId = "xiaomi-token-plan-ams"
	ProviderXiaomiTokenPlanSGP      ProviderId = "xiaomi-token-plan-sgp"
)

// ToolChoice is provider-neutral tool selection for simple requests.
//
// pi: packages/ai/src/types.ts:132.
type ToolChoice string

const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

// Transport selects a provider transport when several are available.
//
// pi: packages/ai/src/types.ts:144.
type Transport string

const (
	TransportSSE             Transport = "sse"
	TransportWebsocket       Transport = "websocket"
	TransportWebsocketCached Transport = "websocket-cached"
	TransportAuto            Transport = "auto"
)

// ModelInputModality is one input modality a model accepts.
//
// pi: packages/ai/src/providers/model-schema.ts:5.
type ModelInputModality string

const (
	ModalityText  ModelInputModality = "text"
	ModalityImage ModelInputModality = "image"
)

// CacheRetention is a prompt-cache retention preference.
//
// pi: packages/ai/src/providers/model-schema.ts:6.
type CacheRetention string

const (
	CacheRetentionNone  CacheRetention = "none"
	CacheRetentionShort CacheRetention = "short"
	CacheRetentionLong  CacheRetention = "long"
)

// ModelThinkingLevel is a thinking level as models report support for it:
// the agent-level levels plus "off".
//
// pi: packages/ai/src/providers/model-schema.ts:3-4.
type ModelThinkingLevel string

const (
	ModelThinkOff     ModelThinkingLevel = "off"
	ModelThinkMinimal ModelThinkingLevel = "minimal"
	ModelThinkLow     ModelThinkingLevel = "low"
	ModelThinkMedium  ModelThinkingLevel = "medium"
	ModelThinkHigh    ModelThinkingLevel = "high"
	ModelThinkXHigh   ModelThinkingLevel = "xhigh"
	ModelThinkMax     ModelThinkingLevel = "max"
)

// ThinkingLevel is a requested reasoning level, without "off".
//
// pi: packages/ai/src/providers/model-schema.ts:3 (THINKING_LEVELS), used by
// SimpleStreamOptions.reasoning (types.ts:372).
type ThinkingLevel string

const (
	ThinkMinimal ThinkingLevel = "minimal"
	ThinkLow     ThinkingLevel = "low"
	ThinkMedium  ThinkingLevel = "medium"
	ThinkHigh    ThinkingLevel = "high"
	ThinkXHigh   ThinkingLevel = "xhigh"
	ThinkMax     ThinkingLevel = "max"
)

// ThinkingBudgets are custom token budgets per thinking level.
//
// pi: packages/ai/src/types.ts:136-141.
type ThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

// SamplingParams are arbitrary provider sampling parameters.
//
// pi: packages/ai/src/types.ts:133.
type SamplingParams map[string]any

// SamplingParamsByThinkingLevel overrides sampling params per thinking level.
//
// pi: packages/ai/src/types.ts:134.
type SamplingParamsByThinkingLevel map[ModelThinkingLevel]SamplingParams

// ThinkingLevelMap maps pi thinking levels to provider/model-specific values.
// A nil *string marks a level as unsupported.
//
// pi: packages/ai/src/providers/model-schema.ts:14-15.
type ThinkingLevelMap map[ModelThinkingLevel]*string

// ModelPromptCache holds best-effort cache lifetimes in seconds per retention
// tier; keys are "short" and "long". A missing tier means unknown.
//
// pi: packages/ai/src/providers/model-schema.ts:6-7, 17-23.
type ModelPromptCache map[string]int

// ModelCost is a model's pricing: per-million-token rates plus optional
// request-wide tiers; the highest matching input threshold applies.
//
// pi: packages/ai/src/providers/model-schema.ts:25-46.
type ModelCost struct {
	Input      float64         `json:"input"`
	Output     float64         `json:"output"`
	CacheRead  float64         `json:"cacheRead"`
	CacheWrite float64         `json:"cacheWrite"`
	Tiers      []ModelCostTier `json:"tiers,omitempty"`
}

// ModelCostTier is one pricing tier keyed by total request input tokens.
//
// pi: packages/ai/src/providers/model-schema.ts:33-38.
type ModelCostTier struct {
	InputTokensAbove int     `json:"inputTokensAbove"`
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cacheRead"`
	CacheWrite       float64 `json:"cacheWrite"`
}

// ModelImageResizeOptions is a cache-safe resize profile for images.
//
// pi: packages/ai/src/providers/model-schema.ts:48-62.
type ModelImageResizeOptions struct {
	MaxWidth    *int `json:"maxWidth,omitempty"`
	MaxHeight   *int `json:"maxHeight,omitempty"`
	MaxBytes    *int `json:"maxBytes,omitempty"`
	JPEGQuality *int `json:"jpegQuality,omitempty"`
}

// ModelImageInputLimits bounds images per message and per request.
//
// pi: packages/ai/src/providers/model-schema.ts:64-76.
type ModelImageInputLimits struct {
	Resize        *ModelImageResizeOptions `json:"resize,omitempty"`
	MaxPerMessage *int                     `json:"maxPerMessage,omitempty"`
	MaxPerRequest *int                     `json:"maxPerRequest,omitempty"`
}

// ModelInputLimits bounds the serialized provider request size and images.
//
// pi: packages/ai/src/providers/model-schema.ts:78-83.
type ModelInputLimits struct {
	MaxRequestBytes *int                   `json:"maxRequestBytes,omitempty"`
	Images          *ModelImageInputLimits `json:"images,omitempty"`
}

// Model is a chat model catalog entry: usable with Stream and friends.
//
// Compat is intentionally `any`: it is an API-specific override object whose
// shape depends on Api (pi: types.ts:856-866). Providers narrow it.
//
// pi: packages/ai/src/types.ts:820-867.
type Model struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	API      Api                  `json:"api"`
	Provider ProviderId           `json:"provider"`
	BaseURL  string               `json:"baseUrl"`
	Input    []ModelInputModality `json:"input"`

	InputLimits *ModelInputLimits `json:"inputLimits,omitempty"`
	Cost        ModelCost         `json:"cost"`
	Headers     map[string]string `json:"headers,omitempty"`

	// Type is "chat" when set; chat is the default model type (types.ts:840).
	Type string `json:"type,omitempty"`

	Reasoning                     bool                          `json:"reasoning"`
	ThinkingLevelMap              ThinkingLevelMap              `json:"thinkingLevelMap,omitempty"`
	PromptCache                   ModelPromptCache              `json:"promptCache,omitempty"`
	ContextWindow                 int                           `json:"contextWindow"`
	MaxTokens                     int                           `json:"maxTokens"`
	SamplingParams                SamplingParams                `json:"samplingParams,omitempty"`
	SamplingParamsByThinkingLevel SamplingParamsByThinkingLevel `json:"samplingParamsByThinkingLevel,omitempty"`

	Compat any `json:"compat,omitempty"`
}
