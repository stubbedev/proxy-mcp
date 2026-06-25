package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	reconnectInitialBackoff = 1 * time.Second
	reconnectMaxBackoff     = 30 * time.Second
)

// upstream owns one backend end to end: the persistent proxy-side server (whose
// handler is mounted once) and the current client connection, which is rebuilt
// on connection loss. Forwarding hooks/providers read the live client via cur(),
// so they keep working across reconnects.
type upstream struct {
	name      string
	clientCfg *MCPClientConfigV2
	info      mcp.Implementation
	srv       *Server

	mu     sync.RWMutex
	client *Client
}

// newUpstream builds the upstream and its persistent proxy server. The server's
// completion/subscribe/setLevel forwarding is wired to a forwarder that reads
// this upstream's live client.
func newUpstream(name string, serverConfig *MCPProxyConfigV2, clientConfig *MCPClientConfigV2, info mcp.Implementation) (*upstream, error) {
	u := &upstream{name: name, clientCfg: clientConfig, info: info}
	srv, err := newMCPServer(name, serverConfig, clientConfig, forwarder{up: u})
	if err != nil {
		return nil, err
	}
	u.srv = srv
	return u, nil
}

func (u *upstream) cur() *Client {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.client
}

// connect builds a fresh client, wires auto-reconnect, initializes it, and
// (re)registers the upstream's capabilities on the persistent server. Safe to
// call repeatedly: capability sets are replaced, not appended.
func (u *upstream) connect(ctx context.Context) error {
	c, err := newMCPClient(u.name, u.clientCfg)
	if err != nil {
		return err
	}
	// Rebuild on connection loss. Registered before Start so no drop is missed.
	c.client.OnConnectionLost(func(e error) {
		log.Printf("<%s> connection lost: %v", u.name, e)
		go u.reconnect(ctx)
	})
	if err := c.addToMCPServer(ctx, u.info, u.srv.mcpServer); err != nil {
		_ = c.Close()
		return err
	}
	u.mu.Lock()
	u.client = c
	u.mu.Unlock()
	return nil
}

// reconnect rebuilds the upstream connection after a drop, retrying with
// exponential backoff until it succeeds or ctx is cancelled. On success the
// proxy's capability set is refreshed and one list-changed reaches the clients.
func (u *upstream) reconnect(ctx context.Context) {
	backoff := reconnectInitialBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		log.Printf("<%s> reconnecting", u.name)
		if err := u.connect(ctx); err != nil {
			log.Printf("<%s> reconnect failed: %v", u.name, err)
			backoff = min(backoff*2, reconnectMaxBackoff)
			continue
		}
		log.Printf("<%s> reconnected", u.name)
		return
	}
}

// close tears down the current client.
func (u *upstream) close() error {
	if c := u.cur(); c != nil {
		return c.Close()
	}
	return nil
}
