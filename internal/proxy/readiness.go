package proxy

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamStatus is the per-upstream readiness state reported by /readyz.
type upstreamStatus string

const (
	statusConnected upstreamStatus = "connected"
	statusFailed    upstreamStatus = "failed"
	statusDisabled  upstreamStatus = "disabled"
)

type upstreamState struct {
	Status upstreamStatus `json:"status"`
	Error  string         `json:"error,omitempty"`
}

// readinessReport is the JSON body served by /readyz.
type readinessReport struct {
	Ready     bool                     `json:"ready"`
	Degraded  bool                     `json:"degraded"`
	Upstreams map[string]upstreamState `json:"upstreams"`
}

// readinessTracker records per-upstream connect state and the overall ready
// gate. mcp-proxy binds its port and registers each /<name>/ route
// asynchronously as upstreams connect, so a bare port-accept check can pass
// while a route still 404s. The tracker exposes a real gate: /readyz stays 503
// until signalReady fires (every upstream resolved and every successful route
// registered), then 200 — closing the "fails on first load" race.
type readinessTracker struct {
	mu        sync.Mutex
	upstreams map[string]upstreamState
	ready     atomic.Bool
}

func newReadinessTracker() *readinessTracker {
	return &readinessTracker{upstreams: make(map[string]upstreamState)}
}

func (t *readinessTracker) set(name string, status upstreamStatus, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := upstreamState{Status: status}
	if err != nil {
		st.Error = err.Error()
	}
	t.upstreams[name] = st
}

func (t *readinessTracker) setDisabled(name string)        { t.set(name, statusDisabled, nil) }
func (t *readinessTracker) setConnected(name string)       { t.set(name, statusConnected, nil) }
func (t *readinessTracker) setFailed(name string, e error) { t.set(name, statusFailed, e) }

// report snapshots the current state. ready reflects the gate; degraded is
// true when any upstream failed to connect (the proxy still serves the ones
// that did).
func (t *readinessTracker) report() readinessReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	rep := readinessReport{
		Ready:     t.ready.Load(),
		Upstreams: make(map[string]upstreamState, len(t.upstreams)),
	}
	for name, st := range t.upstreams {
		rep.Upstreams[name] = st
		if st.Status == statusFailed {
			rep.Degraded = true
		}
	}
	return rep
}

// registerProbes wires the liveness + readiness HTTP probes onto mux. Call it
// before the listener starts so the probes answer from the first accepted
// connection.
//
//   - GET /healthz — liveness. Always 200 once the server is listening.
//   - GET /readyz  — readiness. 503 until signalReady fires, then 200. The
//     JSON body always reports per-upstream state and a degraded flag.
func (t *readinessTracker) registerProbes(mux handlerMux) {
	mux.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	mux.Handle("/readyz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rep := t.report()
		code := http.StatusServiceUnavailable
		if rep.Ready {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(rep)
	}))
}

// remove drops an upstream from the readiness report (used when a reload removes
// it from the config).
func (t *readinessTracker) remove(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.upstreams, name)
}

// signalReady marks the proxy ready: it flips the /readyz gate to 200, logs a
// stable readiness line, fires systemd sd_notify(READY=1) so a Type=notify
// unit only reaches `active` here, and — if the unit set WatchdogSec — starts
// the watchdog keepalive bound to ctx.
func (t *readinessTracker) signalReady(ctx context.Context) {
	t.ready.Store(true)
	rep := t.report()
	connected := 0
	for _, st := range rep.Upstreams {
		if st.Status == statusConnected {
			connected++
		}
	}
	log.Printf("proxy ready: %d upstreams connected, degraded=%t", connected, rep.Degraded)
	if err := sdNotify("READY=1"); err != nil {
		log.Printf("sd_notify failed: %v", err)
	}
	startWatchdog(ctx)
}

// sdNotify sends a status datagram to the systemd notify socket named by
// $NOTIFY_SOCKET. It is a no-op (nil error) when the variable is unset — i.e.
// the process is not running under a notify-capable unit — so it stays
// harmless outside systemd. Minimal native implementation of sd_notify(3);
// avoids pulling in coreos/go-systemd for a single datagram.
func sdNotify(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	// systemd uses an abstract-namespace socket when the path starts with '@'.
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(state))
	return err
}

// startWatchdog pings systemd's watchdog at half the interval named by
// $WATCHDOG_USEC (microseconds), so a unit with WatchdogSec= restarts the
// proxy if it ever wedges. No-op when WATCHDOG_USEC is unset/zero, or when
// $WATCHDOG_PID names a different process. Stops when ctx is cancelled.
func startWatchdog(ctx context.Context) {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return
	}
	usec, err := strconv.ParseInt(usecStr, 10, 64)
	if err != nil || usec <= 0 {
		return
	}
	interval := time.Duration(usec) * time.Microsecond / 2
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := sdNotify("WATCHDOG=1"); e != nil {
					log.Printf("sd_notify watchdog failed: %v", e)
				}
			}
		}
	}()
}
