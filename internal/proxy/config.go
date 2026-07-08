package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/go-sphere/confstore"
	"github.com/go-sphere/confstore/codec"
	"github.com/go-sphere/confstore/provider"
	"github.com/go-sphere/confstore/provider/file"
	"github.com/go-sphere/confstore/provider/http"
	"github.com/tbxark/optional-go"
)

type StdioMCPClientConfig struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
	Args    []string          `json:"args"`
}

type SSEMCPClientConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type StreamableMCPClientConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Timeout time.Duration     `json:"timeout"`
}

type MCPClientType string

const (
	MCPClientTypeStdio      MCPClientType = "stdio"
	MCPClientTypeSSE        MCPClientType = "sse"
	MCPClientTypeStreamable MCPClientType = "streamable-http"
)

type MCPServerType string

const (
	MCPServerTypeSSE        MCPServerType = "sse"
	MCPServerTypeStreamable MCPServerType = "streamable-http"
)

// ---- V2 ----

type ToolFilterMode string

const (
	ToolFilterModeAllow ToolFilterMode = "allow"
	ToolFilterModeBlock ToolFilterMode = "block"
)

// ConnMode selects how an upstream's connection is shared across downstream
// clients. The values are the single source for both perSession() and the
// generated config schema.
type ConnMode string

const (
	ConnModePerSession ConnMode = "perSession"
	ConnModeShared     ConnMode = "shared"
)

type ToolFilterConfig struct {
	// Mode is "allow" (expose only listed tools) or "block" (hide listed tools).
	Mode ToolFilterMode `json:"mode,omitempty"`
	// List is the tool names the mode applies to.
	List []string `json:"list,omitempty"`
}

type OptionsV2 struct {
	// PanicIfInvalid aborts startup if this (eager) upstream fails to connect,
	// instead of degrading.
	PanicIfInvalid optional.Field[bool] `json:"panicIfInvalid"`
	// LogEnabled logs each request to this upstream's route.
	LogEnabled optional.Field[bool] `json:"logEnabled"`
	// AuthTokens are the bearer tokens accepted on this route. Inherited from
	// mcpProxy.options when unset.
	AuthTokens []string `json:"authTokens,omitempty"`
	// ToolFilter optionally allow- or block-lists this upstream's tools.
	ToolFilter *ToolFilterConfig `json:"toolFilter,omitempty"`
	// Disabled skips this upstream entirely (no route, no connection).
	Disabled bool `json:"disabled,omitempty"`
	// CallTimeout bounds each forwarded request to this upstream (tool call,
	// prompt get, resource read, completion). A Go duration string like "30s";
	// empty or "0" means no timeout. Invalid values are ignored (logged).
	CallTimeout string `json:"callTimeout,omitempty"`
	// Mode selects how upstream connections are shared across downstream clients:
	//   "perSession" (default) — one dedicated upstream connection per client,
	//       giving full transparency including server→client requests
	//       (sampling/roots/elicitation), routed 1:1 to the right client.
	//   "shared" — a single upstream connection multiplexed across all clients
	//       (one backend process). Server→client requests are not bridged
	//       (an upstream request can't be attributed to one of N clients).
	// Use "shared" for a singleton backend you want exactly one of (e.g. a
	// browser); use the default for everything else.
	Mode ConnMode `json:"mode,omitempty"`
	// IdleTimeout, when set (a Go duration like "5m"), makes this upstream
	// lazy: its backend process is NOT started at boot but on the first request
	// to its route, and is torn down again after this much idle time, then
	// re-started lazily on the next request. Empty/"0" keeps the upstream eager
	// (connected at boot, never torn down). This is per-upstream and
	// independent of the process-level --idle-timeout, which exits the whole
	// proxy: several lazy upstreams can share one process, each retiring its own
	// backend on its own clock.
	IdleTimeout string `json:"idleTimeout,omitempty"`
	// RepoWhitelist gates this upstream's advertised capabilities by the
	// downstream client's repository. When non-empty, tools/prompts/resources
	// are only listed to a client whose workspace resolves to one of these
	// repos; other clients see an empty list (the upstream still connects).
	// Each entry is either a local directory (matched worktree-aware via the
	// git common dir, so the repo and every worktree of it match) or a git
	// remote URL (matched against the client repo's remotes, normalized across
	// ssh/https and a trailing ".git"). A client that exposes no workspace
	// signal (no roots, no header) matches nothing — gating fails closed.
	RepoWhitelist []string `json:"repoWhitelist,omitempty"`
}

// perSession reports whether this upstream uses a dedicated connection per
// downstream client (the default). Only "shared" opts out.
func (o *OptionsV2) perSession() bool {
	return o == nil || o.Mode != ConnModeShared
}

// callTimeout parses CallTimeout into a duration. Returns 0 (no timeout) when
// empty, zero, or malformed.
func (o *OptionsV2) callTimeout() time.Duration {
	if o == nil || o.CallTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(o.CallTimeout)
	if err != nil || d < 0 {
		log.Printf("ignoring invalid callTimeout %q: %v", o.CallTimeout, err)
		return 0
	}
	return d
}

// idleTimeout parses IdleTimeout into a duration. Returns 0 (eager, no
// teardown) when empty, zero, or malformed.
func (o *OptionsV2) idleTimeout() time.Duration {
	if o == nil || o.IdleTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(o.IdleTimeout)
	if err != nil || d < 0 {
		log.Printf("ignoring invalid idleTimeout %q: %v", o.IdleTimeout, err)
		return 0
	}
	return d
}

type MCPProxyConfigV2 struct {
	// BaseURL is the public URL clients reach the proxy at (e.g.
	// http://127.0.0.1:9090); its path becomes the mount prefix for every route.
	BaseURL string `json:"baseURL"`
	// Addr is the listen address, e.g. ":9090" or "127.0.0.1:9090". Ignored under
	// socket activation.
	Addr string `json:"addr"`
	// Name is the proxy server name advertised to downstream clients.
	Name string `json:"name"`
	// Version is the proxy server version advertised to downstream clients.
	Version string `json:"version"`
	// Type is the transport the proxy serves to clients. Defaults to sse.
	Type MCPServerType `json:"type,omitempty"`
	// Options set here are the defaults inherited by every upstream (authTokens,
	// panicIfInvalid, logEnabled).
	Options *OptionsV2 `json:"options,omitempty"`
}

type MCPClientConfigV2 struct {
	// TransportType is the upstream transport. Optional: inferred from
	// command/url when omitted (command => stdio, url => sse/streamable-http).
	TransportType MCPClientType `json:"transportType,omitempty"`

	// Command is the stdio backend executable to run.
	Command string `json:"command,omitempty"`
	// Args are the stdio command's arguments.
	Args []string `json:"args,omitempty"`
	// Env adds environment variables to the stdio backend (merged over the
	// proxy's own env). $VAR is expanded when --expand-env is on.
	Env map[string]string `json:"env,omitempty"`

	// URL is the sse/streamable-http upstream endpoint.
	URL string `json:"url,omitempty"`
	// Headers are static headers added to every request to an sse/streamable-http
	// upstream.
	Headers map[string]string `json:"headers,omitempty"`
	// Timeout bounds the streamable-http handshake (nanoseconds, Go
	// time.Duration JSON form). Prefer options.callTimeout for per-call bounds.
	Timeout time.Duration `json:"timeout,omitempty"`

	Options *OptionsV2 `json:"options,omitempty"`
}

func parseMCPClientConfigV2(conf *MCPClientConfigV2) (any, error) {
	if conf.Command != "" || conf.TransportType == MCPClientTypeStdio {
		if conf.Command == "" {
			return nil, errors.New("command is required for stdio transport")
		}
		return &StdioMCPClientConfig{
			Command: conf.Command,
			Env:     conf.Env,
			Args:    conf.Args,
		}, nil
	}
	if conf.URL != "" {
		if conf.TransportType == MCPClientTypeStreamable {
			return &StreamableMCPClientConfig{
				URL:     conf.URL,
				Headers: conf.Headers,
				Timeout: conf.Timeout,
			}, nil
		}
		return &SSEMCPClientConfig{
			URL:     conf.URL,
			Headers: conf.Headers,
		}, nil
	}
	return nil, errors.New("invalid server type")
}

// ---- Config ----

type Config struct {
	// Schema optionally points at this config's JSON Schema, so editors validate
	// the file. Ignored by the loader.
	Schema     string                        `json:"$schema,omitempty"`
	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"          jsonschema:"required"`
	McpServers map[string]*MCPClientConfigV2 `json:"mcpServers"`
}

type FullConfig struct {
	DeprecatedServerV1  *MCPProxyConfigV1             `json:"server"`
	DeprecatedClientsV1 map[string]*MCPClientConfigV1 `json:"clients"`

	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"`
	McpServers map[string]*MCPClientConfigV2 `json:"mcpServers"`
}

// parseHTTPHeaders parses the 'Key1:Value1;Key2:Value2' header string passed
// via -http-headers into an http.Header, skipping malformed or empty entries.
func parseHTTPHeaders(httpHeaders string) nethttp.Header {
	headers := make(nethttp.Header)
	for kv := range strings.SplitSeq(httpHeaders, ";") {
		parts := strings.SplitN(kv, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key != "" && value != "" {
			headers.Add(key, value)
		}
	}
	return headers
}

// newHTTPConfProvider builds a provider that fetches the config from an http(s)
// URL, honouring -insecure, -http-timeout, and -http-headers.
func newHTTPConfProvider(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) provider.Provider {
	httpClient := nethttp.DefaultClient
	if insecure {
		transport, ok := nethttp.DefaultTransport.(*nethttp.Transport)
		if ok {
			transport = transport.Clone()
		} else {
			transport = &nethttp.Transport{}
		}
		// InsecureSkipVerify is opt-in via the explicit -insecure flag.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		httpClient = &nethttp.Client{Transport: transport}
	}
	if httpTimeout > 0 {
		httpClient.Timeout = time.Duration(httpTimeout) * time.Second
	}
	opts := []http.Option{http.WithClient(httpClient)}
	if httpHeaders != "" {
		if headers := parseHTTPHeaders(httpHeaders); len(headers) > 0 {
			opts = append(opts, http.WithHeaders(headers))
		}
	}
	pro := http.New(path, opts...)
	if expandEnv {
		return provider.NewExpandEnv(pro)
	}
	return pro
}

func newConfProvider(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) (provider.Provider, error) {
	if http.IsRemoteURL(path) {
		return newHTTPConfProvider(path, insecure, expandEnv, httpHeaders, httpTimeout), nil
	}
	if file.IsLocalPath(path) {
		if expandEnv {
			return provider.NewExpandEnv(file.New(path, file.WithExpandEnv())), nil
		}
		return file.New(path), nil
	}
	return nil, errors.New("unsupported config path")
}

func load(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) (*Config, error) {
	pro, err := newConfProvider(path, insecure, expandEnv, httpHeaders, httpTimeout)
	if err != nil {
		return nil, err
	}
	conf, err := confstore.Load[FullConfig](pro, codec.JsonCodec())
	if err != nil {
		return nil, err
	}
	adaptMCPClientConfigV1ToV2(conf)

	if conf.McpProxy == nil {
		return nil, errors.New("mcpProxy is required")
	}
	if conf.McpProxy.Options == nil {
		conf.McpProxy.Options = &OptionsV2{}
	}
	for _, clientConfig := range conf.McpServers {
		if clientConfig.Options == nil {
			clientConfig.Options = &OptionsV2{}
		}
		if clientConfig.Options.AuthTokens == nil {
			clientConfig.Options.AuthTokens = conf.McpProxy.Options.AuthTokens
		}
		if !clientConfig.Options.PanicIfInvalid.Present() {
			clientConfig.Options.PanicIfInvalid = conf.McpProxy.Options.PanicIfInvalid
		}
		if !clientConfig.Options.LogEnabled.Present() {
			clientConfig.Options.LogEnabled = conf.McpProxy.Options.LogEnabled
		}
	}

	if conf.McpProxy.Type == "" {
		conf.McpProxy.Type = MCPServerTypeSSE // default to SSE
	}

	return &Config{
		McpProxy:   conf.McpProxy,
		McpServers: conf.McpServers,
	}, nil
}

// validateConfig checks a loaded config for problems that load() doesn't catch
// on its own: the proxy server type must be known, and every (enabled) upstream
// must resolve to a valid stdio/sse/streamable client config. Returns the first
// error found, or nil. Used by the -validate flag.
func validateConfig(config *Config) error {
	switch config.McpProxy.Type {
	case MCPServerTypeSSE, MCPServerTypeStreamable:
	default:
		return fmt.Errorf("unknown mcpProxy.type: %q", config.McpProxy.Type)
	}
	for name, clientConfig := range config.McpServers {
		if clientConfig.Options != nil && clientConfig.Options.Disabled {
			continue
		}
		if _, err := parseMCPClientConfigV2(clientConfig); err != nil {
			return fmt.Errorf("server %q: %w", name, err)
		}
	}
	return nil
}
