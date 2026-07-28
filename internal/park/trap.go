package park

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// addressKey is the network.env key carrying a VM's public /128. provision
// writes that sidecar and the network hooks read it back, which is how a host
// rebuilds a VM's networking after a reboot without a database; the boot sweep
// reads it for exactly the same reason.
const addressKey = "VIRTUAL_MACHINE_IPV6"

// clock is the seam over the poll's pacing. A trap waits a second between ticks,
// and a test that waited would not be a test anyone runs.
type clock interface {
	// Wait blocks for duration and reports whether the trap should keep going.
	// False means the context ended, which is how a daemon is asked to stop
	// between two ticks rather than in the middle of one.
	Wait(ctx context.Context, duration time.Duration) bool
}

type systemClock struct{}

func (systemClock) Wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Trap is the resident wake reflex: it watches the counters the parked rules
// feed and, on the first SYN for a VM this host is holding asleep, wakes it.
//
// Nothing it decides needs a database. That a packet arrived for a VM whose
// marker is on this host's disk is answerable from the host alone, and the
// answer cannot wait for a control plane that may be unreachable — the client is
// retransmitting a SYN roughly once a second and the guest has about that long
// to be listening.
type Trap struct {
	// poller reads the counters and the markers. Its runner has no trace writer:
	// see NewTrap.
	poller *parker
	// sweeper rebuilds park state at startup through the daemon's own runner. A
	// re-park is rare and is a real mutation of the host, so it belongs in the
	// record the way the poll does not.
	sweeper *parker
	wake    func(ctx context.Context, uuid string) error
	// serialize runs a re-park as the VM's single actor, so the boot sweep cannot
	// drive a machine a verb or a reconcile pass is already driving. Nil means
	// this Trap is the only thing touching the host; see reparkInTurn.
	serialize func(ctx context.Context, uuid string, fn func(context.Context) error) error
	clock     clock
}

// NewTrap returns the trap.
//
// runner is the daemon's runner and is used only for the startup sweep. The poll
// deliberately does not use it: reading the counters once a second through a
// runner that traces would write tens of thousands of lines a day into the
// journal and bury the rare line that matters, so the poll runs through a runner
// that has no trace writer at all. That is the Python read's trace=False, made
// structural instead of remembered.
//
// wake is the local wake — remove the marker, start the unit — and is the
// caller's verb rather than something this package does. So the wake gets an
// operation record of its own while the poll gets none, and the one rule this
// package cannot see stays with the caller: an operator who stopped a VM outranks
// a stranger's SYN.
func NewTrap(runner *run.Runner, wake func(ctx context.Context, uuid string) error) *Trap {
	return &Trap{
		poller:  newParker(run.NewRunner(nil)),
		sweeper: newParker(runner),
		wake:    wake,
		clock:   systemClock{},
	}
}

// SerializeWith gives the trap the reconciler's per-VM turn, so the boot sweep
// takes each VM's turn before it re-parks it. Without it the sweep is a second
// driver of the host — the one path in this daemon that mutates a VM outside an
// actor — and it acts on a list that may already be stale.
func (trap *Trap) SerializeWith(
	serialize func(ctx context.Context, uuid string, fn func(context.Context) error) error,
) {
	trap.serialize = serialize
}

// stillSleeping re-reads the marker, through sudo for the same 0700 reason the
// listing does.
func (trap *Trap) stillSleeping(ctx context.Context, uuid string) bool {
	return trap.sweeper.commands.OK(ctx, "sudo test -f {}", trap.sweeper.filesFor(uuid).sleepingMarker)
}

// resident is set while a Trap is polling in this process.
//
// Package state, which is normally the wrong shape and is the right one here:
// the question it answers is about the PROCESS rather than about any particular
// value. A host has one wake reflex, and the code that needs to know whether it
// is running — internal/vm's sleep gate — must not be wired to a Trap, because a
// Manager that held one could be built without it and the gate would then assert
// its own field instead of the host's fact.
var resident atomic.Bool

// Resident reports whether this process is running the host's wake reflex right
// now.
//
// It is what a sleep is gated on: a VM parked with nothing watching its counter
// answers nothing and stays dark until an operator clicks Start. False in the
// window between a daemon building its Trap and the goroutine reaching Run is
// the honest answer and the safe one — a sleep refused there leaves a VM awake,
// which is the failure everyone can see, and a sleep allowed there leaves one
// asleep with nobody listening, which is the failure nobody can.
//
// When the trap becomes a unit of its own (`boat wake-trap`, THE RULE), the
// reflex moves to another process and this stops being the right question. It
// will read false in the daemon on that day, which fails sleeps loudly rather
// than silently — the direction that gets it noticed.
func Resident() bool { return resident.Load() }

// Run sweeps the host's park state, then polls until ctx ends.
//
// A cancelled context is how the daemon is asked to stop, so it ends the loop
// rather than becoming an error: there is nothing an operator would retry.
func (trap *Trap) Run(ctx context.Context, interval time.Duration) error {
	resident.Store(true)
	defer resident.Store(false)
	trap.sweep(ctx)
	for {
		trap.tick(ctx)
		if !trap.clock.Wait(ctx, interval) {
			return nil
		}
	}
}

// tick is one poll: read every wake counter, and wake the VMs a SYN arrived for.
//
// Nothing here returns an error. A tick that failed must not take the daemon
// down with it — the next one is a second away, the counter is still non-zero,
// and the client is still retransmitting.
func (trap *Trap) tick(ctx context.Context) {
	counters, err := trap.poller.counters(ctx)
	if err != nil {
		slog.Error("could not read the wake counters", "error", err)
		return
	}
	// Sorted, so that a host with several VMs waking in one tick does them in a
	// defined order rather than a map's.
	for _, uuid := range slices.Sorted(maps.Keys(counters)) {
		if counters[uuid] > 0 {
			trap.wakeIfSleeping(ctx, uuid)
		}
	}
}

// wakeIfSleeping does the wake, unless the VM is not asleep any more.
//
// The marker is the authority, not the counter. A non-zero count for a VM with
// no marker is stale: the bring-up removes the counter while it rebuilds the
// real path, so a count outlives the sleep by a moment, and by then an
// operator's start or an earlier tick has already woken the VM.
func (trap *Trap) wakeIfSleeping(ctx context.Context, uuid string) {
	if !trap.poller.commands.OK(ctx, "sudo test -f {}", trap.poller.filesFor(uuid).sleepingMarker) {
		return
	}
	slog.Info("waking a sleeping virtual machine", "uuid", uuid, "cause", "inbound TCP SYN")
	if err := trap.wake(ctx, uuid); err != nil {
		// One VM's failed wake must not skip the others and must not stop the
		// poll. The counter stays non-zero and the marker stays on disk, so the
		// next tick tries again — which is the whole retry mechanism.
		slog.Error("could not wake a virtual machine", "uuid", uuid, "error", err)
	}
}

// sweep rebuilds park state from the on-disk markers.
//
// A sleeping VM's unit is suppressed by its marker, so after a host reboot the
// unit never runs, nothing rebuilds the dummy, the NDP entry, the route or the
// rule, and a VM nobody re-parks is unreachable for as long as it sleeps: there
// is no other path back, because the only thing that would have woken it is the
// trap that was never re-armed. The markers plus each VM's network.env are
// enough to rebuild all of it with no database, the same self-healing the pool
// service does for its thin pool.
func (trap *Trap) sweep(ctx context.Context) {
	if err := trap.sweeper.ensureDevice(ctx); err != nil {
		// Reported and carried on: every re-park below needs the device and each
		// one says so on its own, so this is a first warning rather than a wall.
		slog.Error("could not bring up the park device", "device", Device, "error", err)
	}
	for _, uuid := range trap.sleeping(ctx) {
		// Each VM's re-park takes that VM's turn, and the marker is read AGAIN
		// inside it. Both halves are load-bearing, and the reason is that this
		// sweep walks a list it materialized at the top.
		//
		// A wake — an operator's verb, or the reconciler's stepWake — removes the
		// marker and then starts the unit, whose ExecStartPre unparks and builds
		// the VM's real network path. A sweep still walking its stale list would
		// then re-install the /128 route into the black-hole dummy and the rule
		// that counts and DROPS every inbound SYN, on a VM that is now RUNNING.
		// Nothing would ever undo it: the poll only acts on VMs whose marker is
		// present, and this one's is gone. Atlas reads Running, the tenant gets a
		// black hole, and the only exits are another sleep/wake cycle or a reboot.
		//
		// The turn is what makes the re-read meaningful rather than a smaller
		// window: a wake cannot begin while this VM's turn is held.
		if err := trap.reparkInTurn(ctx, uuid); err != nil {
			// Best effort per VM: one VM whose rule will not install must not
			// leave the rest of this host's sleeping VMs unreachable.
			slog.Error("could not re-park a sleeping virtual machine", "uuid", uuid, "error", err)
		}
	}
}

// reparkInTurn re-parks one VM as that VM's single actor.
//
// A Trap built with no serializer parks directly. That is legitimate — a test,
// or a tool that owns the host outright — and it is spelled as a branch here
// rather than as a nil check at the call site so there is exactly one place
// where "no serializer" has to be read as a decision rather than an oversight.
func (trap *Trap) reparkInTurn(ctx context.Context, uuid string) error {
	repark := func(ctx context.Context) error {
		if !trap.stillSleeping(ctx, uuid) {
			// Woken between the listing and this turn. Its unit is running and has
			// rebuilt the real path; re-parking now would take it back down.
			return nil
		}
		return trap.sweeper.park(ctx, uuid, trap.sweeper.address(ctx, uuid))
	}
	if trap.serialize == nil {
		return repark(ctx)
	}
	return trap.serialize(ctx, uuid, repark)
}

// sleeping is every VM directory still carrying a sleeping marker: the DB-free
// set of VMs that are supposed to be parked right now.
func (trap *Trap) sleeping(ctx context.Context) []string {
	// Through sudo, for the same reason the marker check below is: the VM tree is
	// 0700 and root-owned. Listing it unprivileged fails with EACCES on every
	// bootstrapped host, and reading that failure as "this host has no VMs" made
	// the sweep a no-op exactly where it is load-bearing — after a reboot, when
	// nftables is empty and every sleeping VM's unit is suppressed by its own
	// marker, so nothing else rebuilds their counters. Every sleeping VM on the
	// host would then be unreachable for good, which is the case this sweep exists
	// to cover.
	listing, err := trap.sweeper.commands.RunUnchecked(ctx, "sudo ls -1 {}", paths.VirtualMachinesDirectory)
	if err != nil {
		// A host that cannot list its VM directory has nothing this sweep can
		// rebuild. Said once, and the poll still starts: the counters of any VM
		// that IS still parked are read regardless.
		slog.Error("could not list the virtual machines", "error", err)
		return nil
	}
	var sleeping []string
	for line := range strings.Lines(listing) {
		uuid := strings.TrimSpace(line)
		if uuid == "" {
			continue
		}
		// Through sudo, and not an in-process stat: the VM tree is 0700 owned by
		// the per-VM uid, so a stat would report "absent" for a marker that is
		// plainly there and the sweep would skip every sleeping VM on the host.
		if trap.sweeper.commands.OK(ctx, "sudo test -f {}", trap.sweeper.filesFor(uuid).sleepingMarker) {
			sleeping = append(sleeping, uuid)
		}
	}
	return sleeping
}

// address reads a VM's /128 out of its network.env.
//
// Unchecked: a VM whose sidecar is gone — a terminate that got as far as the
// files but not the marker — yields "", and parking an empty address is a no-op
// rather than a trap installed for an address this host no longer holds.
func (parker *parker) address(ctx context.Context, uuid string) string {
	text, err := parker.commands.RunUnchecked(ctx, "sudo cat {}", parker.filesFor(uuid).networkEnvironment)
	if err != nil {
		return ""
	}
	return sidecar.Value(text, addressKey)
}
