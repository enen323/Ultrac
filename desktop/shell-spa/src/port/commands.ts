// commands.ts — the C4 command names this page is allowed to send. A mirror
// of the const block in desktop/shell/src/types.ts, which is itself a mirror
// of internal/rpc/types.go. Three files, one set of names: the kernel's
// dispatch switch answers an unknown command with an error response, so a
// typo here surfaces as a rejected call rather than a hang.
//
// The set is closed on purpose (plan D3): the page implements the 18-command
// face of docs/desktop-shell-plan.md §4.2 plus the P-C4 branch quartet's
// user-facing half (compact, fork, get_fork_messages — get_tree and clone
// stay kernel-side: the tree navigator and the extraction have no page yet),
// add_custom_model (custom-model-plan.md: the custom-model dialog's one
// command) and get_mcp_status (mcp-plan.md D6: the MCP panel's one command).

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
export const CmdCompact = "compact";
export const CmdFork = "fork";
export const CmdGetForkMessages = "get_fork_messages";
export const CmdAddCustomModel = "add_custom_model";
export const CmdGetMCPStatus = "get_mcp_status";
