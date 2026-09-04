package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMonitorIdleFiresWhenQuiet(t *testing.T) {
	tr := newActivityTracker()

	fired := make(chan struct{})
	tr.monitorIdle(t.Context(), 200*time.Millisecond, func() { close(fired) })

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("idle monitor never fired on a quiet proxy")
	}
}

func TestMonitorIdleStaysAliveWhileActive(t *testing.T) {
	tr := newActivityTracker()

	fired := make(chan struct{})
	tr.monitorIdle(t.Context(), 200*time.Millisecond, func() { close(fired) })

	// Touch steadily for longer than the timeout; it must not fire.
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	done := time.After(800 * time.Millisecond)
	for {
		select {
		case <-fired:
			t.Fatal("idle monitor fired while traffic was active")
		case <-tick.C:
			tr.touch()
		case <-done:
			return
		}
	}
}

// TestMonitorIdleWaitsForInflight covers Edge 1: a request that runs longer
// than idleTimeout must not have its backend (or the process) torn down under
// it. While a request/response exchange is in flight the monitor stays its
// hand, then fires once it drains.
func TestMonitorIdleWaitsForInflight(t *testing.T) {
	tr := newActivityTracker()
	tr.begin() // a request is in flight

	fired := make(chan struct{})
	tr.monitorIdle(t.Context(), 150*time.Millisecond, func() { close(fired) })

	select {
	case <-fired:
		t.Fatal("idle monitor fired while a request was in flight")
	case <-time.After(600 * time.Millisecond):
	}

	tr.end() // request done

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("idle monitor never fired after the in-flight request drained")
	}
}

func TestMonitorIdleDisabled(t *testing.T) {
	tr := newActivityTracker()

	fired := make(chan struct{})
	tr.monitorIdle(t.Context(), 0, func() { close(fired) })

	select {
	case <-fired:
		t.Fatal("idle monitor fired with idleTimeout=0 (should be disabled)")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestMiddlewareIgnoresGETStreams covers the SSE-reconnect leak: clients re-open
// the server→client GET stream on a timer, so touching on GET let a reconnect
// cadence at or under idleTimeout hold a backend nobody was calling alive
// forever. GET must not reset the clock; POST must.
func TestMiddlewareIgnoresGETStreams(t *testing.T) {
	for _, tc := range []struct {
		method    string
		wantFired bool
	}{
		{http.MethodGet, true},
		{http.MethodPost, false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			tr := newActivityTracker()
			h := tr.middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

			fired := make(chan struct{})
			tr.monitorIdle(t.Context(), 200*time.Millisecond, func() { close(fired) })

			// Hit the route faster than the timeout for longer than the timeout.
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			done := time.After(700 * time.Millisecond)
			for done != nil {
				select {
				case <-fired:
					if !tc.wantFired {
						t.Fatalf("idle monitor fired while %s traffic was active", tc.method)
					}
					return
				case <-tick.C:
					h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, "/x/mcp", nil))
				case <-done:
					done = nil
				}
			}
			if tc.wantFired {
				t.Fatalf("idle monitor never fired though only %s traffic arrived", tc.method)
			}
		})
	}
}
