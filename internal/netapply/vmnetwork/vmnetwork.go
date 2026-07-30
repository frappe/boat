// Package vmnetwork brings a VM's host-side networking up and takes it down: the
// per-VM network namespace, the veth pair that bridges it to the host, the tap
// Firecracker opens inside it, the addresses and routes on both, proxy-NDP for
// the VM's /128 on the uplink, and the per-VM nft forward rules. It is the port
// of the `firecracker-vm@` unit's ExecStartPre/ExecStopPost hooks —
// scripts/vm-network-up.py and vm-network-down.py — and the highest-restart-
// sensitivity path in Boat, so it is held to the Python's rendered commands byte
// for byte (spec/33 §3.5).
//
// Each VM gets its OWN namespace so a jail breakout cannot see the host's
// interfaces, the uplink or another VM's tap. The guest keeps exactly its old
// gateways — fe80::1 for v6, the host end of its NAT44 /30 for v4 — moved inside
// the namespace; the only change is one extra link-local hop across the veth,
// fully on the host.
//
// What this package does NOT yet do, and leaves to the Python hook until its own
// work order: the private-plane block (apply_private_network + the WireGuard host
// mesh + local-ownership write, gated on PRIVATE_ADDRESS), persisted VPN tunnels,
// and the per-VM public-ingress firewall. Those are config-gated and absent on an
// ordinary public VM, so this bring-up matches the Python's for one exactly. The
// reserved-IP 1:1-NAT (step 8) and the unpark at the top ARE done, by delegating
// to internal/netapply/reservedip and internal/park.
package vmnetwork

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/netapply/localownership"
	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/park"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// The network.env keys a bring-up reads. Every one is provision's own record of
// what it derived for this UUID; the values are read back rather than recomputed
// here, so a second derivation can never disagree with the one the jail was built
// from — the argument sidecar.go makes for every VM fact.
const (
	tapDeviceKey      = "TAP_DEVICE"
	virtualMachineKey = "VIRTUAL_MACHINE_IPV6"
	namespaceKey      = "ATLAS_NETNS"
	hostVethKey       = "HOST_VETH"
	namespaceVethKey  = "NAMESPACE_VETH"
	ipv4HostCIDRKey   = "IPV4_HOST_CIDR"
	ipv4GuestCIDRKey  = "IPV4_GUEST_CIDR"
	reservedIPv4Key   = "RESERVED_IPV4"
	// The private-plane pair, present once the VM is enrolled in universal private
	// addressing. Absent on a public-only VM, so the whole private block is skipped.
	privateAddressKey = "PRIVATE_ADDRESS"
	tenantPrefixKey   = "TENANT_PREFIX"
)

// commands is the host-touching surface, one implementation in production
// (*run.Runner) and a recorder in tests — the same seam park and reservedip have.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
}

var _ commands = (*run.Runner)(nil)

// Up brings a VM's host-side networking up. Idempotent: the namespace is
// re-created from scratch (its delete takes the old tap and veth with it), the
// nft scaffold is guarded, and every route is `replace`.
func Up(ctx context.Context, runner *run.Runner, uuid string) error {
	bringUp := &bringUp{
		commands: runner,
		unpark:   func(ctx context.Context) error { return park.Unpark(ctx, runner, uuid) },
		attachReservedIP: func(ctx context.Context, guestIPv4, hostVeth, reservedIPv4 string) error {
			_, err := reservedip.Attach(ctx, runner, guestIPv4, hostVeth, reservedIPv4)
			return err
		},
		addLocalOwned:      func(address string) error { return localownership.Add(localownership.DefaultPath, address) },
		networkEnvironment: paths.ForVirtualMachine(uuid).NetworkEnvironment(),
	}
	return bringUp.run(ctx)
}

type bringUp struct {
	commands         commands
	unpark           func(ctx context.Context) error
	attachReservedIP func(ctx context.Context, guestIPv4, hostVeth, reservedIPv4 string) error
	// addLocalOwned records the VM's private /128 in the ANCP ownership cache. A
	// field because it writes a root-owned file directly rather than through the
	// command seam, so a test substitutes it the way it does unpark.
	addLocalOwned      func(address string) error
	networkEnvironment string
}

func (bringUp *bringUp) run(ctx context.Context) error {
	// Unpark BEFORE the namespace is rebuilt, so a client's retransmitted SYN meets
	// the resumed guest rather than the park drop rule. A no-op on an ordinary start.
	if err := bringUp.unpark(ctx); err != nil {
		return err
	}

	facts, err := bringUp.read(ctx)
	if err != nil {
		return err
	}

	// The uplinks: the v6 default-route device to answer NDP on, and the v4
	// default-route device the masquerade sends egress out. Read-only, no sudo.
	uplink, err := bringUp.defaultRouteDevice(ctx, "-6")
	if err != nil {
		return err
	}
	ipv4Uplink, err := bringUp.defaultRouteDevice(ctx, "")
	if err != nil {
		return err
	}

	if err := bringUp.scaffold(ctx, ipv4Uplink); err != nil {
		return err
	}
	if err := bringUp.namespaceAndVeth(ctx, facts); err != nil {
		return err
	}
	if err := bringUp.hostRoutesAndProxyNDP(ctx, facts, uplink); err != nil {
		return err
	}
	if err := bringUp.forwardRules(ctx, facts); err != nil {
		return err
	}
	// Step 7a: the private plane, present only once the VM is enrolled in universal
	// private addressing. Its routes come first, then the isolation rules, then the
	// ownership-cache record ANCP gossips. Skipped entirely on a public-only VM.
	if facts.privateAddress != "" {
		if err := bringUp.privatePlaneUp(ctx, facts); err != nil {
			return err
		}
	}
	// Step 8: re-apply the inbound-v4 1:1-NAT if a Reserved IP is attached. Absent
	// on an ordinary VM. The env already carries RESERVED_IPV4, so this is the apply
	// path (reservedip.Attach does not itself write the env).
	if facts.reservedIPv4 != "" {
		return bringUp.attachReservedIP(ctx, facts.ipv4GuestAddress, facts.hostVeth, facts.reservedIPv4)
	}
	return nil
}

// facts is the slice of network.env a bring-up needs, validated at the boundary
// that renders each value into an ip or nft command.
type facts struct {
	tapDevice        string
	virtualMachine   string // the VM's public /128, bare
	namespace        string
	hostVeth         string
	namespaceVeth    string
	ipv4HostCIDR     string
	ipv4GuestCIDR    string
	ipv4GuestAddress string // ipv4GuestCIDR with its prefix stripped
	reservedIPv4     string
	privateAddress   string // the VM's private-plane /128, bare; "" on a public VM
	tenantPrefix     string // the VM's tenant /48
}

// read parses the sidecar and validates every field that reaches a command. The
// tree is 0700 owned by the per-VM uid, so it is read through sudo. Validation is
// the same first line of defence park applies to the /128: an address or a device
// name is spliced into commands whose sudoers grants end in a wildcard, and nft
// re-lexes `;` and `#` even out of a value run.Quote made one shell token.
func (bringUp *bringUp) read(ctx context.Context) (facts, error) {
	text, err := bringUp.commands.Run(ctx, "sudo cat {}", bringUp.networkEnvironment)
	if err != nil {
		return facts{}, err
	}
	value := func(key string) string { return sidecar.Value(text, key) }

	virtualMachine, ok := canonicalIPv6(value(virtualMachineKey))
	if !ok {
		return facts{}, fmt.Errorf("%s: %s is not a canonical IPv6 /128", bringUp.networkEnvironment, virtualMachineKey)
	}
	names := map[string]string{
		tapDeviceKey:     value(tapDeviceKey),
		namespaceKey:     value(namespaceKey),
		hostVethKey:      value(hostVethKey),
		namespaceVethKey: value(namespaceVethKey),
	}
	for key, name := range names {
		if !validName(name) {
			return facts{}, fmt.Errorf("%s: %s=%q is not a usable interface or namespace name", bringUp.networkEnvironment, key, name)
		}
	}
	ipv4HostCIDR, ok := canonicalIPv4CIDR(value(ipv4HostCIDRKey))
	if !ok {
		return facts{}, fmt.Errorf("%s: %s is not an IPv4 CIDR", bringUp.networkEnvironment, ipv4HostCIDRKey)
	}
	ipv4GuestCIDR, ok := canonicalIPv4CIDR(value(ipv4GuestCIDRKey))
	if !ok {
		return facts{}, fmt.Errorf("%s: %s is not an IPv4 CIDR", bringUp.networkEnvironment, ipv4GuestCIDRKey)
	}
	guestAddress, _, _ := strings.Cut(ipv4GuestCIDR, "/")

	built := facts{
		tapDevice:        names[tapDeviceKey],
		virtualMachine:   virtualMachine,
		namespace:        names[namespaceKey],
		hostVeth:         names[hostVethKey],
		namespaceVeth:    names[namespaceVethKey],
		ipv4HostCIDR:     ipv4HostCIDR,
		ipv4GuestCIDR:    ipv4GuestCIDR,
		ipv4GuestAddress: guestAddress,
		reservedIPv4:     value(reservedIPv4Key),
	}
	// The reserved IP, when present, reaches the reserved-ip apply, which validates
	// it again; refusing a non-address here keeps a garbled sidecar out of the env
	// re-read entirely.
	if built.reservedIPv4 != "" {
		if _, ok := canonicalIPv4(built.reservedIPv4); !ok {
			return facts{}, fmt.Errorf("%s: %s=%q is not an IPv4 address", bringUp.networkEnvironment, reservedIPv4Key, built.reservedIPv4)
		}
	}
	// The private plane needs BOTH its address and its tenant prefix; the Python
	// applies it only when both are set. Present-but-one-missing is a public VM here,
	// as there, but present-and-malformed is refused — these reach isolation rules.
	privateAddress := value(privateAddressKey)
	tenantPrefix := value(tenantPrefixKey)
	if privateAddress != "" && tenantPrefix != "" {
		canonicalPrivate, ok := canonicalIPv6(privateAddress)
		if !ok {
			return facts{}, fmt.Errorf("%s: %s=%q is not a canonical IPv6 address", bringUp.networkEnvironment, privateAddressKey, privateAddress)
		}
		canonicalTenant, ok := canonicalIPv6Prefix(tenantPrefix)
		if !ok {
			return facts{}, fmt.Errorf("%s: %s=%q is not an IPv6 prefix", bringUp.networkEnvironment, tenantPrefixKey, tenantPrefix)
		}
		built.privateAddress = canonicalPrivate
		built.tenantPrefix = canonicalTenant
	}
	return built, nil
}

// privatePlaneUp routes the VM's private /128 into the namespace and to the host
// veth, installs the tenant-isolation rules, and records the /128 in the ANCP
// ownership cache the mesh gossips. The routes mirror the public /128's in steps 4
// and 6; there is deliberately no proxy-NDP for fdaa:: — it is never on-link.
func (bringUp *bringUp) privatePlaneUp(ctx context.Context, facts facts) error {
	if err := bringUp.perform(ctx, []step{
		checked("sudo ip netns exec {} ip -6 route replace {} dev {}", facts.namespace, facts.privateAddress+"/128", facts.tapDevice),
		checked("sudo ip -6 route replace {} via fe80::3 dev {}", facts.privateAddress+"/128", facts.hostVeth),
	}); err != nil {
		return err
	}
	if err := applyPrivateNetwork(ctx, bringUp.commands, facts.hostVeth, facts.privateAddress, facts.tenantPrefix); err != nil {
		return err
	}
	return bringUp.addLocalOwned(facts.privateAddress)
}

// scaffold re-asserts the host-wide nft floor: the table, the forward chain, the
// IMDS drop that stops a guest reaching the droplet's own metadata credentials,
// the srcnat postrouting chain, the NAT44 masquerade, and the forwarding sysctls.
// The first VM to start after a host reboot rebuilds all of it; every guard makes
// a re-run a no-op. Each `add` is gated on a substring of the live chain, matching
// the Python's idempotency exactly.
func (bringUp *bringUp) scaffold(ctx context.Context, ipv4Uplink string) error {
	if !bringUp.commands.OK(ctx, "sudo nft list table inet atlas") {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add table inet atlas"); err != nil {
			return err
		}
	}
	if !bringUp.commands.OK(ctx, "sudo nft list chain inet atlas forward") {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add chain inet atlas forward {}", forwardChainSpecification); err != nil {
			return err
		}
	}
	forward, err := bringUp.commands.Run(ctx, "sudo nft list chain inet atlas forward")
	if err != nil {
		return err
	}
	if !strings.Contains(forward, "ip daddr 169.254.169.254") {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add rule inet atlas forward ip daddr 169.254.169.254 drop"); err != nil {
			return err
		}
	}
	if !bringUp.commands.OK(ctx, "sudo nft list chain inet atlas postrouting") {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add chain inet atlas postrouting {}", postroutingChainSpecification); err != nil {
			return err
		}
	}
	postrouting, err := bringUp.commands.Run(ctx, "sudo nft list chain inet atlas postrouting")
	if err != nil {
		return err
	}
	if !strings.Contains(postrouting, "ip saddr 100.64.0.0/16") {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add rule inet atlas postrouting ip saddr 100.64.0.0/16 oifname {} masquerade", ipv4Uplink); err != nil {
			return err
		}
	}
	// check=False in the Python: a sysctl already set by /etc/sysctl.d is not a
	// failure to re-assert.
	_, err = bringUp.commands.RunUnchecked(ctx, "sudo sysctl -q -w net.ipv6.conf.all.forwarding=1 net.ipv6.conf.all.proxy_ndp=1 net.ipv4.ip_forward=1")
	return err
}

// namespaceAndVeth builds the namespace, the veth pair, the tap inside the
// namespace, and addresses both — steps 1 through 5 of the Python. The namespace
// is deleted first so a restart starts from a known state; the delete takes the
// old tap and namespace-side veth with it, which is why only the host veth needs
// its own delete.
func (bringUp *bringUp) namespaceAndVeth(ctx context.Context, facts facts) error {
	steps := []step{
		unchecked("sudo ip netns del {}", facts.namespace),
		unchecked("sudo ip link del {}", facts.hostVeth),
		checked("sudo ip netns add {}", facts.namespace),
		checked("sudo ip link add {} type veth peer name {}", facts.hostVeth, facts.namespaceVeth),
		checked("sudo ip link set {} netns {}", facts.namespaceVeth, facts.namespace),
		unchecked("sudo ip netns exec {} sysctl -q -w net.ipv6.conf.all.forwarding=1 net.ipv4.ip_forward=1", facts.namespace),
		checked("sudo ip netns exec {} ip tuntap add {} mode tap vnet_hdr", facts.namespace, facts.tapDevice),
		checked("sudo ip netns exec {} ip link set {} up", facts.namespace, facts.tapDevice),
		checked("sudo ip netns exec {} ip -6 addr add fe80::1/64 dev {} nodad", facts.namespace, facts.tapDevice),
		checked("sudo ip netns exec {} ip -6 route replace {} dev {}", facts.namespace, facts.virtualMachine+"/128", facts.tapDevice),
		checked("sudo ip netns exec {} ip -4 addr replace {} dev {}", facts.namespace, facts.ipv4HostCIDR, facts.tapDevice),
		checked("sudo ip link set {} up", facts.hostVeth),
		checked("sudo ip -6 addr add fe80::2/64 dev {} nodad", facts.hostVeth),
		checked("sudo ip -4 addr replace 169.254.0.1/30 dev {}", facts.hostVeth),
		checked("sudo ip netns exec {} ip link set {} up", facts.namespace, facts.namespaceVeth),
		checked("sudo ip netns exec {} ip -6 addr add fe80::3/64 dev {} nodad", facts.namespace, facts.namespaceVeth),
		checked("sudo ip netns exec {} ip -4 addr replace 169.254.0.2/30 dev {}", facts.namespace, facts.namespaceVeth),
		checked("sudo ip netns exec {} ip -6 route replace default via fe80::2 dev {}", facts.namespace, facts.namespaceVeth),
		checked("sudo ip netns exec {} ip -4 route replace default via 169.254.0.1 dev {}", facts.namespace, facts.namespaceVeth),
	}
	return bringUp.perform(ctx, steps)
}

// hostRoutesAndProxyNDP routes the VM's /128 (v6) and /32 (v4) into the namespace
// via the veth, and answers NDP for the VM on the uplink so the upstream router
// delivers its v6 packets here — step 6.
func (bringUp *bringUp) hostRoutesAndProxyNDP(ctx context.Context, facts facts, uplink string) error {
	return bringUp.perform(ctx, []step{
		checked("sudo ip -6 route replace {} via fe80::3 dev {}", facts.virtualMachine+"/128", facts.hostVeth),
		checked("sudo ip -6 neigh replace proxy {} dev {}", facts.virtualMachine, uplink),
		checked("sudo ip -4 route replace {} via 169.254.0.2 dev {}", facts.ipv4GuestAddress+"/32", facts.hostVeth),
	})
}

// forwardRules installs the per-VM v6 forward accepts — one each way — that carry
// a `counter` poll-vm-traffic reads for idle detection. Guarded on the live chain
// so a restart does not split a counter across two rules.
//
// The guard matches against a quote-stripped listing, and that is a fix, not a
// port. nft echoes an interface name back QUOTED — `oifname "atlas-hdeadbe"` — so
// the Python's unquoted `oifname {host_veth}` substring never matches its own rule
// on a re-list, and vm-network-up.py therefore adds BOTH accepts again on every
// restart, splitting the very counter its comment warns about. Stripping the
// quotes before the match makes the check see the rule that is actually there, so
// a restart is the no-op it was meant to be. The rendered `add` is byte-identical
// to the Python's; only the idempotency guard diverges, and only to stop a
// duplicate. See llm/wo-3b-notes.md.
func (bringUp *bringUp) forwardRules(ctx context.Context, facts facts) error {
	forward, err := bringUp.commands.Run(ctx, "sudo nft list chain inet atlas forward")
	if err != nil {
		return err
	}
	present := strings.ReplaceAll(forward, "\"", "")
	if !strings.Contains(present, "ip6 daddr "+facts.virtualMachine+" oifname "+facts.hostVeth) {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add rule inet atlas forward ip6 daddr {} oifname {} counter accept", facts.virtualMachine, facts.hostVeth); err != nil {
			return err
		}
	}
	if !strings.Contains(present, "ip6 saddr "+facts.virtualMachine+" iifname "+facts.hostVeth) {
		if _, err := bringUp.commands.Run(ctx, "sudo nft add rule inet atlas forward ip6 saddr {} iifname {} counter accept", facts.virtualMachine, facts.hostVeth); err != nil {
			return err
		}
	}
	return nil
}

// defaultRouteDevice reads the interface carrying the default route for a family.
// family is "-6" for the v6 uplink and "" for v4 (which may differ on a
// multi-homed host); the empty family renders no flag, matching the Python's
// `ip -j route show default`. Read-only, so no sudo. It is Run, not OK: the
// device name IS the answer, so a probe that could not be made must not read as
// an empty string spliced into the masquerade and proxy-NDP commands.
func (bringUp *bringUp) defaultRouteDevice(ctx context.Context, family string) (string, error) {
	template := "ip -j route show default"
	if family != "" {
		template = "ip -j " + family + " route show default"
	}
	output, err := bringUp.commands.Run(ctx, template)
	if err != nil {
		return "", err
	}
	device, err := firstRouteDevice(output)
	if err != nil {
		return "", fmt.Errorf("no %s default route to bring the VM's networking up against: %w", familyName(family), err)
	}
	return device, nil
}
