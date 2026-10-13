package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ultrac/internal/logging"
)

// sseData extracts the payload of one SSE line.
//
// Twin of the anthropic adapter's sseData: both dialects frame events the
// same way (`data: <json>`, with the event name redundant). The duplication
// is deliberate for now — each adapter stays independently reviewable, the
// way pi's per-adapter iterators are — and is a P7 extraction candidate if a
// third provider appears.
func sseData(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	value := bytes.TrimPrefix(trimmed, []byte("data:"))
	value = bytes.TrimPrefix(value, []byte(" "))

	// The dialect's end-of-stream sentinel.
	if len(bytes.TrimSpace(value)) == 0 {
		return nil, false
	}
	return value, true
}

func (c *Client) post(ctx context.Context, body []byte, opts Options) (*http.Response, error) {
	url := c.baseURL + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
	for name, value := range opts.Headers {
		req.Header.Set(name, value)
	}
	// A key configured on the client is sent as Bearer; a header-owned
	// credential (authorization) already arrived above and wins.
	if c.apiKey != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// readErrorResponse turns a non-200 into a typed APIError, parsing the
// dialect's error body ({error: {message, type, code}}).
func (c *Client) readErrorResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	apiErr := &logging.APIError{
		StatusCode:        resp.StatusCode,
		RequestID:         resp.Header.Get("x-request-id"),
		RetryAfterSeconds: parseRetryAfter(resp.Header.Get("Retry-After")),
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
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
	// The dialect may send a delay in milliseconds as retry-after-ms.
	if ms, ok := strings.CutPrefix(v, "ms="); ok {
		return parseRetryAfter(ms) / 1000
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

// shouldRetry decides whether a failure is worth another attempt: the same
// transient set the Anthropic SDK treats as retryable (408/409/429/5xx plus
// transport errors), and nothing else — a 400 is wrong the same way next
// time (pi: provider-retry.ts:25-37).
func (c *Client) shouldRetry(e *logging.APIError, attempt int) bool {
	if attempt >= c.maxRetries {
		return false
	}
	return e.Temporary()
}

// retryDelay computes the wait before the next attempt. A server-supplied
// Retry-After wins outright: backing off on our own schedule after being
// told otherwise is how a rate limit turns into a ban.
func (c *Client) retryDelay(attempt int, e *logging.APIError) time.Duration {
	if e.RetryAfterSeconds > 0 {
		return time.Duration(e.RetryAfterSeconds) * time.Second
	}
	delay := c.retryBase << attempt
	if delay > c.retryMax || delay <= 0 {
		delay = c.retryMax
	}
	jitter := time.Duration(c.randFloat() * float64(delay) * 0.25)
	return delay + jitter
}

// sleepCtx waits unless the context ends first: a 30-second backoff must
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

// randFloat wraps the crypto-backed jitter source so Client can hold a plain
// func for test injection.
func randFloat() float64 {
	v, ok := jitterFloat()
	if !ok {
		return 0
	}
	return v
}

// jitterFloat returns a value in [0,1) for retry jitter.
//
// crypto/rand rather than math/rand on purpose: jitter exists to
// de-synchronise clients that hit the same overload, and a seeded source
// makes every client back off in lockstep. If the entropy source fails there
// is no correct fallback, so the caller retries with no jitter — degraded,
// but never a predictable jitter.
func jitterFloat() (float64, bool) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, false
	}
	v := binary.BigEndian.Uint64(b[:]) >> 11
	return float64(v) / float64(uint64(1)<<53), true
}

func drainLimit(r io.Reader, n int64) (int64, error) {
	return io.Copy(io.Discard, io.LimitReader(r, n))
}
