package vmnetwork

import (
	"context"
	"strings"

	"github.com/frappe/boat/internal/netapply/localownership"
	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/park"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// Down tears a VM's host-side networking down — the symmetric partner of Up, run
// by the unit's ExecStopPost. Idempotent and best-effort: a missing rule, device
// or namespace is not an error, because a stop may run after a terminate already
// swept the tree, and a teardown that could not finish is a host left answering
// for an address it no longer holds.
//
// The host-wide scaffold is deliberately kept: the masquerade matches the whole
// 100.64.0.0/16 source and the IMDS drop and the chains serve the next VM, so
// none is removed per-VM — the same asymmetry the Python draws.
//
// The private-plane teardown (remove_private_network + the local-ownership
// withdrawal) and the per-VM firewall removal are deferred to the Python hook
// with their bring-up siblings; they are gated on PRIVATE_ADDRESS / a firewall
// sidecar and no-ops on an ordinary public VM.
func Down(ctx context.Context, runner *run.Runner, uuid string) error {
	bringDown := &bringDown{
		commands:           runner,
		unpark:             func(ctx context.Context) error { return park.Unpark(ctx, runner, uuid) },
		detachReservedIP:   func(ctx context.Context, guestIPv4 string) error { return reservedip.Detach(ctx, runner, guestIPv4) },
		removeLocalOwned:   func(address string) error { return localownership.Remove(localownership.DefaultPath, address) },
		networkEnvironment: paths.ForVirtualMachine(uuid).NetworkEnvironment(),
	}
	return bringDown.run(ctx)
}

type bringDown struct {
	commands           commands
	unpark             func(ctx context.Context) error
	detachReservedIP   func(ctx context.Context, guestIPv4 string) error
	removeLocalOwned   func(address string) error
	networkEnvironment string
}

func (bringDown *bringDown) run(ctx context.Context) error {
	// Clear any parked state first — a Sleeping VM's unit will not re-run this hook,
	// so the trap and its counter must come off here — and before the rule-handle
	// sweep, so the wake rule is gone by the time the chain is listed.
	if err := bringDown.unpark(ctx); err != nil {
		return err
	}

	// A missing env (terminate already swept the tree) is ordinary here, unlike at
	// bring-up: read it tolerantly and let each empty value skip its own step.
	text, _ := bringDown.commands.RunUnchecked(ctx, "sudo cat {}", bringDown.networkEnvironment)

	// A malformed value is treated as absent, not as a failure — the same judgement
	// park.removableAddress makes: nothing could have been installed for a value the
	// bring-up would have refused, so there is nothing to remove, and a garbled
	// sidecar must not turn into a VM that cannot be torn down.
	virtualMachine, _ := canonicalIPv6(sidecar.Value(text, virtualMachineKey))
	hostVeth := sidecar.Value(text, hostVethKey)
	if !validName(hostVeth) {
		hostVeth = ""
	}
	namespace := sidecar.Value(text, namespaceKey)
	if !validName(namespace) {
		namespace = ""
	}
	guestAddress := ""
	if guestCIDR, ok := canonicalIPv4CIDR(sidecar.Value(text, ipv4GuestCIDRKey)); ok {
		guestAddress = stripPrefix(guestCIDR)
	}
	reservedIPv4, _ := canonicalIPv4(sidecar.Value(text, reservedIPv4Key))
	privateAddress, _ := canonicalIPv6(sidecar.Value(text, privateAddressKey))

	// Drop the inbound-v4 1:1-NAT first, while the guest /30 is still known — the
	// namespace delete below would otherwise strand the host-table rules and the
	// policy route. Keyed on the guest alone, so no anchor rediscovery.
	if reservedIPv4 != "" && guestAddress != "" {
		if err := bringDown.detachReservedIP(ctx, guestAddress); err != nil {
			return err
		}
	}

	// The v6 uplink for the proxy-NDP delete. Tolerant of a host with no default
	// route: "" simply skips the delete, as the Python's tolerate_missing does.
	uplink := bringDown.uplink(ctx)

	steps := []step{}
	if virtualMachine != "" && uplink != "" {
		steps = append(steps, unchecked("sudo ip -6 neigh del proxy {} dev {}", virtualMachine, uplink))
	}
	if hostVeth != "" {
		if virtualMachine != "" {
			steps = append(steps, unchecked("sudo ip -6 route del {} dev {}", virtualMachine+"/128", hostVeth))
		}
		if privateAddress != "" {
			steps = append(steps, unchecked("sudo ip -6 route del {} dev {}", privateAddress+"/128", hostVeth))
		}
		if guestAddress != "" {
			steps = append(steps, unchecked("sudo ip -4 route del {} dev {}", guestAddress+"/32", hostVeth))
		}
	}
	if namespace != "" {
		steps = append(steps, unchecked("sudo ip netns del {}", namespace))
	}
	if hostVeth != "" {
		steps = append(steps, unchecked("sudo ip link del {}", hostVeth))
	}
	if err := bringDown.perform(ctx, steps); err != nil {
		return err
	}

	// The two public forward rules, deleted by handle. Look them up by the VM's
	// /128 — the one match key common to both — tolerating an absent chain.
	if virtualMachine != "" {
		if err := bringDown.removeForwardRules(ctx, virtualMachine); err != nil {
			return err
		}
	}
	// The private-plane isolation rules, deleted independently of the public /128:
	// a dark VM has no public address at all, so a public-gated sweep would miss its
	// tenant rules and leave a stale accept pointing at a recycled veth — a leak.
	// The ownership-cache withdrawal follows, which ANCP gossips on its next scan.
	if privateAddress != "" && hostVeth != "" {
		if err := removePrivateNetwork(ctx, bringDown.commands, privateAddress, hostVeth); err != nil {
			return err
		}
		return bringDown.removeLocalOwned(privateAddress)
	}
	return nil
}

// removeForwardRules scrapes the forward chain for every rule mentioning the VM's
// /128 and deletes each by handle, best-effort. `nft -a` prints `... # handle N`;
// the handle is the last token.
func (bringDown *bringDown) removeForwardRules(ctx context.Context, virtualMachine string) error {
	listing, err := bringDown.commands.RunUnchecked(ctx, "sudo nft -a list chain inet atlas forward")
	if err != nil {
		return err
	}
	for _, handle := range handlesFor(listing, virtualMachine) {
		if _, err := bringDown.commands.RunUnchecked(ctx, "sudo nft delete rule inet atlas forward handle {}", handle); err != nil {
			return err
		}
	}
	return nil
}

// uplink is the v6 default-route device, or "" when there is none — the
// tolerate_missing form, since a teardown must proceed even with no uplink left.
func (bringDown *bringDown) uplink(ctx context.Context) string {
	output, err := bringDown.commands.RunUnchecked(ctx, "ip -j -6 route show default")
	if err != nil {
		return ""
	}
	device, err := firstRouteDevice(output)
	if err != nil {
		return ""
	}
	return device
}

func (bringDown *bringDown) perform(ctx context.Context, steps []step) error {
	return perform(ctx, bringDown.commands, steps)
}

// handlesFor is the trailing handle of every rule line mentioning needle. Shared
// by the down sweep; keyed on the VM's /128, which is unique per VM.
func handlesFor(listing string, needle string) []string {
	var handles []string
	for line := range strings.Lines(listing) {
		if !strings.Contains(line, needle) {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			handles = append(handles, fields[len(fields)-1])
		}
	}
	return handles
}
