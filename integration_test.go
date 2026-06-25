package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startUpstreamHTTP starts an MCP server over streamable HTTP for tests. The
// configure callback registers tools/prompts on it.
func startUpstreamHTTP(t *testing.T, name string, configure func(*mcp.Server)) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil)
	configure(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// startProxyFor connects the proxy to one upstream and serves it over HTTP.
func startProxyFor(ctx context.Context, t *testing.T, clientCfg *MCPClientConfigV2) string {
	t.Helper()
	proxyCfg := &MCPProxyConfigV2{Name: "proxy", Version: "1.0.0", Type: MCPServerTypeStreamable}
	up := newUpstream("up", proxyCfg, clientCfg)
	if err := up.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(up.close)
	ps := httptest.NewServer(mcpHandler(MCPServerTypeStreamable, up.server))
	t.Cleanup(ps.Close)
	return ps.URL
}

// headerRT adds fixed headers to every downstream→proxy request.
type headerRT struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	b := h.base
	if b == nil {
		b = http.DefaultTransport
	}
	return b.RoundTrip(req)
}

// dialDownstream connects a client to the proxy with optional headers.
func dialDownstream(ctx context.Context, t *testing.T, url string, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url}
	if len(headers) > 0 {
		tr.HTTPClient = &http.Client{Transport: headerRT{headers: headers}}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "1.0.0"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("downstream connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func streamableCfg(url string) *MCPClientConfigV2 {
	return &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: url, Options: &OptionsV2{}}
}

func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want text content, got %T", res.Content[0])
	}
	return tc.Text
}

func TestProxyForwardsHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	up := startUpstreamHTTP(t, "up", func(s *mcp.Server) {
		mcp.AddTool(
			s,
			&mcp.Tool{Name: "echo_header"},
			func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
				v := ""
				if req.Extra != nil {
					v = req.Extra.Header.Get("X-Test")
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: v}}}, struct{}{}, nil
			},
		)
	})
	proxyURL := startProxyFor(ctx, t, streamableCfg(up.URL))
	down := dialDownstream(ctx, t, proxyURL, map[string]string{"X-Test": "hello", "Authorization": "Bearer tok"})

	res, err := down.CallTool(ctx, &mcp.CallToolParams{Name: "echo_header"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := toolText(t, res); got != "hello" {
		t.Fatalf("header passthrough: upstream saw %q, want hello", got)
	}
}

func TestProxyRelaysToolsListChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var srv *mcp.Server
	up := startUpstreamHTTP(t, "up", func(s *mcp.Server) {
		srv = s
		mcp.AddTool(
			s,
			&mcp.Tool{Name: "first"},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				return &mcp.CallToolResult{}, struct{}{}, nil
			},
		)
	})
	proxyURL := startProxyFor(ctx, t, streamableCfg(up.URL))
	down := dialDownstream(ctx, t, proxyURL, nil)

	// Add a tool upstream → upstream emits list_changed → proxy re-registers.
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "late_tool"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		},
	)

	// The new tool becomes visible through the proxy.
	deadline := time.Now().Add(5 * time.Second)
	for {
		tools, err := down.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		found := false
		for _, tool := range tools.Tools {
			if tool.Name == "late_tool" {
				found = true
			}
		}
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("late_tool never propagated through proxy")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProxyForwardsCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Upstream with a completion handler (needs ServerOptions, so built inline).
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1.0.0"}, &mcp.ServerOptions{
		CompletionHandler: func(context.Context, *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{"alpha", "beta"}}}, nil
		},
	})
	srv.AddPrompt(&mcp.Prompt{Name: "greet"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	upHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(upHTTP.Close)

	proxyURL := startProxyFor(ctx, t, streamableCfg(upHTTP.URL))
	down := dialDownstream(ctx, t, proxyURL, nil)

	res, err := down.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greet"},
		Argument: mcp.CompleteParamsArgument{Name: "name", Value: "a"},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(res.Completion.Values) != 2 || res.Completion.Values[0] != "alpha" {
		t.Fatalf("completion not forwarded: %#v", res.Completion.Values)
	}
}

func TestProxyCallTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	up := startUpstreamHTTP(t, "up", func(s *mcp.Server) {
		mcp.AddTool(
			s,
			&mcp.Tool{Name: "slow"},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
				select {
				case <-time.After(3 * time.Second):
					return &mcp.CallToolResult{}, struct{}{}, nil
				case <-ctx.Done():
					return nil, struct{}{}, ctx.Err()
				}
			},
		)
	})
	cfg := streamableCfg(up.URL)
	cfg.Options.CallTimeout = "200ms"
	proxyURL := startProxyFor(ctx, t, cfg)
	down := dialDownstream(ctx, t, proxyURL, nil)

	start := time.Now()
	res, err := down.CallTool(ctx, &mcp.CallToolParams{Name: "slow"})
	elapsed := time.Since(start)
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("expected timeout, got success after %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("call timeout slow: %s", elapsed)
	}
}
