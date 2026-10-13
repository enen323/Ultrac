package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ultrac/internal/logging"
)

// apiError is a failure reported inside the SSE stream (an `error` event).
//
// It carries the provider's error type as Code, mirroring what
// logging.APIError does for HTTP failures, so both paths classify the same
// way (a 529-style overload arriving mid-stream is still an overload).
type apiError struct {
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return "anthropic: stream error" + orCode(e.Code)
	}
	return "anthropic: stream error" + orCode(e.Code) + ": " + e.Message
}

func orCode(code string) string {
	if code == "" {
		return ""
	}
	return " (" + code + ")"
}

func (c *Client) post(ctx context.Context, body []byte, opts Options) (*http.Response, error) {
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
	for name, value := range opts.Headers {
		req.Header.Set(name, value)
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
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
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
// 429 is retryable here even for subscribers, unlike the TypeScript side,
// which gates it on subscription type. This client has no subscription
// concept — it is configured with a base URL and a key — so there is nothing
// to gate on. A caller that needs the gate should wrap this rather than have
// it guess.
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
