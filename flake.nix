{
  description = "proxy-mcp — aggregating MCP proxy with a real readiness gate";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # Package builder, shared by the per-system packages and the overlay so
      # there's a single source of truth for pname/version/vendorHash/ldflags.
      # `just sync-flake` (and CI) rewrite the version + vendorHash lines below
      # by regex, so keep them on their own lines.
      mkProxyMcp = pkgs: pkgs.buildGoModule {
        pname = "proxy-mcp";
        version = "0.0.7";
        src = ./.;
        # buildGoModule fetches Go deps through the module proxy and hashes the
        # resulting vendor tree; `vendorHash` pins that hash so the sandboxed
        # build is reproducible. Bump after any `go get` / `go mod tidy` that
        # changes go.sum — `just sync-flake` (and CI) regenerates it; `nix
        # build` prints the expected hash on mismatch.
        # go-sum: dd6cd4f1b2271faf2215fcaf0251073ad509e73f472ec3954b02dbc85a7200d8
        vendorHash = "sha256-5+MOQgyhQEau2SuixnKAx0N9Am+L2xQKTDmq6yEuC9w=";
        ldflags = [
          "-s"
          "-w"
          "-X main.BuildVersion=0.0.7"
        ];
        doCheck = true;
        meta = {
          description = "Aggregating MCP proxy with a real readiness gate";
          homepage = "https://github.com/stubbedev/proxy-mcp";
          license = nixpkgs.lib.licenses.mit;
          mainProgram = "proxy-mcp";
        };
      };
    in
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        proxy-mcp = mkProxyMcp pkgs;
      in
      {
        packages = {
          default = proxy-mcp;
          proxy-mcp = proxy-mcp;
        };

        apps.default = {
          type = "app";
          program = "${proxy-mcp}/bin/proxy-mcp";
          meta.description = "Aggregating MCP proxy with a real readiness gate";
        };

        checks.build = proxy-mcp;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            golangci-lint
            just
            git
          ];
          shellHook = ''
            echo "proxy-mcp dev shell — \`just build\` to compile, \`just test\` to test"
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      })
    // {
      # System-independent outputs. The overlay adds `proxy-mcp` to a pkgs set;
      # the modules wire a Type=notify systemd service that consumes the proxy's
      # sd_notify readiness gate (and optional watchdog).
      overlays.default = final: _prev: { proxy-mcp = mkProxyMcp final; };
      nixosModules.default = import ./nix/module.nix { inherit self; isHome = false; };
      homeModules.default = import ./nix/module.nix { inherit self; isHome = true; };
    };
}
