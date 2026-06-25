# Shared NixOS / home-manager module for proxy-mcp. Imported twice from the
# flake: with isHome=false for nixosModules.default (system service) and
# isHome=true for homeModules.default (systemd --user service).
#
# The service runs as Type=notify, so it only reaches `active (running)` once
# proxy-mcp has connected every upstream and registered its routes (the
# sd_notify READY=1 gate). Anything ordered `after`/`requires` it therefore
# never races a not-yet-registered route. Set watchdogSec to have systemd
# restart the proxy if it wedges (proxy-mcp pings WATCHDOG=1 automatically).
{ self, isHome }:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.proxy-mcp;
  jsonFormat = pkgs.formats.json { };
  configFile =
    if cfg.configFile != null
    then cfg.configFile
    else jsonFormat.generate "proxy-mcp-config.json" cfg.settings;

  execStart = lib.concatStringsSep " " (
    [
      (lib.getExe cfg.package)
      "--config"
      "${configFile}"
      "--expand-env=${lib.boolToString cfg.expandEnv}"
    ]
    ++ lib.optional (cfg.idleTimeout != null) "--idle-timeout=${cfg.idleTimeout}"
    ++ cfg.extraArgs
  );

  serviceConfig = {
    Type = "notify";
    ExecStart = execStart;
    Restart = "on-failure";
    RestartSec = 2;
  }
  // lib.optionalAttrs (cfg.watchdogSec != null) { WatchdogSec = cfg.watchdogSec; }
  // lib.optionalAttrs (cfg.environmentFile != null) { EnvironmentFile = cfg.environmentFile; };
in
{
  options.services.proxy-mcp = {
    enable = lib.mkEnableOption "the proxy-mcp MCP proxy service";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.proxy-mcp;
      defaultText = lib.literalExpression "proxy-mcp.packages.\${system}.proxy-mcp";
      description = "The proxy-mcp package to run.";
    };

    settings = lib.mkOption {
      type = jsonFormat.type;
      default = { };
      example = lib.literalExpression ''
        {
          mcpProxy = {
            baseURL = "http://localhost:9090";
            addr = ":9090";
            name = "proxy-mcp";
            version = "1.0.0";
            type = "streamable-http";
          };
          mcpServers.fetch = { command = "uvx"; args = [ "mcp-server-fetch" ]; };
        }
      '';
      description = ''
        Contents of the proxy-mcp config.json, written to the Nix store.
        Ignored when configFile is set. For secrets, reference an env var in
        the value (e.g. "$TOKEN") and supply it via environmentFile.
      '';
    };

    configFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to an existing config.json. Overrides settings when set.";
    };

    expandEnv = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Expand environment variables in the config file (--expand-env).";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/run/secrets/proxy-mcp.env";
      description = "EnvironmentFile with secrets (tokens) referenced from settings.";
    };

    watchdogSec = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "30s";
      description = "systemd WatchdogSec. proxy-mcp pings WATCHDOG=1 automatically.";
    };

    idleTimeout = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "5m";
      description = ''
        Pass --idle-timeout: exit after this much idle time with no proxied
        requests (a Go duration like "5m"). Null omits the flag (no idle exit).
        Pair with socket activation to start on demand and stop when quiet.
      '';
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "--insecure" ];
      description = "Extra command-line arguments passed to proxy-mcp.";
    };
  };

  config = lib.mkIf cfg.enable (
    if isHome then {
      systemd.user.services.proxy-mcp = {
        Unit.Description = "proxy-mcp — aggregating MCP proxy";
        Unit.After = [ "network.target" ];
        Install.WantedBy = [ "default.target" ];
        Service = serviceConfig;
      };
    } else {
      systemd.services.proxy-mcp = {
        description = "proxy-mcp — aggregating MCP proxy";
        after = [ "network.target" ];
        wantedBy = [ "multi-user.target" ];
        inherit serviceConfig;
      };
    }
  );
}
