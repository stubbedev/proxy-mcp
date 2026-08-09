// Command server-requests is a stdio MCP server used by
// TestPerSessionServerRequestsStdio. Each tool needs something only the client
// can supply (a sample, its roots, an elicited answer) and echoes what came
// back, so the test can prove the proxy bridged the exchange to the right
// downstream client over a per-session connection.
//
// It asks the MCP >= 2026-07-28 way: the handler returns an InputRequests map
// (SEP-2322) and is called again with the answers in Params.InputResponses.
// The older way — a server→client JSON-RPC request mid-handler — is what the
// streamable sibling test still covers; this transport negotiates the new
// protocol, where that call is refused outright.
package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reqID is the key each tool files its single input request under, and the key
// the answer comes back on.
const reqID = "ask"

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "server-requests", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "do_sampling"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		// A sampling answer always decodes to the WithTools shape (the SDK
		// discriminates on the "role" key, and that form is the superset).
		if answer, ok := req.Params.InputResponses[reqID]; ok {
			res, ok := answer.(*mcp.CreateMessageWithToolsResult)
			if !ok {
				return errResult(fmt.Errorf("want *CreateMessageWithToolsResult, got %T", answer)), struct{}{}, nil
			}
			if len(res.Content) == 0 {
				return errResult(fmt.Errorf("sampling answer had no content")), struct{}{}, nil
			}
			tc, _ := res.Content[0].(*mcp.TextContent)
			return textResult(tc.Text), struct{}{}, nil
		}
		return ask(&mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "ping"}}},
			MaxTokens: 16,
		}), struct{}{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{Name: "do_roots"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		if answer, ok := req.Params.InputResponses[reqID]; ok {
			res, ok := answer.(*mcp.ListRootsResult)
			if !ok {
				return errResult(fmt.Errorf("want *ListRootsResult, got %T", answer)), struct{}{}, nil
			}
			if len(res.Roots) == 0 {
				return textResult("no-roots"), struct{}{}, nil
			}
			return textResult(res.Roots[0].URI), struct{}{}, nil
		}
		return ask(&mcp.ListRootsParams{}), struct{}{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{Name: "do_elicit"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		if answer, ok := req.Params.InputResponses[reqID]; ok {
			res, ok := answer.(*mcp.ElicitResult)
			if !ok {
				return errResult(fmt.Errorf("want *ElicitResult, got %T", answer)), struct{}{}, nil
			}
			return textResult(res.Action), struct{}{}, nil
		}
		return ask(&mcp.ElicitParams{
			Message: "name?",
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
			},
		}), struct{}{}, nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		panic(err)
	}
}

// ask returns the input-required result that hands one request to the client.
// RequestState is what the client must echo back on the retry; a fixed value is
// enough here because each tool has exactly one outstanding question.
func ask(p mcp.InputRequest) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{reqID: p},
		RequestState:  "awaiting-" + reqID,
	}
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
