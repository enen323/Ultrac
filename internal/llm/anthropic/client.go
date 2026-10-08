// Package anthropic is the streaming client for the Messages API.
//
// # Scope
//
// This is T0.4: single request, streamed response, tool_use accumulated by
// index, retry with backoff, ctx cancellation. No conversation management, no
// compaction, no prompt assembly — those are T1 and T2. The package implements
// internal/contract/provider so the engine never learns which vendor it is
// talking to.
//
// # Why streaming is not an optimisation
//
// A streamed response arrives as a sequence of partial tool inputs. If the
// caller saw fragments, every consumer would have to reassemble them and could
// get it subtly wrong. Accumulating here means a Chunk with ChunkToolCall
// always carries parseable JSON, and that invariant is what lets the rest of
// the system treat a partial tool call as a bug rather than a case to handle.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"ultrac/internal/contract/provider"
	"ultrac/internal/logging"
)

// Kind is the registered provider kind.
const Kind = "anthropic"

// DefaultBaseURL is the first-party endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// defaultAPIVersion is the dated API version header.
//
// Pinned rather than floating: the SDKs pin it too, and a client that silently
// upgrades its API version is a client whose behaviour changes without a code
// change. Bump it deliberately.
const defaultAPIVersion = "2023-06-01"

// defaultMaxTokens is used when a Request leaves MaxTokens at zero.
//
// The API requires the field, so a zero cannot be sent as "unlimited". 4096
// is large enough for a tool call and small enough that a runaway generation
// cannot exhaust the user's quota in one turn.
const defaultMaxTokens = 4096

func init() {
	// Self-registration, so cmd/ultrac only needs a blank import. This mirrors
	// database/sql's Register: adding a vendor must not touch the engine.
	provider.Register(Kind, New)
}

// Client is a Messages API client.
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
	// sleep is injected so retry tests do not actually wait.
	sleep func(ctx context.Context, d time.Duration) error
	// randFloat supplies retry jitter.
	randFloat func() float64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. Use it to control timeouts, proxies or
// TLS — and in tests, to point at an httptest server.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithAPIVersion overrides the anthropic-version header.
func WithAPIVersion(v string) Option {
	return func(c *Client) {
		if v != "" {
			c.apiVersion = v
		}
	}
}

// WithBetas adds anthropic-beta feature flags.
func WithBetas(betas ...string) Option {
	return func(c *Client) { c.betas = append(c.betas, betas...) }
}

// WithMaxRetries caps retry attempts. Zero disables retrying.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithBackoff sets the base and ceiling of the exponential backoff.
func WithBackoff(base, max time.Duration) Option {
	return func(c *Client) {
		if base > 0 {
			c.retryBase = base
		}
		if max > 0 {
			c.retryMax = max
		}
	}
}

// WithLogger attaches a logger for request-level diagnostics.
func WithLogger(l *logging.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithBaseURL overrides the endpoint. This is how ANTHROPIC_BASE_URL reaches
// the client, and how a test points it at a local server.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// New builds a Client from a resolved provider Config.
func New(cfg provider.Config) (provider.Provider, error) {
	// NewClient(cfg), not NewClient(cfg, nil): passing an explicit nil to a
	// variadic parameter yields a one-element slice containing a nil Option,
	// which then panics when applied. Expanding the call is the fix.
	return NewClient(cfg)
}

// NewClient builds a Client with options. It is exported separately so a test
// or a tool can construct one without going through the registry.
func NewClient(cfg provider.Config, opts ...Option) (*Client, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("anthropic: no model configured")
	}

	c := &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		model:      cfg.Model,
		apiKey:     cfg.APIKey,
		authToken:  cfg.AuthToken,
		apiVersion: defaultAPIVersion,
		// 10 matches the TypeScript default. The cap matters: a permanent
		// failure that retries ten times turns one bad request into a minute
		// of the user's time and quota.
		maxRetries: 10,
		retryBase:  500 * time.Millisecond,
		retryMax:   32 * time.Second,
		// No Client.Timeout: a streamed response legitimately stays open for
		// minutes, and a whole-request timeout would kill healthy long
		// generations. Cancellation comes from the context instead.
		httpClient: &http.Client{},
		log:        logging.Default(),
		sleep:      sleepCtx,
		randFloat:  randFloat,
	}

	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	// ANTHROPIC_AUTH_TOKEN wins over ANTHROPIC_API_KEY, matching the SDK:
	// a proxy token takes precedence over a first-party key, because that is
	// the configuration a proxy user actually set up.
	if c.authToken != "" {
		c.apiKey = ""
	}

	for _, opt := range opts {
		// A nil Option is tolerated rather than fatal. It costs one branch
		// here and removes a whole class of nil-deref panic from every caller
		// that assembles options conditionally.
		if opt == nil {
			continue
		}
		opt(c)
	}

	if c.apiKey == "" && c.authToken == "" {
		// Not an error at construction: Bedrock and Vertex carry their
		// credentials elsewhere. The request path checks again and produces a
		// precise message there.
		c.log.Debug("no anthropic credential configured; requests will be sent unauthenticated")
	}
	return c, nil
}

// Name reports the configured instance name.
func (c *Client) Name() string { return Kind }

// Kind reports the registered implementation.
func (c *Client) Kind() string { return Kind }

// Stream runs one request and yields the response as chunks.
//
// The returned Seq2 is a plain func(yield func(Chunk, error) bool), which is
// what Go 1.23's iter.Seq2 is. That is deliberate: the consumer breaking out of
// the loop is the cancellation signal, so a frontend that stops reading cannot
// leave this blocked on a send, and no channel needs closing.
//
// Setup failures arrive as the first yielded error, never as a panic and never
// as a zero-value chunk. A caller iterating this cannot distinguish "no more
// data" from "it went wrong" unless the error arrives on the same channel.
//
// Retries happen only before the first chunk is emitted. Once a caller has seen
// text, replaying would duplicate output it already rendered, so a mid-stream
// failure is terminal: the error is yielded after whatever arrived.
func (c *Client) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Chunk, error] {
	return func(yield func(provider.Chunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(provider.Chunk{}, err)
			return
		}

		body, err := c.encodeRequest(req)
		if err != nil {
			yield(provider.Chunk{}, err)
			return
		}

		for attempt := 0; ; attempt++ {
			if err := ctx.Err(); err != nil {
				yield(provider.Chunk{}, err)
				return
			}

			emitted := false
			err = c.attempt(ctx, body, yield, &emitted)
			if err == nil {
				return
			}

			// Mid-stream failures are terminal: the caller already rendered
			// what arrived, so replaying would show them the same text twice.
			if emitted {
				yield(provider.Chunk{}, err)
				return
			}

			apiErr, ok := logging.AsAPIError(err)
			if !ok || !c.shouldRetry(apiErr, attempt) {
				yield(provider.Chunk{}, err)
				return
			}

			delay := c.retryDelay(attempt, apiErr)
			c.log.Warn("retrying anthropic request",
				logging.F("attempt", attempt+1),
				logging.F("status", apiErr.StatusCode),
				logging.F("delayMs", delay.Milliseconds()))

			// A cancelled context during the backoff must abort immediately, not
			// after the remaining delay: a 30-second wait is a 30-second wait
			// from the user's point of view.
			if err := c.sleep(ctx, delay); err != nil {
				yield(provider.Chunk{}, err)
				return
			}
		}
	}
}

// attempt performs one HTTP round trip.
//
// emitted is set as soon as anything is yielded, so the caller can tell a
// pre-stream failure (retryable) from a mid-stream one (terminal).
func (c *Client) attempt(
	ctx context.Context,
	body []byte,
	yield func(provider.Chunk, error) bool,
	emitted *bool,
) error {
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer func() {
		// Drain before close so the connection can be reused. Without this,
		// a partially-read body leaves the socket unusable and the next
		// request pays for a fresh TCP and TLS handshake.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return c.readErrorResponse(resp)
	}

	acc := newAccumulator()
	scanner := bufio.NewScanner(resp.Body)
	// A single SSE line holds one JSON event. A large input_json_delta for a
	// big tool argument can be hundreds of kilobytes, well past bufio's 64KB
	// default, which would fail mid-tool-call.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		raw, ok := sseData(line)
		if !ok {
			continue
		}

		res, err := acc.consume(raw)
		if err != nil {
			return err
		}
		// Stop before usage: a consumer finalises token accounting when it
		// sees the reason generation ended, and reversing that order means the
		// accounting is attributed after the turn is already closed.
		if res.Stop != "" {
			*emitted = true
			if !yield(provider.Chunk{Kind: provider.ChunkStop, Stop: res.Stop}, nil) {
				return nil
			}
		}
		if res.Usage != nil {
			*emitted = true
			if !yield(provider.Chunk{Kind: provider.ChunkUsage, Usage: res.Usage}, nil) {
				return nil
			}
		}
		for _, ch := range res.Chunks {
			*emitted = true
			if !yield(ch, nil) {
				return nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// A cancelled context shows up here rather than at the HTTP call:
		// reading the body is where cancellation lands.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("anthropic: read stream: %w", err)
	}

	// The stream ended without a message_stop, or with an unclosed block.
	// Reporting that is what stops a truncated answer from looking complete.
	// Checked only after a clean end-of-body, because a scanner error has
	// already explained itself.
	return acc.finish()
}

func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	url := c.baseURL + "/v1/messages"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", c.apiVersion)
	if len(c.betas) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(c.betas, ","))
	}
	// Bearer when a proxy token is configured, x-api-key for a first-party
	// key. Sending both makes some proxies reject the request outright.
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	} else if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &logging.APIError{Err: err, Message: "connection failed"}
	}
	return resp, nil
}

// readErrorResponse turns a non-200 into a typed APIError.
//
// The body is parsed for the server's own error code and message rather than
// being flattened into text, because the retry policy and the user-facing
// surface both branch on them.
func (c *Client) readErrorResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	apiErr := &logging.APIError{
		StatusCode:        resp.StatusCode,
		RequestID:         resp.Header.Get("request-id"),
		RetryAfterSeconds: parseRetryAfter(resp.Header.Get("Retry-After")),
	}

	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Error.Message != "" {
		apiErr.Code = payload.Error.Type
		apiErr.Message = payload.Error.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	return apiErr
}

func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 86400 {
			return 0
		}
	}
	return n
}

// shouldRetry decides whether a failure is worth another attempt.
//
// Two deliberate exclusions:
//
//   - Anything with a client 4xx other than 408/409/429. A 400 is wrong the
//     same way next time; retrying burns quota to reproduce it.
//   - Non-transport errors. A marshalling failure will fail identically
//     forever, and retrying it hides a bug behind a slow timeout.
//
// 429 is retryable here even for subscribers, unlike the TypeScript side, which
// gates it on subscription type. This client has no subscription concept — it
// is configured with a base URL and a key — so there is nothing to gate on. A
// caller that needs the gate should wrap this rather than have it guess.
func (c *Client) shouldRetry(e *logging.APIError, attempt int) bool {
	if attempt >= c.maxRetries {
		return false
	}
	return e.Temporary()
}

// retryDelay computes the wait before the next attempt.
//
// A server-supplied Retry-After wins outright. It is a directive, not advice:
// backing off on our own schedule after being told otherwise is how a rate
// limit turns into a ban.
func (c *Client) retryDelay(attempt int, e *logging.APIError) time.Duration {
	if e.RetryAfterSeconds > 0 {
		return time.Duration(e.RetryAfterSeconds) * time.Second
	}
	delay := c.retryBase << attempt
	if delay > c.retryMax || delay <= 0 {
		delay = c.retryMax
	}
	// Jitter. Without it, every client that hit the same 429 retries in
	// lockstep and reproduces the overload it is backing off from.
	jitter := time.Duration(c.randFloat() * float64(delay) * 0.25)
	return delay + jitter
}

// sleepCtx waits unless the context ends first.
//
// The select matters more than the sleep: a retry backoff of 30 seconds must
// abort the moment the user presses escape, not 30 seconds later.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// randFloat returns the jitter source, wrapping jitterFloat so Client can hold
// a plain func for test injection.
func randFloat() float64 {
	v, ok := jitterFloat()
	if !ok {
		return 0
	}
	return v
}
