// Package anthropic is the Anthropic Messages API adapter of the pi harness
// port: it turns a normalized transcript into a POST /v1/messages stream and
// translates the SSE payloads into the 12-event ai.AssistantEvent protocol
// (contract C2).
//
// # Provenance
//
// Migrated from internal/llm/anthropic (T0/T1) and upgraded from the old
// 5-kind Chunk contract to pi's event granularity. The pieces that were
// already proven are kept verbatim in spirit — SSE framing, tool arguments
// accumulated by content-block index, retries only before the first event,
// context-cancellable backoff — see the notes on each. What changed is the
// output shape: a consumer of this package sees pi's events, not chunks.
//
// # The 12-event translation
//
// Anthropic's stream is block-indexed: content_block_start opens a block, N
// deltas grow it, content_block_stop closes it. pi's protocol is
// block-positional: every event carries the index of the block within the
// assistant message being built. The translator keeps one blockState per
// provider index, appended to the output message in arrival order, so the
// contentIndex of an event is the block's position in that message.
//
// Tool arguments are the reason this is careful: input_json_delta carries
// fragments that are only valid JSON once concatenated, so fragments
// accumulate in a string and only content_block_stop emits a complete
// EvToolCallEnd. Decoding each fragment as it arrives would be O(n²) over a
// large write and would fail on every intermediate prefix.
//
// # Failure encoding
//
// stream.go never throws: every failure — encode, HTTP, protocol, truncation —
// becomes a terminal EvError inside the stream (pi: anthropic-messages.ts
// :887-897). A truncated stream is reported, never silently accepted: a
// stream that ends without message_start, without a stop reason, or with an
// unclosed block is an error, because a half answer that looks complete is
// worse than a visible failure.
package anthropic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/logging"
)

// DefaultBaseURL is the first-party endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// defaultAPIVersion is the dated API version header.
//
// Pinned rather than floating: the SDKs pin it too, and a client that silently
// upgrades its API version is a client whose behaviour changes without a code
// change. Bump it deliberately.
const defaultAPIVersion = "2023-06-01"

// InterleavedThinkingBeta lets thinking blocks appear between tool calls on
// models that support it; requested whenever thinking is enabled.
//
// pi: anthropic-messages.ts:191 (INTERLEAVED_THINKING_BETA).
const InterleavedThinkingBeta = "interleaved-thinking-2025-05-14"

// defaultMaxTokens is used when neither the request nor the model supplies a
// cap.
//
// The API requires the field, so a zero cannot be sent as "unlimited". 4096
// is large enough for a tool call and small enough that a runaway generation
// cannot exhaust the user's quota in one turn.
const defaultMaxTokens = 4096

func init() {
	// Self-registration under the api dialect, so a host needs only a blank
	// import. Keyed by Api rather than provider id: this one adapter also
	// serves DeepSeek's anthropic-compatible endpoint and any proxy speaking
	// the same dialect, exactly as pi's registry does.
	ai.Register(ai.ApiProvider{API: ai.APIAnthropicMessages, Stream: Stream})
}

// Stream is the registry entry point (ai.StreamFn). It builds a per-request
// client from the model and options — pi does the same (createClient per call,
// anthropic-messages.ts:627) — and delegates to it.
func Stream(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	return StreamWith(ctx, model, tc, Options{StreamOptions: opts})
}

// StreamWith is Stream with Anthropic-specific options.
func StreamWith(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts Options) ai.Stream {
	client, err := New(Config{
		Model:      model,
		APIKey:     opts.APIKey,
		AuthToken:  opts.AuthToken,
		BaseURL:    firstNonEmpty(opts.BaseURL, model.BaseURL),
		Betas:      opts.Betas,
		MaxRetries: opts.MaxRetries,
		HTTPClient: opts.HTTPClient,
		Logger:     opts.Logger,
		Sleep:      opts.Sleep,
		RandFloat:  opts.RandFloat,
	})
	if err != nil {
		return errorStream(model, err)
	}
	return client.stream(ctx, model, tc, opts)
}

// errorStream returns a stream that has already failed: used for failures
// that happen before a request exists (unknown model, no credential shape).
// The contract forbids returning nil, so the failure travels as EvError.
func errorStream(model ai.Model, err error) ai.Stream {
	s := ai.NewEventStream()
	msg := newAssistantMessage(model)
	msg.StopReason = ai.StopError
	msg.ErrorMessage = err.Error()
	s.Push(ai.EvError{Reason: ai.StopError, Error: msg})
	s.End()
	return s
}

// Options extends the frozen ai.StreamOptions with Anthropic-specific knobs.
//
// The split mirrors pi (AnthropicOptions extends StreamOptions): the contract
// layer carries what every provider agrees on; everything else lives here so
// the frozen contract does not grow a vendor field per feature.
type Options struct {
	ai.StreamOptions

	// AuthToken is sent as `Authorization: Bearer`. It wins over APIKey when
	// both are set, matching the Anthropic SDK: a proxy token takes
	// precedence over a first-party key, because that is the configuration a
	// proxy user actually set up.
	AuthToken string
	// BaseURL overrides the model's baseUrl. This is how
	// ANTHROPIC_BASE_URL reaches the client.
	BaseURL string
	// Betas are extra anthropic-beta feature flags, appended after the ones
	// the adapter derives (e.g. interleaved thinking).
	Betas []string
	// Headers are extra request headers; an `anthropic-beta` header here
	// replaces the derived beta list entirely (pi: getBetaFeatures).
	Headers map[string]string
	// SendCacheControl adds cache_control ephemeral markers to the system
	// prompt, the tool list and the last message. Off by default: the
	// endpoints this adapter talks to are not guaranteed to accept it, and a
	// 400 on a cache marker is a confusing way to learn that. pi defaults it
	// on (retention "short"); we make it opt-in until the catalog records
	// per-model support.
	SendCacheControl bool
	// HTTPClient overrides the default (no Timeout: a streamed response
	// legitimately stays open for minutes; cancellation comes from ctx).
	HTTPClient *http.Client
	// Logger receives request-level diagnostics.
	Logger *logging.Logger
	// Sleep and RandFloat are injection points for retry tests.
	Sleep     func(ctx context.Context, d time.Duration) error
	RandFloat func() float64
}

// Config resolves one Client.
type Config struct {
	Model      ai.Model
	APIKey     string
	AuthToken  string
	BaseURL    string
	Betas      []string
	MaxRetries int

	HTTPClient *http.Client
	Logger     *logging.Logger
	Sleep      func(ctx context.Context, d time.Duration) error
	RandFloat  func() float64
}

// Client is a configured Messages API client. It is safe for concurrent use.
type Client struct {
	baseURL    string
	model      string
	apiKey     string
	authToken  string
	apiVersion string
	betas      []string
	maxRetries int
	retryBase  time.Duration
	retryMax   time.Duration

	httpClient *http.Client
	log        *logging.Logger
	sleep      func(ctx context.Context, d time.Duration) error
	randFloat  func() float64
}

// New builds a Client from a resolved Config.
func New(cfg Config) (*Client, error) {
	if cfg.Model.ID == "" {
		return nil, fmt.Errorf("anthropic: no model configured")
	}

	c := &Client{
		// The endpoint resolves as Config.BaseURL → Model.BaseURL →
		// DefaultBaseURL. Skipping the middle step is a footgun: a client
		// built from a model whose baseUrl points at a proxy would silently
		// talk to the first-party API instead.
		baseURL:    firstNonEmpty(strings.TrimRight(cfg.BaseURL, "/"), strings.TrimRight(cfg.Model.BaseURL, "/")),
		model:      cfg.Model.ID,
		apiKey:     cfg.APIKey,
		authToken:  cfg.AuthToken,
		apiVersion: defaultAPIVersion,
		betas:      append([]string(nil), cfg.Betas...),
		// Provider-level retries default to off, following pi
		// (provider-retry.ts:111 — `maxRetries ?? 0`); the retry *policy*
		// lives in settings and the loop passes it in. The boundary rule
		// below (never after the first event) is the part that must not
		// regress.
		maxRetries: cfg.MaxRetries,
		retryBase:  500 * time.Millisecond,
		retryMax:   32 * time.Second,
		// No Client.Timeout: a streamed response legitimately stays open
		// for minutes, and a whole-request timeout would kill healthy
		// long generations. Cancellation comes from the context instead.
		httpClient: cfg.HTTPClient,
		log:        cfg.Logger,
		sleep:      cfg.Sleep,
		randFloat:  cfg.RandFloat,
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.log == nil {
		c.log = logging.Default()
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.randFloat == nil {
		c.randFloat = randFloat
	}
	// A Bearer token wins over a first-party key (see Options.AuthToken).
	if c.authToken != "" {
		c.apiKey = ""
	}
	return c, nil
}

// stream runs one request. It returns immediately with an ai.Stream; the
// request runs in a goroutine that pushes events and always terminates the
// stream with done or error.
func (c *Client) stream(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts Options) ai.Stream {
	s := ai.NewEventStream()
	go func() {
		defer func() {
			// A panic in a provider must not take the process down: the
			// contract says failures are encoded in the stream. This is a
			// backstop for genuine bugs, not an error path.
			if r := recover(); r != nil {
				s.Push(errorEvent(ctx, model, fmt.Errorf("anthropic: panic: %v", r)))
			}
			s.End()
		}()
		c.run(ctx, s, model, tc, opts)
	}()
	return s
}

// run performs the request with retries. Retries happen only while nothing
// has been pushed: once the consumer has seen an event, replaying would
// duplicate output it already rendered, so a mid-stream failure is terminal.
func (c *Client) run(ctx context.Context, s *ai.EventStream, model ai.Model, tc ai.TranscriptContext, opts Options) {
	body, err := c.encodeRequest(model, tc, opts)
	if err != nil {
		s.Push(errorEvent(ctx, model, err))
		return
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			s.Push(errorEvent(ctx, model, err))
			return
		}

		emitted, err := c.attempt(ctx, s, body, model, opts)
		if err == nil {
			return
		}

		// Mid-stream failures are terminal.
		if emitted {
			s.Push(errorEvent(ctx, model, err))
			return
		}

		apiErr, ok := logging.AsAPIError(err)
		if !ok || !c.shouldRetry(apiErr, attempt) {
			s.Push(errorEvent(ctx, model, err))
			return
		}

		delay := c.retryDelay(attempt, apiErr)
		c.log.Warn("retrying anthropic request",
			logging.F("attempt", attempt+1),
			logging.F("status", apiErr.StatusCode),
			logging.F("delayMs", delay.Milliseconds()))

		// A cancelled context during the backoff must abort immediately,
		// not after the remaining delay: a 30-second wait is a 30-second
		// wait from the user's point of view.
		if err := c.sleep(ctx, delay); err != nil {
			s.Push(errorEvent(ctx, model, err))
			return
		}
	}
}

// attempt performs one HTTP round trip, pushing events into s as they are
// translated. It reports whether anything was pushed (so the caller can tell
// a pre-stream failure, retryable, from a mid-stream one, terminal) and a
// non-nil error when the round trip did not reach a clean end.
func (c *Client) attempt(
	ctx context.Context,
	s *ai.EventStream,
	body []byte,
	model ai.Model,
	opts Options,
) (bool, error) {
	resp, err := c.post(ctx, body, opts)
	if err != nil {
		return false, err
	}
	defer func() {
		// Drain before close so the connection can be reused. Without
		// this, a partially-read body leaves the socket unusable and the
		// next request pays for a fresh TCP and TLS handshake.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return false, c.readErrorResponse(resp)
	}

	tr := newTranslator(model)
	emitted := false
	push := func(ev ai.AssistantEvent) {
		emitted = true
		s.Push(ev)
	}

	// consumeStream pushes its own terminal event in every case — done on a
	// clean end, error on any failure including truncation.
	tr.consumeStream(ctx, resp.Body, push)
	return emitted, nil
}

// errorEvent builds the terminal EvError for err. A cancelled context is
// reported as aborted rather than error, so callers can tell "the user
// stopped this" from "this failed" (pi: anthropic-messages.ts:893).
func errorEvent(ctx context.Context, model ai.Model, err error) ai.EvError {
	reason := ai.StopError
	if ctx.Err() != nil {
		reason = ai.StopAborted
	}
	msg := newAssistantMessage(model)
	msg.StopReason = reason
	msg.ErrorMessage = err.Error()
	return ai.EvError{Reason: reason, Error: msg}
}

// newAssistantMessage builds the assistant message every event's partial and
// every terminal message starts from (pi: anthropic-messages.ts:582-599).
//
// TimestampMS is set at construction, not at termination: pi stamps
// `timestamp: Date.now()` when the message is created, and EventStream uses
// it to decide whether a terminal message predates the stream (a message
// with a zero timestamp would never get its durationMs filled in).
func newAssistantMessage(model ai.Model) ai.Message {
	return ai.Message{
		Role:        ai.RoleAssistant,
		API:         string(model.API),
		Provider:    string(model.Provider),
		Model:       model.ID,
		Usage:       &ai.Usage{},
		StopReason:  ai.StopPending,
		TimestampMS: time.Now().UnixMilli(),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
