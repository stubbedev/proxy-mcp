package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

// ready gates the /readyz probe. It flips to true exactly once, from
// signalReady, after every upstream MCP client has connected and its route is
// registered on the proxy mux.
var ready atomic.Bool

// registerReadinessProbes wires the liveness + readiness HTTP probes onto mux.
// Call it before the listener starts so the probes answer from the first
// accepted connection.
//
//   - GET /healthz — liveness. Always 200 once the server is listening. Use it
//     to detect a hung/crashed process.
//   - GET /readyz  — readiness. 503 "starting" until signalReady fires, then
//     200 "ok". Gate a socket-activated frontend / orchestrator on this to
//     avoid racing the proxy's asynchronous route registration.
func registerReadinessProbes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("starting"))
	})
}

// signalReady marks the proxy ready. It is called once, at the point where all
// upstream clients are initialized and all routes are registered:
//
//  1. flips the /readyz gate to 200,
//  2. logs a stable, greppable readiness line,
//  3. fires systemd sd_notify(READY=1) so a Type=notify unit only reaches
//     `active (running)` here — closing the "fails on first load" race where a
//     dependent service starts the instant the port binds, before routes exist.
//
// n is the number of registered upstream routes.
func signalReady(n int) {
	ready.Store(true)
	log.Printf("proxy ready: %d routes registered", n)
	if err := sdNotifyReady(); err != nil {
		log.Printf("sd_notify failed: %v", err)
	}
}

// sdNotifyReady sends READY=1 to the systemd notify socket named by
// $NOTIFY_SOCKET. It is a no-op (nil error) when the variable is unset — i.e.
// the process is not running under a Type=notify unit — so it stays harmless
// outside systemd. This is a minimal native implementation of the sd_notify(3)
// protocol; it avoids pulling in coreos/go-systemd for a single datagram.
func sdNotifyReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	// systemd uses an abstract namespace socket when the path starts with '@'.
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("READY=1"))
	return err
}
