package proxy

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// activityTracker records the time of the last proxied request so an idle
// monitor can shut the proxy down after a quiet period. This lets the proxy be
// driven by pure socket activation (e.g. systemd .socket): start on the first
// connection, exit once traffic stops, with no external idle-watcher process.
//
// Probe traffic (/healthz, /readyz) deliberately does NOT touch the tracker —
// those are registered straight on the mux, never wrapped by middleware() — so
// an orchestrator polling readiness can't keep the proxy alive forever.
type activityTracker struct {
	lastNano atomic.Int64
	inflight atomic.Int64
}

func newActivityTracker() *activityTracker {
	t := &activityTracker{}
	t.touch()
	return t
}

// touch marks "now" as the most recent activity.
func (t *activityTracker) touch() { t.lastNano.Store(time.Now().UnixNano()) }

// idleFor reports how long it has been since the last touch.
func (t *activityTracker) idleFor() time.Duration {
	return time.Duration(time.Now().UnixNano() - t.lastNano.Load())
}

// begin/end bracket a request/response exchange. While at least one is in
// flight the tracker reports busy(), so the idle monitor never tears the
// backend (or the process) down mid-request — even a single request that runs
// longer than idleTimeout is safe, since touch() only fires at its edges.
func (t *activityTracker) begin() {
	t.inflight.Add(1)
	t.touch()
}

func (t *activityTracker) end() {
	t.touch()
	t.inflight.Add(-1)
}

// busy reports whether any request/response exchange is in flight.
func (t *activityTracker) busy() bool { return t.inflight.Load() > 0 }

// middleware records activity on every request that reaches an upstream route.
// Placed outermost in the chain so any contact with a mounted server counts,
// including requests later rejected by auth.
//
// Long-lived GET streams (the server→client SSE channel) are touched but NOT
// counted as in-flight: a client can hold that stream open indefinitely while
// doing nothing, and counting it would pin the backend forever and defeat idle
// teardown. Request/response traffic (POST/DELETE/…) IS counted, so an active
// call can never have its backend cancelled out from under it.
func (t *activityTracker) middleware() MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				t.touch()
				next.ServeHTTP(w, r)
				return
			}
			t.begin()
			defer t.end()
			next.ServeHTTP(w, r)
		})
	}
}

// monitorIdle calls onIdle exactly once, after the proxy has gone idleTimeout
// without a proxied request. It (re)seeds the activity clock to now — call it
// after readiness so the upstream cold-start window isn't counted as idle —
// then polls at a quarter of the timeout (floored at 100ms). It stops when ctx
// is cancelled. A no-op when idleTimeout <= 0.
func (t *activityTracker) monitorIdle(ctx context.Context, idleTimeout time.Duration, onIdle func()) {
	if idleTimeout <= 0 {
		return
	}
	t.touch()
	interval := max(idleTimeout/4, 100*time.Millisecond)
	go func() {
		// onIdle teardown runs SDK/upstream close paths; recover so a panic in
		// one upstream's teardown can't kill the process and its siblings.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("idle monitor recovered from panic: %v", r)
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Never fire while a request is in flight — wait for the next
				// tick after it drains, so teardown/exit can't race an active
				// call.
				if t.busy() {
					continue
				}
				if idle := t.idleFor(); idle >= idleTimeout {
					log.Printf("idle for %s (>= %s), shutting down", idle.Round(time.Second), idleTimeout)
					onIdle()
					return
				}
			}
		}
	}()
}
