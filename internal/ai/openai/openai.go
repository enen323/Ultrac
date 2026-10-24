// Package openai is the OpenAI-compatible Chat Completions adapter of the pi
// harness port: it turns a normalized transcript into a
// POST /chat/completions stream and translates the SSE chunks into the
// 12-event ai.AssistantEvent protocol (contract C2).
//
// # Why one adapter covers many providers
//
// "OpenAI-compatible" is a dialect, not a vendor: DeepSeek, Moonshot, xAI,
// OpenRouter, vLLM, Ollama and OpenAI itself all speak it with local
// variations. This adapter ports pi's openai-completions.ts with the compat
// matrix compressed to the fields that change behaviour for the endpoints
// this project actually targets (OpenAI itself and DeepSeek, the latter
// verified against the real API in P1). The full matrix — zai/qwen/together
// thinking formats, OpenRouter routing, vLLM priority — is deliberately not
// ported; model.compat overrides exist for when one of those is needed.
//
// # Streaming shape
//
// The dialect streams choices[0].delta fragments: content text, reasoning
// text (under one of three field names, since endpoints disagree), and
// tool_calls entries keyed by a stream index that may arrive before the
// call's id or name. Blocks are created lazily on first sight and closed in
// order at end of stream, which is what makes toolcall_start carry an id the
// later deltas then fill in.
package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/logging"
)

// DefaultBaseURL is OpenAI's first-party endpoint.
const DefaultBaseURL = "https://api.openai.com"

func init() {
	ai.Register(ai.ApiProvider{API: ai.APIOpenAICompletions, Stream: Stream})
}

// Stream is the registry entry point (ai.StreamFn). Like the anthropic
// adapter, it builds a per-request client from the model and options
// (pi: openai-completions.ts:349-357).
func Stream(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	return StreamWith(ctx, model, tc, Options{StreamOptions: opts})
}

// StreamWith is Stream with OpenAI-specific options.
//
// model.Headers rides along into the client's Config: they are the
// provider- and model-level headers the merged catalog baked onto the
// entry (models.json's headers fields), and Config.Headers is also where
// a header-owned credential (Authorization) satisfies New's
// "key or auth header" check. A per-request Options.Headers still wins,
// because post() applies it after the client's headers.
func StreamWith(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts Options) ai.Stream {
	client, err := New(Config{
		Model:      model,
		APIKey:     opts.APIKey,
		BaseURL:    firstNonEmpty(opts.BaseURL, model.BaseURL),
		Headers:    model.Headers,
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

// errorStream returns a stream that has already failed, for failures that
// happen before a request exists.
func errorStream(model ai.Model, err error) ai.Stream {
	s := ai.NewEventStream()
	msg := newAssistantMessage(model)
	msg.StopReason = ai.StopError
	msg.ErrorMessage = err.Error()
	s.Push(ai.EvError{Reason: ai.StopError, Error: msg})
	s.End()
	return s
}

// Options extends the frozen ai.StreamOptions with OpenAI-specific knobs.
type Options struct {
	ai.StreamOptions

	// BaseURL overrides the model's baseUrl. This is how
	// OPENAI_BASE_URL reaches the client.
	BaseURL string
	// Headers are extra request headers (e.g. OpenRouter routing hints).
	Headers map[string]string
	// HTTPClient overrides the default (no Timeout: streamed responses stay
	// open for minutes; cancellation comes from ctx).
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
	BaseURL    string
	MaxRetries int
	// Headers may carry the credential (authorization) instead of APIKey,
	// which is how OpenRouter-style header-owned auth reaches the client.
	Headers map[string]string

	HTTPClient *http.Client
	Logger     *logging.Logger
	Sleep      func(ctx context.Context, d time.Duration) error
	RandFloat  func() float64
}

// Client is a configured Chat Completions client. It is safe for concurrent use.
type Client struct {
	baseURL    string
	model      string
	apiKey     string
	headers    map[string]string
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
		return nil, fmt.Errorf("openai: no model configured")
	}
	if cfg.APIKey == "" && !hasAuthHeader(cfg.Headers) {
		return nil, fmt.Errorf("openai: no API key configured")
	}

	c := &Client{
		// Config.BaseURL → Model.BaseURL → DefaultBaseURL. Skipping the
		// middle step would send a proxy-configured model to OpenAI.
		baseURL: firstNonEmpty(strings.TrimRight(cfg.BaseURL, "/"), strings.TrimRight(cfg.Model.BaseURL, "/")),
		model:   cfg.Model.ID,
		apiKey:  cfg.APIKey,
		headers: cfg.Headers,
		// Provider-level retries default to off, following pi
		// (provider-retry.ts:111 — `maxRetries ?? 0`).
		maxRetries: cfg.MaxRetries,
		retryBase:  500 * time.Millisecond,
		retryMax:   32 * time.Second,
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
	return c, nil
}

// hasAuthHeader reports whether a header carries the credential, so a
// header-owned-auth configuration does not need a redundant key
// (pi: hasRequestAuth, openai-completions.ts:78-85).
func hasAuthHeader(headers map[string]string) bool {
	for name, value := range headers {
		if value == "" {
			continue
		}
		switch strings.ToLower(name) {
		case "authorization", "cf-aig-authorization", "x-api-key":
			return true
		}
	}
	return false
}

// stream runs one request, returning immediately with an ai.Stream; the
// request runs in a goroutine that always terminates the stream.
func (c *Client) stream(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts Options) ai.Stream {
	s := ai.NewEventStream()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Push(errorEvent(ctx, model, fmt.Errorf("openai: panic: %v", r)))
			}
			s.End()
		}()
		c.run(ctx, s, model, tc, opts)
	}()
	return s
}

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
		// attempt pushes the stream's terminal event itself; a non-nil
		// error means the round trip failed before any event, which is the
		// only retryable case (replaying after events would duplicate
		// output the consumer already rendered).
		err := c.attempt(ctx, s, body, model, opts)
		if err == nil {
			return
		}
		apiErr, ok := logging.AsAPIError(err)
		if !ok || !c.shouldRetry(apiErr, attempt) {
			s.Push(errorEvent(ctx, model, err))
			return
		}

		delay := c.retryDelay(attempt, apiErr)
		c.log.Warn("retrying openai request",
			logging.F("attempt", attempt+1),
			logging.F("status", apiErr.StatusCode),
			logging.F("delayMs", delay.Milliseconds()))
		if err := c.sleep(ctx, delay); err != nil {
			s.Push(errorEvent(ctx, model, err))
			return
		}
	}
}

// attempt performs one HTTP round trip. It returns nil after pushing the
// stream's terminal event (done or error), and a non-nil error only when the
// round trip failed before any event — the retryable case.
func (c *Client) attempt(
	ctx context.Context,
	s *ai.EventStream,
	body []byte,
	model ai.Model,
	opts Options,
) error {
	resp, err := c.post(ctx, body, opts)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = drainLimit(resp.Body, 64*1024)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return c.readErrorResponse(resp)
	}

	tr := newTranslator(model)
	tr.consumeStream(ctx, resp.Body, func(ev ai.AssistantEvent) { s.Push(ev) })
	return nil
}

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
