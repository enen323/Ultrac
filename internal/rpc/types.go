// types.go — the RPC wire protocol: the C4 contract from
// docs/pi-core-harness-plan.md §3.7, a field-by-field port of pi's
// packages/coding-agent/src/modes/rpc/rpc-types.ts (303 lines).
//
// # The protocol
//
// Commands arrive as JSON lines on stdin; responses and events leave as
// JSON lines on stdout. Every command carries an optional id that its
// response echoes, which is what lets a client correlate a response with
// the command that caused it — events have no id, they are pushes.
//
//	command   {"id?":"1","type":"prompt","message":"hi"}
//	response  {"id":"1","type":"response","command":"prompt","success":true,"data":{...}}
//	event     {"type":"message_start","message":{...}}
//
// # Why one struct, not a union
//
// TypeScript spells the command set as a discriminated union; Go has no
// union, so this is one struct with every field any command uses. The
// discriminant is Type, and the dispatch (server.go) is the exhaustive
// switch the compiler cannot check — the unknown-command branch is the
// runtime guard, and it returns an error response rather than panicking
// (plan DoD: "未知命令返回 success:false + 明确 error，不崩").
//
// The response is a single struct rather than per-command shapes: the only
// variation is data-vs-error, which omitempty covers, and one type keeps
// the field order pi writes (id, type, command, success, data|error) for
// every command instead of trusting per-struct luck.
package rpc

import (
	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// ImageContent is one image attachment on a prompt-shaped command.
//
// pi: pi-ai's ImageContent ({type:"image", data: base64, mimeType}),
// rpc-types.ts:22-24. The agent layer stores it as an ai.ContentBlock
// (ai.ImageBlock); this is the wire form the commands carry.
type ImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// RpcCommand is one line of stdin: a discriminant plus the fields the
// commands in this port use.
//
// Fields belonging to commands this port does not implement (bash,
// switch_session, …) are deliberately absent: an unimplemented command
// falls through to the unknown-command branch with a clear error rather
// than being silently accepted and dropped. The set of implemented
// commands is documented on the dispatch switch in server.go.
type RpcCommand struct {
	// ID correlates the command with its response. Optional: a command
	// without one gets a response without one (pi: rpc-types.ts:20+).
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`

	// prompt / steer / follow_up
	Message string         `json:"message,omitempty"`
	Images  []ImageContent `json:"images,omitempty"`
	// StreamingBehavior is prompt-only: "steer" or "followUp", deciding
	// which queue absorbs the message when a run is already active.
	StreamingBehavior string `json:"streamingBehavior,omitempty"`

	// new_session
	ParentSession string `json:"parentSession,omitempty"`

	// set_model
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	// add_custom_model (an Ultrac extension, like list_sessions): the
	// provider/modelId fields above are reused for the spec's provider
	// id and model id; the rest are this command's own.
	BaseURL       string `json:"baseUrl,omitempty"`
	APIKey        string `json:"apiKey,omitempty"`
	API           string `json:"api,omitempty"`
	ModelName     string `json:"modelName,omitempty"`
	ContextWindow *int   `json:"contextWindow,omitempty"`
	MaxTokens     *int   `json:"maxTokens,omitempty"`
	// ThinkingLevels is the set of thinking levels the new model supports
	// (low / medium / high — the dialog's checkboxes). Absent or empty
	// means "not a reasoning model"; there is deliberately no scalar
	// "highest level" form, because a provider may offer high without
	// medium (custom-model-thinking-limits-plan.md D1).
	ThinkingLevels []string `json:"thinkingLevels,omitempty"`

	// set_thinking_level
	Level string `json:"level,omitempty"`

	// set_steering_mode / set_follow_up_mode
	Mode string `json:"mode,omitempty"`

	// get_entries
	Since string `json:"since,omitempty"`

	// set_session_name
	Name string `json:"name,omitempty"`

	// resume_session (Ultrac extension — see the command-set note on the
	// constants below)
	SessionPath string `json:"sessionPath,omitempty"`

	// compact
	CustomInstructions string `json:"customInstructions,omitempty"`

	// set_auto_compaction. A pointer so "absent" is distinguishable from
	// "false": the command's argument is required (pi's rpc-types.ts types
	// it as a plain boolean), and answering a malformed command with a
	// silent "disabled" is how a client's typo becomes a setting.
	Enabled *bool `json:"enabled,omitempty"`

	// fork / clone. fork names the entry to branch from; clone forks the
	// current leaf and takes no argument.
	EntryID string `json:"entryId,omitempty"`
}

// Command types. The closed set this port implements; anything else is
// unknown (rpc-types.ts:20-74 has pi's full list).
const (
	CmdPrompt               = "prompt"
	CmdSteer                = "steer"
	CmdFollowUp             = "follow_up"
	CmdAbort                = "abort"
	CmdClearQueue           = "clear_queue"
	CmdNewSession           = "new_session"
	CmdGetState             = "get_state"
	CmdSetModel             = "set_model"
	CmdGetAvailableModels   = "get_available_models"
	CmdSetThinkingLevel     = "set_thinking_level"
	CmdGetAvailableThinking = "get_available_thinking_levels"
	CmdSetSteeringMode      = "set_steering_mode"
	CmdSetFollowUpMode      = "set_follow_up_mode"
	CmdGetMessages          = "get_messages"
	CmdGetLastAssistantText = "get_last_assistant_text"
	CmdSetSessionName       = "set_session_name"
	CmdGetEntries           = "get_entries"
	CmdGetCommands          = "get_commands"
	// CmdListSessions and CmdResumeSession are Ultrac extensions beyond
	// pi's command set: the desktop shell's session sidebar needs to
	// enumerate and reopen transcripts (docs/desktop-shell-plan.md §1,
	// P-D0), and pi's RPC has no list — its only session-switching command
	// is switch_session, which stays unimplemented here because the
	// runtime-replacement machinery it belongs to (trust contexts, the
	// before-switch dialog, session_start events) is a non-goal. Adding
	// commands rather than changing existing ones is what keeps the wire
	// compatible: every other command's shape is untouched.
	CmdListSessions  = "list_sessions"
	CmdResumeSession = "resume_session"
	// CmdAddCustomModel is the desktop shell's custom-model flow
	// (docs/开发计划/新功能计划/custom-model-plan.md): one command writes
	// auth.json + models.json and reloads the catalog, so a
	// user-declared provider is selectable without a restart (D3). It is
	// an Ultrac extension beyond pi's command set — pi has no RPC surface
	// for provider configuration — added the same way list_sessions was:
	// a new name, no existing command's shape touched.
	CmdAddCustomModel = "add_custom_model"
	// CmdCompact and CmdSetAutoCompaction are pi's compaction pair
	// (rpc-mode.ts:533-540), landed with P-C2 of
	// docs/compaction-branch-plan.md. The wire shapes are pi's: compact
	// answers with the CompactionResult the agent-session layer builds
	// (CompactResult below), set_auto_compaction with nothing.
	CmdCompact           = "compact"
	CmdSetAutoCompaction = "set_auto_compaction"
	// CmdFork / CmdClone / CmdGetForkMessages / CmdGetTree are pi's branch
	// quartet (rpc-types.ts:62-66, rpc-mode.ts:611-651), landed with P-C4
	// of docs/compaction-branch-plan.md over the P-C3 write API
	// (Branch / ResetLeaf / CreateBranchedSession). The response shapes
	// follow the plan's §5.3 table, which is pi's minus the pieces this
	// port has no runtime replacement for (see each command's doc on the
	// dispatch switch).
	CmdFork            = "fork"
	CmdClone           = "clone"
	CmdGetForkMessages = "get_fork_messages"
	CmdGetTree         = "get_tree"
	cmdParse           = "parse" // not a command: the parse-error response
)

// Dispositions a prompt-shaped command reports.
//
// pi: core/agent-session.ts:310-311 — QueuedInputDisposition is
// "handled" | "queued"; PromptDisposition adds "started". "handled" is
// reachable in pi through input interception (extension commands), which
// this port has none of, so it never appears here.
const (
	DispositionStarted = "started"
	DispositionQueued  = "queued"
)

// Queue modes for steering and follow-up delivery.
//
// pi: rpc-types.ts:43-44.
const (
	QueueModeAll        = "all"
	QueueModeOneAtATime = "one-at-a-time"
)

// RpcResponse is one line of stdout answering a command.
//
// pi: rpc-types.ts:116-245. Success carries data (omitted when the command
// has none); failure carries error. Type is always "response".
type RpcResponse struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RpcSessionState is get_state's data.
//
// pi: rpc-types.ts:96-109. Model is optional because a session can exist
// before a model resolves; in this port a session always has one.
// IsCompacting and AutoCompactionEnabled are the live values since P-C2
// (docs/compaction-branch-plan.md): the host owns both, because compaction
// runs above the agent.
type RpcSessionState struct {
	Model                 *ai.Model `json:"model,omitempty"`
	ThinkingLevel         string    `json:"thinkingLevel"`
	IsStreaming           bool      `json:"isStreaming"`
	IsCompacting          bool      `json:"isCompacting"`
	SteeringMode          string    `json:"steeringMode"`
	FollowUpMode          string    `json:"followUpMode"`
	SessionFile           string    `json:"sessionFile,omitempty"`
	SessionID             string    `json:"sessionId"`
	SessionName           string    `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool      `json:"autoCompactionEnabled"`
	MessageCount          int       `json:"messageCount"`
	PendingMessageCount   int       `json:"pendingMessageCount"`
}

// CompactResult is the compact command's response data, and the payload
// compaction_end's result field carries (pi's CompactionResult as the
// agent-session layer assembles it — agent-session.ts:2892-2899 for the
// manual path, :3222-3229 for the automatic one).
//
// The low-level compact() in pi's compaction package returns no
// estimatedTokensAfter; the session layer adds it after appending the entry,
// which is why it lives here rather than in compact.Result.
//
// pi: rpc-types.ts (compact's data) + compaction.ts:104-114.
type CompactResult struct {
	Summary              string                    `json:"summary"`
	FirstKeptEntryID     string                    `json:"firstKeptEntryId"`
	TokensBefore         int                       `json:"tokensBefore"`
	EstimatedTokensAfter int                       `json:"estimatedTokensAfter"`
	Usage                ai.Usage                  `json:"usage"`
	Details              session.CompactionDetails `json:"details"`
}

// CustomModelSpec is the add_custom_model command's payload (plan D3:
// one command, one user intent — write the credential, write the
// declaration, reload, answer).
//
// It deliberately carries no source/layer field: both files are written
// at the user layer (ConfigHome), which is cwd-independent, so the layer
// choice is not the client's to make (D8 — a project-layer write would
// make the model vanish when the shell opens from another directory).
type CustomModelSpec struct {
	// Provider is the models.json key, and the "provider/" part of
	// set_model's syntax. The client derives it from the baseUrl host
	// when the user leaves the field alone.
	Provider string
	// BaseURL is the endpoint root ("https://api.example.com/v1").
	BaseURL string
	// APIKey is the literal credential. It lands in auth.json, never in
	// models.json (D1): the chain's first step reads models.json's
	// apiKey, and a key left there would shadow an auth.json edit.
	APIKey string
	// API is the dialect. Only the two registered ones are accepted
	// (F2): openai-completions and anthropic-messages.
	API string
	// ModelID is the endpoint's model id ("deepseek-chat").
	ModelID string
	// ModelName is the display name in the picker; empty means the id.
	ModelName string
	// ContextWindow and MaxTokens are the model's limits. The server
	// fills the plan's defaults for an absent command field (R1: a zero
	// window would disable auto-compaction silently); the host rejects
	// non-positive values.
	ContextWindow int
	MaxTokens     int
	// ThinkingLevels is the set of levels the model supports, as ticked
	// in the dialog: each element is "low", "medium" or "high", and an
	// empty slice means "no reasoning" — the host then writes neither
	// reasoning nor thinkingLevelMap, so a non-reasoning declaration
	// stays byte-identical to the one this flow wrote before the levels
	// existed (D2). Unselected levels are written as explicit nulls by
	// the host, because a level merely absent from thinkingLevelMap
	// counts as SUPPORTED (ai.SupportedThinkingLevels).
	ThinkingLevels []string
}

// CustomModelProviderInfo is one provider in add_custom_model's
// response: what the dialog shows under "已添加".
type CustomModelProviderInfo struct {
	ID       string   `json:"id"`
	BaseURL  string   `json:"baseUrl"`
	API      string   `json:"api"`
	ModelIDs []string `json:"modelIds"`
}

// AddCustomModelResult is the command's response data: the fresh model
// catalog — the picker patches from it, no second round trip — plus
// every user-declared provider (built-in-only providers are absent:
// nothing was declared for them).
type AddCustomModelResult struct {
	Models    []ai.Model                `json:"models"`
	Providers []CustomModelProviderInfo `json:"providers"`
}

// successResponse builds a success response. data nil omits the field,
// which is what pi does for the commands that answer with nothing.
func successResponse(id, command string, data any) RpcResponse {
	return RpcResponse{ID: id, Type: "response", Command: command, Success: true, Data: data}
}

// errorResponse builds a failure response. Every command can fail this way
// (rpc-types.ts:245); the message is what the client shows.
func errorResponse(id, command, message string) RpcResponse {
	return RpcResponse{ID: id, Type: "response", Command: command, Success: false, Error: message}
}
