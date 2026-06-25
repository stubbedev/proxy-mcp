package main

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

type ToolFilterConfig struct {
	Mode ToolFilterMode `json:"mode,omitempty"`
	List []string       `json:"list,omitempty"`
}

type OptionsV2 struct {
	PanicIfInvalid optional.Field[bool] `json:"panicIfInvalid"`
	LogEnabled     optional.Field[bool] `json:"logEnabled"`
	AuthTokens     []string             `json:"authTokens,omitempty"`
	ToolFilter     *ToolFilterConfig    `json:"toolFilter,omitempty"`
	Disabled       bool                 `json:"disabled,omitempty"`
	// CallTimeout bounds each forwarded request to this upstream (tool call,
	// prompt get, resource read, completion). A Go duration string like "30s";
	// empty or "0" means no timeout. Invalid values are ignored (logged).
	CallTimeout string `json:"callTimeout,omitempty"`
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

type MCPProxyConfigV2 struct {
	BaseURL string        `json:"baseURL"`
	Addr    string        `json:"addr"`
	Name    string        `json:"name"`
	Version string        `json:"version"`
	Type    MCPServerType `json:"type,omitempty"`
	Options *OptionsV2    `json:"options,omitempty"`
}

type MCPClientConfigV2 struct {
	TransportType MCPClientType `json:"transportType,omitempty"`

	// Stdio
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// SSE or Streamable HTTP
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`

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
	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"`
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
