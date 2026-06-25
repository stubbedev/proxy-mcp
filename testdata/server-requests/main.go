// Command server-requests is a stdio MCP server used by TestPerSessionServerRequests.
// Each tool issues a server→client request (sampling, roots, elicitation) and
// echoes what the client returned, so the test can prove the proxy bridged the
// request to the right downstream client over a per-session connection.
package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "server-requests", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "do_sampling"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "ping"}}},
			MaxTokens: 16,
		})
		if err != nil {
			return errResult(err), struct{}{}, nil
		}
		tc, _ := res.Content.(*mcp.TextContent)
		return textResult(tc.Text), struct{}{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{Name: "do_roots"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		res, err := req.Session.ListRoots(ctx, &mcp.ListRootsParams{})
		if err != nil {
			return errResult(err), struct{}{}, nil
		}
		if len(res.Roots) == 0 {
			return textResult("no-roots"), struct{}{}, nil
		}
		return textResult(res.Roots[0].URI), struct{}{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{Name: "do_elicit"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
			Message: "name?",
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
			},
		})
		if err != nil {
			return errResult(err), struct{}{}, nil
		}
		return textResult(res.Action), struct{}{}, nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		panic(err)
	}
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
