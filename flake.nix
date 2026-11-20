{
  description = "Ichnora: local memory with provenance for coding agents";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/0bb8272318b4ca41bf0edc6224df3e3e3046b70d";
    project-check = {
      url = "github:timfewi/project-check/56962fbd92f992938f39b44b2590adf843ebe757";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    inputs@{
      self,
      nixpkgs,
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
          pkgs = nixpkgs.legacyPackages.${system};
          ichnora = pkgs.callPackage ./nix/package.nix {
            buildGoModule = pkgs.buildGo126Module;
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

      checks = forAllSystems (
        system:
        {
          # Building it runs `go test` against the linked Nix SQLite.
          package = self.packages.${system}.ichnora;
          module-eval = import ./tests/module-eval.nix {
            inherit nixpkgs self;
            pkgs = nixpkgs.legacyPackages.${system};
          };
        }
        // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          module-vm = import ./tests/module-vm.nix {
            inherit self;
            pkgs = nixpkgs.legacyPackages.${system};
          };
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell (import ./devenv.nix { inherit pkgs inputs; });
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt-tree);
    };
}
