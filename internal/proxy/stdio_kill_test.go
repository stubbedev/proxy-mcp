//go:build !windows

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStdioCancelKillsProcessGroup covers the orphan leak: a stdio backend
// like `npx foo` wraps the real server in sh/node grandchildren. Cancelling
// the transport context must kill the whole process group, not just the
// direct child, or the grandchildren outlive every teardown.
func TestStdioCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// Direct child (sh) spawns a grandchild (sleep) and writes its pid.
	cfg := &MCPClientConfigV2{
		Command: "sh",
		Args:    []string{"-c", "sleep 60 & echo $! > " + pidFile + "; wait"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr, err := buildTransport(ctx, cfg)
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	conn, err := tr.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			grandchild, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && grandchild > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("grandchild pid never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	_ = conn.Close()

	deadline = time.Now().Add(5 * time.Second)
	for syscall.Kill(grandchild, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived context cancel (orphan leak)", grandchild)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
