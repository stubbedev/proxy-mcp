package proxy

import (
	"context"
	"log"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gitTimeout bounds each git invocation so a hung repo (e.g. on a stalled
// network mount) can't wedge a list request.
const gitTimeout = 5 * time.Second

// repoMatcher decides whether a downstream client's workspace belongs to a
// whitelisted repo. Local whitelist entries are resolved to their git common
// dir (so the repo and all its worktrees share one identity); remote entries
// are normalized URLs matched against the client repo's configured remotes.
type repoMatcher struct {
	commonDirs map[string]struct{} // resolved git common dirs of local entries
	remotes    map[string]struct{} // normalized remote URLs
}

// newRepoMatcher builds a matcher from whitelist entries, resolving each local
// path's git common dir now (the path exists at config-load time). Entries that
// look like git remotes are normalized; the rest are treated as local dirs.
// Returns nil when the list is empty (no gating).
func newRepoMatcher(name string, entries []string) *repoMatcher {
	if len(entries) == 0 {
		return nil
	}
	m := &repoMatcher{
		commonDirs: make(map[string]struct{}),
		remotes:    make(map[string]struct{}),
	}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if isRemoteURL(e) {
			m.remotes[normalizeRemote(e)] = struct{}{}
			continue
		}
		cd := gitCommonDir(context.Background(), e)
		if cd == "" {
			log.Printf("<%s> repoWhitelist entry %q is not a git repo; ignoring", name, e)
			continue
		}
		m.commonDirs[cd] = struct{}{}
	}
	if len(m.commonDirs) == 0 && len(m.remotes) == 0 {
		return nil
	}
	return m
}

// matches reports whether any of the client's workspace dirs resolves to a
// whitelisted repo (by git common dir or by remote URL).
func (m *repoMatcher) matches(ctx context.Context, dirs []string) bool {
	for _, dir := range dirs {
		if cd := gitCommonDir(ctx, dir); cd != "" {
			if _, ok := m.commonDirs[cd]; ok {
				return true
			}
		}
		if len(m.remotes) > 0 {
			for _, r := range gitRemotes(ctx, dir) {
				if _, ok := m.remotes[r]; ok {
					return true
				}
			}
		}
	}
	return false
}

// isRemoteURL reports whether a whitelist entry denotes a git remote rather
// than a local path: an explicit scheme (https://, ssh://, git://) or the
// scp-like "user@host:path" form.
func isRemoteURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	// scp-like: user@host:path, with the colon before any slash.
	if at := strings.IndexByte(s, '@'); at > 0 {
		if colon := strings.IndexByte(s, ':'); colon > at {
			if slash := strings.IndexByte(s, '/'); slash == -1 || colon < slash {
				return true
			}
		}
	}
	return false
}

// normalizeRemote reduces a git remote URL to a scheme/credential/suffix-
// agnostic "host/path" key, so ssh and https forms of the same repo compare
// equal: git@github.com:o/r.git and https://github.com/o/r both -> github.com/o/r.
func normalizeRemote(raw string) string {
	s := raw
	// scp-like git@host:path -> host/path
	if !strings.Contains(s, "://") {
		if at := strings.IndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		s = strings.Replace(s, ":", "/", 1)
	} else {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			s = u.Host + u.Path
		}
	}
	s = strings.ToLower(s)
	return strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
}

// gitCommonDir returns the absolute git common dir for a directory, or "" if it
// is not inside a git repo. The common dir is shared by a repo and all its
// worktrees, so it is a stable per-repo identity.
func gitCommonDir(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

// gitRemotes returns the normalized URLs of every remote configured in dir's
// repo (empty if none / not a repo).
func gitRemotes(ctx context.Context, dir string) []string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return nil
	}
	var res []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		// line: "remote.origin.url git@github.com:o/r.git"
		if _, urlPart, ok := strings.Cut(line, " "); ok {
			res = append(res, normalizeRemote(strings.TrimSpace(urlPart)))
		}
	}
	return res
}

// clientRepoDirs collects the client's candidate workspace directories from its
// MCP roots (fetched on demand) and the X-Mcp-Roots / X-Mcp-Cwd headers, as
// filesystem paths. file:// root URIs are converted to paths; non-file roots
// are ignored (they can't be a local repo).
func clientRepoDirs(ctx context.Context, ss *mcp.ServerSession, hdr map[string][]string) []string {
	var dirs []string
	if ss != nil {
		if res, err := ss.ListRoots(ctx, nil); err == nil {
			for _, r := range res.Roots {
				if p := fileURIToPath(r.URI); p != "" {
					dirs = append(dirs, p)
				}
			}
		}
	}
	for _, h := range []string{"X-Mcp-Roots", "X-Mcp-Cwd"} {
		for _, v := range hdr[h] {
			for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' }) {
				part = strings.TrimSpace(part)
				if p := fileURIToPath(part); p != "" {
					dirs = append(dirs, p)
				} else if part != "" {
					dirs = append(dirs, part)
				}
			}
		}
	}
	return dirs
}

// fileURIToPath converts a file:// URI to a local path, returning "" for a
// non-file URI. A plain path is returned unchanged only via the caller.
func fileURIToPath(s string) string {
	if !strings.HasPrefix(s, "file://") {
		return ""
	}
	if u, err := url.Parse(s); err == nil {
		return u.Path
	}
	return ""
}
