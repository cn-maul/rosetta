// Package httpx provides a small HTTP helper with bounded retries and
// exponential backoff for transport errors and retryable status codes.
package httpx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Call describes one HTTP request. Body is re-invoked on every retry
// attempt so the payload can be rebuilt cheaply.
type Call struct {
	Method string
	URL    string
	Header http.Header
	Body   func() ([]byte, error) // nil for bodyless requests
}

// Client wraps *http.Client with retries on transport errors and retryable
// status codes (408/429/500/502/503/504), honoring the Retry-After header.
// It never sets a global timeout: callers control deadlines via context.
type Client struct {
	HTTP       *http.Client
	MaxRetries int
	Base       time.Duration // initial backoff, doubled per attempt
	Cap        time.Duration // backoff ceiling
	Logger     *slog.Logger
}

// New returns a Client with production defaults.
func New() *Client {
	return &Client{
		HTTP:       &http.Client{},
		MaxRetries: 2,
		Base:       400 * time.Millisecond,
		Cap:        8 * time.Second,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Do executes the call, retrying as configured. It returns the first
// response that is not retryable, or the last response/error once retries
// are exhausted. The caller owns the returned response body.
func (c *Client) Do(ctx context.Context, call *Call) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, call)
		if err == nil && !RetryableStatus(resp.StatusCode) {
			return resp, nil
		}
		if attempt >= c.MaxRetries || ctx.Err() != nil {
			return resp, err
		}
		var wait time.Duration
		if resp != nil {
			wait = c.backoff(attempt)
			if ra := retryAfter(resp.Header.Get("Retry-After")); ra > wait {
				wait = ra
			}
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
			drain(resp)
		} else {
			wait = c.backoff(attempt)
		}
		c.log("retrying request", "url", call.URL, "attempt", attempt+1, "wait", wait.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (c *Client) attempt(ctx context.Context, call *Call) (*http.Response, error) {
	var body []byte
	if call.Body != nil {
		var err error
		if body, err = call.Body(); err != nil {
			return nil, err
		}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, call.Method, call.URL, rd)
	if err != nil {
		return nil, err
	}
	if call.Header != nil {
		req.Header = call.Header.Clone()
	}
	return c.HTTP.Do(req)
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.Base << attempt
	if d <= 0 || d > c.Cap {
		d = c.Cap
	}
	j := 0.8 + 0.4*rand.Float64() // ±20% jitter
	d = time.Duration(float64(d) * j)
	if d > c.Cap { // Cap is a hard ceiling, even under jitter
		d = c.Cap
	}
	return d
}

func (c *Client) log(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Debug(msg, args...)
	}
}

// RetryableStatus reports whether an HTTP status code is worth retrying.
// It is the single source of truth shared with the SDK's error typing.
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529: // Anthropic "overloaded"
		return true
	}
	return false
}

// retryAfter parses a Retry-After value (delay-seconds or HTTP-date).
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// maxRetryAfter bounds a server-provided Retry-After so a hostile or
// misconfigured gateway cannot stall callers for arbitrarily long.
const maxRetryAfter = 60 * time.Second

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
}
