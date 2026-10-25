{
  description = "Ichnora: local memory with provenance for coding agents";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/eaad089433ca2bb662274377d33df3d0e51ef28b";
    project-check = {
      url = "github:timfewi/project-check-nix/56962fbd92f992938f39b44b2590adf843ebe757";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      project-check,
      ...
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      version = "0.1.0";
    in
    {
      packages = forAllSystems (
        system:
        let
          ichnora = nixpkgs.legacyPackages.${system}.callPackage ./nix/package.nix {
            inherit version;
            src = self;
          };
        in
        {
          inherit ichnora;
          default = ichnora;
        }
      );

      nixosModules.default = import ./nix/module.nix { inherit self; };

      checks = forAllSystems (system: {
        # Building it runs `go test` against the linked Nix SQLite.
        package = self.packages.${system}.ichnora;
        module-eval = import ./tests/module-eval.nix {
          inherit nixpkgs self;
          pkgs = nixpkgs.legacyPackages.${system};
        };
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              project-check.packages.${system}.project-check
              pkgs.bashInteractive
              pkgs.coreutils
              pkgs.deadnix
              pkgs.git
              pkgs.go
              pkgs.jq
              pkgs.just
              pkgs.nix
              pkgs.nixfmt
              pkgs.pkg-config
              pkgs.prettier
              pkgs.python3
              pkgs.ripgrep
              pkgs.shfmt
              pkgs.sqlite
              pkgs.statix
            ];
            # Builds never download another toolchain.
            env = {
              CGO_ENABLED = "1";
              GOTOOLCHAIN = "local";
              GOFLAGS = "-tags=libsqlite3";
            };
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt-tree);
    };
}
