package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// upHeaderKey carries the X-Test header the upstream server received, so the
// echo_header tool can prove the proxy forwarded the downstream caller's header.
type upHeaderKey struct{}

// newUpstream starts a real MCP server exposing echo_header (returns the X-Test
// header it saw). It returns the server and its *MCPServer so the test can add a
// tool later and exercise tools/list_changed propagation through the proxy.
func startTestUpstream(t *testing.T) (*httptest.Server, *server.MCPServer) {
	t.Helper()
	up := server.NewMCPServer("upstream", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithRecovery(),
	)
	up.AddTool(mcp.NewTool("echo_header"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		v, _ := ctx.Value(upHeaderKey{}).(string)
		return mcp.NewToolResultText(v), nil
	})

	handler := server.NewStreamableHTTPServer(up,
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, upHeaderKey{}, r.Header.Get("X-Test"))
		}),
	)
	httpSrv := httptest.NewServer(handler)
	// Registered first → closed last (after the clients), so Close doesn't block
	// on an upstream listening stream the proxy still holds open.
	t.Cleanup(httpSrv.Close)
	return httpSrv, up
}

// startProxy wires the code under test: a proxy server fronting one streamable
// upstream, served over HTTP. Returns the proxy's URL.
func startProxy(ctx context.Context, t *testing.T, upstreamURL string) string {
	t.Helper()
	proxyCfg := &MCPProxyConfigV2{Name: "proxy", Version: "1.0.0", Type: MCPServerTypeStreamable}
	clientCfg := &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: upstreamURL, Options: &OptionsV2{}}

	up, err := newUpstream("up", proxyCfg, clientCfg, mcp.Implementation{Name: "proxy"})
	if err != nil {
		t.Fatalf("newUpstream: %v", err)
	}
	if err := up.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	proxyHTTP := httptest.NewServer(up.srv.handler)
	t.Cleanup(proxyHTTP.Close)
	t.Cleanup(func() { _ = up.close() })
	return proxyHTTP.URL
}

func TestProxyForwardsHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upstream, _ := startTestUpstream(t)
	proxyURL := startProxy(ctx, t, upstream.URL)

	down := dialDownstream(ctx, t, proxyURL, map[string]string{
		"X-Test":        "hello-from-client",
		"Authorization": "Bearer tok",
	})

	// echo_header is visible through the proxy and the caller's header reaches
	// the upstream verbatim.
	echo, err := down.CallTool(ctx, callToolReq("echo_header"))
	if err != nil {
		t.Fatalf("call echo_header: %v", err)
	}
	if got := resultText(t, echo); got != "hello-from-client" {
		t.Fatalf("header passthrough: upstream saw X-Test=%q, want %q", got, "hello-from-client")
	}
}

func TestProxyRelaysToolsListChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upstream, up := startTestUpstream(t)
	proxyURL := startProxy(ctx, t, upstream.URL)

	down := dialDownstream(ctx, t, proxyURL, nil)

	// Watch for the proxy's tools/list_changed notification to the downstream.
	changed := make(chan struct{}, 1)
	down.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationToolsListChanged) {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	})

	// Add a tool upstream → upstream emits tools/list_changed → proxy re-lists
	// and re-registers → proxy emits tools/list_changed to the downstream.
	up.AddTool(mcp.NewTool("late_tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("late"), nil
	})

	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("downstream never received tools/list_changed from the proxy")
	}

	// And the new tool is now visible through the proxy.
	tools, err := down.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "late_tool" {
			found = true
		}
	}
	if !found {
		t.Fatal("late_tool not visible through proxy after list_changed resync")
	}
}

// dialDownstream connects a streamable client to the proxy with the given
// headers, listening continuously so it receives server notifications.
func dialDownstream(ctx context.Context, t *testing.T, url string, headers map[string]string) *client.Client {
	t.Helper()
	opts := []transport.StreamableHTTPCOption{transport.WithContinuousListening()}
	if len(headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(headers))
	}
	tr, err := transport.NewStreamableHTTP(url, opts...)
	if err != nil {
		t.Fatalf("downstream transport: %v", err)
	}
	down := client.NewClient(tr)
	if err := down.Start(ctx); err != nil {
		t.Fatalf("downstream start: %v", err)
	}
	t.Cleanup(func() { _ = down.Close() })
	if _, err := down.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("downstream initialize: %v", err)
	}
	return down
}

// fixedCompletion is an upstream completion provider returning canned values,
// to prove the proxy forwards completion/complete to the upstream.
type fixedCompletion struct{}

func (fixedCompletion) CompletePromptArgument(context.Context, string, mcp.CompleteArgument, mcp.CompleteContext) (*mcp.Completion, error) {
	return &mcp.Completion{Values: []string{"alpha", "beta"}}, nil
}

func TestProxyForwardsCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	up := server.NewMCPServer("upstream", "1.0.0",
		server.WithPromptCapabilities(true),
		server.WithCompletions(),
		server.WithPromptCompletionProvider(fixedCompletion{}),
		server.WithRecovery(),
	)
	up.AddPrompt(mcp.NewPrompt("greet"), func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	upHTTP := httptest.NewServer(server.NewStreamableHTTPServer(up))
	t.Cleanup(upHTTP.Close)

	proxyURL := startProxy(ctx, t, upHTTP.URL)
	down := dialDownstream(ctx, t, proxyURL, nil)

	req := mcp.CompleteRequest{}
	req.Params.Ref = mcp.PromptReference{Type: "ref/prompt", Name: "greet"}
	req.Params.Argument = mcp.CompleteArgument{Name: "name", Value: "a"}
	res, err := down.Complete(ctx, req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(res.Completion.Values) != 2 || res.Completion.Values[0] != "alpha" {
		t.Fatalf("completion not forwarded from upstream: %#v", res.Completion.Values)
	}
}

func TestProxyCallTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	up := server.NewMCPServer("upstream", "1.0.0", server.WithToolCapabilities(true), server.WithRecovery())
	up.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-time.After(3 * time.Second):
			return mcp.NewToolResultText("done"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	upHTTP := httptest.NewServer(server.NewStreamableHTTPServer(up))
	t.Cleanup(upHTTP.Close)

	// Proxy with a 200ms per-call timeout fronting the slow upstream.
	proxyCfg := &MCPProxyConfigV2{Name: "proxy", Version: "1.0.0", Type: MCPServerTypeStreamable}
	clientCfg := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           upHTTP.URL,
		Options:       &OptionsV2{CallTimeout: "200ms"},
	}
	u, err := newUpstream("up", proxyCfg, clientCfg, mcp.Implementation{Name: "proxy"})
	if err != nil {
		t.Fatalf("newUpstream: %v", err)
	}
	if err := u.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = u.close() })
	proxyHTTP := httptest.NewServer(u.srv.handler)
	t.Cleanup(proxyHTTP.Close)

	down := dialDownstream(ctx, t, proxyHTTP.URL, nil)
	start := time.Now()
	res, err := down.CallTool(ctx, callToolReq("slow"))
	elapsed := time.Since(start)
	// A timeout surfaces either as a transport error or an error result; either
	// way it must happen well before the tool's 3s sleep.
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("expected call to time out, got success after %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("call timeout did not fire fast: took %s", elapsed)
	}
}

func TestProxyForwardsSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subscribed := make(chan string, 1)
	hooks := &server.Hooks{}
	hooks.AddBeforeSubscribe(func(_ context.Context, _ any, m *mcp.SubscribeRequest) {
		select {
		case subscribed <- m.Params.URI:
		default:
		}
	})
	up := server.NewMCPServer("upstream", "1.0.0",
		server.WithResourceCapabilities(true, true),
		server.WithHooks(hooks),
		server.WithRecovery(),
	)
	up.AddResource(mcp.NewResource("mem://x", "x"), func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: "mem://x", Text: "v"}}, nil
	})
	upHTTP := httptest.NewServer(server.NewStreamableHTTPServer(up))
	t.Cleanup(upHTTP.Close)

	proxyURL := startProxy(ctx, t, upHTTP.URL)
	down := dialDownstream(ctx, t, proxyURL, nil)

	subReq := mcp.SubscribeRequest{}
	subReq.Params.URI = "mem://x"
	if err := down.Subscribe(ctx, subReq); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	select {
	case uri := <-subscribed:
		if uri != "mem://x" {
			t.Fatalf("upstream got subscribe for %q, want mem://x", uri)
		}
	case <-ctx.Done():
		t.Fatal("subscribe never forwarded to upstream")
	}
}

func TestUpstreamConnectIsReentrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upstream, _ := startTestUpstream(t)
	proxyCfg := &MCPProxyConfigV2{Name: "proxy", Version: "1.0.0", Type: MCPServerTypeStreamable}
	clientCfg := &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: upstream.URL, Options: &OptionsV2{}}
	u, err := newUpstream("up", proxyCfg, clientCfg, mcp.Implementation{Name: "proxy"})
	if err != nil {
		t.Fatalf("newUpstream: %v", err)
	}
	t.Cleanup(func() { _ = u.close() })

	// connect twice — the reconnect path replaces the capability set rather than
	// appending, so the second connect must succeed and leave a working proxy.
	if err := u.connect(ctx); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if err := u.connect(ctx); err != nil {
		t.Fatalf("second connect (reconnect): %v", err)
	}

	proxyHTTP := httptest.NewServer(u.srv.handler)
	t.Cleanup(proxyHTTP.Close)
	down := dialDownstream(ctx, t, proxyHTTP.URL, nil)
	tools, err := down.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools after reconnect: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo_header" {
		t.Fatalf("reconnect produced wrong tool set: %#v", tools.Tools)
	}
}

func callToolReq(name string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	return req
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned error result: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("tool returned no content")
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	return tc.Text
}
