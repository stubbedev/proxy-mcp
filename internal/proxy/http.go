package proxy

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deliberate request log of method + path for the proxied route.
			log.Printf("<%s> Request [%s] %s", prefix, r.Method, r.URL.Path) //nolint:gosec
			next.ServeHTTP(w, r)
		})
	}
}

func recoverMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Printf("<%s> Recovered from panic: %v", prefix, err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// routeForServer returns the ServeMux pattern an upstream mounts at, under the
// optional baseURL path. A streamable-HTTP upstream mounts at an explicit
// `/<base>/<name>/mcp` endpoint — the conventional streamable-HTTP path, and a
// drop-in match for TBXark/mcp-proxy clients — so a client URL of
// `/<name>/mcp` hits a registered route rather than relying on subtree
// fall-through. An SSE upstream mounts at the `/<base>/<name>/` subtree because
// its server serves both `/sse` and `/message` beneath that base.
func routeForServer(basePath, name string, serverType MCPServerType) string {
	route := path.Join("/", basePath, name)
	if serverType == MCPServerTypeStreamable {
		return route + "/mcp"
	}
	return route + "/"
}

// buildListener returns the proxy's listener. Under socket activation it adopts
// the passed-in socket — systemd's LISTEN_FDS on Linux or launchd's
// launch_activate_socket on macOS — and reports activated=true; otherwise it
// binds addr itself. Adopting the socket lets the init system own the port and
// start the proxy on demand, exiting (see --idle-timeout) when quiet without
// dropping the port.
func buildListener(ctx context.Context, addr string) (net.Listener, bool, error) {
	if ln := systemdListener(); ln != nil {
		return ln, true, nil
	}
	if ln := launchdListener(); ln != nil {
		return ln, true, nil
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	return ln, false, err
}

// systemdListener adopts the first systemd socket-activation fd when this
// process is the activation target (LISTEN_PID == pid and LISTEN_FDS >= 1),
// else nil. A minimal sd_listen_fds(3): the first passed fd is
// SD_LISTEN_FDS_START (3). net.FileListener dups the fd, so the os.File is
// closed afterwards.
func systemdListener() net.Listener {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil
	}
	const sdListenFdsStart = 3
	f := os.NewFile(uintptr(sdListenFdsStart), "systemd-activation-socket")
	if f == nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	ln, err := net.FileListener(f)
	if err != nil {
		log.Printf("socket activation: adopting LISTEN_FDS failed: %v", err)
		return nil
	}
	log.Printf("socket activation: adopted listener on %s", ln.Addr())
	return ln
}

// mcpHandler builds the SDK HTTP handler that serves a proxy server.
func mcpHandler(serverType MCPServerType, srv *mcp.Server) http.Handler {
	get := func(*http.Request) *mcp.Server { return srv }
	if serverType == MCPServerTypeSSE {
		return mcp.NewSSEHandler(get, nil)
	}
	return mcp.NewStreamableHTTPHandler(get, nil)
}

// serveHTTP runs the HTTP server on listener. When socket-activated it waits
// for readyCh (route registration complete) before accepting, so the queued
// client doesn't race route registration; when self-bound it serves at once
// (/readyz already gates external callers). Blocks until the server stops.
func serveHTTP(
	ctx context.Context,
	httpServer *http.Server,
	listener net.Listener,
	activated bool,
	readyCh <-chan struct{},
	serverType MCPServerType,
) {
	if activated {
		select {
		case <-readyCh:
		case <-ctx.Done():
			return
		}
	}
	log.Printf("Starting %s server", serverType)
	log.Printf("%s server listening on %s", serverType, listener.Addr())
	if hErr := httpServer.Serve(listener); hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
		log.Fatalf("Failed to start server: %v", hErr)
	}
}

func startHTTPServer(config *Config, idleTimeout time.Duration, loadConfig func() (*Config, error), configPath string) error {
	baseURL, uErr := url.Parse(config.McpProxy.BaseURL)
	if uErr != nil {
		return uErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optional idle auto-shutdown: track per-route activity and, once ready,
	// exit after idleTimeout of silence. Lets pure socket activation own the
	// lifecycle (start on connect, exit when quiet). Disabled when <= 0.
	activity := newActivityTracker()
	idleShutdown := make(chan struct{})
	var idleOnce sync.Once

	// readyCh closes once every upstream route is registered (signalReady).
	// Under socket activation we hold off Serve until then, so the activating
	// client's connection waits in the socket backlog through the upstream
	// cold-start instead of racing route registration and 404ing.
	readyCh := make(chan struct{})

	listener, activated, lErr := buildListener(ctx, config.McpProxy.Addr)
	if lErr != nil {
		return lErr
	}

	// A mutable router (not http.ServeMux) so upstream routes can be unmounted on
	// a config reload — ServeMux can only ever add patterns.
	rt := newRouter()
	httpServer := &http.Server{
		Addr:              config.McpProxy.Addr,
		Handler:           rt,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Liveness/readiness probes. Registered BEFORE the listener starts so they
	// serve from the first accepted connection. /healthz is liveness (always 200
	// once listening); /readyz is readiness — 503 until every upstream is
	// resolved and its /<name>/ route is registered, then 200, with per-upstream
	// state in the JSON body. Routes register asynchronously, so without this
	// gate a client's first request can race ahead and hit a 404. See
	// readinessTracker.
	tracker := newReadinessTracker()
	tracker.registerProbes(rt)

	mgr := &proxyManager{
		ctx:         ctx,
		router:      rt,
		tracker:     tracker,
		basePath:    baseURL.Path,
		serverType:  config.McpProxy.Type,
		activity:    activity,
		idleTimeout: idleTimeout,
		loadConfig:  loadConfig,
		configPath:  configPath,
		proxyCfg:    config.McpProxy,
		servers:     make(map[string]*managed),
	}

	go func() {
		if err := mgr.boot(config); err != nil {
			log.Fatalf("Failed to add clients: %v", err)
		}
		log.Printf("All clients initialized")
		// Every upstream is resolved and every successful /<name>/ route is now
		// registered. Flip the readiness gate, log the stable "proxy ready" line,
		// fire systemd sd_notify(READY=1) so a Type=notify unit only reaches
		// `active` now (never mid-startup), and start the watchdog keepalive if
		// the unit set WatchdogSec.
		tracker.signalReady(ctx)
		close(readyCh)
		// Seed the idle clock at readiness (not boot) so a slow upstream
		// cold-start isn't counted against the idle window, then watch for quiet.
		activity.monitorIdle(ctx, idleTimeout, func() {
			idleOnce.Do(func() { close(idleShutdown) })
		})
		// Watch the config for live add/remove only after the initial set is up.
		mgr.watchConfig()
	}()

	go serveHTTP(ctx, httpServer, listener, activated, readyCh, config.McpProxy.Type)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigChan:
		log.Println("Shutdown signal received")
	case <-idleShutdown:
		// monitorIdle already logged the reason.
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	err := httpServer.Shutdown(shutdownCtx)
	mgr.closeAll()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
