package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadV2(t *testing.T) {
	p := writeConfig(t, `{
      "mcpProxy": { "baseURL": "http://localhost:9090", "addr": ":9090", "name": "p", "version": "1.0.0", "type": "streamable-http" },
      "mcpServers": { "fetch": { "command": "uvx", "args": ["mcp-server-fetch"] } }
    }`)
	cfg, err := load(p, false, false, "", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.McpProxy.Type != MCPServerTypeStreamable {
		t.Errorf("type = %q, want streamable-http", cfg.McpProxy.Type)
	}
	if _, ok := cfg.McpServers["fetch"]; !ok {
		t.Fatalf("missing 'fetch' server")
	}
	if err := validateConfig(cfg); err != nil {
		t.Errorf("validateConfig: %v", err)
	}
}

func TestLoadDefaultsTypeToSSE(t *testing.T) {
	p := writeConfig(t, `{
      "mcpProxy": { "baseURL": "http://localhost:9090", "addr": ":9090", "name": "p", "version": "1.0.0" },
      "mcpServers": { "x": { "command": "echo" } }
    }`)
	cfg, err := load(p, false, false, "", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.McpProxy.Type != MCPServerTypeSSE {
		t.Errorf("default type = %q, want sse", cfg.McpProxy.Type)
	}
}

func TestLoadMissingProxy(t *testing.T) {
	p := writeConfig(t, `{ "mcpServers": { "x": { "command": "echo" } } }`)
	if _, err := load(p, false, false, "", 10); err == nil {
		t.Fatal("load with no mcpProxy: want error, got nil")
	}
}

func TestLoadV1Adaptation(t *testing.T) {
	p := writeConfig(t, `{
      "server": { "baseURL": "http://localhost:9090", "addr": ":9090", "name": "p", "version": "1.0.0", "globalAuthTokens": ["tok"] },
      "clients": { "legacy": { "type": "stdio", "config": { "command": "echo", "args": ["hi"] } } }
    }`)
	cfg, err := load(p, false, false, "", 10)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	legacy, ok := cfg.McpServers["legacy"]
	if !ok {
		t.Fatalf("v1 client 'legacy' not adapted to v2")
	}
	if legacy.Command != "echo" {
		t.Errorf("adapted command = %q, want echo", legacy.Command)
	}
	if len(legacy.Options.AuthTokens) == 0 || legacy.Options.AuthTokens[0] != "tok" {
		t.Errorf("global auth tokens not propagated: %+v", legacy.Options.AuthTokens)
	}
}

func TestValidateConfigRejectsEmptyServer(t *testing.T) {
	cfg := &Config{
		McpProxy:   &MCPProxyConfigV2{Type: MCPServerTypeSSE},
		McpServers: map[string]*MCPClientConfigV2{"broken": {Options: &OptionsV2{}}},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig with no command/url: want error, got nil")
	}
}

func TestValidateConfigSkipsDisabled(t *testing.T) {
	cfg := &Config{
		McpProxy:   &MCPProxyConfigV2{Type: MCPServerTypeSSE},
		McpServers: map[string]*MCPClientConfigV2{"off": {Options: &OptionsV2{Disabled: true}}},
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig skipping disabled server: %v", err)
	}
}

func TestValidateConfigRejectsUnknownType(t *testing.T) {
	cfg := &Config{McpProxy: &MCPProxyConfigV2{Type: "grpc"}}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig with unknown proxy type: want error, got nil")
	}
}

func TestParseHTTPHeaders(t *testing.T) {
	h := parseHTTPHeaders("Authorization: Bearer x ; X-Env:prod; bad-no-colon; :novalue; key:")
	if got := h.Get("Authorization"); got != "Bearer x" {
		t.Errorf("Authorization = %q, want 'Bearer x'", got)
	}
	if got := h.Get("X-Env"); got != "prod" {
		t.Errorf("X-Env = %q, want prod", got)
	}
	if len(h) != 2 {
		t.Errorf("header count = %d, want 2 (malformed entries dropped): %+v", len(h), h)
	}
}

func TestParseMCPClientConfigV2(t *testing.T) {
	stdio, err := parseMCPClientConfigV2(&MCPClientConfigV2{Command: "echo"})
	if err != nil {
		t.Fatalf("stdio parse: %v", err)
	}
	if _, ok := stdio.(*StdioMCPClientConfig); !ok {
		t.Errorf("stdio parse type = %T, want *StdioMCPClientConfig", stdio)
	}

	stream, err := parseMCPClientConfigV2(&MCPClientConfigV2{URL: "https://x/", TransportType: MCPClientTypeStreamable})
	if err != nil {
		t.Fatalf("streamable parse: %v", err)
	}
	if _, ok := stream.(*StreamableMCPClientConfig); !ok {
		t.Errorf("streamable parse type = %T, want *StreamableMCPClientConfig", stream)
	}

	sse, err := parseMCPClientConfigV2(&MCPClientConfigV2{URL: "https://x/"})
	if err != nil {
		t.Fatalf("sse parse: %v", err)
	}
	if _, ok := sse.(*SSEMCPClientConfig); !ok {
		t.Errorf("sse parse type = %T, want *SSEMCPClientConfig", sse)
	}

	if _, err := parseMCPClientConfigV2(&MCPClientConfigV2{}); err == nil {
		t.Error("empty config parse: want error, got nil")
	}
}
