// server.go — the RPC mode: a headless agent server speaking the C4
// protocol on stdin/stdout, a port of pi's
// packages/coding-agent/src/modes/rpc/rpc-mode.ts (819 lines), minus the
// extension-UI half this port has no extensions to serve
// (docs/pi-core-harness-plan.md §3.7).
//
// # The shape
//
// One process, one command loop, one event pump. The loop reads JSONL
// commands from stdin, dispatches them against the Host, and writes one
// response line per command. The pump subscribes to the agent and writes
// every event as a line — through the same PiJSONWriter the responses use,
// so a response and an event can never interleave into one broken line
// (transport/pi_json.go, file comment).
//
// The Host is pi's AgentSessionRuntime role: the harness (agent, session,
// model, tools, prompt sections) assembled by the CLI layer and driven
// through this narrow interface. Keeping it an interface is what lets the
// dispatch tests run against a real agent with a fake stream function and
// no network.
//
// # Command set
//
// Implemented: prompt, steer, follow_up, abort, clear_queue, new_session,
// get_state, set_model, get_available_models, set_thinking_level,
// get_available_thinking_levels, set_steering_mode, set_follow_up_mode,
// get_messages, get_last_assistant_text, set_session_name, get_entries,
// get_commands, list_sessions, resume_session, compact,
// set_auto_compaction, fork, clone, get_fork_messages, get_tree,
// add_custom_model.
//
// list_sessions, resume_session and add_custom_model are Ultrac
// extensions beyond pi's set (types.go explains why and what pi's
// switch_session would have been); compact and set_auto_compaction are
// pi's own, landed with P-C2 of docs/compaction-branch-plan.md; fork /
// clone / get_fork_messages / get_tree are pi's branch quartet, landed
// with P-C4; add_custom_model is the desktop shell's custom-model flow,
// landed with docs/开发计划/新功能计划/custom-model-plan.md.
//
// Not implemented (rpc-types.ts has them; this port does not):
// set_auto_retry, abort_retry, bash, abort_bash, export_html,
// switch_session, get_session_stats, cycle_model, cycle_thinking_level,
// and the extension_ui_* pair. They fall through to the unknown-command
// branch with a clear error rather than being silently dropped — switch
// _session in particular stays unimplemented because resume_session is its
// semantic twin here (see types.go), and the rest belong to subsystems
// this port has none of.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/session"
	"ultrac/internal/transport"
)

// Host is the harness the server drives — pi's AgentSessionRuntime
// (rpc-mode.ts:54).
//
// Every method must be safe to call from the command-loop goroutine;
// Persist is also called from the run goroutine (the event pump).
type Host interface {
	// Agent is the live agent. NewSession swaps in a fresh one, so callers
	// re-read it per command instead of caching the pointer.
	Agent() *agent.Agent
	// Session is the live session manager (the transcript).
	Session() *session.SessionManager
	// RunContext is the context prompt runs derive from. The host cancels
	// it on a termination signal; the loop turns the cancellation into an
	// aborted stream.
	RunContext() context.Context

	// Persist writes one completed message to the session file. The host
	// owns the --no-transcript flag and the persistable-role rule (pi:
	// agent-session.ts:1158-1163), so the server just forwards.
	Persist(msg ai.Message)
	// NewSession replaces the agent and the session with a fresh pair,
	// aborting any active run first (bounded wait). parentSession links the
	// new transcript to its parent in the header.
	NewSession(parentSession string)

	// ListSessions summarizes the session files for the host's cwd,
	// newest first — what a client's session sidebar renders. An error is
	// a listing failure (an unreadable sessions directory), which the
	// server turns into an error response.
	ListSessions() ([]session.SessionSummary, error)
	// ResumeSession swaps the live agent and session to an existing
	// transcript at path, aborting any active run first (bounded wait) —
	// the same abort→swap shape as NewSession, but loading instead of
	// creating. The error is the load failing (a missing or invalid
	// file); the host leaves the live pair untouched in that case.
	ResumeSession(path string) error

	// SetModel validates provider/modelId against AvailableModels, applies
	// it, and persists a model_change entry. The error message is what the
	// response carries ("Model not found: provider/id").
	SetModel(provider, modelID string) (ai.Model, error)
	// AvailableModels lists the models set_model accepts.
	AvailableModels() []ai.Model
	// SetThinkingLevel applies the level and persists the change.
	SetThinkingLevel(level agent.ThinkingLevel)
	// ThinkingLevels lists the levels the current model supports, in
	// canonical order (ai.SupportedThinkingLevels).
	ThinkingLevels() []ai.ModelThinkingLevel
	// SetSessionName persists the display name (a session_info entry).
	SetSessionName(name string) error

	// BuildPrompt assembles one prompt command's messages: the system
	// sections patch (nil when the transcript already declares the current
	// sections) plus the user message with its images. The patch is
	// computed per prompt, exactly as pi recomputes it in
	// _preparePromptAndToolLoadout (agent-session.ts:1742-1754). The error
	// is the prompt build failing; the server turns it into an error
	// response rather than sending a degraded prompt.
	BuildPrompt(text string, images []ImageContent) (PromptInput, error)

	// Compact runs a manual compaction: the host aborts any active run
	// first (bounded wait), then plans, summarizes and appends the
	// checkpoint, and swaps the agent's context to the compressed
	// projection (pi: agent-session.ts:2789-2909). The error is the
	// compaction failing — including pi's idempotence errors "Already
	// compacted" and "Nothing to compact (session too small)", which the
	// server surfaces verbatim so a client can tell them apart from a
	// summarization failure.
	Compact(customInstructions string) (*CompactResult, error)
	// SetAutoCompaction persists the auto-compaction gate to the user
	// settings layer and applies it to the live session (pi:
	// setAutoCompactionEnabled + settings-manager persistence). The error
	// is the settings write failing; the live gate stays untouched then.
	SetAutoCompaction(enabled bool) error
	// IsCompacting reports whether a compaction (automatic or manual) is
	// running right now (pi: agent-session.ts:1627-1633).
	IsCompacting() bool
	// AutoCompactionEnabled reports the resolved auto-compaction gate —
	// the settings value, not the runtime state.
	AutoCompactionEnabled() bool

	// Fork moves the session leaf to branch from entryID (pi's navigateTree
	// without the summary, agent-session.ts:4113-4128 — plan D7). A user
	// message target moves the leaf to the message's parent (the
	// resetLeaf position for a root message) and returns the message's
	// text for the client's editor; any other target moves the leaf to the
	// target itself. The host aborts any active run first, the way every
	// other session-mutating command does, and re-seeds the agent from the
	// fork-point projection so the next prompt carries the branch's
	// context rather than the abandoned history. leafID is "" for the
	// before-any-entry position; editorText is "" for non-user targets.
	// The error is the entry not existing (pi's "Invalid entry ID for
	// forking").
	Fork(entryID string) (leafID, editorText string, err error)
	// GetForkMessages lists the fork selector's candidates: every user
	// message with text, in file order (pi's getUserMessagesForForking).
	GetForkMessages() ([]session.ForkMessage, error)
	// GetTree returns the whole session forest plus the current leaf id
	// ("" before any entry) — pi's getTree behind the get_tree command
	// (rpc-mode.ts:649-651, plan D8).
	GetTree() (tree []*session.TreeNode, leafID string, err error)
	// Clone extracts the current root→leaf path into a new session file
	// and returns its path (pi's clone → createBranchedSession,
	// rpc-mode.ts:619-629). The live session is untouched — switching to
	// the clone is the client's resume_session, the twin of pi's
	// switch_session. The error is the session having no leaf yet (pi's
	// "Cannot clone session: no current entry selected") or the extraction
	// failing.
	Clone() (string, error)

	// AddCustomModel persists a user-declared provider and reloads the
	// catalog (custom-model-plan.md D3/D4): the credential goes to
	// auth.json, the declaration to models.json — both at the user layer,
	// which is cwd-independent (D8) — and the fresh snapshot is published
	// so the new model is selectable in this process without a restart.
	// The spec arrives already validated (validateCustomModelSpec); the
	// error is a write failure or a reload failure. A models.json write
	// failure leaves an orphan credential in auth.json, which is harmless
	// and visible (`ultrac config` lists auth-only providers) — the
	// reverse order would leave a model with no credential, failing the
	// next launch's fail-fast (D7).
	AddCustomModel(spec CustomModelSpec) (AddCustomModelResult, error)
}

// PromptInput is one prompt command's assembled messages.
type PromptInput struct {
	// Patch is the system sections diff, or nil when nothing changed. A
	// queued message (steer / follow_up / prompt during streaming) drops
	// it: only a run start re-declares sections.
	Patch *ai.Message
	// User is the user message (text plus images).
	User ai.Message
}

// Server is one RPC session.
type Server struct {
	host Host
	out  *transport.PiJSONWriter

	mu        sync.Mutex
	unsub     func()
	runActive bool

	// abortWait bounds how long the abort command waits for the run to
	// settle. A wedged run must not wedge the command loop behind it.
	abortWait time.Duration
}

// NewServer builds a server over an assembled host and starts the event
// pump. The returned server owns its subscription until Close.
func NewServer(host Host, out *transport.PiJSONWriter) *Server {
	s := &Server{host: host, out: out, abortWait: 10 * time.Second}
	s.rebind()
	return s
}

// Close detaches the event pump. Safe to call twice.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsub != nil {
		s.unsub()
		s.unsub = nil
	}
}

// Serve runs the command loop until the input ends or ctx is cancelled.
//
// Returning nil means the input ended (stdin closed — pi's onInputEnd,
// rpc-mode.ts:802-805); returning ctx.Err() means the host cancelled the
// run context (a termination signal). Either way the caller decides the
// exit code; the server itself never calls os.Exit.
//
// The reader runs in its own goroutine and the loop selects on it, because
// a plain blocking read would ignore the cancellation: a client that
// closes the connection without ending stdin would leave the process
// hanging until the next line arrives — if one ever does.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult)
	go func() {
		defer close(lines)
		lr := NewLineReader(in)
		for {
			line, err := lr.Next()
			if err != nil {
				select {
				case lines <- lineResult{err: err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case lines <- lineResult{line: line}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Give an in-flight run its settle window: the cancellation
			// reaches the loop as an aborted stream and the bash tool as a
			// killed process tree, and exiting before either lands would
			// orphan children and truncate the transcript's last line.
			waitCtx, cancel := context.WithTimeout(context.Background(), s.abortWait)
			_ = s.host.Agent().WaitForIdle(waitCtx)
			cancel()
			return ctx.Err()
		case r, ok := <-lines:
			if !ok {
				return nil // stdin closed
			}
			if r.err != nil {
				if r.err == io.EOF {
					return nil
				}
				return r.err
			}
			s.handleLine(r.line)
		}
	}
}

// handleLine parses and dispatches one record. A parse failure answers
// with a "parse" error response and keeps the loop alive (pi:
// rpc-mode.ts:750-764) — a client that sends one malformed line is not a
// reason to drop the session.
func (s *Server) handleLine(line string) {
	var cmd RpcCommand
	if err := json.Unmarshal([]byte(line), &cmd); err != nil {
		_ = s.out.WriteObject(errorResponse("", cmdParse, "Failed to parse command: "+err.Error()))
		return
	}
	resp, launch := s.dispatch(cmd)
	_ = s.out.WriteObject(resp)
	// The response is written before the run starts, which is what keeps
	// it ahead of the first event on the wire — the ordering pi gets from
	// its preflight callback (rpc-mode.ts:394-414). launch is non-nil only
	// for a prompt that starts a run.
	if launch != nil {
		launch()
	}
}

// dispatch routes one command. The second return starts the prompt run
// after the response has been written (see handleLine); it is nil for
// every other command.
func (s *Server) dispatch(cmd RpcCommand) (RpcResponse, func()) {
	switch cmd.Type {
	case CmdPrompt:
		return s.cmdPrompt(cmd)
	case CmdSteer:
		return s.cmdQueue(cmd, true), nil
	case CmdFollowUp:
		return s.cmdQueue(cmd, false), nil
	case CmdAbort:
		return s.cmdAbort(cmd), nil
	case CmdClearQueue:
		return s.cmdClearQueue(cmd), nil
	case CmdNewSession:
		return s.cmdNewSession(cmd), nil
	case CmdListSessions:
		return s.cmdListSessions(cmd), nil
	case CmdResumeSession:
		return s.cmdResumeSession(cmd), nil
	case CmdGetState:
		return s.cmdGetState(cmd), nil
	case CmdSetModel:
		return s.cmdSetModel(cmd), nil
	case CmdGetAvailableModels:
		return s.cmdGetAvailableModels(cmd), nil
	case CmdSetThinkingLevel:
		return s.cmdSetThinkingLevel(cmd), nil
	case CmdGetAvailableThinking:
		return s.cmdGetAvailableThinkingLevels(cmd), nil
	case CmdSetSteeringMode:
		return s.cmdSetQueueMode(cmd, true), nil
	case CmdSetFollowUpMode:
		return s.cmdSetQueueMode(cmd, false), nil
	case CmdGetMessages:
		return s.cmdGetMessages(cmd), nil
	case CmdGetLastAssistantText:
		return s.cmdGetLastAssistantText(cmd), nil
	case CmdSetSessionName:
		return s.cmdSetSessionName(cmd), nil
	case CmdGetEntries:
		return s.cmdGetEntries(cmd), nil
	case CmdGetCommands:
		return s.cmdGetCommands(cmd), nil
	case CmdCompact:
		return s.cmdCompact(cmd), nil
	case CmdSetAutoCompaction:
		return s.cmdSetAutoCompaction(cmd), nil
	case CmdFork:
		return s.cmdFork(cmd), nil
	case CmdClone:
		return s.cmdClone(cmd), nil
	case CmdGetForkMessages:
		return s.cmdGetForkMessages(cmd), nil
	case CmdGetTree:
		return s.cmdGetTree(cmd), nil
	case CmdAddCustomModel:
		return s.cmdAddCustomModel(cmd), nil
	default:
		// Unknown command: a clear error, never a crash (plan DoD). This is
		// also where every pi command this port deliberately does not
		// implement lands.
		return errorResponse(cmd.ID, cmd.Type, "Unknown command: "+cmd.Type), nil
	}
}

// ---------------------------------------------------------------------------
// Prompting
// ---------------------------------------------------------------------------

func (s *Server) cmdPrompt(cmd RpcCommand) (RpcResponse, func()) {
	id := cmd.ID
	in, err := s.host.BuildPrompt(cmd.Message, cmd.Images)
	if err != nil {
		return errorResponse(id, CmdPrompt, err.Error()), nil
	}

	s.mu.Lock()
	active := s.runActive
	s.mu.Unlock()

	if !active {
		return successResponse(id, CmdPrompt, disposition(DispositionStarted)), func() {
			s.startRun(in)
		}
	}

	// A prompt that arrives while a run is active is queued, and the queue
	// it joins must be named — pi throws the same error when
	// streamingBehavior is absent (agent-session.ts:2063-2067).
	switch cmd.StreamingBehavior {
	case "steer":
		s.host.Agent().Steer(in.User)
	case "followUp":
		s.host.Agent().FollowUp(in.User)
	case "":
		return errorResponse(id, CmdPrompt,
			"Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message."), nil
	default:
		return errorResponse(id, CmdPrompt,
			"streamingBehavior must be 'steer' or 'followUp'"), nil
	}
	return successResponse(id, CmdPrompt, disposition(DispositionQueued)), nil
}

func (s *Server) cmdQueue(cmd RpcCommand, steer bool) RpcResponse {
	in, err := s.host.BuildPrompt(cmd.Message, cmd.Images)
	if err != nil {
		name := CmdFollowUp
		if steer {
			name = CmdSteer
		}
		return errorResponse(cmd.ID, name, err.Error())
	}
	name := CmdFollowUp
	if steer {
		name = CmdSteer
		s.host.Agent().Steer(in.User)
	} else {
		s.host.Agent().FollowUp(in.User)
	}
	// pi's _queueUserInput returns "queued" for every message that is not
	// intercepted by an input handler (agent-session.ts:2285-2305); this
	// port has no input handlers, so "handled" is unreachable.
	return successResponse(cmd.ID, name, disposition(DispositionQueued))
}

func (s *Server) cmdAbort(cmd RpcCommand) RpcResponse {
	a := s.host.Agent()
	a.Abort()
	// pi awaits session.abort() (rpc-mode.ts:426-429). Answering before
	// the run settles would let the next prompt race the aborted one into
	// errAlreadyProcessing; the wait is bounded so a wedged run cannot
	// wedge the loop.
	ctx, cancel := context.WithTimeout(context.Background(), s.abortWait)
	defer cancel()
	_ = a.WaitForIdle(ctx)
	return successResponse(cmd.ID, CmdAbort, nil)
}

func (s *Server) cmdClearQueue(cmd RpcCommand) RpcResponse {
	a := s.host.Agent()
	steering := messageTexts(a.SteeringMessages())
	followUp := messageTexts(a.FollowUpMessages())
	a.ClearSteeringQueue()
	a.ClearFollowUpQueue()
	return successResponse(cmd.ID, CmdClearQueue, map[string]any{
		"steering": steering,
		"followUp": followUp,
	})
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

func (s *Server) cmdNewSession(cmd RpcCommand) RpcResponse {
	// The host aborts any active run before swapping (bounded wait), so
	// the pump resubscribes to a quiet agent. cancelled is always false:
	// pi's value comes from a confirm dialog this port does not have
	// (agent-session-runtime.ts:231-234).
	s.host.NewSession(cmd.ParentSession)
	s.rebind()
	return successResponse(cmd.ID, CmdNewSession, map[string]any{"cancelled": false})
}

func (s *Server) cmdListSessions(cmd RpcCommand) RpcResponse {
	// The listing is the host's: it owns the cwd the sessions belong to.
	// An empty list is a success with an empty array — a project that has
	// never chatted has nothing to show, which is not an error.
	sessions, err := s.host.ListSessions()
	if err != nil {
		return errorResponse(cmd.ID, CmdListSessions, err.Error())
	}
	if sessions == nil {
		sessions = []session.SessionSummary{}
	}
	return successResponse(cmd.ID, CmdListSessions, map[string]any{"sessions": sessions})
}

func (s *Server) cmdResumeSession(cmd RpcCommand) RpcResponse {
	// The path is the wire contract: the client got it from
	// list_sessions (or get_state's sessionFile). A blank one is a
	// malformed command, not a missing file — answering with the host's
	// open error would blame the filesystem for an empty request.
	if strings.TrimSpace(cmd.SessionPath) == "" {
		return errorResponse(cmd.ID, CmdResumeSession, "sessionPath is required")
	}
	if err := s.host.ResumeSession(cmd.SessionPath); err != nil {
		return errorResponse(cmd.ID, CmdResumeSession, err.Error())
	}
	// The pump follows the swap exactly as it does after new_session:
	// without the rebind, the next run's events would go to the abandoned
	// agent's subscribers and the client would see nothing.
	s.rebind()
	// cancelled mirrors new_session / pi's switch_session: the confirm
	// dialog that could cancel a switch does not exist here.
	return successResponse(cmd.ID, CmdResumeSession, map[string]any{"cancelled": false})
}

func (s *Server) cmdGetState(cmd RpcCommand) RpcResponse {
	a := s.host.Agent()
	state := a.State()
	mgr := s.host.Session()
	model := state.Model
	return successResponse(cmd.ID, CmdGetState, RpcSessionState{
		Model:                 &model,
		ThinkingLevel:         string(state.ThinkingLevel),
		IsStreaming:           state.IsStreaming,
		IsCompacting:          s.host.IsCompacting(),
		SteeringMode:          string(a.SteeringMode()),
		FollowUpMode:          string(a.FollowUpMode()),
		SessionFile:           mgr.SessionFile(),
		SessionID:             mgr.SessionID(),
		SessionName:           mgr.SessionName(),
		AutoCompactionEnabled: s.host.AutoCompactionEnabled(),
		MessageCount:          len(state.Messages),
		PendingMessageCount:   len(a.SteeringMessages()) + len(a.FollowUpMessages()),
	})
}

func (s *Server) cmdGetMessages(cmd RpcCommand) RpcResponse {
	return successResponse(cmd.ID, CmdGetMessages, map[string]any{
		"messages": s.host.Agent().State().Messages,
	})
}

func (s *Server) cmdGetLastAssistantText(cmd RpcCommand) RpcResponse {
	msgs := s.host.Agent().State().Messages
	var text any // null when there is none (pi: string | undefined)
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != ai.RoleAssistant {
			continue
		}
		// Skip an aborted message with no content (agent-session.ts:4380).
		if m.StopReason == ai.StopAborted && len(m.Content) == 0 {
			continue
		}
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == ai.BlockText {
				sb.WriteString(b.Text)
			}
		}
		text = sb.String()
		break
	}
	return successResponse(cmd.ID, CmdGetLastAssistantText, map[string]any{"text": text})
}

func (s *Server) cmdSetSessionName(cmd RpcCommand) RpcResponse {
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		return errorResponse(cmd.ID, CmdSetSessionName, "Session name cannot be empty")
	}
	if err := s.host.SetSessionName(name); err != nil {
		return errorResponse(cmd.ID, CmdSetSessionName, err.Error())
	}
	return successResponse(cmd.ID, CmdSetSessionName, nil)
}

func (s *Server) cmdGetEntries(cmd RpcCommand) RpcResponse {
	mgr := s.host.Session()
	entries := mgr.Entries()
	if cmd.Since != "" {
		idx := -1
		for i, e := range entries {
			if e.ID == cmd.Since {
				idx = i
				break
			}
		}
		if idx == -1 {
			return errorResponse(cmd.ID, CmdGetEntries, "Entry not found: "+cmd.Since)
		}
		entries = entries[idx+1:]
	}
	// Entries leave through MarshalEntry, not the struct: SessionEntry's
	// Extra is json:"-" (a struct tag cannot express pi's flat line), so a
	// plain marshal would drop every type-specific body — compaction
	// summaries, branch-summary details, usage — that pi puts on this
	// wire. Same reason get_tree's nodes carry raw entry lines.
	lines, err := marshalEntryLines(entries)
	if err != nil {
		return errorResponse(cmd.ID, CmdGetEntries, err.Error())
	}
	// leafId is null, not "", before any entry (pi: leafId: string | null).
	return successResponse(cmd.ID, CmdGetEntries, map[string]any{
		"entries": lines,
		"leafId":  leafOrNull(mgr.GetLeafID()),
	})
}

// marshalEntryLines encodes entries as the file carries them: one JSON
// object per entry, Extra merged in under pi's exact key names.
func marshalEntryLines(entries []session.SessionEntry) ([]json.RawMessage, error) {
	lines := make([]json.RawMessage, 0, len(entries))
	for i := range entries {
		line, err := session.MarshalEntry(&entries[i])
		if err != nil {
			return nil, err
		}
		lines = append(lines, json.RawMessage(bytes.TrimSuffix(line, []byte("\n"))))
	}
	return lines, nil
}

func (s *Server) cmdGetCommands(cmd RpcCommand) RpcResponse {
	// Empty on purpose: this port has no extensions, prompt templates or
	// skills to register (rpc-mode.ts:680-711 lists all three sources).
	// Returning the empty list is the honest answer; a client polling for
	// slash commands learns there are none.
	return successResponse(cmd.ID, CmdGetCommands, map[string]any{"commands": []any{}})
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

func (s *Server) cmdCompact(cmd RpcCommand) RpcResponse {
	// The host owns the abort-then-compress ordering (pi: compact() awaits
	// this.abort() first, agent-session.ts:2790), so the command is a
	// straight delegation: the response is the CompactionResult, or the
	// failure verbatim — including the idempotence errors, which are the
	// client's signal that the session was already compacted.
	result, err := s.host.Compact(cmd.CustomInstructions)
	if err != nil {
		return errorResponse(cmd.ID, CmdCompact, err.Error())
	}
	return successResponse(cmd.ID, CmdCompact, result)
}

func (s *Server) cmdSetAutoCompaction(cmd RpcCommand) RpcResponse {
	// pi types enabled as a required boolean; a missing one is a malformed
	// command, not a request to disable.
	if cmd.Enabled == nil {
		return errorResponse(cmd.ID, CmdSetAutoCompaction, "enabled is required")
	}
	if err := s.host.SetAutoCompaction(*cmd.Enabled); err != nil {
		return errorResponse(cmd.ID, CmdSetAutoCompaction, err.Error())
	}
	return successResponse(cmd.ID, CmdSetAutoCompaction, nil)
}

// ---------------------------------------------------------------------------
// Branch tree (P-C4)
// ---------------------------------------------------------------------------

func (s *Server) cmdFork(cmd RpcCommand) RpcResponse {
	// The host owns the whole decision (plan D7): abort the run, resolve
	// the target, move the leaf, re-seed the agent from the fork-point
	// projection. What is left here is the wire shape: leafId null — not
	// "" — in the before-any-entry position (pi: leafId: string | null),
	// and editorText absent rather than empty when the target was not a
	// user message (pi drops an undefined selectedText).
	leafID, editorText, err := s.host.Fork(cmd.EntryID)
	if err != nil {
		return errorResponse(cmd.ID, CmdFork, err.Error())
	}
	data := map[string]any{"leafId": leafOrNull(leafID)}
	if editorText != "" {
		data["editorText"] = editorText
	}
	return successResponse(cmd.ID, CmdFork, data)
}

func (s *Server) cmdGetForkMessages(cmd RpcCommand) RpcResponse {
	// The selector's options, straight from the session (pi:
	// getUserMessagesForForking → rpc-mode.ts:631-634). An empty list is a
	// success with an empty array — a session with no user messages has
	// nothing to fork from, which is not an error.
	messages, err := s.host.GetForkMessages()
	if err != nil {
		return errorResponse(cmd.ID, CmdGetForkMessages, err.Error())
	}
	if messages == nil {
		messages = []session.ForkMessage{}
	}
	return successResponse(cmd.ID, CmdGetForkMessages, map[string]any{"messages": messages})
}

func (s *Server) cmdGetTree(cmd RpcCommand) RpcResponse {
	// The whole forest plus the leaf (pi: rpc-mode.ts:649-651). The tree's
	// entries arrive through MarshalEntry — every field the file carries,
	// Extra included — and label is absent because this version writes no
	// label entries (plan D8).
	tree, leafID, err := s.host.GetTree()
	if err != nil {
		return errorResponse(cmd.ID, CmdGetTree, err.Error())
	}
	if tree == nil {
		tree = []*session.TreeNode{}
	}
	return successResponse(cmd.ID, CmdGetTree, map[string]any{
		"tree":   tree,
		"leafId": leafOrNull(leafID),
	})
}

func (s *Server) cmdClone(cmd RpcCommand) RpcResponse {
	// The path of the freshly written root→leaf extraction. Switching to
	// it is the client's resume_session — pi rebinds its runtime here
	// (rpc-mode.ts:619-629), this port's twin of switch_session is
	// resume_session (plan D7's note), so the clone answers with the path
	// and leaves the switch to the client.
	path, err := s.host.Clone()
	if err != nil {
		return errorResponse(cmd.ID, CmdClone, err.Error())
	}
	return successResponse(cmd.ID, CmdClone, map[string]any{"sessionPath": path})
}

// leafOrNull maps the manager's "" leaf (before any entry) to JSON null,
// the way pi types leafId (string | null).
func leafOrNull(leafID string) any {
	if leafID == "" {
		return nil
	}
	return leafID
}

// ---------------------------------------------------------------------------
// Custom model (custom-model-plan.md)
// ---------------------------------------------------------------------------

// The form defaults the plan prescribes (R1). They live on the protocol
// side because an absent command field means "the default", never zero:
// a zero contextWindow would disable auto-compaction silently.
const (
	defaultContextWindow = 128000
	defaultMaxTokens     = 8192
)

func (s *Server) cmdAddCustomModel(cmd RpcCommand) RpcResponse {
	spec := CustomModelSpec{
		Provider:      strings.TrimSpace(cmd.Provider),
		BaseURL:       strings.TrimSpace(cmd.BaseURL),
		APIKey:        cmd.APIKey,
		API:           strings.TrimSpace(cmd.API),
		ModelID:       strings.TrimSpace(cmd.ModelID),
		ModelName:     strings.TrimSpace(cmd.ModelName),
		ContextWindow: defaultContextWindow,
		MaxTokens:     defaultMaxTokens,
	}
	if cmd.ContextWindow != nil {
		spec.ContextWindow = *cmd.ContextWindow
	}
	if cmd.MaxTokens != nil {
		spec.MaxTokens = *cmd.MaxTokens
	}
	// The protocol-level half of the validation: a malformed command is
	// answered here and never reaches the host (the set_session_name /
	// set_auto_compaction pattern). Semantic checks that need the
	// filesystem — an unregistered dialect, a write failure — are the
	// host's, and come back as its error.
	if err := ValidateCustomModelSpec(spec); err != nil {
		return errorResponse(cmd.ID, CmdAddCustomModel, err.Error())
	}
	result, err := s.host.AddCustomModel(spec)
	if err != nil {
		return errorResponse(cmd.ID, CmdAddCustomModel, err.Error())
	}
	return successResponse(cmd.ID, CmdAddCustomModel, result)
}

// ValidateCustomModelSpec checks the fields that make the command
// malformed rather than the write failing: the required ones, the
// baseUrl's shape, the provider id's fitness as a models.json key (it is
// also set_model's "provider/" part — a "/" in it would be
// unselectable), and the dialect (only the two registered ones, F2).
//
// It is exported because the host runs the same rules on its own entry
// point (cmd/ultrac's AddCustomModel): a direct caller — the P6
// persistence test — must get the same refusal the protocol gives, from
// one source of truth rather than a second copy that can drift.
func ValidateCustomModelSpec(spec CustomModelSpec) error {
	if spec.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.ContainsAny(spec.Provider, "/") || strings.ContainsFunc(spec.Provider, unicode.IsSpace) {
		return fmt.Errorf("provider %q must not contain %q or whitespace", spec.Provider, "/")
	}
	if spec.BaseURL == "" {
		return fmt.Errorf("baseUrl is required")
	}
	u, err := url.Parse(spec.BaseURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("baseUrl %q must be an http(s) URL with a host", spec.BaseURL)
	}
	if spec.APIKey == "" {
		return fmt.Errorf("apiKey is required")
	}
	if spec.ModelID == "" {
		return fmt.Errorf("modelId is required")
	}
	switch spec.API {
	case string(ai.APIOpenAICompletions), string(ai.APIAnthropicMessages):
	default:
		return fmt.Errorf("api %q is not a supported dialect (want %q or %q)",
			spec.API, ai.APIOpenAICompletions, ai.APIAnthropicMessages)
	}
	if spec.ContextWindow <= 0 {
		return fmt.Errorf("contextWindow must be > 0")
	}
	if spec.MaxTokens <= 0 {
		return fmt.Errorf("maxTokens must be > 0")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Model and thinking
// ---------------------------------------------------------------------------

func (s *Server) cmdSetModel(cmd RpcCommand) RpcResponse {
	model, err := s.host.SetModel(cmd.Provider, cmd.ModelID)
	if err != nil {
		return errorResponse(cmd.ID, CmdSetModel, err.Error())
	}
	return successResponse(cmd.ID, CmdSetModel, model)
}

func (s *Server) cmdGetAvailableModels(cmd RpcCommand) RpcResponse {
	return successResponse(cmd.ID, CmdGetAvailableModels, map[string]any{
		"models": s.host.AvailableModels(),
	})
}

func (s *Server) cmdSetThinkingLevel(cmd RpcCommand) RpcResponse {
	// pi clamps the requested level against the model's supported levels
	// rather than rejecting it (agent-session.ts:2638-2639); the clamp
	// itself is ai.ClampThinkingLevel, ported from models.ts:1241-1260.
	level := ai.ClampThinkingLevel(s.host.ThinkingLevels(), ai.ModelThinkingLevel(cmd.Level))
	s.host.SetThinkingLevel(agent.ThinkingLevel(level))
	return successResponse(cmd.ID, CmdSetThinkingLevel, nil)
}

func (s *Server) cmdGetAvailableThinkingLevels(cmd RpcCommand) RpcResponse {
	levels := s.host.ThinkingLevels()
	out := make([]string, len(levels))
	for i, l := range levels {
		out[i] = string(l)
	}
	return successResponse(cmd.ID, CmdGetAvailableThinking, map[string]any{"levels": out})
}

func (s *Server) cmdSetQueueMode(cmd RpcCommand, steering bool) RpcResponse {
	var mode agent.QueueMode
	switch cmd.Mode {
	case QueueModeAll:
		mode = agent.QueueAll
	case QueueModeOneAtATime:
		mode = agent.QueueOneAtATime
	default:
		name := CmdSetFollowUpMode
		if steering {
			name = CmdSetSteeringMode
		}
		return errorResponse(cmd.ID, name,
			"mode must be 'all' or 'one-at-a-time'")
	}
	name := CmdSetFollowUpMode
	if steering {
		name = CmdSetSteeringMode
		s.host.Agent().SetSteeringMode(mode)
	} else {
		s.host.Agent().SetFollowUpMode(mode)
	}
	return successResponse(cmd.ID, name, nil)
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// startRun launches one prompt run in the background. The response has
// already been written by the time this is called (handleLine).
func (s *Server) startRun(in PromptInput) {
	a := s.host.Agent()
	msgs := make([]ai.Message, 0, 2)
	if in.Patch != nil {
		msgs = append(msgs, *in.Patch)
	}
	msgs = append(msgs, in.User)

	s.mu.Lock()
	s.runActive = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.runActive = false
			s.mu.Unlock()
		}()
		// Run-level failures arrive as events and State().ErrorMessage, not
		// as a return value (agent.Prompt's contract); the only error here
		// is the programming-mistake class (a concurrent run), which the
		// client already saw as a failed prompt.
		_ = a.Prompt(s.host.RunContext(), msgs...)
	}()
}

// rebind attaches the event pump to the current agent. Called at startup
// and after every NewSession.
func (s *Server) rebind() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsub != nil {
		s.unsub()
	}
	s.unsub = s.host.Agent().Subscribe(s.pump)
}

// pump is the event subscription: persist completed messages, then write
// every event as one line. It runs on the run's goroutine (listener
// dispatch), which is why the writer is mutex-protected.
func (s *Server) pump(_ context.Context, ev agent.AgentEvent) error {
	if e, ok := ev.(agent.EvMessageEnd); ok {
		s.host.Persist(e.Message)
	}
	return s.out.WriteEvent(ev)
}

// disposition wraps a disposition string the way pi's response data does.
func disposition(d string) map[string]any {
	return map[string]any{"disposition": d}
}

// messageTexts extracts the text of each queued message. Commands build
// single-text-block messages, so this is normally the message itself; the
// join keeps multi-block messages lossless.
func messageTexts(msgs []ai.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == ai.BlockText {
				sb.WriteString(b.Text)
			}
		}
		out = append(out, sb.String())
	}
	return out
}
