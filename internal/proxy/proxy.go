package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- header forwarding ----------

// forwardedHeadersKey carries the downstream client's HTTP headers through the
// call context so the upstream HTTP transport can replay them verbatim.
type forwardedHeadersKey struct{}

func ctxWithHeaders(ctx context.Context, h http.Header) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, forwardedHeadersKey{}, h)
}

// hopByHopHeaders are per-hop headers regenerated for the upstream hop;
// replaying them would corrupt the request. This is the RFC hop-by-hop set
// (managed by net/http) plus the MCP streamable transport-control headers,
// which scope a session/protocol to one hop — forwarding the downstream hop's
// Mcp-Session-Id onto the upstream would hijack the wrong session.
var hopByHopHeaders = map[string]struct{}{
	"Connection":           {},
	"Proxy-Connection":     {},
	"Keep-Alive":           {},
	"Proxy-Authenticate":   {},
	"Proxy-Authorization":  {},
	"Te":                   {},
	"Trailer":              {},
	"Transfer-Encoding":    {},
	"Upgrade":              {},
	"Host":                 {},
	"Content-Length":       {},
	"Mcp-Session-Id":       {},
	"Mcp-Protocol-Version": {},
}

// headerRoundTripper applies the per-server static headers and then replays the
// downstream caller's headers (from the call context) onto each upstream
// request — every header, multi-value preserved, except the hop-by-hop set. So
// an HTTP/SSE upstream sees the caller's headers as if no proxy sat between.
type headerRoundTripper struct {
	base   http.RoundTripper
	static map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.static {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	if hdr, ok := req.Context().Value(forwardedHeadersKey{}).(http.Header); ok {
		for k, vs := range hdr {
			if _, hop := hopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
				continue
			}
			req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// buildTransport turns a client config into an SDK transport. stdio upstreams
// run a command; HTTP/SSE upstreams get a header-forwarding round tripper.
func buildTransport(ctx context.Context, conf *MCPClientConfigV2) (mcp.Transport, error) {
	parsed, err := parseMCPClientConfigV2(conf)
	if err != nil {
		return nil, err
	}
	switch v := parsed.(type) {
	case *StdioMCPClientConfig:
		cmd := exec.CommandContext(ctx, v.Command, v.Args...)
		cmd.Env = os.Environ()
		for k, val := range v.Env {
			cmd.Env = append(cmd.Env, k+"="+val)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case *StreamableMCPClientConfig:
		return &mcp.StreamableClientTransport{
			Endpoint:   v.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{static: v.Headers}},
		}, nil
	case *SSEMCPClientConfig:
		return &mcp.SSEClientTransport{
			Endpoint:   v.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{static: v.Headers}},
		}, nil
	}
	return nil, errors.New("invalid client type")
}

// ---------- upstream ----------

// upstream owns one backend end to end: a persistent proxy *mcp.Server (its
// handler is mounted once) plus upstream client sessions. A "template" session
// enumerates the upstream's capabilities, relays its notifications, and is the
// fallback. In per-session mode (default) each downstream client also gets a
// dedicated upstream session whose sampling/roots/elicitation handlers target
// that client 1:1 — full server→client bridging. In shared mode every client
// multiplexes onto the template (one backend process), without server→client
// requests.
type upstream struct {
	name        string
	clientCfg   *MCPClientConfigV2
	info        *mcp.Implementation
	server      *mcp.Server
	perSession  bool
	callTimeout time.Duration
	matcher     *repoMatcher // nil unless repoWhitelist is set

	// Lazy lifecycle (idleTimeout > 0). The backend is connected on the first
	// request and torn down after idleTimeout of silence, then re-connected on
	// demand. connMu guards the connected/connCancel transition so concurrent
	// first requests serialize and exactly one connect/teardown wins.
	lazy        bool
	idleTimeout time.Duration
	activity    *activityTracker
	connMu      sync.Mutex
	connected   bool
	connCancel  context.CancelFunc

	// gen labels the current connection generation. teardown bumps it so a
	// per-session dial that was in flight when the backend was torn down is
	// discarded instead of being stored against a dead generation. atomic so
	// clientFor can snapshot it without ordering against connMu.
	gen atomic.Uint64

	mu       sync.RWMutex
	baseCtx  context.Context
	tmpl     *sessConn
	sessions map[string]*sessConn

	regMu            sync.Mutex // serializes registerCapabilities
	regTools         []string
	regPrompts       []string
	regResources     []string
	regResourceTmpls []string
}

// sessConn is one upstream connection. The client handle is retained alongside
// the session so its advertised roots can be updated (AddRoots/RemoveRoots) when
// the downstream client's roots change.
type sessConn struct {
	cl       *mcp.Client
	cs       *mcp.ClientSession
	rootURIs []string
}

func newUpstream(name string, proxyCfg *MCPProxyConfigV2, clientCfg *MCPClientConfigV2) *upstream {
	u := &upstream{
		name:        name,
		clientCfg:   clientCfg,
		info:        &mcp.Implementation{Name: proxyCfg.Name, Version: proxyCfg.Version},
		perSession:  clientCfg.Options.perSession(),
		callTimeout: clientCfg.Options.callTimeout(),
		idleTimeout: clientCfg.Options.idleTimeout(),
		matcher:     newRepoMatcher(name, clientCfg.Options.RepoWhitelist),
		sessions:    make(map[string]*sessConn),
	}
	if u.idleTimeout > 0 {
		u.lazy = true
		u.activity = newActivityTracker()
	}
	u.server = mcp.NewServer(u.info, &mcp.ServerOptions{
		HasTools:                true,
		CompletionHandler:       u.handleComplete,
		SubscribeHandler:        u.handleSubscribe,
		UnsubscribeHandler:      u.handleUnsubscribe,
		RootsListChangedHandler: u.handleRootsChanged,
	})
	// Forward logging/setLevel to the caller's upstream connection.
	u.server.AddReceivingMiddleware(u.serverMiddleware)
	// Gate advertised capabilities by the client's repo when a whitelist is set.
	if u.matcher != nil {
		u.server.AddReceivingMiddleware(u.gateMiddleware)
	}
	return u
}

// connectTimeout bounds the upstream MCP handshake (initialize + capability
// enumeration), so one backend that hangs on startup can never block readiness
// or wedge a request forever — it fails its own route and leaves siblings
// untouched. It bounds only the handshake, not the backend process lifetime.
const connectTimeout = 30 * time.Second

// safeGo runs fn in a goroutine that recovers from panics. Every background
// task a backend can drive (template watch, capability re-registration, session
// sweep, idle teardown) goes through here, so a misbehaving upstream that
// panics in one of them is logged and contained instead of crashing the whole
// proxy and taking its siblings down.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("<%s> recovered from panic in background task: %v", name, r)
			}
		}()
		fn()
	}()
}

// ensureConnected lazily establishes the upstream on first use and (re)arms its
// idle teardown timer. Cheap and idempotent once connected (just records
// activity). Serialized by connMu so concurrent first requests start exactly
// one backend. On connect failure it returns the error (the route answers 503)
// without affecting any other upstream.
func (u *upstream) ensureConnected(ctx context.Context) error {
	u.connMu.Lock()
	defer u.connMu.Unlock()
	if u.connected {
		u.activity.touch()
		return nil
	}
	// connCtx scopes this connection generation: teardown cancels it to stop the
	// backend process, the template watcher, and the idle monitor together,
	// while leaving ctx (the server lifetime) intact for the next reconnect.
	connCtx, cancel := context.WithCancel(ctx)
	if err := u.connect(connCtx); err != nil {
		cancel()
		return err
	}
	u.connCancel = cancel
	u.connected = true
	u.activity.touch()
	u.activity.monitorIdle(connCtx, u.idleTimeout, u.teardown)
	log.Printf("<%s> connected on demand", u.name)
	return nil
}

// teardown stops the lazy upstream's backend process and connections after it
// has gone idle, and re-arms lazy connect for the next request. Idempotent.
// Cancelling connCtx first makes the template watcher treat the close as
// intentional (it sees ctx.Err() and does not reconnect).
func (u *upstream) teardown() {
	u.connMu.Lock()
	defer u.connMu.Unlock()
	if !u.connected {
		return
	}
	log.Printf("<%s> idle %s, stopping backend", u.name, u.idleTimeout)
	if u.connCancel != nil {
		u.connCancel()
		u.connCancel = nil
	}
	u.connected = false
	// Advance the generation BEFORE close() empties the session map, so any
	// per-session dial still in flight commits against the old generation and
	// is rejected instead of stranded in the freshly emptied map.
	u.gen.Add(1)
	u.close()
}

// isConnected reports whether a lazy upstream currently has a live backend.
func (u *upstream) isConnected() bool {
	u.connMu.Lock()
	defer u.connMu.Unlock()
	return u.connected
}

// connect establishes the template session and registers the upstream's
// capabilities on the proxy server. Re-callable (reconnect): Set/Remove make
// registration idempotent.
func (u *upstream) connect(ctx context.Context) error {
	sc, err := u.dial(ctx, nil)
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.baseCtx = ctx
	u.tmpl = sc
	u.mu.Unlock()
	u.registerCapabilities(ctx)
	safeGo(u.name, func() { u.watchTemplate(ctx, sc.cs) })
	if u.perSession {
		safeGo(u.name, func() { u.sweepSessions(ctx) })
	}
	return nil
}

// watchTemplate reconnects the template if its session ends while the proxy is
// still running.
func (u *upstream) watchTemplate(ctx context.Context, cs *mcp.ClientSession) {
	_ = cs.Wait()
	if ctx.Err() != nil {
		return
	}
	log.Printf("<%s> template connection lost; reconnecting", u.name)
	u.reconnect(ctx)
}

func (u *upstream) reconnect(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := u.connect(ctx); err != nil {
			log.Printf("<%s> reconnect failed: %v", u.name, err)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		log.Printf("<%s> reconnected", u.name)
		return
	}
}

// dial opens an upstream client session. When downstream is non-nil the session
// is dedicated to that client: sampling/elicitation handlers and roots/list +
// notification relay all target it. The template (downstream nil) only
// enumerates and broadcasts list-changed.
func (u *upstream) dial(ctx context.Context, downstream *mcp.ServerSession) (*sessConn, error) {
	opts := &mcp.ClientOptions{}
	if downstream != nil {
		opts.CreateMessageHandler = func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return downstream.CreateMessage(ctx, req.Params)
		}
		opts.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return downstream.Elicit(ctx, req.Params)
		}
	}
	cl := mcp.NewClient(u.info, opts)
	cl.AddReceivingMiddleware(u.clientMiddleware(downstream))
	sc := &sessConn{cl: cl}
	// Mirror the downstream client's workspace roots onto this connection before
	// connecting, so the upstream sees the right roots from initialize on.
	if downstream != nil {
		u.syncRoots(ctx, downstream, sc)
	}
	// buildTransport gets the long-lived ctx (a stdio child's lifetime is tied
	// to it); only the handshake below is bounded by connectTimeout, so a hung
	// backend fails its own connect instead of blocking forever.
	tr, err := buildTransport(ctx, u.clientCfg)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cs, err := cl.Connect(dialCtx, tr, nil)
	if err != nil {
		return nil, err
	}
	sc.cs = cs
	return sc, nil
}

// template returns the control-plane session.
func (u *upstream) template() *mcp.ClientSession {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.tmpl == nil {
		return nil
	}
	return u.tmpl.cs
}

// clientFor resolves the upstream session a request should use: the caller's
// dedicated session in per-session mode (created on first use), else template.
func (u *upstream) clientFor(ss *mcp.ServerSession) *mcp.ClientSession {
	if !u.perSession || ss == nil {
		return u.template()
	}
	id := ss.ID()
	u.mu.RLock()
	sc := u.sessions[id]
	base := u.baseCtx
	u.mu.RUnlock()
	if sc != nil {
		return sc.cs
	}
	// Snapshot the generation we're dialing under. If a teardown bumps it while
	// we dial, commitSession discards this connection rather than stranding it.
	gen := u.gen.Load()
	sc, err := u.dial(base, ss)
	if err != nil {
		log.Printf("<%s> per-session connect failed for %s: %v (using template)", u.name, id, err)
		return u.template()
	}
	if cs := u.commitSession(id, sc, gen); cs != nil {
		return cs
	}
	// The backend was torn down while we dialed; fall back to the template
	// (which is nil after teardown, so sessionFor returns a clean error and the
	// client retries into a fresh revive).
	log.Printf("<%s> per-session connect raced teardown for %s; using template", u.name, id)
	return u.template()
}

// commitSession stores a freshly dialed per-session connection unless the
// connection generation advanced while dialing (a teardown raced the dial) or a
// concurrent dial already won. In both cases the surplus connection is closed
// and the existing/template path is used instead. Returns the session to use,
// or nil when the generation is stale.
func (u *upstream) commitSession(id string, sc *sessConn, gen uint64) *mcp.ClientSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.gen.Load() != gen {
		u.closeConn(sc)
		return nil
	}
	if existing := u.sessions[id]; existing != nil {
		u.closeConn(sc)
		return existing.cs
	}
	u.sessions[id] = sc
	log.Printf("<%s> opened per-session connection for %s", u.name, id)
	return sc.cs
}

// closeConn closes a session connection, tolerating a nil session (a dial that
// failed before establishing one).
func (u *upstream) closeConn(sc *sessConn) {
	if sc != nil && sc.cs != nil {
		_ = sc.cs.Close()
	}
}

// sessionFor resolves the upstream session for a request, returning an error
// instead of a nil session when the upstream has no live connection (e.g. a
// crashed backend mid-reconnect). Callers forward the error to the downstream
// client as a clean failure rather than dereferencing nil and panicking.
func (u *upstream) sessionFor(ss *mcp.ServerSession) (*mcp.ClientSession, error) {
	if cs := u.clientFor(ss); cs != nil {
		return cs, nil
	}
	return nil, fmt.Errorf("upstream %q is not connected", u.name)
}

// syncRoots mirrors the downstream client's workspace roots onto sc's upstream
// connection. AddRoots/RemoveRoots notify a connected upstream (roots/list_
// changed), so an upstream that caches roots re-fetches and stays correct.
func (u *upstream) syncRoots(ctx context.Context, downstream *mcp.ServerSession, sc *sessConn) {
	res, err := downstream.ListRoots(ctx, &mcp.ListRootsParams{})
	if err != nil {
		return // downstream doesn't support roots — nothing to mirror
	}
	uris := make([]string, 0, len(res.Roots))
	for _, r := range res.Roots {
		uris = append(uris, r.URI)
	}
	u.mu.Lock()
	old := sc.rootURIs
	sc.rootURIs = uris
	u.mu.Unlock()
	if gone := missing(old, uris); len(gone) > 0 {
		sc.cl.RemoveRoots(gone...)
	}
	if len(res.Roots) > 0 {
		sc.cl.AddRoots(res.Roots...)
	}
}

// handleRootsChanged re-mirrors a downstream client's roots onto its upstream
// connection when the client signals roots/list_changed.
func (u *upstream) handleRootsChanged(ctx context.Context, req *mcp.RootsListChangedRequest) {
	ss := req.Session
	if ss == nil {
		return
	}
	u.mu.RLock()
	sc := u.sessions[ss.ID()]
	u.mu.RUnlock()
	if sc != nil {
		u.syncRoots(ctx, ss, sc)
	}
}

// sweepSessions periodically closes per-session upstream connections whose
// downstream client session is no longer live. (ServerSession.Wait tracks a
// single request connection, not the session lifetime, so it can't drive
// cleanup; comparing against the server's live sessions can.)
func (u *upstream) sweepSessions(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.reapDeadSessions()
		}
	}
}

func (u *upstream) reapDeadSessions() {
	live := make(map[string]struct{})
	for ss := range u.server.Sessions() {
		live[ss.ID()] = struct{}{}
	}
	u.mu.Lock()
	var dead []*sessConn
	for id, sc := range u.sessions {
		if _, ok := live[id]; !ok {
			dead = append(dead, sc)
			delete(u.sessions, id)
		}
	}
	u.mu.Unlock()
	for _, sc := range dead {
		_ = sc.cs.Close()
	}
	if len(dead) > 0 {
		log.Printf("<%s> reaped %d ended per-session connection(s)", u.name, len(dead))
	}
}

// opCtx injects the caller's headers and bounds the call by callTimeout.
func (u *upstream) opCtx(ctx context.Context, extra *mcp.RequestExtra) (context.Context, context.CancelFunc) {
	if extra != nil {
		ctx = ctxWithHeaders(ctx, extra.Header)
	}
	if u.callTimeout > 0 {
		return context.WithTimeout(ctx, u.callTimeout)
	}
	return context.WithCancel(ctx)
}

// ---------- forwarding handlers (registered on the proxy server) ----------

func (u *upstream) toolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := u.opCtx(ctx, req.Extra)
	defer cancel()
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return nil, err
	}
	return cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      req.Params.Name,
		Arguments: req.Params.Arguments,
		Meta:      req.Params.Meta,
	})
}

func (u *upstream) promptHandler(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	ctx, cancel := u.opCtx(ctx, req.Extra)
	defer cancel()
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return nil, err
	}
	return cs.GetPrompt(ctx, req.Params)
}

func (u *upstream) resourceHandler(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	ctx, cancel := u.opCtx(ctx, req.Extra)
	defer cancel()
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return nil, err
	}
	return cs.ReadResource(ctx, req.Params)
}

func (u *upstream) handleComplete(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	ctx, cancel := u.opCtx(ctx, req.Extra)
	defer cancel()
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return nil, err
	}
	return cs.Complete(ctx, req.Params)
}

func (u *upstream) handleSubscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return err
	}
	return cs.Subscribe(ctx, req.Params)
}

func (u *upstream) handleUnsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	cs, err := u.sessionFor(req.Session)
	if err != nil {
		return err
	}
	return cs.Unsubscribe(ctx, req.Params)
}

// serverMiddleware forwards logging/setLevel to the caller's upstream session,
// in addition to the server's own per-session bookkeeping.
func (u *upstream) serverMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "logging/setLevel" {
			if p, ok := req.GetParams().(*mcp.SetLoggingLevelParams); ok {
				if ss, ok := sessionOf(req); ok {
					if cs, err := u.sessionFor(ss); err != nil {
						log.Printf("<%s> forward setLevel skipped: %v", u.name, err)
					} else if err := cs.SetLoggingLevel(ctx, p); err != nil {
						log.Printf("<%s> forward setLevel failed: %v", u.name, err)
					}
				}
			}
		}
		return next(ctx, method, req)
	}
}

// gateMiddleware suppresses tools/prompts/resources listings for a client whose
// workspace does not resolve to a whitelisted repo, returning an empty list so
// the upstream stays connected but advertises nothing. It fails closed: a
// client that exposes no workspace signal matches nothing and sees empty lists.
//
// ponytail: re-resolves the client's repo (a roots round-trip + a couple of git
// execs) on every list request. List requests are rare per session (once at
// connect, again on a capability change), so this is cheap in practice; add a
// per-session cache if a client is found to spam list requests.
func (u *upstream) gateMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list", "prompts/list", "resources/list", "resources/templates/list":
			if !u.advertiseTo(ctx, req) {
				log.Printf("<%s> %s hidden: client repo not in whitelist", u.name, method)
				return emptyListResult(method), nil
			}
		}
		return next(ctx, method, req)
	}
}

// advertiseTo reports whether this upstream should advertise its capabilities to
// the request's client, i.e. the client's workspace matches the whitelist.
func (u *upstream) advertiseTo(ctx context.Context, req mcp.Request) bool {
	ss, _ := sessionOf(req)
	var hdr http.Header
	if extra := req.GetExtra(); extra != nil {
		hdr = extra.Header
	}
	dirs := clientRepoDirs(ctx, ss, hdr)
	return u.matcher.matches(ctx, dirs)
}

// emptyListResult returns a zero-length result of the type the SDK expects for
// each list method, so a gated client gets a well-formed empty listing.
func emptyListResult(method string) mcp.Result {
	switch method {
	case "prompts/list":
		return &mcp.ListPromptsResult{}
	case "resources/list":
		return &mcp.ListResourcesResult{}
	case "resources/templates/list":
		return &mcp.ListResourceTemplatesResult{}
	default:
		return &mcp.ListToolsResult{}
	}
}

// sessionOf extracts the downstream ServerSession from a server-side request.
func sessionOf(req mcp.Request) (*mcp.ServerSession, bool) {
	getter, ok := req.(interface{ GetSession() mcp.Session })
	if !ok {
		return nil, false
	}
	ss, ok := getter.GetSession().(*mcp.ServerSession)
	return ss, ok
}

// clientMiddleware intercepts upstream→proxy traffic: it re-registers on
// list-changed and relays the remaining notifications to the downstream
// client(s). (roots/list is answered natively from the connection's mirrored
// root set — see syncRoots.)
func (u *upstream) clientMiddleware(downstream *mcp.ServerSession) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "notifications/tools/list_changed",
				"notifications/prompts/list_changed",
				"notifications/resources/list_changed":
				safeGo(u.name, func() { u.registerCapabilities(u.baseCtx) })
				return nil, nil
			case "notifications/message":
				u.relayLog(ctx, downstream, req)
				return nil, nil
			case "notifications/resources/updated":
				if p, ok := req.GetParams().(*mcp.ResourceUpdatedNotificationParams); ok {
					_ = u.server.ResourceUpdated(ctx, p)
				}
				return nil, nil
			}
			return next(ctx, method, req)
		}
	}
}

// relayLog forwards an upstream logging notification to the downstream client(s).
func (u *upstream) relayLog(ctx context.Context, downstream *mcp.ServerSession, req mcp.Request) {
	p, ok := req.GetParams().(*mcp.LoggingMessageParams)
	if !ok {
		return
	}
	if downstream != nil {
		_ = downstream.Log(ctx, p)
		return
	}
	for ss := range u.server.Sessions() {
		_ = ss.Log(ctx, p)
	}
}
