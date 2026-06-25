package main

import (
	"context"
	"maps"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// clientHeadersKey carries the downstream client's HTTP request headers through
// the request context so an upstream HTTP/SSE call can replay them verbatim.
type clientHeadersKey struct{}

// withClientHeaders stashes a clone of the downstream request's headers in ctx.
// Wired as the proxy server's HTTPContextFunc, so every inbound MCP request
// carries the caller's headers down to the upstream transport.
func withClientHeaders(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, clientHeadersKey{}, r.Header.Clone())
}

// hopByHopHeaders are connection-scoped headers the HTTP layer regenerates for
// the upstream hop; replaying them would corrupt the new request. This is the
// same set net/http and httputil.ReverseProxy manage themselves — everything
// else is forwarded untouched, so the upstream sees the caller's headers as if
// no proxy sat in between.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Host":                {},
	"Content-Length":      {},
}

// forwardedHeaders pulls the stashed downstream headers from ctx and returns
// them for the upstream transport's per-request header func. It forwards every
// header except the hop-by-hop framing set above. Multi-valued headers are
// comma-joined (RFC 9110 §5.3) because mcp-go's header func is single-valued;
// realistic request headers (incl. Cookie) carry a single combined value.
// Returns nil when no headers were stashed (e.g. a stdio downstream), leaving
// the upstream request untouched.
func forwardedHeaders(ctx context.Context) map[string]string {
	hdr, ok := ctx.Value(clientHeadersKey{}).(http.Header)
	if !ok || len(hdr) == 0 {
		return nil
	}
	out := make(map[string]string, len(hdr))
	for k, vs := range hdr {
		if _, hop := hopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		if len(vs) > 0 {
			out[k] = strings.Join(vs, ", ")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// notificationParams flattens a relayed notification's params back into the
// map[string]any shape SendNotificationToAllClients expects, preserving both
// the reserved _meta field and any additional fields.
func notificationParams(n mcp.JSONRPCNotification) map[string]any {
	params := make(map[string]any, len(n.Params.AdditionalFields)+1)
	maps.Copy(params, n.Params.AdditionalFields)
	if n.Params.Meta != nil {
		params["_meta"] = n.Params.Meta
	}
	if len(params) == 0 {
		return nil
	}
	return params
}
