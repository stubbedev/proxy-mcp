package proxy

import (
	"context"
	"log"
	"net"
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
// are normalized URLs matched against the client repo's configured remotes;
// host entries match every repo hosted on a given git host.
type repoMatcher struct {
	commonDirs map[string]struct{} // resolved git common dirs of local entries
	remotes    map[string]struct{} // normalized remote URLs
	hosts      map[string]struct{} // bare git hosts, port-stripped
}

// newRepoMatcher builds a matcher from whitelist entries, resolving each local
// path's git common dir now (the path exists at config-load time). An entry
// that names a whole git host becomes a host pattern; one that looks like a git
// remote is normalized; the rest are treated as local dirs. Returns nil when
// the list is empty (no gating).
//
// A local path is tried BEFORE the host reading, so a directory that happens to
// look like a hostname keeps its old meaning.
func newRepoMatcher(name string, entries []string) *repoMatcher {
	if len(entries) == 0 {
		return nil
	}
	m := &repoMatcher{
		commonDirs: make(map[string]struct{}),
		remotes:    make(map[string]struct{}),
		hosts:      make(map[string]struct{}),
	}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if isRemoteURL(e) {
			if h := hostEntry(e); h != "" {
				m.hosts[h] = struct{}{}
			} else {
				m.remotes[normalizeRemote(e)] = struct{}{}
			}
			continue
		}
		if cd := gitCommonDir(context.Background(), e); cd != "" {
			m.commonDirs[cd] = struct{}{}
			continue
		}
		if h := hostEntry(e); h != "" {
			m.hosts[h] = struct{}{}
			continue
		}
		log.Printf("<%s> repoWhitelist entry %q is neither a git repo, a remote URL nor a git host; ignoring", name, e)
	}
	// A configured whitelist whose entries all turned out unusable — empty
	// strings from an unexpanded ${VAR}, a path that is not a repo — must gate
	// EVERYTHING off, not silently un-gate the upstream. Returning nil here
	// would make an operator's missing secret read as "no whitelist".
	if len(m.commonDirs) == 0 && len(m.remotes) == 0 && len(m.hosts) == 0 {
		log.Printf("<%s> repoWhitelist has %d entries but none resolved; gating all clients out", name, len(entries))
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
		if len(m.remotes) > 0 || len(m.hosts) > 0 {
			for _, r := range gitRemotes(ctx, dir) {
				if _, ok := m.remotes[r]; ok {
					return true
				}
				if _, ok := m.hosts[remoteHost(r)]; ok {
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

// hostEntry reports the bare git host a whitelist entry gates, or "" when the
// entry names one specific repo instead. Both an explicit URL with no path
// ("https://git.example.com") and a bare "git.example.com[:port]" name a host;
// anything carrying a path component ("github.com/o/r") names a repo. The port
// is stripped so it matches remotes regardless of how they are cloned — an ssh
// remote on :7999 and its https twin on :443 are the same host.
func hostEntry(e string) string {
	s := strings.ToLower(strings.TrimSpace(e))
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || strings.Trim(u.Path, "/") != "" {
			return ""
		}
		return stripPort(u.Host)
	}
	// Bare form. Reject anything with a path or scp-style separator, and
	// require a dotted name so a local dir like "." or "repo" is never read
	// as a host.
	if strings.ContainsAny(s, "/@") || !strings.Contains(s, ".") ||
		strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return ""
	}
	return stripPort(s)
}

// remoteHost returns the port-stripped host of a key from normalizeRemote
// ("git.example.com:7999/o/r" -> "git.example.com").
func remoteHost(key string) string {
	host, _, _ := strings.Cut(key, "/")
	return stripPort(host)
}

// stripPort drops a trailing :port and any IPv6 brackets, leaving a bare host
// untouched.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
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

// repoHeaders are the request headers a client (or a fronting harness) may use
// to declare its workspace, in the order they are collected. X-Repo-Root is the
// convention the rest of this fleet already reads (jenkins-mcp, srv, treeman);
// the X-Mcp-* forms predate it and stay supported.
var repoHeaders = []string{"X-Repo-Root", "X-Mcp-Roots", "X-Mcp-Root", "X-Mcp-Cwd"}

// clientRepoDirs collects the client's candidate workspace directories from the
// repoHeaders and, for clients that still allow it, its MCP roots, as
// filesystem paths. file:// root URIs are converted to paths; non-file roots
// are ignored (they can't be a local repo).
//
// Headers are the durable signal: MCP 2026-07-28 (SEP-2322/2575) forbids
// server-initiated requests, so roots/list simply stops being answerable. This
// gate fails CLOSED, so a client that only ever spoke roots would silently lose
// every whitelisted tool — hence rootsUsable, and hence X-Repo-Root above.
func clientRepoDirs(ctx context.Context, ss *mcp.ServerSession, hdr map[string][]string) []string {
	var dirs []string
	if rootsUsable(ss) {
		if res, err := ss.ListRoots(ctx, nil); err == nil {
			for _, r := range res.Roots {
				if p := fileURIToPath(r.URI); p != "" {
					dirs = append(dirs, p)
				}
			}
		}
	}
	for _, h := range repoHeaders {
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

// rootsRemovedFrom is the first protocol revision that forbids server-initiated
// JSON-RPC requests (SEP-2322 / SEP-2575). From there on roots/list can only be
// requested as an InputRequest from a tool handler, so the plain call below is
// dead weight. ISO dates compare correctly as strings.
const rootsRemovedFrom = "2026-07-28"

// rootsUsable reports whether the proxy may still ask this downstream client
// for its roots: it advertised the capability, and it negotiated a protocol
// version where a server is allowed to ask.
func rootsUsable(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	return rootsAllowed(ss.InitializeParams())
}

// rootsAllowed is rootsUsable's decision, split out so it can be tested without
// a live session.
func rootsAllowed(ip *mcp.InitializeParams) bool {
	if ip == nil || ip.Capabilities == nil || ip.ProtocolVersion >= rootsRemovedFrom {
		return false
	}
	return ip.Capabilities.RootsV2 != nil
}
