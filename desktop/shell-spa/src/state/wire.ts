// wire.ts — TypeScript mirrors of the Go wire shapes. Every interface here
// has a struct twin in the Ultrac kernel with the same JSON field names:
//
//   internal/transport/pi_json.go   the event lines (agent events)
//   internal/rpc/types.go           the command/response/state shapes
//   internal/rpc/server.go          the per-command response data
//   internal/ai/types.go            messages, content blocks, usage
//   internal/session/list.go        SessionSummary (list_sessions)
//
// The page never sees Go: it sees these. Field names are the wire's
// (camelCase), never Go's. When the kernel changes a shape, this file
// changes with it — that is the whole contract.

// ---------------------------------------------------------------------------
// Commands and responses
// ---------------------------------------------------------------------------

/** One command line written to the kernel. */
export interface RpcCommand {
  type: string;

  // prompt / steer / follow_up
  message?: string;
  images?: RpcImage[];
  streamingBehavior?: "steer" | "followUp";

  // set_model
  provider?: string;
  modelId?: string;

  // set_thinking_level
  level?: string;

  // set_session_name
  name?: string;

  // resume_session
  sessionPath?: string;

  // compact
  customInstructions?: string;

  // add_custom_model (an Ultrac extension, like list_sessions): the
  // provider/modelId fields above are reused for the spec's provider id
  // and model id; the rest are this command's own.
  baseUrl?: string;
  apiKey?: string;
  api?: string;
  modelName?: string;
  contextWindow?: number;
  maxTokens?: number;

  // fork / clone
  entryId?: string;
}

/** One image attachment on a prompt-shaped command (base64 payload). */
export interface RpcImage {
  type: "image";
  data: string;
  mimeType: string;
}

/** One response line answering a command (RpcResponse). */
export interface RpcResponse {
  type: "response";
  command: string;
  success: boolean;
  data?: unknown;
  error?: string;
}

/** One pushed event line. Discriminated by `type`. */
export interface RpcEvent {
  type: string;
  [key: string]: unknown;
}

// ---------------------------------------------------------------------------
// Session state (get_state's data, and the shell's session_loaded payload)
// ---------------------------------------------------------------------------

/** RpcSessionState — internal/rpc/types.go. */
export interface RpcSessionState {
  model?: RpcModel;
  thinkingLevel: string;
  isStreaming: boolean;
  isCompacting: boolean;
  steeringMode: string;
  followUpMode: string;
  sessionFile?: string;
  sessionId: string;
  sessionName?: string;
  autoCompactionEnabled: boolean;
  messageCount: number;
  pendingMessageCount: number;
}

/** The model a session runs with (ai.Model). */
export interface RpcModel {
  id: string;
  name: string;
  api: string;
  provider: string;
  baseUrl?: string;
  input?: string[];
  reasoning?: boolean;
  contextWindow?: number;
  maxTokens?: number;
  cost?: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
  };
}

/** SessionSummary — internal/session/list.go (list_sessions' entries). */
export interface SessionSummary {
  sessionId: string;
  path: string;
  title: string;
  timestamp: string;
  messageCount: number;
  name?: string;
  cwd?: string;
}

// ---------------------------------------------------------------------------
// Compaction and the branch tree (P-C2 / P-C4)
// ---------------------------------------------------------------------------

/**
 * CompactResult — internal/rpc/types.go (compact's response data).
 * tokensBefore is what the conversation cost; estimatedTokensAfter is the
 * projection's estimate the session layer adds (pi's compaction.ts:104-114).
 */
export interface CompactResult {
  summary: string;
  firstKeptEntryId: string;
  tokensBefore: number;
  estimatedTokensAfter: number;
  usage: Usage;
  details: { readFiles: string[]; modifiedFiles: string[] };
}

/**
 * ForkMessage — internal/session/pi_tree.go (get_fork_messages' entries):
 * one candidate in the fork selector, a user message with text.
 */
export interface ForkMessage {
  entryId: string;
  text: string;
}

/**
 * ForkResult — internal/rpc/server.go's cmdFork data. leafId is null in
 * the before-any-entry position (pi's string | null); editorText is
 * absent unless the target was a user message, whose text the client
 * puts back in the composer.
 */
export interface ForkResult {
  leafId: string | null;
  editorText?: string;
}

// ---------------------------------------------------------------------------
// Custom models (custom-model-plan.md)
// ---------------------------------------------------------------------------

/**
 * CustomModelSpec — add_custom_model's payload (internal/rpc/types.go).
 * One command, one user intent: the kernel writes the credential to
 * the provider entry, reloads, and answers (D3).
 *
 * It deliberately carries no source/layer field: both files are written
 * at the user layer, which is cwd-independent, so the layer is not the
 * client's to choose (D8).
 */
export interface CustomModelSpec {
  /** The provider id: the providers map key and set_model's "provider/" part. */
  provider: string;
  /** The endpoint root ("https://api.example.com/v1"). */
  baseUrl: string;
  /** The literal credential; it lands in the provider entry's apiKey. */
  apiKey: string;
  /** The dialect: one of the two the kernel registers (F2). */
  api: string;
  /** The endpoint's model id ("deepseek-chat"). */
  modelId: string;
  /** The picker's display name; the kernel falls back to the id. */
  modelName: string;
  contextWindow: number;
  maxTokens: number;
}

/** CustomModelProviderInfo — one provider in add_custom_model's response. */
export interface CustomModelProviderInfo {
  id: string;
  baseUrl: string;
  api: string;
  modelIds: string[];
}

/**
 * AddCustomModelResult — the command's response data: the fresh model
 * catalog (the picker patches from it, no second round trip) plus every
 * user-declared provider (built-in-only providers are absent).
 */
export interface CustomModelResult {
  models: RpcModel[];
  providers: CustomModelProviderInfo[];
}

// ---------------------------------------------------------------------------
// Messages and content blocks (ai.Message / ai.ContentBlock)
// ---------------------------------------------------------------------------

export type Role = "system" | "user" | "assistant" | "toolResult";

/** ai.ContentBlock on the wire: only the active type's fields appear. */
export interface ContentBlock {
  type: "text" | "thinking" | "image" | "toolCall";
  text?: string;
  thinking?: string;
  redacted?: boolean;
  data?: string;
  mimeType?: string;
  id?: string;
  name?: string;
  arguments?: Record<string, unknown>;
}

/** ai.Usage. */
export interface Usage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  reasoning?: number;
  totalTokens: number;
  cost: { input: number; output: number; cacheRead: number; cacheWrite: number; total: number };
}

/**
 * ai.Message on the wire. System and user messages may carry their content
 * as a bare string; assistant and toolResult always carry the block array
 * (ai.Message.MarshalJSON picks the per-role shape).
 */
export interface WireMessage {
  role: Role;
  content: string | ContentBlock[];
  toolCallId?: string;
  toolName?: string;
  isError?: boolean;
  errorMessage?: string;
  stopReason?: string;
  usage?: Usage;
  timestamp: number;
}

// ---------------------------------------------------------------------------
// Events (internal/transport/pi_json.go — the pi JSON dialect)
// ---------------------------------------------------------------------------

/** The assistant-event half of a message_update envelope (ai.AssistantEvent). */
export type AssistantMessageEvent =
  | { type: "start" }
  | { type: "text_start"; contentIndex: number }
  | { type: "text_delta"; contentIndex: number; delta: string }
  | { type: "text_end"; contentIndex: number; content: string }
  | { type: "thinking_start"; contentIndex: number }
  | { type: "thinking_delta"; contentIndex: number; delta: string }
  | { type: "thinking_end"; contentIndex: number; content: string }
  | { type: "toolcall_start"; contentIndex: number }
  | { type: "toolcall_delta"; contentIndex: number; delta: string }
  | { type: "toolcall_end"; contentIndex: number; toolCall: { id: string; name: string; arguments: Record<string, unknown> } }
  | { type: "done"; reason: string; message: WireMessage }
  | { type: "error"; reason: string; error: WireMessage };

/** The tool-result payload carried by tool_execution_* events (agent.Result). */
export interface ToolResultWire {
  content: ContentBlock[];
  details?: unknown;
  isError?: boolean;
}
