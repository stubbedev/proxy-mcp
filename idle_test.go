package main

import (
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
