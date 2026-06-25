package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/errgroup"
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

func startHTTPServer(config *Config) error {
	baseURL, uErr := url.Parse(config.McpProxy.BaseURL)
	if uErr != nil {
		return uErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errorGroup errgroup.Group
	httpMux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:              config.McpProxy.Addr,
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	info := mcp.Implementation{
		Name: config.McpProxy.Name,
	}

	// Liveness/readiness probes. Registered on the mux BEFORE the listener
	// starts so they serve from the first accepted connection. /healthz is
	// liveness (always 200 once listening); /readyz is readiness — it returns
	// 503 until every upstream is resolved and its /<name>/ route is
	// registered, then 200, with per-upstream state in the JSON body. Upstream
	// mcp-proxy binds the port and registers routes asynchronously, so without
	// this gate a client's first request can race ahead of route registration
	// and hit a 404. See readinessTracker.
	tracker := newReadinessTracker()
	tracker.registerProbes(httpMux)

	for name, clientConfig := range config.McpServers {
		if clientConfig.Options.Disabled {
			log.Printf("<%s> Disabled", name)
			tracker.setDisabled(name)
			continue
		}
		mcpClient, err := newMCPClient(name, clientConfig)
		if err != nil {
			return err
		}
		server, err := newMCPServer(name, config.McpProxy, clientConfig)
		if err != nil {
			return err
		}
		errorGroup.Go(func() error {
			log.Printf("<%s> Connecting", name)
			addErr := mcpClient.addToMCPServer(ctx, info, server.mcpServer)
			if addErr != nil {
				log.Printf("<%s> Failed to add client to server: %v", name, addErr)
				tracker.setFailed(name, addErr)
				if clientConfig.Options.PanicIfInvalid.OrElse(false) {
					return addErr
				}
				return nil
			}
			log.Printf("<%s> Connected", name)

			middlewares := make([]MiddlewareFunc, 0)
			middlewares = append(middlewares, recoverMiddleware(name))
			if clientConfig.Options.LogEnabled.OrElse(false) {
				middlewares = append(middlewares, loggerMiddleware(name))
			}
			if len(clientConfig.Options.AuthTokens) > 0 {
				middlewares = append(middlewares, newAuthMiddleware(clientConfig.Options.AuthTokens))
			}
			mcpRoute := routeForServer(baseURL.Path, name, config.McpProxy.Type)
			log.Printf("<%s> Handling requests at %s", name, mcpRoute)
			httpMux.Handle(mcpRoute, chainMiddleware(server.handler, middlewares...))
			tracker.setConnected(name)
			httpServer.RegisterOnShutdown(func() {
				log.Printf("<%s> Shutting down", name)
				_ = mcpClient.Close()
			})
			return nil
		})
	}

	go func() {
		err := errorGroup.Wait()
		if err != nil {
			log.Fatalf("Failed to add clients: %v", err)
		}
		log.Printf("All clients initialized")
		// Every upstream is resolved and every successful /<name>/ route is now
		// registered on the mux. Flip the readiness gate, log the stable
		// "proxy ready" line, fire systemd sd_notify(READY=1) so a Type=notify
		// unit only reaches `active` now (never mid-startup), and start the
		// watchdog keepalive if the unit set WatchdogSec.
		tracker.signalReady(ctx)
	}()

	go func() {
		log.Printf("Starting %s server", config.McpProxy.Type)
		log.Printf("%s server listening on %s", config.McpProxy.Type, config.McpProxy.Addr)
		hErr := httpServer.ListenAndServe()
		if hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", hErr)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	err := httpServer.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
