# The single `ichnora` executable linked against the pinned Nix SQLite.
{
  lib,
  buildGoModule,
  pkg-config,
  sqlite,
  src,
  version,
}:
buildGoModule {
  pname = "ichnora";
  inherit version src;
  # NAR hash of `go mod vendor` for go.sum (computed with `nix hash path
  # vendor`); recompute after dependency changes.
  vendorHash = "sha256-8gMQJJUk56OsEOlMbpviLeaEUVxgiPPBwRdaAaA/xt8=";
  subPackages = [ "cmd/ichnora" ];
  tags = [ "libsqlite3" ];
  env.CGO_ENABLED = "1";
  nativeBuildInputs = [ pkg-config ];
  buildInputs = [ sqlite ];
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];
  # The OpenCode adapter is executed by OpenCode's own runtime; it only
  # forwards messages to `ichnora hook opencode`.
  postInstall = ''
    install -Dm0644 adapters/opencode/index.js "$out/share/ichnora/opencode/index.js"
    install -Dm0644 -t "$out/share/doc/ichnora" LICENSE THIRD_PARTY_NOTICES.txt
    install -Dm0644 -t "$out/share/doc/ichnora/LICENSES" LICENSES/*.txt
  '';
  meta = {
    description = "Local memory with provenance for coding agents";
    homepage = "https://github.com/timfewi/ichnora";
    license = lib.licenses.asl20;
    mainProgram = "ichnora";
    platforms = lib.platforms.linux;
  };
}
