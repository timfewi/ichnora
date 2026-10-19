# Evaluate the NixOS module without building a system closure.
{
  pkgs,
  nixpkgs,
  self,
}:
let
  inherit (pkgs.stdenv.hostPlatform) system;
  inherit (nixpkgs) lib;
  mkConfig =
    extra:
    (lib.nixosSystem {
      inherit system;
      modules = [
        self.nixosModules.default
        {
          system.stateVersion = "26.05";
          services.ichnora = {
            enable = true;
            user = "alice";
            stateDir = "/home/alice/.local/state/ichnora";
            workspaceRoots = [ "/home/alice/code" ];
            sessionSources.claude = [ "/home/alice/.claude/projects" ];
            tools.tabula = "/run/current-system/sw/bin/tabula";
          }
          // extra;
        }
      ];
    }).config;
  plain = mkConfig { };
  described = mkConfig {
    environmentDescriptor = {
      enable = true;
      systemIdentity = "fixture-host";
    };
  };
  disabled = mkConfig { enable = false; };
  custom = mkConfig {
    agentSocket = "/run/custom-memory/reader.sock";
    adminSocket = "/run/custom-memory/owner.sock";
  };
  clientSocketsMatch =
    host:
    host.environment.variables.ICHNORA_SOCKET or null
    == host.services.ichnora.generatedConfig.agent_socket
    &&
      host.environment.variables.ICHNORA_ADMIN_SOCKET or null
      == host.services.ichnora.generatedConfig.admin_socket;
  service = plain.systemd.services.ichnora;
  exec = service.serviceConfig.ExecStart;
  generated = plain.services.ichnora.generatedConfig;
  describedConfig = described.services.ichnora.generatedConfig;
in
if
  service.serviceConfig.User == "alice"
  && !(service.serviceConfig ? Group)
  && service.serviceConfig.PrivateNetwork
  && service.serviceConfig.RestrictAddressFamilies == [ "AF_UNIX" ]
  && service.serviceConfig.ReadWritePaths == [ "/home/alice/.local/state/ichnora" ]
  && lib.hasInfix "\"serve\" \"--config\"" exec
  && generated.agent_socket == "/run/ichnora/agent.sock"
  && generated.admin_socket == "/run/ichnora/admin.sock"
  && generated.workspace_roots == [ "/home/alice/code" ]
  && generated.tools == { tabula = "/run/current-system/sw/bin/tabula"; }
  && !(generated ? environment_descriptor)
  && !(plain.systemd.services ? ichnora-environment)
  && described.systemd.services ? ichnora-environment
  && describedConfig.environment_descriptor == "/run/ichnora-environment/environment.json"
  && !(disabled.systemd.services ? ichnora)
  && clientSocketsMatch plain
  && clientSocketsMatch custom
  && !(disabled.environment.variables ? ICHNORA_SOCKET)
  && !(disabled.environment.variables ? ICHNORA_ADMIN_SOCKET)
then
  pkgs.runCommand "ichnora-module-eval" { } ''
    touch "$out"
  ''
else
  throw "services.ichnora did not evaluate as expected"
