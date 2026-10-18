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
// get_commands.
//
// Not implemented (rpc-types.ts has them; this port does not): compact,
// set_auto_compaction, set_auto_retry, abort_retry, bash, abort_bash,
// export_html, switch_session, fork, clone, get_fork_messages, get_tree,
// get_session_stats, cycle_model, cycle_thinking_level, and the
// extension_ui_* pair. They fall through to the unknown-command branch
// with a clear error rather than being silently dropped — compaction and
// the branch tree are non-goals (plan §2.2 / §3.5), and a client that
// sends one learns it is unsupported instead of waiting for a response
// that never comes.
package rpc

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

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

func (s *Server) cmdGetState(cmd RpcCommand) RpcResponse {
	a := s.host.Agent()
	state := a.State()
	mgr := s.host.Session()
	model := state.Model
	return successResponse(cmd.ID, CmdGetState, RpcSessionState{
		Model:                 &model,
		ThinkingLevel:         string(state.ThinkingLevel),
		IsStreaming:           state.IsStreaming,
		IsCompacting:          false, // compaction is a non-goal (plan §2.2)
		SteeringMode:          string(a.SteeringMode()),
		FollowUpMode:          string(a.FollowUpMode()),
		SessionFile:           mgr.SessionFile(),
		SessionID:             mgr.SessionID(),
		SessionName:           mgr.SessionName(),
		AutoCompactionEnabled: false, // ditto
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
	// leafId is null, not "", before any entry (pi: leafId: string | null).
	var leaf any
	if id := mgr.GetLeafID(); id != "" {
		leaf = id
	}
	return successResponse(cmd.ID, CmdGetEntries, map[string]any{
		"entries": entries,
		"leafId":  leaf,
	})
}

func (s *Server) cmdGetCommands(cmd RpcCommand) RpcResponse {
	// Empty on purpose: this port has no extensions, prompt templates or
	// skills to register (rpc-mode.ts:680-711 lists all three sources).
	// Returning the empty list is the honest answer; a client polling for
	// slash commands learns there are none.
	return successResponse(cmd.ID, CmdGetCommands, map[string]any{"commands": []any{}})
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
