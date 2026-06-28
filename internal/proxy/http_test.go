package proxy

import "testing"

func TestRouteForServer(t *testing.T) {
	cases := []struct {
		name       string
		basePath   string
		server     string
		serverType MCPServerType
		want       string
	}{
		{"streamable, no base", "", "chrome", MCPServerTypeStreamable, "/chrome/mcp"},
		{"streamable, with base", "/api", "chrome", MCPServerTypeStreamable, "/api/chrome/mcp"},
		{"sse, no base", "", "chrome", MCPServerTypeSSE, "/chrome/"},
		{"sse, with base", "/api", "chrome", MCPServerTypeSSE, "/api/chrome/"},
		{"empty type defaults to subtree", "", "chrome", "", "/chrome/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := routeForServer(c.basePath, c.server, c.serverType); got != c.want {
				t.Errorf("routeForServer(%q, %q, %q) = %q, want %q",
					c.basePath, c.server, c.serverType, got, c.want)
			}
		})
	}
}
