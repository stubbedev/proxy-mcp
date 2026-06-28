package main

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestReloadAddsAndRemovesServers exercises the live-reload diff: a server
// dropped from the config is unmounted and one added to it is mounted, with no
// restart.
func TestReloadAddsAndRemovesServers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	upA := startUpstreamHTTP(t, "A", func(*mcp.Server) {})
	upB := startUpstreamHTTP(t, "B", func(*mcp.Server) {})

	proxyCfg := &MCPProxyConfigV2{Name: "p", Version: "1.0.0", Type: MCPServerTypeStreamable, Options: &OptionsV2{}}
	cfgA := &Config{McpProxy: proxyCfg, McpServers: map[string]*MCPClientConfigV2{"A": streamableCfg(upA.URL)}}
	cfgB := &Config{McpProxy: proxyCfg, McpServers: map[string]*MCPClientConfigV2{"B": streamableCfg(upB.URL)}}

	current := cfgA
	mgr := &proxyManager{
		ctx:        ctx,
		router:     newRouter(),
		tracker:    newReadinessTracker(),
		serverType: MCPServerTypeStreamable,
		loadConfig: func() (*Config, error) { return current, nil },
		proxyCfg:   proxyCfg,
		servers:    make(map[string]*managed),
	}
	defer mgr.closeAll()

	if err := mgr.boot(cfgA); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if mgr.router.match("/A/mcp") == nil {
		t.Fatal("A not mounted after boot")
	}

	current = cfgB
	mgr.reload()
	if mgr.router.match("/A/mcp") != nil {
		t.Fatal("A still mounted after reload removed it")
	}
	if mgr.router.match("/B/mcp") == nil {
		t.Fatal("B not mounted after reload added it")
	}
}

func TestProxyListenerEqual(t *testing.T) {
	base := &MCPProxyConfigV2{Addr: ":9090", Type: MCPServerTypeStreamable, BaseURL: "http://x"}
	same := &MCPProxyConfigV2{Addr: ":9090", Type: MCPServerTypeStreamable, BaseURL: "http://x", Name: "renamed"}
	diff := &MCPProxyConfigV2{Addr: ":9091", Type: MCPServerTypeStreamable, BaseURL: "http://x"}
	if !proxyListenerEqual(base, same) {
		t.Fatal("name-only change should be listener-equal")
	}
	if proxyListenerEqual(base, diff) {
		t.Fatal("addr change should not be listener-equal")
	}
}
