package proxy

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeRemote(t *testing.T) {
	want := "github.com/o/r"
	for _, in := range []string{
		"git@github.com:o/r.git",
		"https://github.com/o/r.git",
		"https://github.com/o/r",
		"ssh://git@github.com/o/r.git",
		"https://user:tok@github.com/o/r/",
		"GIT@GitHub.com:O/R.GIT",
	} {
		if got := normalizeRemote(in); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsRemoteURL(t *testing.T) {
	remotes := []string{"git@github.com:o/r.git", "https://github.com/o/r", "ssh://git@h/o/r"}
	locals := []string{"/Users/abs/git/repo", "./repo", "repo", "/a/b:c"}
	for _, s := range remotes {
		if !isRemoteURL(s) {
			t.Errorf("isRemoteURL(%q) = false, want true", s)
		}
	}
	for _, s := range locals {
		if isRemoteURL(s) {
			t.Errorf("isRemoteURL(%q) = true, want false", s)
		}
	}
}

// TestMatcherWorktreeAndRemote verifies a whitelist entry matches the repo, all
// its worktrees (via git common dir), and by remote URL.
func TestMatcherWorktreeAndRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init")
	run(repo, "remote", "add", "origin", "git@github.com:o/r.git")
	run(repo, "commit", "--allow-empty", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-b", "feat", worktree)

	unrelated := t.TempDir()
	run(unrelated, "init")

	// Local-dir whitelist: matches repo + its worktree, not an unrelated repo.
	local := newRepoMatcher("t", []string{repo})
	if local == nil {
		t.Fatal("expected matcher")
	}
	if !local.matches(context.Background(), []string{repo}) {
		t.Error("repo dir should match")
	}
	if !local.matches(context.Background(), []string{worktree}) {
		t.Error("worktree should match (shared git common dir)")
	}
	if local.matches(context.Background(), []string{unrelated}) {
		t.Error("unrelated repo should not match")
	}
	if local.matches(context.Background(), nil) {
		t.Error("no dirs should fail closed")
	}

	// Remote whitelist (https form) matches the ssh remote of the repo.
	remote := newRepoMatcher("t", []string{"https://github.com/o/r"})
	if !remote.matches(context.Background(), []string{worktree}) {
		t.Error("remote should match via normalized origin URL")
	}
	if remote.matches(context.Background(), []string{unrelated}) {
		t.Error("unrelated repo has no matching remote")
	}
}

// TestClientRepoDirsHeaders pins the header path of the repo gate. It is the
// only path left once a client negotiates MCP >= 2026-07-28, where roots/list
// can no longer be asked for — and because the gate fails closed, a header the
// fleet sends but this list misses would silently hide every gated tool.
func TestClientRepoDirsHeaders(t *testing.T) {
	hdr := map[string][]string{
		"X-Repo-Root": {"/w/repo"},
		"X-Mcp-Roots": {"file:///w/a, /w/b"},
		"X-Mcp-Root":  {"/w/c"},
		"X-Mcp-Cwd":   {"/w/d"},
		"X-Ignored":   {"/w/nope"},
	}
	// ss nil: no session, so roots are out of the picture entirely.
	got := clientRepoDirs(context.Background(), nil, hdr)
	want := []string{"/w/repo", "file:///w/a", "/w/b", "/w/c", "/w/d"}
	// file:// entries are converted to paths.
	want[1] = "/w/a"
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dir %d = %q, want %q", i, got[i], want[i])
		}
	}
	if dirs := clientRepoDirs(context.Background(), nil, nil); len(dirs) != 0 {
		t.Errorf("no headers should yield no dirs, got %v", dirs)
	}
}

// TestRootsUsableProtocolGate: asking a client for roots is only legal below
// 2026-07-28 (SEP-2322/2575 bans server-initiated requests from there on), and
// only if the client advertised the capability at all.
func TestRootsUsableProtocolGate(t *testing.T) {
	if rootsUsable(nil) {
		t.Error("nil session should not be roots-usable")
	}
	if rootsAllowed(nil) {
		t.Error("nil params should not be roots-usable")
	}
	caps := &mcp.ClientCapabilities{RootsV2: &mcp.RootCapabilities{}}
	for ver, want := range map[string]bool{
		"2024-11-05": true,
		"2025-11-25": true,
		"2026-07-28": false,
		"2027-03-01": false,
	} {
		ip := &mcp.InitializeParams{ProtocolVersion: ver, Capabilities: caps}
		if got := rootsAllowed(ip); got != want {
			t.Errorf("rootsAllowed(%s) = %v, want %v", ver, got, want)
		}
	}
	if rootsAllowed(&mcp.InitializeParams{ProtocolVersion: "2025-11-25"}) {
		t.Error("client without the roots capability should not be roots-usable")
	}
}
