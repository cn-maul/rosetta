package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// startTestServer creates an in-memory test server (Go 1.27), usable
// inside a synctest bubble without real network or real time.
func startTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewTestServer(t, h)
}

func TestDoSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
			t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
		}
	})
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		start := time.Now()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || calls.Load() != 3 {
			t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
		}
		// Two 500s: two backoff sleeps of 400ms and 800ms (±20% jitter).
		if d := time.Since(start); d < 960*time.Millisecond || d > 1440*time.Millisecond {
			t.Fatalf("fake elapsed = %s, want ~0.96s-1.44s", d)
		}
	})
}

func TestRetriesExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		c := New() // MaxRetries = 2
		c.HTTP = srv.Client()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || calls.Load() != 3 {
			t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
		}
	})
}

func TestNoRetryOnNonRetryableStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		c := New()
		c.HTTP = srv.Client()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
			t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
		}
	})
}

// Retry-After must be capped so a hostile or misconfigured gateway cannot
// stall the caller for hours.
func TestRetryAfterCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		start := time.Now()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		if d := time.Since(start); d > 61*time.Second {
			t.Fatalf("fake elapsed = %s, want capped at ~60s", d)
		}
	})
}

func TestRetryAfterHTTPDate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		start := time.Now()
		resp, err := c.Do(context.Background(), &Call{Method: http.MethodGet, URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if d := time.Since(start); d < 5*time.Second || d > 61*time.Second {
			t.Fatalf("fake elapsed = %s, want ~5s", d)
		}
	})
}

func TestBodyRebuiltPerAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, builds atomic.Int32
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		call := &Call{
			Method: http.MethodPost,
			URL:    srv.URL,
			Body:   func() ([]byte, error) { builds.Add(1); return []byte("{}"), nil },
		}
		resp, err := c.Do(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if calls.Load() != 2 || builds.Load() != 2 {
			t.Fatalf("calls=%d builds=%d, want 2/2", calls.Load(), builds.Load())
		}
	})
}

func TestContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		c := New()
		c.HTTP = srv.Client()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.Do(ctx, &Call{Method: http.MethodGet, URL: srv.URL})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestRetryAfterParse(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"3", 3 * time.Second},
		{"-5", 0},
		{"not-a-date", 0},
	}
	for _, tt := range tests {
		if got := retryAfter(tt.input); got != tt.want {
			t.Errorf("retryAfter(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}
	// HTTP-date in the future.
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(future); got <= 0 || got > 11*time.Second {
		t.Errorf("retryAfter(http-date) = %s", got)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	c := New()
	for attempt := 0; attempt < 8; attempt++ {
		d := c.backoff(attempt)
		want := 400 * time.Millisecond << attempt
		if want > c.Cap {
			want = c.Cap
		}
		if d < want*8/10 || d > want*12/10 {
			t.Fatalf("backoff(%d) = %s, want within ±20%% of %s", attempt, d, want)
		}
	}
	// Overflow guard: a huge attempt must clamp toward Cap (jitter may
	// land anywhere in [0.8×Cap, Cap] once Cap is a hard ceiling).
	if d := c.backoff(1000); d > c.Cap || d < c.Cap*8/10 {
		t.Fatalf("backoff(1000) = %s, want within [0.8×Cap, Cap]", d)
	}
}

func TestRetryStatus(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504, 529} {
		if !RetryableStatus(code) {
			t.Errorf("RetryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 400, 401, 404, 422} {
		if RetryableStatus(code) {
			t.Errorf("RetryableStatus(%d) = true, want false", code)
		}
	}
}
