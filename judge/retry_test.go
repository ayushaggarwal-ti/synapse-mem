package judge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// A gateway that always answers 429: retried with backoff, capped, and
// reported with its status.
func TestClaudeBacksOffAndReportsStatus(t *testing.T) {
	var calls atomic.Int32
	var times []time.Time
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"echo of secret input"}}`)
	}))
	defer srv.Close()
	c := NewClaude(option.WithBaseURL(srv.URL), option.WithAPIKey("k"))
	c.Retry = Retry{Attempts: 4, Base: 20 * time.Millisecond}
	_, err := c.Ask(context.Background(), req)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 429 || he.Attempts != 4 || calls.Load() != 4 {
		t.Fatalf("want HTTP 429 after 4 attempts, got %v (%d calls)", err, calls.Load())
	}
	if want := "claude: HTTP 429 (rate_limit_error) after 4 attempts"; err.Error() != want {
		t.Fatalf("error %q, want %q (no body text)", err, want)
	}
	// Gaps grow: ~20ms, ~40ms, ~80ms.
	if g1, g3 := times[1].Sub(times[0]), times[3].Sub(times[2]); g3 < 2*g1 {
		t.Fatalf("backoff should be exponential: %v then %v", g1, g3)
	}

	// A 400 is not retried.
	calls.Store(0)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"model not served"}}`)
	}))
	defer bad.Close()
	c = NewClaude(option.WithBaseURL(bad.URL), option.WithAPIKey("k"))
	if _, err := c.Ask(context.Background(), req); !errors.As(err, &he) || he.Status != 400 || calls.Load() != 1 {
		t.Fatalf("400 should fail at once: %v, %d calls", err, calls.Load())
	}
}

func TestLimiterCapsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	j := NewJev("k")
	j.BaseURL, j.Retry, j.Limit = srv.URL, Retry{Attempts: 1}, NewLimiter(2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); j.Ask(context.Background(), req) }()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("at most 2 calls at once, saw %d", peak.Load())
	}
}
