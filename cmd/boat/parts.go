package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/frappe/boat/internal/adopt"
	"github.com/frappe/boat/internal/api"
	"github.com/frappe/boat/internal/journal"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/park"
	"github.com/frappe/boat/internal/reconcile"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/watch"
)

// hostScanner is the startup adoption scan.
//
// It is an interface for one reason: the ordering that matters — adopt, then
// accept — has to be provable without a host under the test, and a real
// adopt.Scanner runs `systemctl list-units` and `lvs` on the first line.
type hostScanner interface {
	Scan(ctx context.Context, runner *run.Runner) (adopt.Result, error)
}

var _ hostScanner = (*adopt.Scanner)(nil)

// daemonParts are the long-lived collaborators one `boat daemon` run owns. They
// are built together, they run for the daemon's lifetime, and they are released
// in the reverse of the order they were built in.
type daemonParts struct {
	store      *store.Store
	journal    *journal.Journal
	machines   *vm.Manager
	reconciler *reconcile.Reconciler
	trap       *park.Trap
	scanner    hostScanner
	api        *api.Server
	// runner is the daemon's own runner, and it traces to stderr. systemd puts
	// stderr in the journal, so the `+ command` lines of the two things that use
	// it — the adoption scan and the wake trap's startup re-park — land beside
	// this daemon's log lines, which is the only record either of them has. The
	// trap's once-a-second poll deliberately does not use it; see park.NewTrap.
	runner *run.Runner
}

// build constructs the daemon in dependency order: the store, the journal over
// it, the mechanics, the reconciler over all three, and then the two things that
// ask the reconciler for work — the API and the wake trap.
//
// Opening the store is the whole of the fallible part now that the journal keeps
// its decisions in the store's own file. Nothing here starts a goroutine or
// touches the host: construction that also ran would leave a half-built daemon
// behind on the next error.
func build(options daemonOptions) (*daemonParts, error) {
	database, err := store.Open(options.storePath)
	if err != nil {
		return nil, fmt.Errorf("could not open the store at %s: %w", options.storePath, err)
	}
	return assemble(database), nil
}

// assemble wires the parts that cannot fail. One vm.Manager serves the API and
// the reconciler, so a verb and a pass reach the host through the same
// mechanics rather than through two copies of them.
func assemble(database *store.Store) *daemonParts {
	decisions := journal.New(database)
	parts := &daemonParts{
		store:    database,
		journal:  decisions,
		machines: vm.NewManager(),
		scanner:  adopt.NewScanner(),
		runner:   run.NewRunner(os.Stderr),
	}
	parts.reconciler = reconcile.New(database, parts.machines, decisions)
	parts.trap = park.NewTrap(parts.runner, parts.wake)
	parts.api = api.NewServer(parts.dependencies())
	return parts
}

// dependencies is the API's collaborators, named rather than inlined so the two
// things that cannot be seen from outside internal/api can be asserted from a
// test.
//
// That the Server is built WITH a reconciler: one built without still serializes
// its own verbs, so nothing fails, breaks or logs — the host simply acquires a
// second driver, and it shows up as a stop that lands in the middle of a start.
//
// And that it is built with the journal: one built without refuses every verb
// that decides something, which is loud rather than silent, but it is a refusal
// nobody would understand from the outside.
//
// One *store.Store satisfies both the operation and the state interfaces; they
// are separate at the API boundary so a handler declares which half of the store
// it actually needs.
func (parts *daemonParts) dependencies() api.Dependencies {
	return api.Dependencies{
		Operations:      parts.store,
		State:           parts.store,
		VirtualMachines: parts.machines,
		Decisions:       parts.journal,
		Reconciler:      parts.reconciler,
		Watch:           watch.NewHub(),
		StartedAt:       time.Now().UTC(),
	}
}

// wake is the wake trap's callback, and it goes through the reconciler on
// purpose.
//
// park.Trap's only gate before it wakes a VM is the sleeping marker on this
// host's disk. It never reads desired power and it cannot: the thing that
// triggered it is an unauthenticated TCP SYN from a stranger. The rule that an
// operator's stop outranks that SYN lives in reconcile.plan and nowhere else,
// which is why this callback asks for a pass instead of performing a wake —
// plan checks desired power BEFORE it reads why the pass was requested, so a
// PowerStopped VM stays down however loudly it is probed.
//
// vm.Manager.Wake fits this signature exactly, and wiring it in here is a
// one-line change that compiles, passes every test in internal/park, and does
// two wrong things at once: it resurrects a VM an operator stopped from an
// unauthenticated packet, and it skips the fence on the way, because vm.Wake is
// mechanics and knows nothing about boot epochs. Do not point this at the VM
// manager. cmd/boat/parts_test.go is the test that says so.
func (parts *daemonParts) wake(ctx context.Context, uuid string) error {
	parts.reconciler.Wake(uuid)
	return nil
}

// startUp adopts what the host already holds and only then opens the listeners.
//
// The order is the whole of it. A Boat that accepted first would answer /export
// out of a store that has not met this host yet, and "this host holds nothing"
// is the one answer a control plane must never be handed by accident: it is
// indistinguishable from a wiped host, and it is what makes Atlas reschedule VMs
// that are running right here.
func (parts *daemonParts) startUp(ctx context.Context, options daemonOptions, token string) ([]listening, error) {
	if err := parts.adopt(ctx); err != nil {
		return nil, err
	}
	return openListeners(options, parts.api, token)
}

// adopt learns this host's VMs by reading the host.
//
// A failed scan stops the daemon rather than serving what it could not confirm.
// A partial scan is a lie (see internal/adopt) and the lie it tells is the
// dangerous one, so the daemon exits and lets its unit restart it — a host that
// will not adopt is a host an operator has to look at, and a crash loop says so
// where a quiet empty export does not.
func (parts *daemonParts) adopt(ctx context.Context) error {
	result, err := parts.scanner.Scan(ctx, parts.runner)
	if err != nil {
		return fmt.Errorf("could not read what this host already holds: %w", err)
	}
	if err := parts.ingest(result.VirtualMachines); err != nil {
		return err
	}
	if err := parts.store.ReplaceQuarantine(result.Quarantined); err != nil {
		return fmt.Errorf("could not record what this host holds that is not a virtual machine: %w", err)
	}
	reportQuarantined(result.Quarantined)
	slog.Info("adopted what this host already holds",
		"virtual_machines", len(result.VirtualMachines), "quarantined", len(result.Quarantined),
		"units", len(result.Units), "logical_volumes", len(result.LogicalVolumes))
	return nil
}

// ingest records the UUIDs whose artifacts read as one coherent VM, and only
// those. The scan's Quarantined set never comes through here, which is the
// point of it existing: ingesting a half-terminated artifact set as a VM is how
// a controller boots a guest whose disk it already released.
func (parts *daemonParts) ingest(adopted []model.VirtualMachine) error {
	for _, record := range adopted {
		if err := parts.store.PutVirtualMachine(record); err != nil {
			return fmt.Errorf("could not record the adopted virtual machine %s: %w", record.UUID, err)
		}
	}
	return nil
}

// reportQuarantined puts each ambiguous artifact set in the daemon's log as
// well as the store, because the two are read by different people at different
// times: the export is how Atlas and an operator find out, the journal is what
// someone reads when the host will not answer its own API.
//
// Warn and not Error: a quarantine is a host state an operator resolves, not a
// fault of the daemon reporting it.
func reportQuarantined(quarantined []model.Quarantine) {
	for _, each := range quarantined {
		slog.Warn("quarantined an artifact set that does not read as a virtual machine",
			"uuid", each.UUID, "reason", each.Reason, "evidence", each.Evidence, "seen_at", each.SeenAt)
	}
}

// close releases the one file this daemon holds. The write-ahead decisions are
// in it too, so there is no second handle to release in an order that could be
// got wrong.
func (parts *daemonParts) close() error {
	return parts.store.Close()
}
