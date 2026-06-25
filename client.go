package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Client struct {
	name            string
	needPing        bool
	needManualStart bool
	client          *client.Client
	options         *OptionsV2
	callTimeout     time.Duration
}

// ctxWithTimeout derives a child context bounded by callTimeout (no bound when
// it is 0). The returned cancel must always be called.
func (c *Client) ctxWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.callTimeout > 0 {
		return context.WithTimeout(ctx, c.callTimeout)
	}
	return context.WithCancel(ctx)
}

// callTool / getPrompt / readResource / complete forward a request to the
// upstream, each bounded by callTimeout. These are the handlers registered on
// the proxy server, so a hung upstream fails fast instead of blocking forever.
func (c *Client) callTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := c.ctxWithTimeout(ctx)
	defer cancel()
	return c.client.CallTool(ctx, req)
}

func (c *Client) getPrompt(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	ctx, cancel := c.ctxWithTimeout(ctx)
	defer cancel()
	return c.client.GetPrompt(ctx, req)
}

func (c *Client) readResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ctx, cancel := c.ctxWithTimeout(ctx)
	defer cancel()
	readResource, err := c.client.ReadResource(ctx, req)
	if err != nil {
		return nil, err
	}
	return readResource.Contents, nil
}

func (c *Client) complete(ctx context.Context, req mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	ctx, cancel := c.ctxWithTimeout(ctx)
	defer cancel()
	return c.client.Complete(ctx, req)
}

// newMCPClient builds the upstream client. HTTP/SSE upstreams get a per-request
// header func that replays the caller's headers verbatim (see forwardedHeaders)
// and — for streamable — a continuous GET listening stream so the proxy can
// receive the upstream's unsolicited notifications (e.g. list-changed) and relay
// them on. (Server→client *requests* like sampling are intentionally not
// bridged: a multiplexing aggregator can't attribute them to one downstream
// session, and mcp-go delivers them on a separate listening stream anyway.)
func newMCPClient(name string, conf *MCPClientConfigV2) (*Client, error) {
	clientInfo, pErr := parseMCPClientConfigV2(conf)
	if pErr != nil {
		return nil, pErr
	}
	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		mcpClient, err := client.NewStdioMCPClient(v.Command, envs, v.Args...)
		if err != nil {
			return nil, err
		}
		return &Client{
			name:        name,
			client:      mcpClient,
			options:     conf.Options,
			callTimeout: conf.Options.callTimeout(),
		}, nil
	case *SSEMCPClientConfig:
		opts := []transport.ClientOption{transport.WithHeaderFunc(forwardedHeaders)}
		if len(v.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(v.Headers))
		}
		mcpClient, err := client.NewSSEMCPClient(v.URL, opts...)
		if err != nil {
			return nil, err
		}
		return &Client{
			name:            name,
			needPing:        true,
			needManualStart: true,
			client:          mcpClient,
			options:         conf.Options,
			callTimeout:     conf.Options.callTimeout(),
		}, nil
	case *StreamableMCPClientConfig:
		// WithContinuousListening opens a GET stream so the proxy sees the
		// upstream's unsolicited notifications (streamable delivers those only on
		// the listening stream), which bridgeNotifications relays downstream.
		opts := []transport.StreamableHTTPCOption{
			transport.WithHTTPHeaderFunc(forwardedHeaders),
			transport.WithContinuousListening(),
		}
		if len(v.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(v.Headers))
		}
		if v.Timeout > 0 {
			opts = append(opts, transport.WithHTTPTimeout(v.Timeout))
		}
		mcpClient, err := client.NewStreamableHttpClient(v.URL, opts...)
		if err != nil {
			return nil, err
		}
		return &Client{
			name:            name,
			needPing:        true,
			needManualStart: true,
			client:          mcpClient,
			options:         conf.Options,
			callTimeout:     conf.Options.callTimeout(),
		}, nil
	}
	return nil, errors.New("invalid client type")
}

// addToMCPServer initializes the upstream and registers its capabilities on the
// proxy server, then starts notification bridging and the ping task. It is
// re-callable (reconnect): capability sets are replaced, not appended.
func (c *Client) addToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	if err := c.initialize(ctx, clientInfo); err != nil {
		return err
	}
	if err := c.registerCapabilities(ctx, mcpServer); err != nil {
		return err
	}
	// Bridge upstream→downstream notifications (progress, logging, resource
	// updates, list-changed) so the caller sees them as if connected directly.
	c.bridgeNotifications(ctx, mcpServer)

	if c.needPing {
		go c.startPingTask(ctx)
	}
	return nil
}

// initialize starts the transport (when needed) and performs the MCP
// initialize handshake against the upstream.
func (c *Client) initialize(ctx context.Context, clientInfo mcp.Implementation) error {
	if c.needManualStart {
		if err := c.client.Start(ctx); err != nil {
			return err
		}
	}
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = clientInfo
	initRequest.Params.Capabilities = mcp.ClientCapabilities{
		Experimental: make(map[string]any),
	}
	if _, err := c.client.Initialize(ctx, initRequest); err != nil {
		return err
	}
	log.Printf("<%s> Successfully initialized MCP client", c.name)
	return nil
}

// registerCapabilities (re)registers the upstream's tools, prompts, resources,
// and resource templates on the proxy server using Set* so it is idempotent
// across reconnects. Tools are required (their list error is fatal); prompts and
// resources are optional and tolerated when the upstream doesn't implement them.
func (c *Client) registerCapabilities(ctx context.Context, mcpServer *server.MCPServer) error {
	if tools, err := c.listTools(ctx); err != nil {
		log.Printf("<%s> Skipping tools: %v", c.name, err)
	} else {
		mcpServer.SetTools(tools...)
	}

	if prompts, err := c.listPrompts(ctx); err != nil {
		log.Printf("<%s> Skipping prompts: %v", c.name, err)
	} else {
		mcpServer.SetPrompts(prompts...)
	}
	if resources, err := c.listResources(ctx); err != nil {
		log.Printf("<%s> Skipping resources: %v", c.name, err)
	} else {
		mcpServer.SetResources(resources...)
	}
	if templates, err := c.listResourceTemplates(ctx); err != nil {
		log.Printf("<%s> Skipping resource templates: %v", c.name, err)
	} else {
		for _, t := range templates {
			mcpServer.AddResourceTemplate(t.Template, t.Handler)
		}
	}
	return nil
}

// bridgeNotifications relays upstream notifications to the downstream clients.
// list-changed notifications are handled by re-listing and re-registering the
// affected capability on the proxy (which itself emits one list-changed to the
// clients); all other notifications are forwarded verbatim.
func (c *Client) bridgeNotifications(ctx context.Context, mcpServer *server.MCPServer) {
	c.client.OnNotification(func(n mcp.JSONRPCNotification) {
		switch n.Method {
		case string(mcp.MethodNotificationToolsListChanged):
			c.resyncTools(ctx, mcpServer)
		case string(mcp.MethodNotificationPromptsListChanged):
			c.resyncPrompts(ctx, mcpServer)
		case string(mcp.MethodNotificationResourcesListChanged):
			c.resyncResources(ctx, mcpServer)
		default:
			mcpServer.SendNotificationToAllClients(n.Method, notificationParams(n))
		}
	})
}

// resyncTools re-lists the upstream's tools and replaces the proxy's registered
// set, so a tools/list_changed makes the new set visible (SetTools emits one
// tools/list_changed to the downstream clients). resyncPrompts/resyncResources
// do the same for their capability.
func (c *Client) resyncTools(ctx context.Context, mcpServer *server.MCPServer) {
	tools, err := c.listTools(ctx)
	if err != nil {
		log.Printf("<%s> tools resync failed: %v", c.name, err)
		return
	}
	mcpServer.SetTools(tools...)
}

func (c *Client) resyncPrompts(ctx context.Context, mcpServer *server.MCPServer) {
	prompts, err := c.listPrompts(ctx)
	if err != nil {
		log.Printf("<%s> prompts resync failed: %v", c.name, err)
		return
	}
	mcpServer.SetPrompts(prompts...)
}

func (c *Client) resyncResources(ctx context.Context, mcpServer *server.MCPServer) {
	resources, err := c.listResources(ctx)
	if err != nil {
		log.Printf("<%s> resources resync failed: %v", c.name, err)
		return
	}
	mcpServer.SetResources(resources...)
}

func (c *Client) startPingTask(ctx context.Context) {
	interval := 30 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failCount := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("<%s> Context done, stopping ping", c.name)
			return
		case <-ticker.C:
			if err := c.client.Ping(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				failCount++
				log.Printf("<%s> MCP Ping failed: %v (count=%d)", c.name, err, failCount)
			} else if failCount > 0 {
				log.Printf("<%s> MCP Ping recovered after %d failures", c.name, failCount)
				failCount = 0
			}
		}
	}
}

// toolFilterFunc returns a predicate that applies this client's configured
// allow/block tool filter. With no filter configured every tool passes.
func (c *Client) toolFilterFunc() func(string) bool {
	if c.options == nil || c.options.ToolFilter == nil || len(c.options.ToolFilter.List) == 0 {
		return func(string) bool { return true }
	}
	filterSet := make(map[string]struct{}, len(c.options.ToolFilter.List))
	for _, toolName := range c.options.ToolFilter.List {
		filterSet[toolName] = struct{}{}
	}
	switch ToolFilterMode(strings.ToLower(string(c.options.ToolFilter.Mode))) {
	case ToolFilterModeAllow:
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if !inList {
				log.Printf("<%s> Ignoring tool %s as it is not in allow list", c.name, toolName)
			}
			return inList
		}
	case ToolFilterModeBlock:
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if inList {
				log.Printf("<%s> Ignoring tool %s as it is in block list", c.name, toolName)
			}
			return !inList
		}
	default:
		log.Printf("<%s> Unknown tool filter mode: %s, skipping tool filter", c.name, c.options.ToolFilter.Mode)
		return func(string) bool { return true }
	}
}

// listTools pages the upstream's full tool list, applies the tool filter, and
// returns ready-to-register ServerTools. Used both for the initial load and
// for resync on tools/list_changed.
func (c *Client) listTools(ctx context.Context) ([]server.ServerTool, error) {
	filter := c.toolFilterFunc()
	var out []server.ServerTool
	req := mcp.ListToolsRequest{}
	for {
		tools, err := c.client.ListTools(ctx, req)
		if err != nil {
			return nil, err
		}
		if tools == nil {
			return nil, fmt.Errorf("<%s> ListTools returned nil response without error", c.name)
		}
		for _, tool := range tools.Tools {
			if filter(tool.Name) {
				out = append(out, server.ServerTool{Tool: tool, Handler: c.callTool})
			}
		}
		if tools.NextCursor == "" {
			break
		}
		req.Params.Cursor = tools.NextCursor
	}
	log.Printf("<%s> Listed %d tools", c.name, len(out))
	return out, nil
}

// listPrompts pages the upstream's full prompt list into ServerPrompts.
func (c *Client) listPrompts(ctx context.Context) ([]server.ServerPrompt, error) {
	var out []server.ServerPrompt
	req := mcp.ListPromptsRequest{}
	for {
		prompts, err := c.client.ListPrompts(ctx, req)
		if err != nil {
			return nil, err
		}
		if prompts == nil {
			return nil, fmt.Errorf("<%s> ListPrompts returned nil response without error", c.name)
		}
		for _, prompt := range prompts.Prompts {
			out = append(out, server.ServerPrompt{Prompt: prompt, Handler: c.getPrompt})
		}
		if prompts.NextCursor == "" {
			break
		}
		req.Params.Cursor = prompts.NextCursor
	}
	return out, nil
}

// listResources pages the upstream's full resource list into ServerResources.
func (c *Client) listResources(ctx context.Context) ([]server.ServerResource, error) {
	var out []server.ServerResource
	req := mcp.ListResourcesRequest{}
	for {
		resources, err := c.client.ListResources(ctx, req)
		if err != nil {
			return nil, err
		}
		if resources == nil {
			return nil, fmt.Errorf("<%s> ListResources returned nil response without error", c.name)
		}
		for _, resource := range resources.Resources {
			out = append(out, server.ServerResource{Resource: resource, Handler: c.readResource})
		}
		if resources.NextCursor == "" {
			break
		}
		req.Params.Cursor = resources.NextCursor
	}
	return out, nil
}

// listResourceTemplates pages the upstream's resource templates.
func (c *Client) listResourceTemplates(ctx context.Context) ([]server.ServerResourceTemplate, error) {
	var out []server.ServerResourceTemplate
	req := mcp.ListResourceTemplatesRequest{}
	for {
		templates, err := c.client.ListResourceTemplates(ctx, req)
		if err != nil {
			return nil, err
		}
		if templates == nil || len(templates.ResourceTemplates) == 0 {
			break
		}
		for _, t := range templates.ResourceTemplates {
			out = append(out, server.ServerResourceTemplate{Template: t, Handler: c.readResource})
		}
		if templates.NextCursor == "" {
			break
		}
		req.Params.Cursor = templates.NextCursor
	}
	return out, nil
}

func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

type Server struct {
	tokens    []string
	mcpServer *server.MCPServer
	handler   http.Handler
}

func newMCPServer(name string, serverConfig *MCPProxyConfigV2, clientConfig *MCPClientConfigV2, fwd forwarder) (*Server, error) {
	// Declare listChanged for every capability so SetTools/SetPrompts/
	// SetResources (used by the resync path) notify the downstream clients.
	// WithCompletions + providers forward completion/complete to the upstream;
	// the hooks forward resources/subscribe, /unsubscribe, and logging/setLevel.
	serverOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
		server.WithCompletions(),
		server.WithPromptCompletionProvider(fwd),
		server.WithResourceCompletionProvider(fwd),
		server.WithHooks(fwd.hooks()),
	}

	if clientConfig.Options.LogEnabled.OrElse(false) {
		serverOpts = append(serverOpts, server.WithLogging())
	}
	mcpServer := server.NewMCPServer(
		name,
		serverConfig.Version,
		serverOpts...,
	)

	var handler http.Handler

	switch serverConfig.Type {
	case MCPServerTypeSSE:
		handler = server.NewSSEServer(
			mcpServer,
			server.WithStaticBasePath(name),
			server.WithBaseURL(serverConfig.BaseURL),
			// Capture the caller's headers for verbatim upstream forwarding.
			server.WithSSEContextFunc(withClientHeaders),
		)
	case MCPServerTypeStreamable:
		handler = server.NewStreamableHTTPServer(
			mcpServer,
			// Default session manager (StatelessGeneratingSessionIdManager), NOT
			// WithStateLess: stateless mode kills the server→client back-channel
			// that progress/logging/list-changed notifications and sampling/roots/
			// elicitation requests all need. The default keeps per-session SSE
			// streams while validating only the session-id format (so an
			// idle-exit + restart doesn't 404 a returning client).
			// Capture the caller's headers for verbatim upstream forwarding.
			server.WithHTTPContextFunc(withClientHeaders),
		)
	default:
		return nil, fmt.Errorf("unknown server type: %s", serverConfig.Type)
	}
	srv := &Server{
		mcpServer: mcpServer,
		handler:   handler,
	}

	if clientConfig.Options != nil && len(clientConfig.Options.AuthTokens) > 0 {
		srv.tokens = clientConfig.Options.AuthTokens
	}

	return srv, nil
}
