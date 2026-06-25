package main

import (
	"context"
	"log"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// forwarder bridges the proxy server's completion/subscribe/setLevel surface to
// the upstream. It reads the live client from the upstream so it keeps working
// across reconnects (which swap the client). A nil current client (during a
// reconnect window) degrades gracefully: completions return empty, hook
// forwards are skipped.
type forwarder struct {
	up *upstream
}

// CompletePromptArgument forwards a prompt-argument completion to the upstream.
func (f forwarder) CompletePromptArgument(
	ctx context.Context,
	promptName string,
	argument mcp.CompleteArgument,
	cctx mcp.CompleteContext,
) (*mcp.Completion, error) {
	return f.complete(ctx, mcp.PromptReference{Type: "ref/prompt", Name: promptName}, argument, cctx)
}

// CompleteResourceArgument forwards a resource-template-argument completion.
func (f forwarder) CompleteResourceArgument(
	ctx context.Context,
	uri string,
	argument mcp.CompleteArgument,
	cctx mcp.CompleteContext,
) (*mcp.Completion, error) {
	return f.complete(ctx, mcp.ResourceReference{Type: "ref/resource", URI: uri}, argument, cctx)
}

func (f forwarder) complete(
	ctx context.Context,
	ref any,
	argument mcp.CompleteArgument,
	cctx mcp.CompleteContext,
) (*mcp.Completion, error) {
	c := f.up.cur()
	if c == nil {
		return &mcp.Completion{Values: []string{}}, nil
	}
	req := mcp.CompleteRequest{}
	req.Params.Ref = ref
	req.Params.Argument = argument
	req.Params.Context = cctx
	res, err := c.complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &res.Completion, nil
}

// hooks returns the proxy server hooks that forward client→server requests the
// mcp-go server otherwise handles only locally: resource subscribe/unsubscribe
// (so the upstream actually sends resources/updated) and logging/setLevel (so
// the upstream emits log notifications at the requested level). The proxy still
// does its own local bookkeeping; these run additionally, before it.
func (f forwarder) hooks() *server.Hooks {
	h := &server.Hooks{}
	h.AddBeforeSubscribe(func(ctx context.Context, _ any, m *mcp.SubscribeRequest) {
		if c := f.up.cur(); c != nil {
			if err := c.client.Subscribe(ctx, *m); err != nil {
				log.Printf("<%s> forward subscribe failed: %v", f.up.name, err)
			}
		}
	})
	h.AddBeforeUnsubscribe(func(ctx context.Context, _ any, m *mcp.UnsubscribeRequest) {
		if c := f.up.cur(); c != nil {
			if err := c.client.Unsubscribe(ctx, *m); err != nil {
				log.Printf("<%s> forward unsubscribe failed: %v", f.up.name, err)
			}
		}
	})
	h.AddBeforeSetLevel(func(ctx context.Context, _ any, m *mcp.SetLevelRequest) {
		if c := f.up.cur(); c != nil {
			if err := c.client.SetLevel(ctx, *m); err != nil {
				log.Printf("<%s> forward setLevel failed: %v", f.up.name, err)
			}
		}
	})
	return h
}
