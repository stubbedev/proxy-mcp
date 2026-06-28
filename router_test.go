package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterExactSubtreeAndRemove(t *testing.T) {
	rt := newRouter()
	h := func(tag string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tag)) })
	}
	rt.Handle("/healthz", h("exact"))
	rt.Handle("/a/", h("a"))
	rt.Handle("/a/b/", h("ab")) // longer prefix must win over /a/

	cases := map[string]string{
		"/healthz": "exact",
		"/a/x":     "a",
		"/a/b/c":   "ab",
		"/nope":    "",
	}
	for path, want := range cases {
		got := serve(t, rt, path)
		if got != want {
			t.Fatalf("path %q: got %q want %q", path, got, want)
		}
	}

	// remove drops both kinds; subtree fall-through then exposes the shorter one.
	rt.remove("/a/b/")
	if got := serve(t, rt, "/a/b/c"); got != "a" {
		t.Fatalf("after removing /a/b/: got %q want fall-through to %q", got, "a")
	}
	rt.remove("/a/")
	rt.remove("/healthz")
	if got := serve(t, rt, "/a/x"); got != "" {
		t.Fatalf("after removing /a/: got %q want 404", got)
	}
	if got := serve(t, rt, "/healthz"); got != "" {
		t.Fatalf("after removing /healthz: got %q want 404", got)
	}
}

func serve(t *testing.T, rt *router, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code == http.StatusNotFound {
		return ""
	}
	return rec.Body.String()
}
