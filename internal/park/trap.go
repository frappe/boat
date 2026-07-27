package park

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
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
	clock   clock
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

// Run sweeps the host's park state, then polls until ctx ends.
//
// A cancelled context is how the daemon is asked to stop, so it ends the loop
// rather than becoming an error: there is nothing an operator would retry.
func (trap *Trap) Run(ctx context.Context, interval time.Duration) error {
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
		if err := trap.sweeper.park(ctx, uuid, trap.sweeper.address(ctx, uuid)); err != nil {
			// Best effort per VM: one VM whose rule will not install must not
			// leave the rest of this host's sleeping VMs unreachable.
			slog.Error("could not re-park a sleeping virtual machine", "uuid", uuid, "error", err)
		}
	}
}

// sleeping is every VM directory still carrying a sleeping marker: the DB-free
// set of VMs that are supposed to be parked right now.
func (trap *Trap) sleeping(ctx context.Context) []string {
	listing, err := trap.sweeper.commands.RunUnchecked(ctx, "ls -1 {}", paths.VirtualMachinesDirectory)
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
	return networkEnvironmentValue(text, addressKey)
}

// networkEnvironmentValue reads one KEY=value out of a network.env. The file was
// written to be sourced by a shell, so blank lines and comments are skipped and
// a quoted value is unquoted — provision writes bare values, but reading
// liberally costs nothing.
func networkEnvironmentValue(text string, key string) string {
	for line := range strings.Lines(text) {
		name, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || strings.HasPrefix(strings.TrimSpace(name), "#") {
			continue
		}
		if strings.TrimSpace(name) == key {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}
