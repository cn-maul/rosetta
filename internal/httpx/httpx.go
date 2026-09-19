// Package httpx provides a small HTTP helper with bounded retries and
// exponential backoff for transport errors and retryable status codes.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

type RetryPolicy int

const (
	// RetryDefault retries only intrinsically safe methods (GET/HEAD/
	// OPTIONS/PUT/DELETE) and leaves non-idempotent POSTs alone.
	RetryDefault RetryPolicy = iota
	// RetryNever disables retry regardless of method.
	RetryNever
	// RetryIdempotent is a caller assertion: "this request is safe to
	// repeat" (e.g. embeddings/rerank, which have no server-side side
	// effects). It behaves like RetryAlways but documents intent — the
	// transport cannot itself verify idempotency, so the caller owns that
	// guarantee.
	RetryIdempotent
	// RetryAlways forces retries for any method.
	RetryAlways
)

// Call describes one HTTP request. Body is re-invoked on every retry
// attempt so the payload can be rebuilt cheaply.
type Call struct {
	Method      string
	URL         string
	Header      http.Header
	Body        func() ([]byte, error) // nil for bodyless requests
	RetryPolicy RetryPolicy
}

// Client wraps *http.Client with bounded retries and exponential backoff.
type Client struct {
	HTTP       *http.Client
	MaxRetries int
	Base       time.Duration
	Cap        time.Duration
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
	var slept time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, call)
		if err == nil && !RetryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// A permanent (non-network) failure — bad method/URL or a Body()
		// that cannot serialize — will fail identically every time; never
		// spend a retry on it.
		if errors.Is(err, ErrPermanent) || !c.canRetry(call) || attempt >= c.MaxRetries || ctx.Err() != nil {
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
		if slept+wait > maxTotalWait {
			return resp, err // retry budget exhausted; surface the last outcome
		}
		slept += wait
		c.log("retrying request", "url", call.URL, "attempt", attempt+1, "wait", wait.String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) canRetry(call *Call) bool {
	switch call.RetryPolicy {
	case RetryNever:
		return false
	case RetryAlways, RetryIdempotent:
		return true
	default:
		switch call.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
			return true
		default:
			return false
		}
	}
}

func (c *Client) attempt(ctx context.Context, call *Call) (*http.Response, error) {
	var body []byte
	if call.Body != nil {
		var err error
		if body, err = call.Body(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPermanent, err)
		}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, call.Method, call.URL, rd)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	if call.Header != nil {
		req.Header = call.Header.Clone()
	}
	return c.HTTP.Do(req)
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.Base
	if base <= 0 {
		base = 400 * time.Millisecond
	}
	lim := c.Cap
	if lim <= 0 {
		lim = 8 * time.Second
	}
	d := base << attempt
	if d <= 0 || d > lim {
		d = lim
	}
	j := 0.8 + 0.4*rand.Float64() // ±20% jitter
	d = time.Duration(float64(d) * j)
	if d > lim { // Cap is a hard ceiling, even under jitter
		d = lim
	}
	if d <= 0 {
		d = time.Millisecond // never a zero-delay retry loop
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

// maxTotalWait caps the cumulative time Do may spend sleeping between
// retries, so a large MaxRetries combined with long Retry-After values
// cannot stall a caller for many minutes.
const maxTotalWait = 3 * time.Minute

// ErrPermanent marks a failure that will recur identically on every attempt
// (a Body() that cannot serialize, or an invalid method/URL). Do returns it
// immediately instead of burning the retry budget.
var ErrPermanent = errors.New("httpx: permanent request error")

// ErrBodyTooLarge is returned by ReadBody when the response exceeds the
// caller-provided cap. Distinguishing it from a JSON decode error makes
// oversized (or truncated) responses diagnosable instead of misleading.
var ErrBodyTooLarge = errors.New("rosetta: response body exceeds limit")

// ReadBody reads r fully but fails with ErrBodyTooLarge once more than
// limit bytes arrive. It is the bounded counterpart of io.ReadAll:
// LimitReader alone would silently truncate and push a confusing parse
// error (or worse, a mis-parse) onto the caller.
func ReadBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrBodyTooLarge, limit)
	}
	return b, nil
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
}
