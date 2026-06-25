package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func doProbe(t *testing.T, mux *http.ServeMux, path string) (int, readinessReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var rep readinessReport
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	}
	return rec.Code, rep
}

func TestReadyzGate(t *testing.T) {
	tr := newReadinessTracker()
	mux := http.NewServeMux()
	tr.registerProbes(mux)

	tr.setConnected("alpha")
	tr.setFailed("beta", context.DeadlineExceeded)
	tr.setDisabled("gamma")

	// Before signalReady: 503 and not ready.
	code, rep := doProbe(t, mux, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("pre-ready /readyz = %d, want 503", code)
	}
	if rep.Ready {
		t.Fatalf("pre-ready report.Ready = true, want false")
	}

	// Healthz is liveness — 200 regardless of readiness.
	if hc, _ := doProbe(t, mux, "/healthz"); hc != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", hc)
	}

	tr.signalReady(context.Background())

	code, rep = doProbe(t, mux, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("post-ready /readyz = %d, want 200", code)
	}
	if !rep.Ready {
		t.Fatalf("post-ready report.Ready = false, want true")
	}
	if !rep.Degraded {
		t.Fatalf("report.Degraded = false, want true (beta failed)")
	}
	if rep.Upstreams["alpha"].Status != statusConnected {
		t.Errorf("alpha status = %q, want connected", rep.Upstreams["alpha"].Status)
	}
	if rep.Upstreams["beta"].Status != statusFailed || rep.Upstreams["beta"].Error == "" {
		t.Errorf("beta state = %+v, want failed with error", rep.Upstreams["beta"])
	}
	if rep.Upstreams["gamma"].Status != statusDisabled {
		t.Errorf("gamma status = %q, want disabled", rep.Upstreams["gamma"].Status)
	}
}

func TestReadyzNotDegradedWhenAllConnected(t *testing.T) {
	tr := newReadinessTracker()
	mux := http.NewServeMux()
	tr.registerProbes(mux)
	tr.setConnected("only")
	tr.signalReady(context.Background())

	code, rep := doProbe(t, mux, "/readyz")
	if code != http.StatusOK || !rep.Ready || rep.Degraded {
		t.Fatalf("got code=%d ready=%t degraded=%t, want 200/true/false", code, rep.Ready, rep.Degraded)
	}
}

func TestSdNotifyNoopWhenUnset(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("sdNotify with unset NOTIFY_SOCKET = %v, want nil", err)
	}
}

func TestSdNotifyWritesToSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "notify.sock")
	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", sockPath)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("sdNotify = %v, want nil", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, rErr := conn.ReadFromUnix(buf)
	if rErr != nil {
		t.Fatalf("read from notify socket: %v", rErr)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("notify payload = %q, want READY=1", got)
	}
}

func TestStartWatchdogNoopWhenUnset(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	// Must return immediately without spawning a ticker goroutine that pings.
	startWatchdog(context.Background())
}

func TestStartWatchdogPings(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "wd.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", sockPath)
	// 100ms watchdog interval -> pings every ~50ms.
	t.Setenv("WATCHDOG_USEC", "100000")
	t.Setenv("WATCHDOG_PID", "") // unset -> applies to us

	startWatchdog(t.Context())

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, rErr := conn.ReadFromUnix(buf)
	if rErr != nil {
		t.Fatalf("expected a watchdog ping: %v", rErr)
	}
	if got := string(buf[:n]); got != "WATCHDOG=1" {
		t.Fatalf("watchdog payload = %q, want WATCHDOG=1", got)
	}
}

func TestStartWatchdogWrongPid(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "x.sock"))
	t.Setenv("WATCHDOG_USEC", "100000")
	t.Setenv("WATCHDOG_PID", "1") // not us -> must be a no-op
	startWatchdog(context.Background())
}
