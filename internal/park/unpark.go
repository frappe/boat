package park

import (
	"context"

	"github.com/frappe/boat/internal/run"
)

// Retire takes a VM's parked state off this host for good, reading its address
// out of its own network.env the way ParkVirtualMachine does.
//
// It is the teardown a terminate needs, and it exists because there is no other
// moment left. A sleeping VM's unit is already inactive, and `systemctl disable
// --now` on an inactive unit does NOT re-run ExecStopPost — verified on a host,
// not reasoned about — so the hook that would have unwound this VM's networking
// never runs again. Everything park installed then outlives the VM: the named
// counter, the rule that counts and DROPS every inbound SYN to the /128, the
// off-link route into the dummy, and the proxy-NDP entry. Atlas re-allocates
// that /128 to the next VM, and the next VM inherits a permanent drop on every
// connection anyone opens to it.
//
// It is Unpark plus the proxy-NDP entry, and the difference is the whole reason
// this is a second function rather than a flag. An unpark runs at BRING-UP,
// where the entry is about to be re-asserted for a VM that is coming back, so
// removing it there would strip the address off a VM on its way up. A terminate
// has no such moment afterwards: the host would go on answering neighbour
// solicitations for a /128 it no longer holds, the upstream router would go on
// delivering that VM's packets here, and on the day the address is re-allocated
// to a VM on another host this one answers for it too — which ANCP correctly
// reports as a conflict and blackholes (spec/31 §18).
//
// What it does NOT do is the rest of a teardown. The namespace, the veth, the
// tap and the per-VM forward rules are the Python `vm-network-down` hook's, run
// by the unit's own ExecStopPost, and a VM whose stop skipped that hook still
// carries them when this returns. Boat gets its own vm-network-down in WO-3;
// until then this is the honest limit of what Boat can unwind, and it is exactly
// the set Boat itself installed.
func Retire(ctx context.Context, runner *run.Runner, uuid string) error {
	parker := newParker(runner)
	return parker.retire(ctx, uuid, parker.address(ctx, uuid))
}

func (parker *parker) retire(ctx context.Context, uuid string, address string) error {
	address = removableAddress(address)
	if err := parker.unpark(ctx, uuid, address); err != nil {
		return err
	}
	if address == "" {
		return nil
	}
	// No uplink is no entry to delete: the park that would have installed one
	// skipped it for the same reason, and a host mid-reconfigure with no default
	// route has nothing here to undo.
	uplink := parker.uplink(ctx)
	if uplink == "" {
		return nil
	}
	// Best-effort like every other removal: an ordinary terminate of a VM that
	// never slept has no proxy entry, and a non-zero exit there is the expected
	// answer rather than a fault.
	_, err := parker.commands.RunUnchecked(ctx, "sudo ip -6 neigh del proxy {} dev {}", address, uplink)
	return err
}

// unpark removes a VM's parked state: the trap rule, its named counter and the
// off-link route.
//
// It belongs at the top of a network bring-up, BEFORE the real namespace is
// rebuilt, so that the client's retransmitted SYN meets a live guest rather than
// the drop rule — that caller is the Go vm-network-up of WO-3 and does not exist
// yet, which is why the only caller today is the teardown above. Best-effort and
// idempotent: an ordinary start unparks a VM that was never parked, and that is
// a no-op.
//
// proxy-NDP is deliberately left alone here; see Retire for the one case that
// has to take it too.
func (parker *parker) unpark(ctx context.Context, uuid string, address string) error {
	address = removableAddress(address)
	removal := &bestEffort{ctx: ctx, commands: parker.commands}
	name := CounterName(uuid)
	// The rules go BEFORE the counter: nft refuses to delete a counter a rule
	// still references, so this order is load-bearing rather than cosmetic. The
	// counter name is unique per VM, which is what makes scraping the chain for
	// it an exact discriminator rather than a guess.
	listing := removal.run("sudo nft -a list chain inet atlas {}", forwardChain)
	for _, handle := range ruleHandles(listing, name) {
		removal.run("sudo nft delete rule inet atlas {} handle {}", forwardChain, handle)
	}
	removal.run("sudo nft delete counter inet atlas {}", name)
	// A VM whose sidecar is already gone still has its rule and counter removed;
	// only the route needs an address to name.
	if address != "" {
		removal.run("sudo ip -6 route del {} dev {}", address+"/128", Device)
	}
	return removal.err
}

// removableAddress is the address a teardown may name in a command, and "" for
// anything this host could not have parked in the first place.
//
// The teardown paths read the address out of network.env, which is a file on
// disk rather than a value Atlas just handed over, and they splice it into
// `ip -6 route del` and `ip -6 neigh del proxy` — where the sudoers grants end in
// a wildcard. park refuses to install for an address that is not a canonical
// IPv6 (see canonicalAddress), so validating here is the same rule applied to the
// undo, and it is what makes those two grants safe to hold.
//
// A malformed address is treated as an ABSENT one rather than as a failure, and
// that is the whole judgement in this function. Nothing could have been installed
// for it — park would have refused — so there is nothing to remove, and the rule
// and counter that a teardown must still take are named after the UUID and not
// the address. Failing instead would turn a garbled sidecar into a terminate that
// cannot complete, which is a VM nobody can delete.
func removableAddress(address string) string {
	canonical, ok := canonicalAddress(address)
	if !ok {
		return ""
	}
	return canonical
}

// bestEffort runs the removals an unpark is made of. A non-zero exit is not an
// error here — every one of these deletes something an ordinary start never
// installed — but a command that could not be run AT ALL is: a missing nft, or a
// context that ended, means the unpark did not happen, and a bring-up that
// rebuilt the real path over a surviving drop rule would leave the VM answering
// nothing with nothing left to trap the next SYN either.
//
// The first such failure stops the rest, because they would fail the same way.
type bestEffort struct {
	ctx      context.Context
	commands commands
	err      error
}

func (removal *bestEffort) run(template string, parameters ...any) string {
	if removal.err != nil {
		return ""
	}
	output, err := removal.commands.RunUnchecked(removal.ctx, template, parameters...)
	removal.err = err
	return output
}
