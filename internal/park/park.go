// Package park keeps a sleeping VM reachable enough to notice it is wanted, and
// wakes it when it is.
//
// Sleep is worth having because the host gets the VM's RAM back, and the price
// of that is total: the stop runs the unit's ExecStopPost, which removes the
// proxy-NDP entry, the /128 route, the namespace, the veth pair, the tap and the
// per-VM forward rules. A sleeping VM's public address is then routed nowhere at
// all, so nothing — not even the SYN that ought to wake it — can arrive.
//
// Park puts back the least that lets one inbound SYN be noticed with no guest
// running:
//
//   - proxy-NDP for the /128 on the uplink, so the upstream router still
//     believes this host holds the address and keeps delivering its packets here;
//   - the /128 routed out a shared, always-up dummy interface (atlas-park0). The
//     device is the mechanism, not a detail. A destination that is off-link and
//     is not a local address is FORWARDED by the kernel, so the packet traverses
//     the forward hook where a rule can see it, instead of being input-delivered
//     to a host that has nothing listening and consumed there. Reserved-IPv4
//     ingress leans on the same off-link trick for the same reason;
//   - one rule in that forward chain, counting and dropping the one packet worth
//     waking for.
//
// The rule is
//
//	ip6 daddr <vm> tcp flags syn / fin,syn,rst,ack counter name wake_<hex> drop
//
// and every clause of it is load-bearing:
//
//   - `tcp flags syn / fin,syn,rst,ack` is nft's mask/value form: it matches a
//     packet with SYN set and FIN, RST and ACK clear — a connection being opened,
//     not a SYN-ACK coming back, not a segment in the middle of one. Matching TCP
//     flags at all implies TCP, so a ping or a UDP datagram never reaches this
//     rule's verdict: it falls through to the chain's accept policy, is forwarded
//     out the dummy and is discarded with nothing woken. "TCP only" is a contract
//     of the match, not an accident of it.
//   - drop, not reject. A dropped SYN leaves the client's TCP stack to retransmit
//     after its RTO of about a second, and that second is the budget the whole
//     design is built around: the trap notices, the VM resumes from its memory
//     snapshot, the real path comes back, and the retransmit lands on a live
//     guest. A reject would hand the client an error to show someone.
//   - a NAMED counter, because `nft list counters` reports only named ones. That
//     is a flat, cheap read a once-a-second poll can afford; re-parsing whole
//     chains at that rate is not. The name is wake_<uuid with the dashes taken
//     out>, because nft identifiers may not contain `-`; deriving it from the
//     UUID rather than storing a map is what lets the daemon restart, or the host
//     reboot, without losing track of which counter belongs to which VM.
//
// Everything here except the four host-touching methods is string construction,
// testable with no host, no nft and no root. That is the division
// scripts/lib/atlas/park.py drew, kept for the same reason.
//
// Ported from scripts/lib/atlas/park.py and scripts/atlas-wake-trap.py; the
// chapter is spec/32-sleepy-vms.md.
package park

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

const (
	// Device is the shared dummy every sleeping VM's /128 routes out of. One for
	// the whole host rather than one per VM: it carries no address and delivers
	// nothing, and exists only so a route can point at something that is not a
	// local address. Bootstrap creates it and a host reset deliberately keeps it,
	// so it is host floor rather than per-VM state.
	Device = "atlas-park0"

	// forwardChain is where a forwarded packet is seen, and is the same chain the
	// live per-VM accept rules live in.
	forwardChain = "forward"

	// forwardChainSpecification is the chain clause the network bring-up uses.
	// The policy is accept, which is what makes the "TCP only" property fall out:
	// a packet the wake rule does not match is forwarded out the dummy and dies
	// there, silently, without waking anything.
	forwardChainSpecification = "{ type filter hook forward priority filter; policy accept; }"
)

// commands is everything this package does to the host, and the only seam it
// has. The command sequence park and unpark emit is the whole of what a machine
// with no nft and no root can check, and it is exactly what a differential test
// against the Python compares. Outside tests there is one implementation,
// *run.Runner, and there is never a second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	Probe(ctx context.Context, template string, parameters ...any) (run.Answer, error)
}

var _ commands = (*run.Runner)(nil)

// virtualMachineFiles is the slice of the path layout this package addresses a
// VM through. Naming it here keeps the dependency on internal/paths to one
// function, so a test can state the layout it expects as literal strings.
type virtualMachineFiles struct {
	sleepingMarker     string
	networkEnvironment string
}

func filesFor(uuid string) virtualMachineFiles {
	virtualMachine := paths.ForVirtualMachine(uuid)
	return virtualMachineFiles{
		sleepingMarker:     virtualMachine.SleepingMarker(),
		networkEnvironment: virtualMachine.NetworkEnvironment(),
	}
}

// parker is the host-touching half of this package: one runner, and the path
// layout it addresses VMs through. The exported functions are thin wrappers
// around it, which is what lets a test drive every mutation through a recorder.
type parker struct {
	commands commands
	filesFor func(uuid string) virtualMachineFiles
}

func newParker(runner *run.Runner) *parker {
	return &parker{commands: runner, filesFor: filesFor}
}

// EnsureDevice creates the shared dummy if it is missing and brings it up.
//
// Bootstrap creates it too. It is re-created here because the trap's boot sweep
// re-parks before any VM unit has run, and on a host whose VMs are all asleep no
// unit ever runs at all — so this is the only thing that would put the device
// back after a reboot. Idempotent.
func EnsureDevice(ctx context.Context, runner *run.Runner) error {
	return newParker(runner).ensureDevice(ctx)
}

// Park installs parked reachability and the SYN trap for a VM that is asleep.
//
// Idempotent and self-healing: it runs when the VM goes to sleep, after its unit
// has stopped, and again at every boot sweep. An empty address is a no-op — a VM
// that was never provisioned, or one whose network.env a terminate already took,
// has no /128 to trap for, and a trap pointing nowhere is worse than no trap.
func Park(ctx context.Context, runner *run.Runner, uuid string, address string) error {
	return newParker(runner).park(ctx, uuid, address)
}

// ParkVirtualMachine parks a VM, reading its address out of its own network.env
// rather than being told it.
//
// This is the form the sleep verb needs, and it exists because the caller that
// puts a VM to sleep should not have to know its address in order to keep it
// reachable. The Python original takes only a UUID for the same reason: the
// sidecar is the host's own record of a VM's addresses, so a caller passing its
// own copy could pass a stale one, and a trap armed for the wrong /128 is a VM
// that never wakes.
//
// It brings the shared dummy up first. A sleep is exactly the moment the device
// is needed, and the VM's own unit has already stopped, so nothing else will.
func ParkVirtualMachine(ctx context.Context, runner *run.Runner, uuid string) error {
	parker := newParker(runner)
	if err := parker.ensureDevice(ctx); err != nil {
		return err
	}
	address, _, err := parker.address(ctx, uuid)
	if err != nil {
		return err
	}
	return parker.park(ctx, uuid, address)
}

func (parker *parker) ensureDevice(ctx context.Context) error {
	// Left as OK on both of the grounds that keep OK honest. `ip link show` on a
	// device that is not there EXPLAINS itself on stderr — `Device "atlas-park0"
	// does not exist.`, exit 1 — which is the shape run.Probe says cannot be told
	// apart from a denial, so this question is not askable in three answers. And
	// the collapse guards a mutation that fails loudly by itself: a wrong "not
	// there" reaches `ip link add`, which answers `RTNETLINK answers: File exists`
	// and fails the park. A wasted command and a red Task, never a quiet one.
	if !parker.commands.OK(ctx, "ip link show {}", Device) {
		if _, err := parker.commands.Run(ctx, "sudo ip link add {} type dummy", Device); err != nil {
			return err
		}
	}
	// Unconditional: a device that exists but is down routes nothing, and a
	// reboot leaves exactly that state behind for a dummy something re-created.
	_, err := parker.commands.Run(ctx, "sudo ip link set {} up", Device)
	return err
}

// ensureForwardChain recreates the inet atlas table and its forward chain when
// they are absent.
//
// nftables does not survive a reboot, and it is the first VM unit to start that
// rebuilds the scaffold. A host whose VMs are ALL asleep starts no unit at all —
// every one of them is suppressed by its marker — so nothing rebuilds it, and
// the boot sweep's counters and rules would have nowhere to go. Those VMs would
// be parked in name only and could never be woken by traffic, which is precisely
// the case the sweep exists to cover.
func (parker *parker) ensureForwardChain(ctx context.Context) error {
	// Both gates stay OK, for the reason run.Probe names nft by: `nft list table
	// inet atlas` on a host without it exits 1 with `Error: No such file or
	// directory` on stderr, which is an ordinary answer wearing a denial's clothes
	// — and for nft not even the exit code differs. Neither is askable in three
	// answers. The collapse is also free here, because both adds are idempotent:
	// `nft add table` and `nft add chain` succeed against a table or chain that is
	// already there, so a wrong "not there" costs one no-op command, and an add
	// this daemon is not allowed to make fails loudly and fails the park.
	if !parker.commands.OK(ctx, "sudo nft list table inet atlas") {
		if _, err := parker.commands.Run(ctx, "sudo nft add table inet atlas"); err != nil {
			return err
		}
	}
	if parker.commands.OK(ctx, "sudo nft list chain inet atlas {}", forwardChain) {
		return nil
	}
	_, err := parker.commands.Run(
		ctx, "sudo nft add chain inet atlas {} {}", forwardChain, forwardChainSpecification,
	)
	return err
}

func (parker *parker) park(ctx context.Context, uuid string, address string) error {
	if address == "" {
		return nil
	}
	// Validate and re-render before anything is installed: the address reaches an
	// nft rule below, and nft re-parses `;` and `#` out of a value run.Quote
	// treats as one safe token. A failure here is loud on purpose — a VM whose
	// sidecar holds something that is not a /128 does not get a trap armed around
	// a string this daemon could not vouch for. See park.canonicalAddress.
	canonical, ok := canonicalAddress(address)
	if !ok {
		return fmt.Errorf(
			"refusing to arm a wake trap for %s: %q is not a canonical IPv6 /128, and rendering it into an nft rule unchecked is how the ruleset is injected",
			uuid, address,
		)
	}
	address = canonical
	if err := parker.ensureDevice(ctx); err != nil {
		return err
	}
	// Before the counter: `nft add counter` fails outright when the table it
	// names does not exist.
	if err := parker.ensureForwardChain(ctx); err != nil {
		return err
	}
	if err := parker.restoreReachability(ctx, address); err != nil {
		return err
	}
	return parker.armTrap(ctx, uuid, address)
}

// restoreReachability re-asserts the two things that bring an inbound packet to
// this host and then make it forwarded rather than consumed by it. Both are
// `replace`, so a re-park on a live host changes nothing and a re-park after a
// reboot builds them from scratch.
func (parker *parker) restoreReachability(ctx context.Context, address string) error {
	// A host with no IPv6 default route has no uplink to answer NDP on. That is
	// skipped rather than fatal, the same tolerance the teardown path has: the
	// route and the rule below are still worth installing, and the next re-park
	// picks up the uplink once the host has one.
	if uplink := parker.uplink(ctx); uplink != "" {
		// `proxy`, and the word is load-bearing. Without it this is a UNICAST
		// neighbour entry that maps the VM's address to the uplink's own MAC —
		// a different kernel table, the same exit code, and no proxy-NDP at all.
		// The host then stops answering neighbour solicitations for the /128, the
		// upstream router stops delivering the VM's packets, no SYN ever reaches
		// the forward chain, and the VM can never be woken by traffic.
		//
		// It also leaves a lie behind: teardown only ever runs `neigh del proxy`,
		// which misses a unicast entry, and reset-server enumerates
		// `neigh show proxy`, which does not list one — so the entry is permanent
		// and invisible to every tool that would clean it up.
		_, err := parker.commands.Run(ctx, "sudo ip -6 neigh replace proxy {} dev {}", address, uplink)
		if err != nil {
			return err
		}
	}
	_, err := parker.commands.Run(ctx, "sudo ip -6 route replace {} dev {}", address+"/128", Device)
	return err
}

// armTrap installs the named counter and the rule that references it.
func (parker *parker) armTrap(ctx context.Context, uuid string, address string) error {
	name := CounterName(uuid)
	// OK for the same two reasons ensureForwardChain's gates are: nft explains its
	// negative so the question has no third answer to keep, and `nft add counter`
	// against a counter that already exists succeeds without resetting it — so a
	// wrong "not there" costs one no-op command, and a denied add fails the park
	// out loud rather than leaving a VM parked with no counter to poll.
	if !parker.commands.OK(ctx, "sudo nft list counter inet atlas {}", name) {
		if _, err := parker.commands.Run(ctx, "sudo nft "+counterCommand(uuid)); err != nil {
			return err
		}
	}
	// Guarded on the live ruleset rather than on anything remembered: a second
	// copy of the rule would split the count across two entries, and the poll
	// reads one of them. The counter name is unique per VM, so its presence in
	// the chain is an exact answer to "is this VM's trap already installed".
	chain, err := parker.commands.RunUnchecked(ctx, "sudo nft list chain inet atlas {}", forwardChain)
	if err != nil {
		return err
	}
	if strings.Contains(chain, name) {
		return nil
	}
	_, err = parker.commands.Run(ctx, "sudo nft "+wakeRuleCommand(uuid, address))
	return err
}

// uplink is the interface carrying the IPv6 default route: the host's link to
// the upstream router, and therefore the one the host must answer NDP on for a
// parked address.
//
// Read-only, so no sudo, and tolerant of every failure — a host mid-reconfigure
// with no default route yields "" rather than failing a park.
func (parker *parker) uplink(ctx context.Context) string {
	output, err := parker.commands.RunUnchecked(ctx, "ip -j -6 route show default")
	if err != nil {
		return ""
	}
	var routes []struct {
		Device string `json:"dev"`
	}
	if json.Unmarshal([]byte(output), &routes) != nil || len(routes) == 0 {
		return ""
	}
	return routes[0].Device
}
