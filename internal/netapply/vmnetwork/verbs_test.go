package vmnetwork

import (
	"context"
	"strings"
	"testing"
)

// The two Task verbs, held to the command sequence firewall-apply.py and
// vm-tunnel.py render — and, just as much, to the sidecar TEXT they leave behind,
// because that file is what the next cold boot replays. A sidecar that drifts is a
// VM whose firewall or tunnel is right until it reboots.

// The UUID the path constants above are built from, spelled out so the golden
// command lines and the argument that produced them can be read together.
const testUUID = "dead0000-0000-4000-8000-000000000001"

const tunnelUUID = "beef0000-0000-4000-8000-000000000002"
const tunnelEnvironmentPath = tunnelsPath + "/" + tunnelUUID + ".env"
const tunnelKeyPath = tunnelsPath + "/" + tunnelUUID + ".key"

// The wg fixtures: a 44-character base64 key, a /127 overlay, and the VM address
// testEnvironment carries.
const (
	testClientKey  = "0123456789abcdefABCDEF0123456789abcdefABCD0="
	testPrivateKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0="
	testPublicKey  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb0="
)

// verbFake is the recorder plus the three write seams a verb has and a bring-up
// does not. The prefixes extend the package's convention — "> " for something
// written to disk, "< " for a command fed on stdin — so the trace still shows how
// much each step's failure mattered.
type verbFake struct {
	*fakeCommands
	installed map[string]string
	stdin     []string
}

func newVerbFake() *verbFake {
	return &verbFake{fakeCommands: newFakeCommands(), installed: map[string]string{}}
}

func (fake *verbFake) InstallFile(_ context.Context, content string, destination string, mode string) error {
	fake.trace = append(fake.trace, "> install -m "+mode+" "+destination)
	fake.installed[destination] = content
	return nil
}

func (fake *verbFake) InstallDirectory(_ context.Context, destination string, mode string) error {
	fake.trace = append(fake.trace, "> install -d -m "+mode+" "+destination)
	return nil
}

func (fake *verbFake) Input(
	_ context.Context, stdin string, template string, parameters ...any,
) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "< "+command)
	fake.stdin = append(fake.stdin, stdin)
	return fake.outputs[command], nil
}

func firewallVM() *verbFake {
	fake := newVerbFake()
	fake.output("sudo cat "+environmentPath, testEnvironment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`)
	return fake
}

// apply writes the sidecar FIRST and then installs the block, and the order is the
// Python's: a host that dies between the two comes back with the firewall applied.
func TestFirewallApplyWritesTheSidecarThenTheBlock(t *testing.T) {
	fake := firewallVM()

	err := Firewall(context.Background(), fake, FirewallParams{
		VirtualMachine: testUUID, Action: "apply", Rules: []string{"tcp/443", "udp/53"},
	})
	if err != nil {
		t.Fatalf("Firewall: %v", err)
	}

	assertTrace(t, fake.trace, []string{
		"sudo cat " + environmentPath,
		"> install -m 0644 " + firewallPath,
		"ip -j -6 route show default",
		"? sudo nft list chain inet atlas public_filter",
		"sudo nft add chain inet atlas public_filter '{ type filter hook forward priority filter - 5; policy accept; }'",
		"- sudo nft -a list chain inet atlas public_filter",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 ct state established,related accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 tcp dport 443 accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 udp dport 53 accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 drop",
	})
	// The sidecar is what vm-network-up replays, so its bytes are contract.
	if got := fake.installed[firewallPath]; got != "VIRTUAL_MACHINE_IPV6=2001:db8::2\nRULES=tcp/443 udp/53\n" {
		t.Errorf("firewall.env is %q", got)
	}
}

// An apply with NO rules is a deny-all-public firewall, not an absent one: the
// sidecar is still written (with an empty RULES=) and the block is still installed,
// so the VM is reachable only over a tunnel.
func TestFirewallApplyWithNoRulesDeniesAllPublic(t *testing.T) {
	fake := firewallVM()

	if err := Firewall(context.Background(), fake, FirewallParams{VirtualMachine: testUUID, Action: "apply"}); err != nil {
		t.Fatalf("Firewall: %v", err)
	}

	if got := fake.installed[firewallPath]; got != "VIRTUAL_MACHINE_IPV6=2001:db8::2\nRULES=\n" {
		t.Errorf("firewall.env is %q, want an empty RULES=", got)
	}
	if !containsCommand(fake.trace, "sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 drop") {
		t.Error("the closing drop was not installed, so the VM is still fully public")
	}
}

// A rule the host cannot read stops the verb BEFORE the sidecar is written: a
// half-understood firewall persisted to disk would be replayed at every boot.
func TestFirewallApplyRefusesABadRuleBeforeWritingAnything(t *testing.T) {
	fake := firewallVM()

	err := Firewall(context.Background(), fake, FirewallParams{
		VirtualMachine: testUUID, Action: "apply", Rules: []string{"tcp/443", "sctp/9"},
	})

	if err == nil {
		t.Fatal("a firewall with an unparseable rule was applied")
	}
	if _, written := fake.installed[firewallPath]; written {
		t.Error("the sidecar was written for a firewall that was refused")
	}
}

// clear takes the block off and deletes the sidecar, best-effort — the rm may run
// after a terminate has already taken the VM tree.
func TestFirewallClearRemovesTheBlockAndTheSidecar(t *testing.T) {
	const listing = "iifname \"eth0\" ip6 daddr 2001:db8::2 tcp dport 443 accept # handle 5\n" +
		"iifname \"eth0\" ip6 daddr 2001:db8::2 drop # handle 6\n"
	fake := firewallVM()
	fake.exists("sudo nft list chain inet atlas public_filter").
		output("sudo nft -a list chain inet atlas public_filter", listing)

	if err := Firewall(context.Background(), fake, FirewallParams{VirtualMachine: testUUID, Action: "clear"}); err != nil {
		t.Fatalf("Firewall: %v", err)
	}

	assertTrace(t, fake.trace, []string{
		"sudo cat " + environmentPath,
		"? sudo nft list chain inet atlas public_filter",
		"- sudo nft -a list chain inet atlas public_filter",
		"- sudo nft delete rule inet atlas public_filter handle 5",
		"- sudo nft delete rule inet atlas public_filter handle 6",
		"- sudo rm -f " + firewallPath,
	})
}

func TestFirewallRefusesAnUnknownAction(t *testing.T) {
	fake := firewallVM()
	if err := Firewall(context.Background(), fake, FirewallParams{VirtualMachine: testUUID, Action: "reset"}); err == nil {
		t.Fatal("an unknown action was accepted")
	}
	if len(fake.trace) != 0 {
		t.Errorf("an unknown action still touched the host: %v", fake.trace)
	}
}

// A name that is not a UUID never becomes a path: it would be spliced into every
// sidecar path this verb writes.
func TestFirewallRefusesANameThatIsNotAUUID(t *testing.T) {
	fake := firewallVM()
	if err := Firewall(context.Background(), fake, FirewallParams{VirtualMachine: "../../etc", Action: "clear"}); err == nil {
		t.Fatal("a name that is not a UUID was accepted")
	}
}

func upTunnel() TunnelParams {
	return TunnelParams{
		TunnelName:      tunnelUUID,
		VirtualMachine:  testUUID,
		Interface:       "wg-abc123",
		Action:          "up",
		ListenPort:      51820,
		ClientPublicKey: testClientKey,
		ClientAddress:   "fdab::2",
		HostAddress:     "fdab::1/127",
	}
}

func tunnelVM() *verbFake {
	fake := newVerbFake()
	fake.output("sudo cat "+environmentPath, testEnvironment).
		output("wg genkey", testPrivateKey+"\n").
		output("wg pubkey", testPublicKey+"\n")
	return fake
}

// A first bring-up: the tunnels/ directory, a freshly minted keypair, both
// sidecars, then the live interface and its isolation rules.
func TestTunnelUpMintsTheKeyAndAppliesTheTunnel(t *testing.T) {
	fake := tunnelVM()

	result, err := Tunnel(context.Background(), fake, upTunnel())
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}

	assertTrace(t, fake.trace, []string{
		"sudo cat " + environmentPath,
		"> install -d -m 0700 " + tunnelsPath,
		"? sudo test -f " + tunnelKeyPath,
		"wg genkey",
		"> install -m 0600 " + tunnelKeyPath,
		"< wg pubkey",
		"> install -m 0644 " + tunnelEnvironmentPath,
		"? sudo ip link show wg-abc123",
		"sudo ip link add wg-abc123 type wireguard",
		"sudo wg set wg-abc123 listen-port 51820 private-key " + tunnelKeyPath,
		"sudo wg set wg-abc123 peer " + testClientKey + " allowed-ips fdab::2/128",
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
	if result.ServerPublicKey != testPublicKey {
		t.Errorf("server_public_key is %q, want the public half", result.ServerPublicKey)
	}
	// The PUBLIC key goes back to the controller; the private half only ever
	// reaches `wg pubkey` on stdin and `wg set` by path.
	if len(fake.stdin) != 1 || fake.stdin[0] != testPrivateKey+"\n" {
		t.Errorf("the private key was not fed to wg pubkey on stdin: %v", fake.stdin)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, testPrivateKey) {
			t.Errorf("the private key reached a command line: %s", line)
		}
	}
	if got := fake.installed[tunnelKeyPath]; got != testPrivateKey+"\n" {
		t.Errorf("the key file holds %q", got)
	}
	want := "INTERFACE=wg-abc123\nLISTEN_PORT=51820\nPRIVATE_KEY_FILE=" + tunnelKeyPath + "\n" +
		"CLIENT_PUBLIC_KEY=" + testClientKey + "\nCLIENT_ADDRESS=fdab::2\nHOST_ADDRESS=fdab::1/127\n" +
		"VIRTUAL_MACHINE_IPV6=2001:db8::2\n"
	if got := fake.installed[tunnelEnvironmentPath]; got != want {
		t.Errorf("the tunnel sidecar is\n  %q\nwant\n  %q", got, want)
	}
}

// A re-apply REUSES the key on disk. Minting a second one would rotate the host
// key out from under a client already holding the config it was given.
func TestTunnelUpReusesAnExistingHostKey(t *testing.T) {
	fake := tunnelVM()
	fake.exists("sudo test -f "+tunnelKeyPath).
		output("sudo cat "+tunnelKeyPath, testPrivateKey+"\n")

	if _, err := Tunnel(context.Background(), fake, upTunnel()); err != nil {
		t.Fatalf("Tunnel: %v", err)
	}

	if containsCommand(fake.trace, "wg genkey") {
		t.Error("a re-apply minted a new key, rotating it out from under the client")
	}
	if !containsCommand(fake.trace, "sudo cat "+tunnelKeyPath) {
		t.Error("the existing key was not read back")
	}
}

// A value the cold-boot reader would refuse is refused HERE, before the sidecar
// lands — otherwise the tunnel works until the host reboots and then silently
// does not.
func TestTunnelUpRefusesWhatTheColdBootReplayWouldRefuse(t *testing.T) {
	for name, broken := range map[string]func(*TunnelParams){
		"interface":  func(params *TunnelParams) { params.Interface = "wg abc" },
		"port":       func(params *TunnelParams) { params.ListenPort = 0 },
		"client key": func(params *TunnelParams) { params.ClientPublicKey = "tooshort" },
		"client v6":  func(params *TunnelParams) { params.ClientAddress = "10.0.0.1" },
		"host cidr":  func(params *TunnelParams) { params.HostAddress = "fdab::1" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := tunnelVM()
			params := upTunnel()
			broken(&params)

			if _, err := Tunnel(context.Background(), fake, params); err == nil {
				t.Fatal("accepted a tunnel the cold-boot replay would refuse")
			}
			if _, written := fake.installed[tunnelEnvironmentPath]; written {
				t.Error("the sidecar was written for a tunnel that was refused")
			}
		})
	}
}

// down removes this interface's rules from BOTH chains — forward governs transit,
// input governs the host itself — then the interface and both sidecars.
func TestTunnelDownRemovesBothChainsAndTheSidecars(t *testing.T) {
	fake := newVerbFake()
	fake.output("sudo nft -a list chain inet atlas forward",
		"iifname \"wg-abc123\" ip6 daddr 2001:db8::2 accept # handle 7\n"+
			"iifname \"wg-abc123\" drop # handle 8\n").
		output("sudo nft -a list chain inet atlas input", "iifname \"wg-abc123\" drop # handle 9\n")

	result, err := Tunnel(context.Background(), fake, TunnelParams{
		TunnelName: tunnelUUID, VirtualMachine: testUUID, Interface: "wg-abc123", Action: "down",
	})
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}

	assertTrace(t, fake.trace, []string{
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 7",
		"- sudo nft delete rule inet atlas forward handle 8",
		"- sudo nft -a list chain inet atlas input",
		"- sudo nft delete rule inet atlas input handle 9",
		"- sudo ip link del wg-abc123",
		"- sudo rm -f " + tunnelEnvironmentPath,
		"- sudo rm -f " + tunnelKeyPath,
	})
	if result.ServerPublicKey != "" {
		t.Errorf("down reported a server_public_key: %q", result.ServerPublicKey)
	}
}

// A tunnel name is a path segment of both sidecars, so it is held to the same
// shape the VM's name is.
func TestTunnelRefusesATunnelNameThatIsNotAUUID(t *testing.T) {
	fake := newVerbFake()
	params := upTunnel()
	params.TunnelName = "../../../root/.ssh/authorized_keys"

	if _, err := Tunnel(context.Background(), fake, params); err == nil {
		t.Fatal("a tunnel name that is not a UUID was accepted")
	}
	if len(fake.trace) != 0 {
		t.Errorf("it still touched the host: %v", fake.trace)
	}
}
