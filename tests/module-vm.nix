# Exercise the actual systemd sandbox and socket ownership in an isolated VM.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "ichnora-module-runtime";

  nodes.machine = {
    imports = [ self.nixosModules.default ];
    users.users.alice.isNormalUser = true;
    users.users.bob.isNormalUser = true;
    services.ichnora = {
      enable = true;
      user = "alice";
      stateDir = "/home/alice/.local/state/ichnora";
      workspaceRoots = [ "/home/alice/code" ];
      agentSocket = "/run/custom-memory/agent/reader.sock";
      adminSocket = "/run/custom-memory/admin/owner.sock";
    };
    systemd.tmpfiles.rules = [
      "d /home/alice/code 0750 alice users - -"
      "d /home/alice/code/example 0750 alice users - -"
    ];
    environment.systemPackages = [ pkgs.git ];
  };

  testScript = ''
    import json
    import shlex

    start_all()
    machine.wait_for_unit("ichnora.service")

    def client(command):
        script = "cd /home/alice/code/example && " + command
        return machine.succeed("su - alice -c " + shlex.quote(script))

    for directory in ["/run/custom-memory/agent", "/run/custom-memory/admin"]:
        assert machine.succeed("stat -c '%U:%a' " + directory).strip() == "alice:700"
    for socket in ["/run/custom-memory/agent/reader.sock", "/run/custom-memory/admin/owner.sock"]:
        machine.wait_until_succeeds("test -S " + socket)
        assert machine.succeed("stat -c '%U:%a' " + socket).strip() == "alice:600"
        machine.fail("su - bob -c " + shlex.quote("test -r " + socket))

    client("git init -q")
    assert client('printf %s "$ICHNORA_SOCKET"') == "/run/custom-memory/agent/reader.sock"
    assert client('printf %s "$ICHNORA_ADMIN_SOCKET"') == "/run/custom-memory/admin/owner.sock"
    enrolled = json.loads(client("ichnora enroll /home/alice/code/example"))
    assert enrolled["ok"], enrolled
    payload = json.dumps({"claims": [{"kind": "decision", "title": "Runtime fixture decision"}]})
    captured = json.loads(client("printf %s " + shlex.quote(payload) + " | ichnora capture"))
    assert captured["ok"], captured
    claim_id = captured["result"]["ids"][0]

    def assert_memory():
        result = json.loads(client("ichnora search runtime --kind decision"))
        assert result["ok"], result
        assert claim_id in [item["id"] for item in result["result"]["items"]], result

    assert_memory()
    machine.succeed("systemctl restart ichnora.service")
    machine.wait_for_unit("ichnora.service")
    machine.wait_until_succeeds("test -S /run/custom-memory/agent/reader.sock")
    assert_memory()

    restricted = client("ICHNORA_ADMIN_SOCKET=$ICHNORA_SOCKET ichnora projects || test $? -eq 4")
    assert json.loads(restricted)["error"]["code"] == "forbidden", restricted
    assert machine.succeed("systemctl show ichnora -p ProtectSystem --value").strip() == "strict"
    assert machine.succeed("systemctl show ichnora -p PrivateNetwork --value").strip() == "yes"
  '';
}
