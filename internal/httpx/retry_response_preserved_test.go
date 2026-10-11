package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A caller's deadline that expires DURING a retry attempt must not cost the
// caller the provider response an earlier attempt already returned.
//
// The ctx.Done branch of the backoff sleep already hands back the last
// response for this reason (see TestDoContextDeadlineDuringBackoffKeepsBody
// Readable), but the exit at the top of the loop — reached when an attempt
// fails because the context is already dead — used to return that attempt's
// (nil) response instead, silently discarding a real 429/503 in favour of an
// opaque "context deadline exceeded". That destroys the upstream error the
// caller classifies on (APIError.Category / AffectsModel / RetryAfter).
func TestDoCtxExpiryDuringAttemptKeepsResponse(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
			return
		}
		// Later attempt outlives the deadline, then returns so the test
		// server can shut down promptly.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	c := New()
	c.MaxRetries = 3
	c.Base = 10 * time.Millisecond
	c.Cap = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	resp, err := c.Do(ctx, &Call{
		Method:      http.MethodPost,
		URL:         srv.URL,
		Body:        func() ([]byte, error) { return []byte(`{}`), nil },
		RetryPolicy: RetryIdempotent,
	})
	if resp == nil {
		t.Fatalf("provider response was discarded; err = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	b, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("body unreadable: %v", rerr)
	}
	if len(b) == 0 {
		t.Fatal("provider error body was lost — the caller cannot classify the failure")
	}
}

// The inverse case stays intact: a pure transport failure with no response
// ever seen must still surface the context error, not a nil-body response.
func TestDoCtxExpiryWithNoResponseAnywhereSurfacesCtxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	c := New()
	c.MaxRetries = 3
	c.Base = 10 * time.Millisecond
	c.Cap = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	resp, err := c.Do(ctx, &Call{
		Method:      http.MethodPost,
		URL:         srv.URL,
		Body:        func() ([]byte, error) { return []byte(`{}`), nil },
		RetryPolicy: RetryIdempotent,
	})
	if resp != nil {
		t.Fatalf("resp = %+v, want nil (no provider response ever seen)", resp)
	}
	if err == nil {
		t.Fatal("want a context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a context error", err)
	}
}
