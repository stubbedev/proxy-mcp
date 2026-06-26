package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLazyUpstreamConnectsOnDemandAndTearsDown covers the lazy lifecycle: an
// upstream with idleTimeout set does not touch its backend until the first
// request, tears the backend down after going idle, and re-connects on the
// next use.
func TestLazyUpstreamConnectsOnDemandAndTearsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var hits atomic.Int32
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, struct{}{}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	upHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(upHTTP.Close)

	cfg := streamableCfg(upHTTP.URL)
	cfg.Options.IdleTimeout = "300ms"
	up := newUpstream("up", &MCPProxyConfigV2{Name: "p", Version: "1.0.0", Type: MCPServerTypeStreamable}, cfg)
	t.Cleanup(up.close)

	if !up.lazy {
		t.Fatal("idleTimeout set but upstream not lazy")
	}
	// Nothing should have reached the backend before the first request.
	if up.template() != nil || hits.Load() != 0 {
		t.Fatalf("lazy upstream connected before first request (hits=%d)", hits.Load())
	}

	if err := up.ensureConnected(ctx); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	if up.template() == nil {
		t.Fatal("template nil after ensureConnected")
	}
	afterConnect := hits.Load()
	if afterConnect == 0 {
		t.Fatal("upstream not contacted on connect")
	}

	// With no traffic it goes idle and tears the backend down.
	deadline := time.Now().Add(5 * time.Second)
	for up.isConnected() {
		if time.Now().After(deadline) {
			t.Fatal("lazy upstream never tore down when idle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if up.template() != nil {
		t.Fatal("template still set after teardown")
	}

	// Next use re-connects (a fresh contact to the backend).
	if err := up.ensureConnected(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if hits.Load() <= afterConnect {
		t.Fatal("reconnect did not re-contact the upstream")
	}
}

// TestSessionForReportsUnavailable confirms the nil-session guard: a torn-down /
// never-connected upstream yields a clean error instead of a nil deref, so one
// dead backend can't panic a shared proxy process.
func TestSessionForReportsUnavailable(t *testing.T) {
	up := newUpstream("up", &MCPProxyConfigV2{Name: "p", Version: "1.0.0", Type: MCPServerTypeStreamable},
		streamableCfg("http://127.0.0.1:1"))
	if _, err := up.sessionFor(nil); err == nil {
		t.Fatal("sessionFor returned nil error for a disconnected upstream")
	}
}
