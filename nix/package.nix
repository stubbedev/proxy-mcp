let
  # Package builder, shared by the per-system packages and the overlay so
  # there's a single source of truth for pname/version/vendorHash/ldflags.
  # `just sync-flake` (and CI) rewrite the version + vendorHash lines below
  # by regex, so keep them on their own lines.
  mkProxyMcp = pkgs: pkgs.buildGo127Module {
    pname = "proxy-mcp";
    version = "0.0.24";
    src = pkgs.lib.fileset.toSource {
      root = ../.;
      fileset = pkgs.lib.fileset.difference (pkgs.lib.fileset.gitTracked ../.) (
        pkgs.lib.fileset.unions [
          ../flake.nix
          ../flake.lock
          ../nix
        ]
      );
    };
    # buildGoModule fetches Go deps through the module proxy and hashes the
    # resulting vendor tree; `vendorHash` pins that hash so the sandboxed
    # build is reproducible. Bump after any `go get` / `go mod tidy` that
    # changes go.sum — `just sync-flake` (and CI) regenerates it; `nix
    # build` prints the expected hash on mismatch.
    # go-sum: d99db3318553b4efc06da6a26141336f878540ce88a45f49303c842782b5a7da
    vendorHash = "sha256-6rYvt2U3tO9mpRfXRA0SFgSzp5rT1koldURMntvLWXc=";
    # Static on Linux: buildGoModule defaults CGO_ENABLED=1, which links the
    # binary against glibc and ties it to this store path's loader. Darwin
    # keeps cgo — launchd socket activation needs launch_activate_socket,
    # and a darwin Go binary always links libSystem dynamically anyway, so
    # dropping cgo there costs the feature and buys no static linkage.
    env.CGO_ENABLED = if pkgs.stdenv.hostPlatform.isDarwin then "1" else "0";
    ldflags = [
      "-s"
      "-w"
      "-X main.BuildVersion=0.0.24"
    ];
    doCheck = true;
    meta = {
      description = "Aggregating MCP proxy with a real readiness gate";
      homepage = "https://github.com/stubbedev/proxy-mcp";
      license = pkgs.lib.licenses.mit;
      mainProgram = "proxy-mcp";
    };
  };
in
{
  # The overlay adds `proxy-mcp` to a pkgs set.
  flake.overlays.default = final: _prev: { proxy-mcp = mkProxyMcp final; };

  perSystem = { lib, pkgs, self', ... }: {
    packages = {
      default = self'.packages.proxy-mcp;
      proxy-mcp = mkProxyMcp pkgs;
    };

    apps.default = {
      type = "app";
      program = lib.getExe self'.packages.proxy-mcp;
      meta = { inherit (self'.packages.proxy-mcp.meta) description; };
    };

    checks.build = self'.packages.proxy-mcp;
  };
}
