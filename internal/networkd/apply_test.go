package networkd

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The load-bearing order: the assembled apply script always has `wg syncconf` before
// `wg set … private-key`. Flipping it clears the interface key and kills every tunnel,
// so the script assembler proves the order and refuses to emit a flipped one.
func TestApplyScriptIsSyncconfBeforeSetPrivateKey(t *testing.T) {
	script, err := applyScript(DefaultRunConfigPath, DefaultWireGuardPrivateKeyPath, wireGuardHostPort, WGDevice)
	if err != nil {
		t.Fatalf("applyScript: %v", err)
	}
	syncconfAt := strings.Index(script, "syncconf")
	privateKeyAt := strings.Index(script, "private-key")
	if syncconfAt < 0 || privateKeyAt < 0 || syncconfAt > privateKeyAt {
		t.Fatalf("apply script must run syncconf before set private-key: %q", script)
	}
}

// assertApplyOrder is the construction-time guard: a hand-flipped script is refused
// with errApplyOrder rather than silently accepted.
func TestAssertApplyOrderRejectsFlippedScript(t *testing.T) {
	flipped := "set -e; wg set wg-mesh private-key /etc/atlas-networkd/wg-private-key listen-port 51820; " +
		"wg syncconf wg-mesh <(wg-quick strip /run/atlas-networkd/wg-mesh.conf)"
	if err := assertApplyOrder(flipped); err == nil {
		t.Fatal("a flipped apply script (set private-key before syncconf) must be refused")
	}
	correct := "set -e; wg syncconf wg-mesh <(wg-quick strip x); wg set wg-mesh private-key y listen-port 51820"
	if err := assertApplyOrder(correct); err != nil {
		t.Fatalf("a correctly ordered script must pass: %v", err)
	}
}

// runApply writes the rendered body atomically and runs exactly one `sudo bash -c`
// carrying the two-step apply in the right order.
func TestRunApplyWritesConfigAndRunsOrderedApply(t *testing.T) {
	directory := t.TempDir()
	configPath := directory + "/wg-mesh.conf"
	body := "[Interface]\nListenPort = 51820\n"

	fake := newFakeCommands()
	if err := runApply(context.Background(), fake, body, configPath, "/etc/atlas-networkd/wg-private-key", 51820); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(written) != body {
		t.Fatalf("written config = %q, want %q", written, body)
	}
	if len(fake.trace) != 1 {
		t.Fatalf("expected exactly one apply command, got %v", fake.trace)
	}
	command := fake.trace[0]
	if !strings.HasPrefix(command, "sudo bash -c set -e;") {
		t.Fatalf("apply must run via sudo bash -c: %q", command)
	}
	if strings.Index(command, "syncconf") > strings.Index(command, "private-key") {
		t.Fatalf("apply command runs set private-key before syncconf: %q", command)
	}
}

// The discrete bring-up commands create the device, pin MTU, assign the host's own
// /128 (before bringing the link up), bring it up, and own the private plane — each a
// clean sudo argv so sudoers pins every one without a wildcard.
func TestBringUpCommandsShapeAndOrder(t *testing.T) {
	create, steps := meshBringUpCommands("fdaa:0:0:abcd::1", 1420, WGDevice)
	if create != "sudo ip link add dev wg-mesh type wireguard" {
		t.Fatalf("device-create command = %q", create)
	}
	joined := strings.Join(steps, "\n")
	for _, fragment := range []string{
		"sudo ip link set dev wg-mesh mtu 1420",
		"sudo ip -6 addr replace fdaa:0:0:abcd::1/128 dev wg-mesh",
		"sudo ip link set dev wg-mesh up",
		"sudo ip -6 route replace fdaa::/16 dev wg-mesh",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("bring-up steps missing %q:\n%s", fragment, joined)
		}
	}
	if strings.Index(joined, "addr replace") > strings.Index(joined, "link set dev wg-mesh up") {
		t.Fatalf("the /128 must be assigned before the link is brought up:\n%s", joined)
	}
}

// runBringUp swallows a device-exists failure on create but fails loud on a later
// step — so a real bring-up fault is not masked by the idempotent create.
func TestRunBringUpFailsLoudOnLaterStep(t *testing.T) {
	fake := newFakeCommands()
	fake.failing["sudo ip link add dev wg-mesh type wireguard"] = true // device exists — ignored
	fake.failing["sudo ip link set dev wg-mesh up"] = true             // a real fault — must surface
	if err := runBringUp(context.Background(), fake, "fdaa:0:0:abcd::1", 1420); err == nil {
		t.Fatal("a failing bring-up step after create must surface an error")
	}
}
