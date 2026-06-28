package proxy

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

// router is a minimal mutable HTTP router: it supports adding AND removing
// routes at runtime, which net/http.ServeMux cannot. That mutability is what
// lets the proxy mount and unmount upstream routes on a config reload without a
// restart. It mirrors the two ServeMux semantics the proxy relies on:
//
//   - an exact pattern ("/base/name/mcp", "/healthz") matches that path only;
//   - a pattern ending in "/" ("/base/name/") matches that subtree, longest
//     prefix winning, and the matched handler sees the full request path (so an
//     SSE upstream serving /sse and /message beneath its base still works).
//
// All lookups take a read lock and mutations a write lock, so routes can change
// underneath in-flight requests safely.
type router struct {
	mu      sync.RWMutex
	exact   map[string]http.Handler
	subtree []subtreeRoute
}

type subtreeRoute struct {
	prefix string
	h      http.Handler
}

// handlerMux is the subset of routing used to register the readiness probes,
// satisfied by both *router and *http.ServeMux (so the probe tests keep using a
// plain ServeMux).
type handlerMux interface {
	Handle(pattern string, handler http.Handler)
}

func newRouter() *router {
	return &router{exact: make(map[string]http.Handler)}
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mu.RLock()
	h := rt.match(r.URL.Path)
	rt.mu.RUnlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

// match resolves a path to a handler: exact wins, else the longest subtree
// prefix. Caller holds at least the read lock.
func (rt *router) match(p string) http.Handler {
	if h, ok := rt.exact[p]; ok {
		return h
	}
	for _, s := range rt.subtree {
		if strings.HasPrefix(p, s.prefix) {
			return s.h // subtree is kept sorted longest-first
		}
	}
	return nil
}

// Handle registers (or replaces) a route. A trailing "/" makes it a subtree.
// Signature matches http.ServeMux so existing registration code is unchanged.
func (rt *router) Handle(pattern string, h http.Handler) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if strings.HasSuffix(pattern, "/") {
		for i := range rt.subtree {
			if rt.subtree[i].prefix == pattern {
				rt.subtree[i].h = h
				return
			}
		}
		rt.subtree = append(rt.subtree, subtreeRoute{prefix: pattern, h: h})
		sort.Slice(rt.subtree, func(i, j int) bool {
			return len(rt.subtree[i].prefix) > len(rt.subtree[j].prefix)
		})
		return
	}
	rt.exact[pattern] = h
}

// remove unmounts a route previously added with Handle. No-op if absent.
func (rt *router) remove(pattern string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if strings.HasSuffix(pattern, "/") {
		for i := range rt.subtree {
			if rt.subtree[i].prefix == pattern {
				rt.subtree = append(rt.subtree[:i], rt.subtree[i+1:]...)
				return
			}
		}
		return
	}
	delete(rt.exact, pattern)
}
