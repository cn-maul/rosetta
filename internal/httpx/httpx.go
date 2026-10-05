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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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

// Connection lifecycle bounds for the SDK-owned transport. http.DefaultTransport
// sets these; a hand-built *http.Transport does NOT inherit them, so omitting
// them would silently restore an unbounded dial/TLS-handshake wait. That is
// reachable from stream callers, which carry no SDK-side timeout (only the
// caller's ctx), so a blackholed SYN or a stalled TLS handshake would hang
// Next() forever (v0.5.x gave up after 30s/10s).
const (
	dialTimeout         = 30 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
)

// newTransport returns the SDK-owned default *http.Transport. It replaces
// http.DefaultTransport (MaxIdleConnsPerHost=2), which under concurrency
// >2 closes connections after each request and re-handshakes on the next
// wave, and lets every Client in the process contend for one shared 100/2
// pool. The tuned values keep idle connections warm for reuse and bound the
// time a server may stall before sending response headers.
func newTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// New returns a Client with production defaults.
func New() *Client {
	return &Client{
		HTTP:       &http.Client{Transport: newTransport(), CheckRedirect: CrossHostSafeRedirect},
		MaxRetries: 2,
		Base:       400 * time.Millisecond,
		Cap:        8 * time.Second,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// CrossHostSafeRedirect is an http.Client CheckRedirect policy that refuses
// to follow a redirect to a different host. net/http automatically strips
// only Authorization/Cookie/Proxy-* on a host change — headers such as
// Anthropic's x-api-key would otherwise be re-sent verbatim to a third-party
// host chosen by the remote endpoint, leaking credentials (and, on 307/308
// replays, the request body). The comparison uses the case-insensitive
// hostname and ignores the port, so legitimate same-host redirects are
// allowed while cross-host ones are refused. A caller supplying their own
// *http.Client keeps any CheckRedirect they set.
func CrossHostSafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
		// Marked permanent so Do returns immediately instead of burning a
		// retry (and its backoff sleep) on a refusal that will recur
		// identically on every attempt.
		return fmt.Errorf("%w: refusing cross-host redirect %s -> %s", ErrPermanent, via[0].URL.Hostname(), req.URL.Hostname())
	}
	return nil
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
		} else {
			wait = c.backoff(attempt)
		}
		if slept+wait > maxTotalWait {
			// Budget exhausted. Return the response with its body still
			// open so the caller can parse the final provider error instead
			// of a dead body that would masquerade as a transport failure.
			return resp, err
		}
		slept += wait
		c.log("retrying request", "url", safeURL(call.URL), "attempt", attempt+1, "wait", wait.String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			// The caller's deadline expired during backoff. Return the last
			// response with its body still open so the caller parses the
			// actual provider error — e.g. a 429 body — instead of a generic
			// context deadline that hides what the endpoint said. Only when
			// there is no response to hand back (a pure transport failure) do
			// we surface the context error. (The body is drained only in the
			// timer branch below, so it is still readable here.)
			if resp != nil {
				return resp, err
			}
			return nil, ctx.Err()
		case <-timer.C:
			// We are actually retrying: discard this attempt's body so its
			// connection can be reused. Done after the sleep, not before, so
			// the body is still open if ctx.Done wins the race above.
			if resp != nil {
				drain(resp)
			}
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

// safeURL renders a request URL for a log line with userinfo, query and
// fragment stripped, and path-embedded credential patterns masked.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return MaskSecrets(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return MaskSecrets(u.String())
}

// secretRe is the single source of truth for credential shapes a provider,
// a URL path or an error body might carry (API keys, JWTs, GitHub/OAuth
// tokens). It lives here so the rosetta package's error redaction and this
// transport's log redaction can never drift apart (they share this one
// regexp through MaskSecrets / MaskSecretBytes).
var secretRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}|AIza[0-9A-Za-z_-]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)

// MaskSecrets replaces credential patterns in s with a fixed marker. It is
// shared by the transport's own log redaction and the rosetta package's
// error/body redaction.
func MaskSecrets(s string) string { return secretRe.ReplaceAllString(s, "***") }

// MaskSecretBytes is MaskSecrets for a raw []byte (used by rosetta's JSON
// body redaction, which works on bytes before re-marshaling).
func MaskSecretBytes(b []byte) []byte { return secretRe.ReplaceAll(b, []byte("***")) }

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

// drain discards a response body being retried so the connection can be
// reused. It reads up to drainLimit bytes before closing: reading only a
// token amount (e.g. 8KiB) would leave larger error pages unread, forcing
// the connection closed instead of returned to the idle pool.
func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	resp.Body.Close()
}

// drainLimit bounds how much of a discarded body is read for connection
// reuse. It is a compromise: large enough to reuse connections after
// typical error pages, small enough to bound the cost of a hostile or
// runaway body.
const drainLimit = 64 << 10
