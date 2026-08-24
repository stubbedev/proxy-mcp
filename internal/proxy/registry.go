package proxy

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerCapabilities lists the upstream's tools/prompts/resources via the
// template session and (re)registers them on the proxy server, binding every
// handler to the upstream router. It replaces the previously registered set
// (remove-then-add), so it is idempotent across reconnects and list-changed
// resyncs; the SDK emits one list-changed to downstream clients on the change.
// Missing capabilities are tolerated — an upstream may expose any subset.
func (u *upstream) registerCapabilities(ctx context.Context) {
	cs := u.template()
	if cs == nil {
		return
	}
	u.regMu.Lock()
	defer u.regMu.Unlock()

	u.registerTools(ctx, cs)
	u.registerPrompts(ctx, cs)
	u.registerResources(ctx, cs)
	u.registerResourceTemplates(ctx, cs)
}

func (u *upstream) registerTools(ctx context.Context, cs *mcp.ClientSession) {
	var names []string
	added := false
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			log.Printf("<%s> Skipping tools: %v", u.name, err)
			return // upstream lacks tools (or transient) — leave existing set intact
		}
		if !u.filterTools(tool.Name) {
			continue
		}
		u.server.AddTool(tool, u.toolHandler)
		names = append(names, tool.Name)
		added = true
	}
	if stale := missing(u.regTools, names); len(stale) > 0 {
		u.server.RemoveTools(stale...)
	}
	u.regTools = names
	if added {
		log.Printf("<%s> registered %d tools", u.name, len(names))
	}
}

func (u *upstream) registerPrompts(ctx context.Context, cs *mcp.ClientSession) {
	var names []string
	for prompt, err := range cs.Prompts(ctx, nil) {
		if err != nil {
			log.Printf("<%s> Skipping prompts: %v", u.name, err)
			return
		}
		if !u.filterPrompts(prompt.Name) {
			continue
		}
		u.server.AddPrompt(prompt, u.promptHandler)
		names = append(names, prompt.Name)
	}
	if stale := missing(u.regPrompts, names); len(stale) > 0 {
		u.server.RemovePrompts(stale...)
	}
	u.regPrompts = names
}

func (u *upstream) registerResources(ctx context.Context, cs *mcp.ClientSession) {
	var uris []string
	for resource, err := range cs.Resources(ctx, nil) {
		if err != nil {
			log.Printf("<%s> Skipping resources: %v", u.name, err)
			return
		}
		if !u.filterResources(resource.URI) {
			continue
		}
		u.server.AddResource(resource, u.resourceHandler)
		uris = append(uris, resource.URI)
	}
	if stale := missing(u.regResources, uris); len(stale) > 0 {
		u.server.RemoveResources(stale...)
	}
	u.regResources = uris
}

func (u *upstream) registerResourceTemplates(ctx context.Context, cs *mcp.ClientSession) {
	var uris []string
	for tmpl, err := range cs.ResourceTemplates(ctx, nil) {
		if err != nil {
			log.Printf("<%s> Skipping resource templates: %v", u.name, err)
			return
		}
		// ponytail: a template is matched by its URI template, not by the URIs it
		// expands to, so an allowed template that covers a blocked concrete
		// resource still lets a client read it. Block the template too if that
		// matters; per-URI expansion checking is not worth the machinery.
		if !u.filterResources(tmpl.URITemplate) {
			continue
		}
		u.server.AddResourceTemplate(tmpl, u.resourceHandler)
		uris = append(uris, tmpl.URITemplate)
	}
	if stale := missing(u.regResourceTmpls, uris); len(stale) > 0 {
		u.server.RemoveResourceTemplates(stale...)
	}
	u.regResourceTmpls = uris
}

// close tears down the template and every per-session connection.
func (u *upstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tmpl != nil {
		_ = u.tmpl.cs.Close()
		// Drop the reference so template() reports nil after teardown; a stale
		// closed session would slip past the sessionFor nil-guard.
		u.tmpl = nil
	}
	for id, sc := range u.sessions {
		_ = sc.cs.Close()
		delete(u.sessions, id)
	}
}

// missing returns the elements of old not present in current.
func missing(old, current []string) []string {
	keep := make(map[string]struct{}, len(current))
	for _, n := range current {
		keep[n] = struct{}{}
	}
	var gone []string
	for _, n := range old {
		if _, ok := keep[n]; !ok {
			gone = append(gone, n)
		}
	}
	return gone
}
