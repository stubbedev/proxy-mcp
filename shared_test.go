package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialWithRoots connects a downstream client advertising a specific workspace
// root, so the test can prove the proxy keeps each client's roots isolated.
func dialWithRoots(ctx context.Context, t *testing.T, url, rootURI string) *mcp.ClientSession {
	t.Helper()
	cl := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "1.0.0"}, nil)
	cl.AddRoots(&mcp.Root{URI: rootURI, Name: "ws"})
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", rootURI, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestRootsListChangedPropagates proves a client changing its workspace roots
// mid-session is reflected upstream — so a roots-caching server (e.g. sentry)
// re-resolves the workspace rather than serving stale roots.
func TestRootsListChangedPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1.0.0"}, nil)
	addServerRequestTools(srv) // do_roots echoes the caller's first root
	upHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(upHTTP.Close)
	proxyURL := startProxyFor(ctx, t, streamableCfg(upHTTP.URL))

	cl := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "1.0.0"}, nil)
	cl.AddRoots(&mcp.Root{URI: "file:///before", Name: "ws"})
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: proxyURL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "do_roots"})
	if err != nil {
		t.Fatalf("do_roots: %v", err)
	}
	if got := toolText(t, res); got != "file:///before" {
		t.Fatalf("initial roots = %q, want file:///before", got)
	}

	// Change the client's roots → roots/list_changed → must reach upstream.
	cl.RemoveRoots("file:///before")
	cl.AddRoots(&mcp.Root{URI: "file:///after", Name: "ws"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "do_roots"})
		if err != nil {
			t.Fatalf("do_roots: %v", err)
		}
		if toolText(t, res) == "file:///after" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("roots/list_changed never propagated to upstream")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSharedInstanceIsolatesClients proves the core goal: a single upstream MCP
// instance, fronted by one proxy, serves many Claude clients — each on its own
// session with its own roots. Two clients with different roots both hit the same
// upstream process via the proxy and each sees only its own workspace.
func TestSharedInstanceIsolatesClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One upstream instance with a tool that echoes the caller's first root.
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1.0.0"}, nil)
	addServerRequestTools(srv) // provides do_roots
	upHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(upHTTP.Close)

	// One proxy fronting it (per-session default = many sessions, one process).
	proxyURL := startProxyFor(ctx, t, streamableCfg(upHTTP.URL))

	// Two distinct clients through the one proxy → the one upstream instance.
	clientA := dialWithRoots(ctx, t, proxyURL, "file:///repo-a")
	clientB := dialWithRoots(ctx, t, proxyURL, "file:///repo-b")

	for _, c := range []struct {
		cs   *mcp.ClientSession
		want string
	}{
		{clientA, "file:///repo-a"},
		{clientB, "file:///repo-b"},
	} {
		res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: "do_roots"})
		if err != nil {
			t.Fatalf("do_roots: %v", err)
		}
		if got := toolText(t, res); got != c.want {
			t.Fatalf("shared-instance isolation: client saw roots %q, want %q", got, c.want)
		}
	}
}
