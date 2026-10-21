{
  perSystem = { pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
      inputsFrom = [ self'.packages.proxy-mcp ];
      packages = with pkgs; [
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
  };
}
