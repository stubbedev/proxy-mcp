{
  description = "proxy-mcp — aggregating MCP proxy with a real readiness gate";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        proxy-mcp = pkgs.buildGoModule {
          pname = "proxy-mcp";
          version = "0.0.0";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that hash
          # so the sandboxed build is reproducible. Bump after any `go
          # get` / `go mod tidy` that changes go.sum — `just sync-flake`
          # (and CI) regenerates it; `nix build` prints the expected hash
          # on mismatch.
          # go-sum: aa3c4a1f72ee40c9841faceb339008be5ea7528885cdfa6b335bfaabd8ffd1e4
          vendorHash = "sha256-MS6eMdECdROjnPoHpUSNRnR7Bp4KUDkuNDXHXLRBfIQ=";
          ldflags = [
            "-s"
            "-w"
            "-X main.BuildVersion=0.0.0"
          ];
          doCheck = true;
        };
      in
      {
        packages = {
          default = proxy-mcp;
          proxy-mcp = proxy-mcp;
        };

        apps.default = flake-utils.lib.mkApp {
          drv = proxy-mcp;
          name = "proxy-mcp";
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
      });
}
