package vmnetwork

import (
	"context"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/run"
)

// The network.env captured from a real public VM, and the exact command sequence
// the Python vm-network-up.py rendered for it on a live host (host-1, 2026-07-30).
// A rendering difference here is "a VM off the network", so these are held to the
// Python's trace command for command. The unpark at the top and the reserved-ip
// apply are asserted in internal/park and internal/netapply/reservedip; here they
// are recorded as markers so this stays a test of the bring-up's own sequence.
const environmentPath = "/var/lib/atlas/virtual-machines/dead0000-0000-4000-8000-000000000001/network.env"
const firewallPath = "/var/lib/atlas/virtual-machines/dead0000-0000-4000-8000-000000000001/firewall.env"

const testEnvironment = "TAP_DEVICE=atlas-deadtap0\n" +
	"VIRTUAL_MACHINE_IPV6=2001:db8::2\n" +
	"ATLAS_NETNS=atlas-deadbeefns\n" +
	"HOST_VETH=atlas-hdeadbe\n" +
	"NAMESPACE_VETH=atlas-ndeadbe\n" +
	"IPV4_HOST_CIDR=100.64.200.9/30\n" +
	"IPV4_GUEST_CIDR=100.64.200.10/30\n" +
	"ATLAS_FC_UID=255999\n"

type fakeCommands struct {
	outputs map[string]string
	present map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, present: map[string]bool{}}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}
func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, command)
	return fake.outputs[command], nil
}

func (fake *fakeCommands) RunUnchecked(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "- "+command)
	return fake.outputs[command], nil
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "? "+command)
	return fake.present[command]
}

// render is the production renderer, so a recorded command is exactly the string
// run.Substitute produces — the brace chain clause shell-quoted, every safe value
// (a device name, an address, a CIDR) left bare — and can be compared to the
// Python's trace byte for byte.
func render(template string, parameters ...any) string {
	rendered, err := run.Substitute(template, parameters...)
	if err != nil {
		panic(err)
	}
	return rendered
}

func assertTrace(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("command count %d, want %d:\ngot:\n  %s\nwant:\n  %s",
			len(got), len(want), strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, got[index], want[index])
		}
	}
}

// A fresh host, ordinary public VM: the whole scaffold and namespace are built.
// The default route dev is eth0, so the masquerade and proxy-NDP name it. Every
// mutating line here was produced verbatim by the Python on a real host.
func TestUpRendersThePublicPlaneLikeThePython(t *testing.T) {
	fake := newFakeCommands().output("sudo cat "+environmentPath, testEnvironment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`).
		output("ip -j route show default", `[{"dev":"eth0"}]`)

	var unparked, attached bool
	bringUp := &bringUp{
		commands:            fake,
		unpark:              func(context.Context) error { unparked = true; return nil },
		attachReservedIP:    func(context.Context, string, string, string) error { attached = true; return nil },
		networkEnvironment:  environmentPath,
		firewallEnvironment: firewallPath,
	}
	if err := bringUp.run(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !unparked {
		t.Error("the bring-up did not unpark before rebuilding the namespace")
	}
	if attached {
		t.Error("an ordinary VM with no reserved IP attached one")
	}

	assertTrace(t, fake.trace, []string{
		"sudo cat " + environmentPath,
		"ip -j -6 route show default",
		"ip -j route show default",
		"? sudo nft list table inet atlas",
		"sudo nft add table inet atlas",
		"? sudo nft list chain inet atlas forward",
		"sudo nft add chain inet atlas forward '{ type filter hook forward priority filter; policy accept; }'",
		"sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip daddr 169.254.169.254 drop",
		"? sudo nft list chain inet atlas postrouting",
		"sudo nft add chain inet atlas postrouting '{ type nat hook postrouting priority srcnat; policy accept; }'",
		"sudo nft list chain inet atlas postrouting",
		"sudo nft add rule inet atlas postrouting ip saddr 100.64.0.0/16 oifname eth0 masquerade",
		"- sudo sysctl -q -w net.ipv6.conf.all.forwarding=1 net.ipv6.conf.all.proxy_ndp=1 net.ipv4.ip_forward=1",
		"- sudo ip netns del atlas-deadbeefns",
		"- sudo ip link del atlas-hdeadbe",
		"sudo ip netns add atlas-deadbeefns",
		"sudo ip link add atlas-hdeadbe type veth peer name atlas-ndeadbe",
		"sudo ip link set atlas-ndeadbe netns atlas-deadbeefns",
		"- sudo ip netns exec atlas-deadbeefns sysctl -q -w net.ipv6.conf.all.forwarding=1 net.ipv4.ip_forward=1",
		"sudo ip netns exec atlas-deadbeefns ip tuntap add atlas-deadtap0 mode tap vnet_hdr",
		"sudo ip netns exec atlas-deadbeefns ip link set atlas-deadtap0 up",
		"sudo ip netns exec atlas-deadbeefns ip -6 addr add fe80::1/64 dev atlas-deadtap0 nodad",
		"sudo ip netns exec atlas-deadbeefns ip -6 route replace 2001:db8::2/128 dev atlas-deadtap0",
		"sudo ip netns exec atlas-deadbeefns ip -4 addr replace 100.64.200.9/30 dev atlas-deadtap0",
		"sudo ip link set atlas-hdeadbe up",
		"sudo ip -6 addr add fe80::2/64 dev atlas-hdeadbe nodad",
		"sudo ip -4 addr replace 169.254.0.1/30 dev atlas-hdeadbe",
		"sudo ip netns exec atlas-deadbeefns ip link set atlas-ndeadbe up",
		"sudo ip netns exec atlas-deadbeefns ip -6 addr add fe80::3/64 dev atlas-ndeadbe nodad",
		"sudo ip netns exec atlas-deadbeefns ip -4 addr replace 169.254.0.2/30 dev atlas-ndeadbe",
		"sudo ip netns exec atlas-deadbeefns ip -6 route replace default via fe80::2 dev atlas-ndeadbe",
		"sudo ip netns exec atlas-deadbeefns ip -4 route replace default via 169.254.0.1 dev atlas-ndeadbe",
		"sudo ip -6 route replace 2001:db8::2/128 via fe80::3 dev atlas-hdeadbe",
		"sudo ip -6 neigh replace proxy 2001:db8::2 dev eth0",
		"sudo ip -4 route replace 100.64.200.10/32 via 169.254.0.2 dev atlas-hdeadbe",
		"sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip6 daddr 2001:db8::2 oifname atlas-hdeadbe counter accept",
		"sudo nft add rule inet atlas forward ip6 saddr 2001:db8::2 iifname atlas-hdeadbe counter accept",
		// Step 10: the firewall probe. No firewall.env, so it is a no-op.
		"? sudo test -f " + firewallPath,
	})
}

// The teardown, likewise captured from the Python on host-1: proxy-NDP and the
// two host routes deleted, the namespace and host veth removed, and the two
// forward rules scraped by handle off the VM's /128.
func TestDownRendersThePublicPlaneLikeThePython(t *testing.T) {
	const chain = "ip6 daddr 2001:db8::2 oifname \"atlas-hdeadbe\" counter accept # handle 7\n" +
		"ip6 saddr 2001:db8::2 iifname \"atlas-hdeadbe\" counter accept # handle 8\n"
	fake := newFakeCommands().
		output("- sudo cat "+environmentPath, testEnvironment).
		output("- ip -j -6 route show default", `[{"dev":"eth0"}]`).
		output("- sudo nft -a list chain inet atlas forward", chain)
	// RunUnchecked records with a "- " prefix but reads outputs by the rendered
	// command; seed both so the reads resolve.
	fake.output("sudo cat "+environmentPath, testEnvironment)
	fake.output("ip -j -6 route show default", `[{"dev":"eth0"}]`)
	fake.output("sudo nft -a list chain inet atlas forward", chain)

	var unparked, detached bool
	bringDown := &bringDown{
		commands:           fake,
		unpark:             func(context.Context) error { unparked = true; return nil },
		detachReservedIP:   func(context.Context, string) error { detached = true; return nil },
		networkEnvironment: environmentPath,
	}
	if err := bringDown.run(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if !unparked {
		t.Error("the teardown did not unpark first")
	}
	if detached {
		t.Error("an ordinary VM with no reserved IP detached one")
	}

	assertTrace(t, fake.trace, []string{
		"- sudo cat " + environmentPath,
		"- ip -j -6 route show default",
		"- sudo ip -6 neigh del proxy 2001:db8::2 dev eth0",
		"- sudo ip -6 route del 2001:db8::2/128 dev atlas-hdeadbe",
		"- sudo ip -4 route del 100.64.200.10/32 dev atlas-hdeadbe",
		"- sudo ip netns del atlas-deadbeefns",
		"- sudo ip link del atlas-hdeadbe",
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 7",
		"- sudo nft delete rule inet atlas forward handle 8",
		// The firewall revert probe. No public_filter chain, so it is a no-op.
		"? sudo nft list chain inet atlas public_filter",
	})
}

// A restart is the case this path exists to survive: on a host whose scaffold and
// per-VM rules already stand, the bring-up re-asserts the namespace but adds no
// nft rule twice — a duplicate forward rule would split the traffic counter the
// idle sweep reads across two entries. The namespace is still torn down and
// rebuilt (its delete is what makes a restart start clean); only the guarded nft
// adds are skipped.
func TestUpIsIdempotentWhenTheScaffoldAndRulesExist(t *testing.T) {
	const forwardChain = "ip daddr 169.254.169.254 drop\n" +
		"ip6 daddr 2001:db8::2 oifname \"atlas-hdeadbe\" counter accept\n" +
		"ip6 saddr 2001:db8::2 iifname \"atlas-hdeadbe\" counter accept\n"
	fake := newFakeCommands().output("sudo cat "+environmentPath, testEnvironment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`).
		output("ip -j route show default", `[{"dev":"eth0"}]`).
		exists("sudo nft list table inet atlas").
		exists("sudo nft list chain inet atlas forward").
		exists("sudo nft list chain inet atlas postrouting").
		output("sudo nft list chain inet atlas forward", forwardChain).
		output("sudo nft list chain inet atlas postrouting", "ip saddr 100.64.0.0/16 oifname \"eth0\" masquerade\n")

	bringUp := &bringUp{
		commands:            fake,
		unpark:              func(context.Context) error { return nil },
		attachReservedIP:    func(context.Context, string, string, string) error { return nil },
		networkEnvironment:  environmentPath,
		firewallEnvironment: firewallPath,
	}
	if err := bringUp.run(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, forbidden := range []string{"add table", "add chain", "add rule"} {
		for _, command := range fake.trace {
			if strings.Contains(command, forbidden) {
				t.Errorf("a re-run issued %q; the scaffold and rules were already present", command)
			}
		}
	}
	// The namespace is still rebuilt from scratch — that is what a restart needs.
	if !containsCommand(fake.trace, "sudo ip netns add atlas-deadbeefns") {
		t.Error("a re-run did not rebuild the namespace")
	}
}

// assertSuffix checks that want is the final run of commands in got — how the
// private block, which comes last on the bring-up, is asserted without restating
// the whole public sequence.
func assertSuffix(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("trace has %d commands, want at least %d:\n  %s", len(got), len(want), strings.Join(got, "\n  "))
	}
	tail := got[len(got)-len(want):]
	for index := range want {
		if tail[index] != want[index] {
			t.Errorf("suffix command %d:\ngot:  %s\nwant: %s", index, tail[index], want[index])
		}
	}
}

func containsCommand(trace []string, command string) bool {
	for _, recorded := range trace {
		if recorded == command {
			return true
		}
	}
	return false
}

// A VM enrolled in the private plane: after the public bring-up, its private /128
// is routed into the namespace and to the host veth, the four tenant-isolation
// rules install, and its /128 is recorded in the ownership cache ANCP gossips.
func TestUpWithThePrivatePlaneRoutesIsolatesAndRecordsOwnership(t *testing.T) {
	environment := testEnvironment +
		"PRIVATE_ADDRESS=fdaa:1a2b:3c4d:0:1:2:3:4\nTENANT_PREFIX=fdaa:1a2b:3c4d::/48\n"
	fake := newFakeCommands().output("sudo cat "+environmentPath, environment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`).
		output("ip -j route show default", `[{"dev":"eth0"}]`)

	var owned string
	bringUp := &bringUp{
		commands:            fake,
		unpark:              func(context.Context) error { return nil },
		attachReservedIP:    func(context.Context, string, string, string) error { return nil },
		addLocalOwned:       func(address string) error { owned = address; return nil },
		networkEnvironment:  environmentPath,
		firewallEnvironment: firewallPath,
	}
	if err := bringUp.run(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if owned != "fdaa:1a2b:3c4d:0:1:2:3:4" {
		t.Errorf("the private /128 was not recorded in the ownership cache: %q", owned)
	}
	assertSuffix(t, fake.trace, []string{
		"sudo ip netns exec atlas-deadbeefns ip -6 route replace fdaa:1a2b:3c4d:0:1:2:3:4/128 dev atlas-deadtap0",
		"sudo ip -6 route replace fdaa:1a2b:3c4d:0:1:2:3:4/128 via fe80::3 dev atlas-hdeadbe",
		"- sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip6 daddr fdaa::/16 drop",
		"- sudo nft list chain inet atlas forward",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:1a2b:3c4d::/48 accept",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:0:0::/48 accept",
		"sudo nft insert rule inet atlas forward iifname wg-mesh oifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d::/48 ip6 daddr fdaa:1a2b:3c4d:0:1:2:3:4 accept",
		"? sudo test -f " + firewallPath,
	})
}

// The private teardown runs independently of the public /128 (a dark VM has none):
// the private route is deleted, the isolation rules scraped by handle, and the
// ownership record withdrawn.
func TestDownWithThePrivatePlaneTearsDownIsolationAndWithdrawsOwnership(t *testing.T) {
	environment := testEnvironment +
		"PRIVATE_ADDRESS=fdaa:1a2b:3c4d:0:1:2:3:4\nTENANT_PREFIX=fdaa:1a2b:3c4d::/48\n"
	const privateRules = `iifname "atlas-hdeadbe" ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop # handle 20` + "\n"
	fake := newFakeCommands().
		output("sudo cat "+environmentPath, environment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`).
		output("sudo nft -a list chain inet atlas forward", privateRules)

	var withdrawn string
	bringDown := &bringDown{
		commands:           fake,
		unpark:             func(context.Context) error { return nil },
		detachReservedIP:   func(context.Context, string) error { return nil },
		removeLocalOwned:   func(address string) error { withdrawn = address; return nil },
		networkEnvironment: environmentPath,
	}
	if err := bringDown.run(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if withdrawn != "fdaa:1a2b:3c4d:0:1:2:3:4" {
		t.Errorf("the private /128 was not withdrawn from the ownership cache: %q", withdrawn)
	}
	if !containsCommand(fake.trace, "- sudo ip -6 route del fdaa:1a2b:3c4d:0:1:2:3:4/128 dev atlas-hdeadbe") {
		t.Error("the private host route was not deleted")
	}
	if !containsCommand(fake.trace, "- sudo nft delete rule inet atlas forward handle 20") {
		t.Error("the private isolation rule was not scraped by handle")
	}
}

// A garbled sidecar must not render into a command: the bring-up refuses before
// touching the host beyond the unpark and the read.
func TestUpRefusesAMalformedAddress(t *testing.T) {
	broken := strings.Replace(testEnvironment, "VIRTUAL_MACHINE_IPV6=2001:db8::2", "VIRTUAL_MACHINE_IPV6=2001:db8::2; drop", 1)
	fake := newFakeCommands().output("sudo cat "+environmentPath, broken)
	bringUp := &bringUp{
		commands:            fake,
		unpark:              func(context.Context) error { return nil },
		attachReservedIP:    func(context.Context, string, string, string) error { return nil },
		networkEnvironment:  environmentPath,
		firewallEnvironment: firewallPath,
	}
	if err := bringUp.run(context.Background()); err == nil {
		t.Fatal("Up accepted a VIRTUAL_MACHINE_IPV6 that would inject into an nft rule")
	}
}
