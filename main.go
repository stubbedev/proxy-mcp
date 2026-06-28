package main

import "github.com/stubbedev/proxy-mcp/internal/proxy"

// BuildVersion is set at link time via -X main.BuildVersion. Kept in package
// main so the existing ldflags (justfile, flake.nix, Docker, release CI) keep
// targeting main.BuildVersion.
var BuildVersion = "dev"

func main() { proxy.Run(BuildVersion) }
