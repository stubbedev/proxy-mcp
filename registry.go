package main

import (
	"context"
	"log"
	"strings"

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
	filter := u.toolFilter()
	var names []string
	added := false
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			log.Printf("<%s> Skipping tools: %v", u.name, err)
			return // upstream lacks tools (or transient) — leave existing set intact
		}
		if !filter(tool.Name) {
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
		u.server.AddResourceTemplate(tmpl, u.resourceHandler)
		uris = append(uris, tmpl.URITemplate)
	}
	if stale := missing(u.regResourceTmpls, uris); len(stale) > 0 {
		u.server.RemoveResourceTemplates(stale...)
	}
	u.regResourceTmpls = uris
}

// toolFilter returns a predicate applying the configured allow/block list.
func (u *upstream) toolFilter() func(string) bool {
	o := u.clientCfg.Options
	if o == nil || o.ToolFilter == nil || len(o.ToolFilter.List) == 0 {
		return func(string) bool { return true }
	}
	set := make(map[string]struct{}, len(o.ToolFilter.List))
	for _, n := range o.ToolFilter.List {
		set[n] = struct{}{}
	}
	block := ToolFilterMode(strings.ToLower(string(o.ToolFilter.Mode))) == ToolFilterModeBlock
	return func(name string) bool {
		_, in := set[name]
		if block {
			return !in
		}
		return in
	}
}

// close tears down the template and every per-session connection.
func (u *upstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tmpl != nil {
		_ = u.tmpl.cs.Close()
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
