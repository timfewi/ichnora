{ pkgs, inputs, ... }:
{
  packages = [
    inputs.project-check.packages.${pkgs.stdenv.hostPlatform.system}.project-check
    pkgs.bashInteractive
    pkgs.coreutils
    pkgs.deadnix
    pkgs.git
    pkgs.go_1_26
    pkgs.govulncheck
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
    pkgs.zizmor
  ];
  env = {
    CGO_ENABLED = "1";
    GOTOOLCHAIN = "local";
    GOFLAGS = "-tags=libsqlite3";
  };
}
