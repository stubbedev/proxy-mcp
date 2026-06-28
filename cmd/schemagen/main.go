// Command schemagen reflects the proxy config structs into a JSON Schema and
// prints it to stdout. It is the single source for config.schema.json,
// regenerated in CI on every push so the schema can never drift from the Go
// types. It lives in its own command (not the proxy package) so neither it nor
// its jsonschema dependency is linked into the shipped proxy binary.
//
//	go run ./cmd/schemagen > config.schema.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"github.com/invopop/jsonschema"
	optional "github.com/tbxark/optional-go"

	"github.com/stubbedev/proxy-mcp/internal/proxy"
)

const (
	schemaID = "https://raw.githubusercontent.com/stubbedev/proxy-mcp/master/config.schema.json"
	// AddGoComments builds keys as moduleBase + the walked dir, so the base is
	// the module root (not the package path) and the dir is where the structs
	// live; together they reconstruct the package's import path, which must equal
	// the runtime reflect PkgPath for descriptions to attach.
	commentsModuleBase = "github.com/stubbedev/proxy-mcp"
	commentsDir        = "./internal/proxy"
	schemaTitle        = "proxy-mcp config"
	schemaDescNotice   = "Configuration for proxy-mcp, the aggregating MCP proxy. Generated from the Go config structs by `go run ./cmd/schemagen` — do not edit by hand."
)

// enumSchema is a string schema constrained to a fixed set of values, sourced
// from the exported consts so the schema's enums are always exactly the values
// the code accepts.
func enumSchema(vals ...string) *jsonschema.Schema {
	e := make([]any, len(vals))
	for i, v := range vals {
		e[i] = v
	}
	return &jsonschema.Schema{Type: "string", Enum: e}
}

func main() {
	r := &jsonschema.Reflector{
		// Inline the root Config instead of emitting a $ref to it.
		ExpandedStruct: true,
		// Don't infer "required" from the absence of omitempty — nearly every
		// config field is optional. Only fields tagged `jsonschema:"required"`
		// (just mcpProxy) are required, so minimal configs validate.
		RequiredFromJSONSchemaTags: true,
	}
	// Field descriptions come from the Go doc comments on the struct fields, so
	// the schema documentation has a single source (the code).
	if err := r.AddGoComments(commentsModuleBase, commentsDir); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: AddGoComments: %v\n", err)
		os.Exit(1)
	}
	// Map the named enum types to string enums (values from the consts) and the
	// optional.Field[bool] wrapper to a plain boolean.
	r.Mapper = func(t reflect.Type) *jsonschema.Schema {
		switch t {
		case reflect.TypeFor[proxy.MCPServerType]():
			return enumSchema(string(proxy.MCPServerTypeSSE), string(proxy.MCPServerTypeStreamable))
		case reflect.TypeFor[proxy.MCPClientType]():
			return enumSchema(string(proxy.MCPClientTypeStdio), string(proxy.MCPClientTypeSSE), string(proxy.MCPClientTypeStreamable))
		case reflect.TypeFor[proxy.ToolFilterMode]():
			return enumSchema(string(proxy.ToolFilterModeAllow), string(proxy.ToolFilterModeBlock))
		case reflect.TypeFor[proxy.ConnMode]():
			return enumSchema(string(proxy.ConnModePerSession), string(proxy.ConnModeShared))
		case reflect.TypeFor[optional.Field[bool]]():
			return &jsonschema.Schema{Type: "boolean"}
		}
		return nil
	}

	s := r.Reflect(&proxy.Config{})
	s.ID = schemaID
	s.Title = schemaTitle
	s.Description = schemaDescNotice

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: marshal: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
