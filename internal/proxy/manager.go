package proxy

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/go-sphere/confstore/provider/file"
	"golang.org/x/sync/errgroup"
)

// managed tracks one mounted upstream so a reload can compare, unmount, and tear
// it down. cfg is the exact (inheritance-applied) client config it was built
// from, so a later reload can detect changes with a deep-equal; cancel stops
// every goroutine the upstream spawned (template watch, session sweep, idle
// monitor) and, for a lazy upstream, its backend process.
type managed struct {
	up     *upstream
	cfg    *MCPClientConfigV2
	route  string
	cancel context.CancelFunc
}

// proxyManager owns the live set of upstreams and the mutable router they mount
// on. It is what makes the proxy responsive to config edits: addServer /
// removeServer mount and unmount a single upstream at runtime, and reload()
// diffs a freshly loaded config against the running set so adding or removing an
// mcpServers entry takes effect without a restart.
type proxyManager struct {
	ctx         context.Context
	router      *router
	tracker     *readinessTracker
	basePath    string
	serverType  MCPServerType
	activity    *activityTracker // process-level, drives --idle-timeout
	idleTimeout time.Duration    // process-level

	loadConfig func() (*Config, error)
	configPath string

	reloadMu sync.Mutex // serializes reload() against itself and boot()

	mu       sync.Mutex
	proxyCfg *MCPProxyConfigV2
	servers  map[string]*managed
}

// boot connects every configured upstream in parallel (preserving the original
// concurrent cold-start) and returns an error only if an eager upstream with
// panicIfInvalid set fails — that still aborts startup, as before. A reload
// never aborts the process; see addServer's bootFatal flag.
func (m *proxyManager) boot(config *Config) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.mu.Lock()
	m.proxyCfg = config.McpProxy
	m.mu.Unlock()

	var g errgroup.Group
	for name, cfg := range config.McpServers {
		g.Go(func() (err error) {
			// A panic during one upstream's registration is contained and marked
			// failed, never propagated to crash the whole proxy.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("<%s> recovered from panic during registration: %v", name, rec)
					m.tracker.setFailed(name, fmt.Errorf("panic: %v", rec))
				}
			}()
			return m.addServer(name, cfg, true)
		})
	}
	return g.Wait()
}

// addServer connects one upstream and mounts its route. A lazy upstream
// (idleTimeout set) is mounted behind lazyConnectHandler and connects on first
// request; an eager upstream is dialed now and mounted only on success. A plain
// connect failure is logged, marked failed, and tolerated (degraded) so one bad
// backend never stops the others — only an eager upstream with panicIfInvalid
// during boot (bootFatal) returns the error that aborts startup.
func (m *proxyManager) addServer(name string, cfg *MCPClientConfigV2, bootFatal bool) error {
	if cfg.Options.Disabled {
		log.Printf("<%s> Disabled", name)
		m.tracker.setDisabled(name)
		m.store(name, &managed{cfg: cfg})
		return nil
	}

	up := newUpstream(name, m.proxyCfg, cfg)
	// Each upstream gets its own cancelable context so removeServer can stop its
	// background goroutines (and lazy backend) independently of its siblings.
	upCtx, cancel := context.WithCancel(m.ctx)
	route := routeForServer(m.basePath, name, m.serverType)
	core := mcpHandler(m.serverType, up.server)

	if up.lazy {
		core = m.lazyConnectHandler(upCtx, up, core)
		log.Printf("<%s> lazy; will connect on first request", name)
	} else {
		log.Printf("<%s> Connecting", name)
		if cErr := up.connect(upCtx); cErr != nil {
			log.Printf("<%s> Failed to connect upstream: %v", name, cErr)
			m.tracker.setFailed(name, cErr)
			cancel()
			// Remember it (unmounted) so a later reload can detect a config change
			// and retry; a failed eager connect otherwise never auto-retries.
			m.store(name, &managed{cfg: cfg})
			if cfg.Options.PanicIfInvalid.OrElse(false) && bootFatal {
				return cErr
			}
			return nil
		}
		log.Printf("<%s> Connected", name)
	}

	handler := chainMiddleware(core, m.middlewares(name, cfg, up)...)
	m.router.Handle(route, handler)
	m.tracker.setConnected(name)
	log.Printf("<%s> Handling requests at %s", name, route)
	m.store(name, &managed{up: up, cfg: cfg, route: route, cancel: cancel})
	return nil
}

// removeServer unmounts an upstream's route, cancels its context (stopping the
// template watcher, session sweep, idle monitor and any lazy backend) and closes
// its connections. Safe for a disabled/failed entry (no route, no backend).
func (m *proxyManager) removeServer(name string) {
	m.mu.Lock()
	md := m.servers[name]
	delete(m.servers, name)
	m.mu.Unlock()
	if md == nil {
		return
	}
	if md.route != "" {
		m.router.remove(md.route)
	}
	if md.cancel != nil {
		md.cancel()
	}
	if md.up != nil {
		md.up.close()
	}
	m.tracker.remove(name)
	log.Printf("<%s> removed", name)
}

// store records (or replaces) the managed entry for name.
func (m *proxyManager) store(name string, md *managed) {
	m.mu.Lock()
	m.servers[name] = md
	m.mu.Unlock()
}

// reload re-reads the config and reconciles the running upstreams with it:
// entries gone from the config are torn down, new ones are connected and
// mounted, and entries whose config changed are re-registered. A load failure
// leaves the running set untouched. mcpProxy listener-level fields (addr, type,
// baseURL) can't change without a restart and are ignored with a warning;
// option changes that affect upstreams (e.g. shared authTokens) propagate
// because load() folds them into each client config, which the deep-equal then
// sees as a change.
func (m *proxyManager) reload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	cfg, err := m.loadConfig()
	if err != nil {
		log.Printf("reload: config load failed, keeping current config: %v", err)
		return
	}
	if !proxyListenerEqual(m.proxyCfg, cfg.McpProxy) {
		log.Printf("reload: mcpProxy addr/type/baseURL changed; that needs a restart and is ignored")
	}

	m.mu.Lock()
	m.proxyCfg = cfg.McpProxy
	current := make(map[string]*managed, len(m.servers))
	maps.Copy(current, m.servers)
	m.mu.Unlock()

	for name := range current {
		if _, ok := cfg.McpServers[name]; !ok {
			m.removeServer(name)
		}
	}
	for name, ncfg := range cfg.McpServers {
		md := current[name]
		switch {
		case md == nil:
			log.Printf("<%s> new in config; registering", name)
			_ = m.addServer(name, ncfg, false)
		case !reflect.DeepEqual(md.cfg, ncfg):
			log.Printf("<%s> config changed; re-registering", name)
			m.removeServer(name)
			_ = m.addServer(name, ncfg, false)
		}
	}
}

// watchConfig wires reload triggers: SIGHUP always (works for file and URL
// configs), plus automatic reload on local-file change so an edit takes effect
// without remembering to restart.
func (m *proxyManager) watchConfig() {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	safeGo("reload", func() {
		for range hup {
			log.Printf("SIGHUP received; reloading config")
			m.reload()
		}
	})

	if !file.IsLocalPath(m.configPath) {
		log.Printf("config %q is not a local file; auto-reload disabled (send SIGHUP to reload)", m.configPath)
		return
	}
	safeGo("reload", m.pollConfigFile)
}

// pollConfigFile reloads whenever the config file's mtime advances.
// ponytail: mtime poll every 2s, no fsnotify dependency; swap to fsnotify only
// if sub-second reload latency ever matters.
func (m *proxyManager) pollConfigFile() {
	var last time.Time
	if st, err := os.Stat(m.configPath); err == nil {
		last = st.ModTime()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			st, err := os.Stat(m.configPath)
			if err != nil {
				continue // transient (e.g. mid-rewrite); try again next tick
			}
			if st.ModTime().After(last) {
				last = st.ModTime()
				log.Printf("config %q changed on disk; reloading", m.configPath)
				m.reload()
			}
		}
	}
}

// closeAll tears down every upstream; called on shutdown after the HTTP server
// drains.
func (m *proxyManager) closeAll() {
	m.mu.Lock()
	mds := make([]*managed, 0, len(m.servers))
	for _, md := range m.servers {
		mds = append(mds, md)
	}
	m.mu.Unlock()
	for _, md := range mds {
		if md.cancel != nil {
			md.cancel()
		}
		if md.up != nil {
			log.Printf("<%s> Shutting down", md.up.name)
			md.up.close()
		}
	}
}

// middlewares builds the per-route chain: recovery always, then optional request
// logging, auth, process-level activity tracking (drives --idle-timeout process
// exit) and, for a lazy upstream, its own activity tracking (drives per-backend
// idle teardown).
func (m *proxyManager) middlewares(name string, cfg *MCPClientConfigV2, up *upstream) []MiddlewareFunc {
	mws := []MiddlewareFunc{recoverMiddleware(name)}
	if cfg.Options.LogEnabled.OrElse(false) {
		mws = append(mws, loggerMiddleware(name))
	}
	if len(cfg.Options.AuthTokens) > 0 {
		mws = append(mws, newAuthMiddleware(cfg.Options.AuthTokens))
	}
	if m.idleTimeout > 0 {
		mws = append(mws, m.activity.middleware())
	}
	if up.activity != nil {
		mws = append(mws, up.activity.middleware())
	}
	return mws
}

// lazyConnectHandler defers the upstream connection until the first request
// reaches its route, then serves it. It runs innermost (after auth) so an
// unauthorized request never starts a backend, and ensureConnected re-connects
// transparently after an idle teardown. A connect failure answers 503 for this
// route only — siblings are untouched.
func (m *proxyManager) lazyConnectHandler(ctx context.Context, up *upstream, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := up.ensureConnected(ctx); err != nil {
			log.Printf("<%s> on-demand connect failed: %v", up.name, err)
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// proxyListenerEqual reports whether the listener-level proxy fields that can't
// change at runtime are unchanged. Other proxy fields/options can change: they
// either don't affect routing or propagate through per-client config.
func proxyListenerEqual(a, b *MCPProxyConfigV2) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Addr == b.Addr && a.Type == b.Type && a.BaseURL == b.BaseURL
}
