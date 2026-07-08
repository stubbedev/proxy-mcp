package proxy

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
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
