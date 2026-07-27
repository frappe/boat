package park

import (
	"context"

	"github.com/frappe/boat/internal/run"
)

// Unpark removes a VM's parked state: the trap rule, its named counter and the
// off-link route.
//
// It runs at the top of the network bring-up, BEFORE the real namespace is
// rebuilt, so that the client's retransmitted SYN meets a live guest rather than
// the drop rule. It runs at teardown too, because an already-stopped unit's
// ExecStopPost will not run a second time and a terminate still has to take the
// park artifacts with it. Best-effort and idempotent: an ordinary start unparks
// a VM that was never parked, and that is a no-op.
//
// proxy-NDP is deliberately left alone. Bring-up re-asserts it and teardown
// deletes it; removing it here would strip the address off a VM on its way up.
func Unpark(ctx context.Context, runner *run.Runner, uuid string, address string) error {
	return newParker(runner).unpark(ctx, uuid, address)
}

func (parker *parker) unpark(ctx context.Context, uuid string, address string) error {
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
