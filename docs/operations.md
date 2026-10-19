# Operations

Steps that need separate owner authorization. Steps 1–3, activation on
memorycrime and enrollment of the infrastructure repositories are done; hooks,
Ichnora recall and historical import are not.
Commands use the implemented syntax; `PATH` placeholders are yours to fill.

## 1. Publish and verify the package

```sh
cd ~/code/harness-tools/private/ichnora
gh repo create timfewi/ichnora --private --source . --push
gh repo view timfewi/ichnora --json visibility   # must print PRIVATE
nix develop . --command just verify             # builds the package and runs go test in the sandbox
```

`just verify` is the first Nix build of the package; if `vendorHash` differs,
Nix prints the expected value for `nix/package.nix`.

## 2. Land the harness registration

`coding-agents-nix` holds the registration (`nix/modules/ichnora.nix`,
`tests/ichnora.nix`); update the host lock after changing it.

## 3. Wire the host (`nixos-host`)

Add the input next to the other private tools and lock both changed inputs:

```nix
ichnora = {
  url = "github:timfewi/ichnora/main";
  inputs.nixpkgs.follows = "nixpkgs";       # host nixpkgs has Go 1.26.8, SQLite 3.53.3
  inputs.project-check.follows = "project-check";
};
```

```sh
cd ~/code/nixos-host
nix flake update ichnora coding-agents
```

Register the prepared check in `checks.nix`:

```nix
ichnora-wiring = import ./tests/ichnora-wiring.nix {
  inherit identity lib pkgs;
  inputs = self.inputs;
};
```

Import the capabilities in the selected host (or a profile):

```nix
../../modules/capabilities/ai/ichnora-service.nix
../../modules/capabilities/ai/ichnora-client.nix
```

Run `project-check fast`, then preview with `just update <host> --plan`.
Activation (`nixos-rebuild switch` or `just update <host>`) is a separate
decision. The service starts with no enrolled project and collects nothing.

## 4. Enroll and collect

```sh
export ICHNORA_ADMIN_SOCKET=/run/ichnora/admin.sock ICHNORA_SOCKET=/run/ichnora/agent.sock
ichnora discover                                  # candidates only; nothing is read
ichnora enroll ~/code/projects/example            # recall and collection off
ichnora enroll ~/code/projects/example --collect  # re-enroll to collect future sessions
ichnora policy ~/code/projects/example/src/main.go
ichnora status
```

Capture hooks are registered only after setting
`programs.coding-agents.ichnora.hooks.enable = true;` in the host and
activating. Codex then shows its hook review once; trust the Ichnora hooks to
run them. To let Ichnora inject context instead of native memory, also set
`programs.coding-agents.ichnora.recallOwner = "ichnora";` (this disables Claude
Code auto-memory and Codex memories) and enroll with `--recall`.

## 5. Historical import (opt-in)

```sh
ichnora import preview --harness claude ~/.claude/projects/<dir>/<session>.jsonl
ichnora import apply --harness claude --confirm <token from preview> ~/.claude/projects/<dir>/<session>.jsonl
```

## 6. Retention, backup and restore

```sh
ichnora retention preview --selector session --target ses_...
ichnora retention apply --selector session --target ses_... --confirm <token>
ichnora backup ~/backups/ichnora-$(date +%F).db  # outside ~/code; 0600
sudo systemctl stop ichnora
ichnora restore --config <config path from `systemctl cat ichnora`> ~/backups/ichnora-YYYY-MM-DD.db
sudo systemctl start ichnora
```

Backups keep data deleted later; delete old backups to complete an erasure.

## 7. Optional checks against installed tools

```sh
nix develop . --command bash -c 'ICHNORA_LIVE_TOOLS=1 go test ./cmd/ichnora -run TestLiveComposition -count=1'
nix develop . --command bash -c 'ICHNORA_OPENCODE_RUNTIME=/nix/store/<hash>-opencode-2.0.32/bin/.opencode-wrapped go test ./cmd/ichnora -run TestOpenCodePlugin -count=1'
```

The second command needs the unwrapped executable of the pinned OpenCode
package (not the bounded launcher); it runs the plugin in OpenCode's embedded
runtime with a stand-in plugin context.
