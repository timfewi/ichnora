# NixOS module for the Ichnora memory service.
#
# The service owns the database and listens on two Unix sockets: the agent
# socket (reader and capture) and the admin socket (owner administration).
# Host policy (user, paths, session-source roots, resources) is supplied by
# the consumer. A fresh service has no enrolled projects and collects nothing.
{ self }:
{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.ichnora;
  json = pkgs.formats.json { };
  descriptorPath = "/run/ichnora-environment/environment.json";
  settings = {
    state_dir = cfg.stateDir;
    agent_socket = cfg.agentSocket;
    admin_socket = cfg.adminSocket;
    workspace_roots = cfg.workspaceRoots;
    session_sources = cfg.sessionSources;
    tools = lib.filterAttrs (_: v: v != null) {
      tabula = cfg.tools.tabula;
      agent_evidence = cfg.tools.agentEvidence;
      ast_index = cfg.tools.astIndex;
      ast_root = cfg.tools.astRoot;
    };
    inherit (cfg) limits;
  }
  // lib.optionalAttrs cfg.environmentDescriptor.enable { environment_descriptor = descriptorPath; };
  configFile = json.generate "ichnora-config.json" settings;
  facts = json.generate "ichnora-environment-facts.json" {
    format = "ichnora.environment/1";
    system_identity = cfg.environmentDescriptor.systemIdentity;
    policy_baseline = cfg.environmentDescriptor.policyBaseline;
    harness_versions = cfg.environmentDescriptor.harnessVersions;
  };
  absolute = lib.types.strMatching "/.+";
  owner = {
    User = cfg.user;
  }
  // lib.optionalAttrs (cfg.group != null) { Group = cfg.group; };
in
{
  options.services.ichnora = {
    enable = lib.mkEnableOption "the Ichnora memory service";

    package = lib.mkOption {
      type = lib.types.package;
      default =
        if self ? packages && self.packages ? ${pkgs.stdenv.hostPlatform.system} then
          self.packages.${pkgs.stdenv.hostPlatform.system}.default
        else
          throw "services.ichnora.package must be set when the flake package is unavailable";
      defaultText = lib.literalExpression "self.packages.\${system}.default";
      description = "The Ichnora package to run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = ''
        Operator account the service runs as. Clients must run as the same
        user: the service accepts only peers with its own UID.
      '';
    };

    group = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Group of the service; null uses the user's primary group.";
    };

    stateDir = lib.mkOption {
      type = absolute;
      example = "/home/alice/.local/state/ichnora";
      description = ''
        Private state directory (database, WAL, secret). It is created with mode
        0700 and must lie outside source trees and the Nix store.
      '';
    };

    agentSocket = lib.mkOption {
      type = absolute;
      default = "/run/ichnora/agent.sock";
      description = "Socket granting reader and capture roles to agents.";
    };

    adminSocket = lib.mkOption {
      type = absolute;
      default = "/run/ichnora/admin.sock";
      description = ''
        Socket that additionally grants owner administration. Do not project
        it into agent sandboxes.
      '';
    };

    workspaceRoots = lib.mkOption {
      type = lib.types.listOf absolute;
      default = [ ];
      description = ''
        Directories inside which projects may be enrolled. They are
        boundaries, not a corpus: nothing is read until a project is enrolled.
      '';
    };

    sessionSources = lib.mkOption {
      type = lib.types.attrsOf (lib.types.listOf absolute);
      default = { };
      example = {
        claude = [ "/home/alice/.claude/projects" ];
        codex = [ "/home/alice/.codex/sessions" ];
      };
      description = ''
        Native transcript roots per harness. A root permits reading only the
        transcripts named by hooks or explicit imports of enrolled projects.
      '';
    };

    tools = {
      tabula = lib.mkOption {
        type = lib.types.nullOr absolute;
        default = null;
        description = "Absolute path of the tabula executable, or null.";
      };
      agentEvidence = lib.mkOption {
        type = lib.types.nullOr absolute;
        default = null;
        description = "Absolute path of the agent-evidence executable, or null.";
      };
      astIndex = lib.mkOption {
        type = lib.types.nullOr absolute;
        default = null;
        description = "Absolute path of the ast-index executable, or null.";
      };
      astRoot = lib.mkOption {
        type = lib.types.nullOr absolute;
        default = null;
        description = "Root declared to the AST Index service; other paths are not passed to it.";
      };
    };

    limits = lib.mkOption {
      inherit (json) type;
      default = { };
      example = {
        max_db_bytes = 536870912;
      };
      description = "Overrides of the documented limits (docs/contract.md).";
    };

    generatedConfig = lib.mkOption {
      inherit (json) type;
      internal = true;
      readOnly = true;
      default = settings;
      description = "The service configuration as generated from these options.";
    };

    readOnlyPaths = lib.mkOption {
      type = lib.types.listOf absolute;
      default = [ ];
      description = ''
        Additional paths the service may read under ProtectHome=read-only.
        Workspace and session-source roots are readable already.
      '';
    };

    environmentDescriptor = {
      enable = lib.mkEnableOption ''
        a host environment descriptor. A oneshot unit writes declared facts
        with a fresh random activation epoch whenever the facts change'';
      systemIdentity = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Non-secret system identity label.";
      };
      policyBaseline = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Non-secret label of the declarative policy baseline.";
      };
      harnessVersions = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        description = "Declared harness versions.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 ${cfg.user} ${if cfg.group == null then "-" else cfg.group} - -"
    ];

    systemd.services.ichnora = {
      description = "Ichnora memory service";
      wantedBy = [ "multi-user.target" ];
      after = lib.optional cfg.environmentDescriptor.enable "ichnora-environment.service";
      wants = lib.optional cfg.environmentDescriptor.enable "ichnora-environment.service";
      serviceConfig = owner // {
        Type = "simple";
        ExecStart = utils.escapeSystemdExecArgs [
          (lib.getExe cfg.package)
          "serve"
          "--config"
          "${configFile}"
        ];
        RuntimeDirectory = "ichnora";
        RuntimeDirectoryMode = "0700";
        UMask = "0077";
        Restart = "on-failure";
        RestartSec = 2;
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateNetwork = true;
        ProtectSystem = "strict";
        ProtectHome = "read-only";
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [ "AF_UNIX" ];
        RestrictNamespaces = true;
        LockPersonality = true;
        ReadWritePaths = [ cfg.stateDir ];
        ReadOnlyPaths = cfg.readOnlyPaths;
      };
    };

    systemd.services.ichnora-environment = lib.mkIf cfg.environmentDescriptor.enable {
      description = "Write the Ichnora host environment descriptor";
      wantedBy = [ "multi-user.target" ];
      # A new epoch only when the declared facts change; never derived from a
      # generation number or flake revision.
      restartTriggers = [ facts ];
      serviceConfig = owner // {
        Type = "oneshot";
        RemainAfterExit = true;
        RuntimeDirectory = "ichnora-environment";
        RuntimeDirectoryMode = "0700";
        UMask = "0077";
      };
      path = [ pkgs.jq ];
      script = ''
        epoch="$(cat /proc/sys/kernel/random/uuid)"
        jq --arg epoch "$epoch" '. + {activation_epoch: $epoch}' ${facts} > ${descriptorPath}.tmp
        mv ${descriptorPath}.tmp ${descriptorPath}
      '';
    };
  };
}
