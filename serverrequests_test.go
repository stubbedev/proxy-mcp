package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addServerRequestTools registers do_sampling/do_roots/do_elicit on an MCP
// server — each issues a server→client request and echoes the answer.
func addServerRequestTools(s *mcp.Server) {
	mcp.AddTool(
		s,
		&mcp.Tool{Name: "do_sampling"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
				Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "ping"}}},
				MaxTokens: 16,
			})
			if err != nil {
				return nil, struct{}{}, err
			}
			tc, _ := res.Content.(*mcp.TextContent)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: tc.Text}}}, struct{}{}, nil
		},
	)
	mcp.AddTool(
		s,
		&mcp.Tool{Name: "do_roots"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			res, err := req.Session.ListRoots(ctx, &mcp.ListRootsParams{})
			if err != nil {
				return nil, struct{}{}, err
			}
			uri := "no-roots"
			if len(res.Roots) > 0 {
				uri = res.Roots[0].URI
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: uri}}}, struct{}{}, nil
		},
	)
	mcp.AddTool(
		s,
		&mcp.Tool{Name: "do_elicit"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "name?",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
			})
			if err != nil {
				return nil, struct{}{}, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, struct{}{}, nil
		},
	)
}

// dialServerRequestClient connects a downstream client that answers all three
// server→client requests, so the round-trip through the proxy can be asserted.
func dialServerRequestClient(ctx context.Context, t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	cl := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "1.0.0"}, &mcp.ClientOptions{
		CreateMessageHandler: func(_ context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			in := ""
			if len(req.Params.Messages) > 0 {
				if tc, ok := req.Params.Messages[0].Content.(*mcp.TextContent); ok {
					in = tc.Text
				}
			}
			return &mcp.CreateMessageResult{Model: "test", Role: "assistant", Content: &mcp.TextContent{Text: "sampled:" + in}}, nil
		},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	cl.AddRoots(&mcp.Root{URI: "file:///workspace", Name: "ws"})
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("downstream connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// assertServerRequests drives the three tools through the proxy and checks each
// server→client request was relayed to the downstream client and answered.
func assertServerRequests(ctx context.Context, t *testing.T, clientCfg *MCPClientConfigV2) {
	t.Helper()
	proxyURL := startProxyFor(ctx, t, clientCfg)
	down := dialServerRequestClient(ctx, t, proxyURL)

	for _, c := range []struct{ tool, want string }{
		{"do_sampling", "sampled:ping"},
		{"do_roots", "file:///workspace"},
		{"do_elicit", "accept"},
	} {
		t.Run(c.tool, func(t *testing.T) {
			res, err := down.CallTool(ctx, &mcp.CallToolParams{Name: c.tool})
			if err != nil {
				t.Fatalf("call %s: %v", c.tool, err)
			}
			if got := toolText(t, res); got != c.want {
				t.Fatalf("%s relayed %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

// TestPerSessionServerRequestsStdio proves sampling/roots/elicitation bridge
// through the proxy to the originating client for a stdio upstream.
func TestPerSessionServerRequestsStdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "server-requests")
	if out, err := exec.Command("go", "build", "-o", bin, "./testdata/server-requests").CombinedOutput(); err != nil {
		t.Fatalf("build stdio upstream: %v\n%s", err, out)
	}
	assertServerRequests(ctx, t, &MCPClientConfigV2{Command: bin, Options: &OptionsV2{}})
}

// TestPerSessionServerRequestsStreamable proves the same over a streamable-HTTP
// upstream — the case mcp-go could not handle.
func TestPerSessionServerRequestsStreamable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1.0.0"}, nil)
	addServerRequestTools(srv)
	upHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(upHTTP.Close)

	assertServerRequests(ctx, t, streamableCfg(upHTTP.URL))
}
