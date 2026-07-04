{
  description = "Reproducible development shell for notty";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
  };

  outputs = { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system:
          f (import nixpkgs {
            inherit system;
          }));
    in
    {
      devShells = forAllSystems (pkgs:
        let
          go = pkgs.go_1_23;
          node = pkgs.nodejs_22;
          docker = pkgs.writeShellScriptBin "docker" ''
            if [ "''${1:-}" = "compose" ]; then
              shift
              exec ${pkgs.docker-compose}/bin/docker-compose "$@"
            fi
            exec ${pkgs.docker}/bin/docker "$@"
          '';
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              node
              cargo
              rustc
              rustfmt
              clippy
              gnumake
              gcc
              pkg-config
              git
              curl
              jq
              postgresql_16
              sqlite
              docker
              docker-compose
              zig
            ] ++ lib.optionals stdenv.isDarwin [
              libiconv
            ];

            CGO_ENABLED = "1";
            NPM_CONFIG_FUND = "false";
            NPM_CONFIG_AUDIT = "false";

            shellHook = ''
              export PATH="$PWD/frontend/node_modules/.bin:$PATH"

              if [ ! -f third_party/y-crdt/Cargo.toml ]; then
                printf 'notty dev shell: third_party/y-crdt is empty; run git submodule update --init --recursive before Go builds/tests.\n' >&2
              fi

              printf 'notty dev shell: Go %s, Node %s, Rust %s\n' \
                "$(go version | awk '{print $3}')" \
                "$(node --version)" \
                "$(rustc --version | awk '{print $2}')"
            '';
          };
        });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}
