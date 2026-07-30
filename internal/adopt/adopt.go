// Package adopt reconstructs what a host already holds, so a Boat started on a
// live machine learns its VMs by reading the host instead of by asking a
// database.
//
// It is Atlas's scripts/reset-server.py inverted. That script enumerates every
// artifact class a host can carry — VM directories, firecracker-vm@ units,
// network namespaces, atlas links, proxy-NDP entries, atlas logical volumes — in
// order to destroy them. The same enumeration, read instead of executed, is how
// Boat learns what is on a host it has just been started on: an upgrade, a
// reboot, a crash recovery.
//
// # It never mutates
//
// Every command this package runs is a listing, a stat or a boolean gate. There
// is no create, no remove, no start and no stop anywhere in it, and there is no
// path that "fixes" what it finds. That is a property of the package rather than
// a convention: adoption runs against a host whose VMs are already serving
// traffic, and the only thing worse than an inaccurate picture of that host is a
// scan that changes it.
//
// # Quarantine, never guess
//
// A UUID whose artifacts agree with each other is a VM. A UUID whose artifacts
// disagree — a systemd unit with no VM directory, a disk no directory claims, a
// jail tree half-removed, an active unit with no network namespace — is not a VM
// and never becomes one: it goes to Result.Quarantined carrying the evidence
// that made it ambiguous. A crash part-way through a terminate leaves exactly
// that state, and ingesting it as a live VM is how a controller boots a VM whose
// disk it already released.
//
// The line the rules draw is between AMBIGUITY and UNTIDINESS. A stopped VM
// whose network namespace outlived it is untidy: its identity is not in doubt,
// its disk is where it should be, and booting it is safe — so it is reported as
// the VM it is. An active unit with no namespace is ambiguous: none of the
// machinery that would make "running" true is there, so the record would assert
// something the host contradicts. Only the second kind is quarantined, because
// quarantine hides a UUID from the controller and hiding a healthy VM has its
// own cost.
//
// # A partial scan is a lie
//
// Any host read that fails, fails the whole scan. A scan that dropped one
// enumeration and returned the rest would report a host holding fewer VMs than
// it holds, and "this host holds nothing" is indistinguishable from a wiped
// host — the one report a control plane must never be handed by accident.
package adopt

import (
	"context"
	"fmt"
	"time"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vm"
)

// commands is everything this package does to the host, and the only seam it
// has. Every method on it is a read: a scan is exactly this interface's worth of
// power, which is what makes "never mutates" checkable rather than aspirational.
// Outside tests there is one implementation, *run.Runner, and there is never a
// second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	// Probe and not OK, throughout. Every question this package asks the host is
	// one whose answer is REPORTED — as an adopted VM, as a quarantine, or as an
	// inventory — and a bool has no room for "I could not look". See run.Probe.
	Probe(ctx context.Context, template string, parameters ...any) (run.Answer, error)
}

var _ commands = (*run.Runner)(nil)

// observer derives one VM's status from the host. internal/vm already owns that
// derivation — the sleeping marker outranking the unit state, a unit caught
// mid-transition being Unknown rather than a guess in either direction — and a
// second copy here would be a second place for those rules to drift.
type observer interface {
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
}

var _ observer = (*vm.Manager)(nil)

// liveness confirms that a Firecracker is answering for a UUID. internal/fcattach
// owns that call and the argument for why socket existence is not liveness; a
// scan needs the answer for its coherence rules, which run before any VM is
// adopted and therefore before Observe is ever asked.
type liveness func(ctx context.Context, runner *run.Runner, uuid string) (fcattach.Process, bool, error)

// clock is the seam over real time, so a quarantine record's SeenAt is a fact a
// test can assert rather than whatever the wall clock said.
type clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Result is one host scan.
//
// VirtualMachines holds only the UUIDs whose artifacts read as one coherent VM.
// Quarantined holds every artifact set that did not, with the evidence; the two
// are disjoint by construction. LogicalVolumes and Units are the raw inventories
// the reconstruction was made from, reported whole so an operator can see the
// host Boat saw and not only the conclusions it drew.
type Result struct {
	VirtualMachines []model.VirtualMachine
	Quarantined     []model.Quarantine
	LogicalVolumes  []model.LogicalVolume
	Units           []model.UnitLiveness
}

// Scanner reads a host and reconstructs what it holds. It keeps no state
// between scans: the host is the state, and re-reading it is the whole job.
type Scanner struct {
	commandsFor func(runner *run.Runner) commands
	observer    observer
	liveness    liveness
	clock       clock
}

// NewScanner returns a Scanner wired to the real host.
func NewScanner() *Scanner {
	return &Scanner{
		commandsFor: func(runner *run.Runner) commands { return runner },
		observer:    vm.NewManager(),
		liveness:    fcattach.Find,
		clock:       systemClock{},
	}
}

// Scan reads the host and reconstructs what it holds. It never mutates.
func (scanner *Scanner) Scan(ctx context.Context, runner *run.Runner) (Result, error) {
	commands := scanner.commandsFor(runner)
	taken, err := takeInventory(ctx, commands)
	if err != nil {
		return Result{}, err
	}
	return scanner.reconstruct(ctx, commands, runner, taken)
}

// reconstruct turns the host's inventory into VMs and quarantines.
//
// The status of a coherent VM comes from internal/vm's Observe rather than from
// anything decided here, so adoption and steady-state observation can never
// report the same host differently.
func (scanner *Scanner) reconstruct(
	ctx context.Context, commands commands, runner *run.Runner, taken inventory,
) (Result, error) {
	result := Result{Units: taken.units, LogicalVolumes: taken.volumes}
	examined, err := scanner.examineAll(ctx, commands, runner, taken)
	if err != nil {
		return Result{}, err
	}
	claimed := claims{}
	for _, artifacts := range examined {
		claimed.add(artifacts.environment)
		if contradictions := artifacts.contradictions(); len(contradictions) > 0 {
			result.Quarantined = append(result.Quarantined, scanner.quarantine(artifacts, contradictions))
			continue
		}
		observed, err := scanner.observer.Observe(ctx, runner, artifacts.uuid)
		if err != nil {
			return Result{}, fmt.Errorf("adopt %s: %w", artifacts.uuid, err)
		}
		result.VirtualMachines = append(result.VirtualMachines, observed)
	}
	result.Quarantined = append(result.Quarantined, scanner.orphans(taken, claimed)...)
	return result, nil
}
