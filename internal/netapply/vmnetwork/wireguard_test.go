package vmnetwork

import (
	"context"
	"testing"
)

const testTunnelEnvironment = "INTERFACE=wg-abc123\n" +
	"LISTEN_PORT=51820\n" +
	"PRIVATE_KEY_FILE=/var/lib/atlas/virtual-machines/x/tunnels/wg-abc123.key\n" +
	"CLIENT_PUBLIC_KEY=0123456789abcdefABCDEF0123456789abcdefABCD0=\n" +
	"CLIENT_ADDRESS=fdab::2\n" +
	"HOST_ADDRESS=fdab::1/127\n" +
	"VIRTUAL_MACHINE_IPV6=2001:db8::2\n"

// The tunnel commands are asserted end-to-end through the fake runner's trace in
// TestApplyTunnelInstallsInterfaceAndIsolation below — the same rendered lines the
// former per-builder test checked, now read where they actually run.

// A fresh host: the tunnel interface, its key/port/peer, the host overlay address,
// and the isolation rules install — drop inserted before accept so the head is
// [accept, drop], and the input host-drop appended.
func TestApplyTunnelInstallsInterfaceAndIsolation(t *testing.T) {
	config, err := parseTunnelConfig(testTunnelEnvironment)
	if err != nil {
		t.Fatalf("parseTunnelConfig: %v", err)
	}
	fake := newFakeCommands() // nothing exists yet
	if err := applyTunnel(context.Background(), fake, config); err != nil {
		t.Fatalf("applyTunnel: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"? sudo ip link show wg-abc123",
		"sudo ip link add wg-abc123 type wireguard",
		"sudo wg set wg-abc123 listen-port 51820 private-key /var/lib/atlas/virtual-machines/x/tunnels/wg-abc123.key",
		"sudo wg set wg-abc123 peer 0123456789abcdefABCDEF0123456789abcdefABCD0= allowed-ips fdab::2/128",
		"- sudo ip -6 addr show dev wg-abc123",
		"sudo ip -6 addr add fdab::1/127 dev wg-abc123",
		"sudo ip link set wg-abc123 up",
		"? sudo nft list chain inet atlas forward",
		"sudo nft add chain inet atlas forward '{ type filter hook forward priority filter; policy accept; }'",
		"sudo nft list chain inet atlas forward",
		"sudo nft insert rule inet atlas forward iifname wg-abc123 drop",
		"sudo nft insert rule inet atlas forward iifname wg-abc123 ip6 daddr 2001:db8::2 accept",
		"? sudo nft list chain inet atlas input",
		"sudo nft add chain inet atlas input '{ type filter hook input priority filter; policy accept; }'",
		"sudo nft list chain inet atlas input",
		"sudo nft add rule inet atlas input iifname wg-abc123 drop",
	})
}

// applyPersistedTunnels reads the tunnels/ dir and applies each .env in sorted
// order; a missing dir is a no-op.
func TestApplyPersistedTunnelsReadsTheDirectory(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -d "+tunnelsPath).
		output("sudo ls -1 "+tunnelsPath, "wg-abc123.env\nnotes.txt\n").
		output("sudo cat "+tunnelsPath+"/wg-abc123.env", testTunnelEnvironment)

	if err := applyPersistedTunnels(context.Background(), fake, tunnelsPath); err != nil {
		t.Fatalf("applyPersistedTunnels: %v", err)
	}
	// It listed the dir, read only the .env (not notes.txt), and applied the tunnel.
	if !containsCommand(fake.trace, "sudo cat "+tunnelsPath+"/wg-abc123.env") {
		t.Error("did not read the tunnel sidecar")
	}
	for _, line := range fake.trace {
		if line == "sudo cat "+tunnelsPath+"/notes.txt" {
			t.Error("read a non-.env file as a tunnel")
		}
	}
	if !containsCommand(fake.trace, "sudo ip link add wg-abc123 type wireguard") {
		t.Error("the tunnel interface was not created")
	}
}

func TestApplyPersistedTunnelsIsANoOpWithoutADirectory(t *testing.T) {
	fake := newFakeCommands()
	if err := applyPersistedTunnels(context.Background(), fake, tunnelsPath); err != nil {
		t.Fatalf("applyPersistedTunnels: %v", err)
	}
	assertTrace(t, fake.trace, []string{"? sudo test -d " + tunnelsPath})
}

func TestParseTunnelConfigRefusesBadValues(t *testing.T) {
	for _, name := range []string{"INTERFACE=wg abc", "LISTEN_PORT=notaport", "VIRTUAL_MACHINE_IPV6=nope",
		"CLIENT_ADDRESS=10.0.0.1", "HOST_ADDRESS=fdab::1", "CLIENT_PUBLIC_KEY=tooshort"} {
		key, _, _ := cut(name, "=")
		broken := replaceLine(testTunnelEnvironment, key, name)
		if _, err := parseTunnelConfig(broken); err == nil {
			t.Errorf("parseTunnelConfig accepted a bad %s", key)
		}
	}
}

func cut(s, sep string) (string, string, bool) {
	for index := 0; index+len(sep) <= len(s); index++ {
		if s[index:index+len(sep)] == sep {
			return s[:index], s[index+len(sep):], true
		}
	}
	return s, "", false
}

// replaceLine swaps the line starting with key= for replacement.
func replaceLine(text, key, replacement string) string {
	var out string
	for _, line := range splitNewlines(text) {
		lineKey, _, _ := cut(line, "=")
		if lineKey == key {
			out += replacement + "\n"
		} else if line != "" {
			out += line + "\n"
		}
	}
	return out
}

func splitNewlines(text string) []string {
	var lines []string
	current := ""
	for _, character := range text {
		if character == '\n' {
			lines = append(lines, current)
			current = ""
		} else {
			current += string(character)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
