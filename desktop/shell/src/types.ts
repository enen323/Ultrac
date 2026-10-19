// types.ts — the C4 wire protocol as the shell sees it. A client-side
// mirror of internal/rpc/types.go (the Go side of the same contract): the
// shell spawns `ultrac --mode rpc` and every shape here has a Go twin with
// the same field names. Field names are the wire's (camelCase), not Go's.
//
// The command set is closed: the const block below is the mirror of the
// one in types.go. Anything the kernel does not implement answers with an
// error response, so a typo surfaces as a rejected call, never a hang.
//
// # Response vs event
//
// A response carries `type: "response"` and the `id` of the command that
// caused it (absent only for fire-and-forget sends). An event carries any
// other type and no id — it is a push. That is the whole discrimination
// rule; see rpc-client.ts.

/** One command line the shell writes to the kernel's stdin. */
export interface RpcCommand {
  /** Correlates the command with its response; omitted when sending without waiting. */
  id?: string;
  type: string;

  // prompt / steer / follow_up
  message?: string;
  images?: RpcImage[];
  streamingBehavior?: "steer" | "followUp";

  // new_session
  parentSession?: string;

  // set_model
  provider?: string;
  modelId?: string;

  // set_thinking_level / set_steering_mode / set_follow_up_mode
  level?: string;
  mode?: string;

  // get_entries
  since?: string;

  // set_session_name
  name?: string;

  // resume_session
  sessionPath?: string;
}

/** One image attachment on a prompt-shaped command (base64 payload). */
export interface RpcImage {
  type: "image";
  data: string;
  mimeType: string;
}

/** One response line answering a command. */
export interface RpcResponse {
  id?: string;
  type: "response";
  command: string;
  success: boolean;
  data?: unknown;
  error?: string;
}

/** One pushed event line. Open-shaped: the agent event vocabulary is wide. */
export interface RpcEvent {
  type: string;
  [key: string]: unknown;
}

/** get_state's data — also the payload the shell sends the SPA as session_loaded. */
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

/** The model a session runs with (subset of the fields the shell reads). */
export interface RpcModel {
  id: string;
  name: string;
  api: string;
  provider: string;
  baseUrl?: string;
  input?: string[];
  cost?: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
  };
  reasoning?: boolean;
  contextWindow?: number;
  maxTokens?: number;
}

// Command names — the closed set, mirroring the const block in
// internal/rpc/types.go. list_sessions / resume_session are the Ultrac
// extensions added for this shell (plan §1, P-D0); everything pi has that
// this port does not implement is absent on purpose, so a client that
// sends one gets an error response instead of silence.
export const CmdPrompt = "prompt";
export const CmdSteer = "steer";
export const CmdFollowUp = "follow_up";
export const CmdAbort = "abort";
export const CmdClearQueue = "clear_queue";
export const CmdNewSession = "new_session";
export const CmdGetState = "get_state";
export const CmdSetModel = "set_model";
export const CmdGetAvailableModels = "get_available_models";
export const CmdSetThinkingLevel = "set_thinking_level";
export const CmdGetAvailableThinking = "get_available_thinking_levels";
export const CmdSetSteeringMode = "set_steering_mode";
export const CmdSetFollowUpMode = "set_follow_up_mode";
export const CmdGetMessages = "get_messages";
export const CmdGetLastAssistantText = "get_last_assistant_text";
export const CmdSetSessionName = "set_session_name";
export const CmdGetEntries = "get_entries";
export const CmdGetCommands = "get_commands";
export const CmdListSessions = "list_sessions";
export const CmdResumeSession = "resume_session";
