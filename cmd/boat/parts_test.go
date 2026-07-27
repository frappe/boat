package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/adopt"
	"github.com/frappe/boat/internal/journal"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/reconcile"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
)

const adoptedUuid = "1f8e0a2c-0000-4000-8000-00000000000a"

// errScanFailed stands in for any host read that would not answer: the whole
// scan fails with it, because a partial scan is a lie.
var errScanFailed = errors.New("could not list this host's units")

// fakeScanner is the host read, without a host. It notes whether the control
// socket existed by the time it ran, which is the whole of what the adoption
// ordering test needs to know.
type fakeScanner struct {
	result           adopt.Result
	err              error
	scans            int
	socketPath       string
	socketWasServing bool
}

func (fake *fakeScanner) Scan(ctx context.Context, runner *run.Runner) (adopt.Result, error) {
	fake.scans++
	if _, err := os.Stat(fake.socketPath); err == nil {
		fake.socketWasServing = true
	}
	return fake.result, fake.err
}

// fakeMechanics is vm.Manager's five methods without systemd or Firecracker. It
// carries all five rather than the three the reconciler asks for today, so the
// tests below hold still while internal/reconcile grows its interface.
type fakeMechanics struct {
	mutex    sync.Mutex
	status   model.VirtualMachineStatus
	starts   int
	stops    int
	sleeps   int
	wakes    int
	observes int
}

func (fake *fakeMechanics) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.starts++
	return false, nil
}

func (fake *fakeMechanics) Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.stops++
	return nil
}

func (fake *fakeMechanics) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest,
) (vm.SleepResult, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.sleeps++
	return vm.SleepResult{}, nil
}

func (fake *fakeMechanics) Wake(ctx context.Context, runner *run.Runner, uuid string) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.wakes++
	return nil
}

func (fake *fakeMechanics) Observe(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.observes++
	return model.VirtualMachine{UUID: uuid, ObservedStatus: fake.status}, nil
}

// resumed reports whether the mechanics were asked to bring the guest back, by
// either path: internal/reconcile is moving a resume from Start to Wake, and
// which of the two it uses is not what these tests are about.
func (fake *fakeMechanics) resumed() bool {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.starts > 0 || fake.wakes > 0
}

func (fake *fakeMechanics) seen() int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.observes
}

// newTestParts builds the daemon's parts over a real store and journal in a
// temporary directory, with the host reads replaced. Nothing here needs root
// and nothing here touches a host.
func newTestParts(t *testing.T, machines reconcile.VirtualMachines) *daemonParts {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "boat.db"))
	if err != nil {
		t.Fatalf("could not open the store: %v", err)
	}
	decisions, err := journal.New(database, journalPath(filepath.Join(directory, "boat.db")))
	if err != nil {
		database.Close()
		t.Fatalf("could not open the journal: %v", err)
	}
	parts := &daemonParts{store: database, journal: decisions, runner: run.NewRunner(nil), scanner: &fakeScanner{}}
	parts.reconciler = reconcile.New(database, machines, decisions)
	t.Cleanup(func() { parts.close() })
	return parts
}

// A Boat that accepted before it had read the host would answer /export out of
// a store that has not met this host yet, and "this host holds nothing" is
// indistinguishable from a wiped host.
func TestAdoptionRunsBeforeTheListenersAccept(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})
	options := daemonOptions{socketPath: filepath.Join(t.TempDir(), "boat.sock")}
	scanner := &fakeScanner{
		socketPath: options.socketPath,
		result:     adopt.Result{VirtualMachines: []model.VirtualMachine{{UUID: adoptedUuid, ObservedStatus: model.StatusRunning}}},
	}
	parts.scanner = scanner

	active, err := parts.startUp(context.Background(), options, "")
	if err != nil {
		t.Fatalf("could not start up: %v", err)
	}
	defer closeListeners(active)

	if scanner.scans != 1 {
		t.Errorf("the host was scanned %d times, want once", scanner.scans)
	}
	if scanner.socketWasServing {
		t.Error("the control socket was already accepting when the host was scanned")
	}
	record, found, err := parts.store.GetVirtualMachine(adoptedUuid)
	if err != nil || !found || record.ObservedStatus != model.StatusRunning {
		t.Errorf("the adopted virtual machine did not reach the store: %+v found=%v err=%v", record, found, err)
	}
}

// Quarantine is reported, never ingested. A half-terminated artifact set that
// became a VM record is how a controller boots a guest whose disk it already
// released.
func TestQuarantinedArtifactsAreNeverIngestedAsVirtualMachines(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})
	parts.scanner = &fakeScanner{result: adopt.Result{
		Quarantined: []model.Quarantine{{
			UUID:     adoptedUuid,
			Reason:   "an active unit with no network namespace",
			Evidence: []string{"firecracker-vm@" + adoptedUuid + ".service is active"},
			SeenAt:   time.Now().UTC(),
		}},
	}}

	if err := parts.adopt(context.Background()); err != nil {
		t.Fatalf("could not adopt: %v", err)
	}

	if _, found, _ := parts.store.GetVirtualMachine(adoptedUuid); found {
		t.Error("a quarantined artifact set was ingested as a virtual machine")
	}
	records, err := parts.store.ListVirtualMachines()
	if err != nil || len(records) != 0 {
		t.Errorf("got %d virtual machines, want none: %v", len(records), err)
	}
}

// A partial picture of a live host is worse than no daemon: the scan fails the
// start rather than serving what it could not confirm.
func TestAFailedAdoptionOpensNoListener(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})
	options := daemonOptions{socketPath: filepath.Join(t.TempDir(), "boat.sock")}
	parts.scanner = &fakeScanner{socketPath: options.socketPath, err: errScanFailed}

	active, err := parts.startUp(context.Background(), options, "")

	if err == nil {
		closeListeners(active)
		t.Fatal("a daemon that could not read its host started serving anyway")
	}
	if _, statErr := os.Stat(options.socketPath); statErr == nil {
		t.Error("a listener was opened despite the failed adoption")
	}
}

// The wake trap's only gate is the sleeping marker on disk: it never reads
// desired power, and the packet that triggered it is a stranger's. So the
// callback asks the reconciler for a pass, and reconcile.plan refuses — an
// operator's stop outranks an unauthenticated SYN.
//
// Wiring this callback to vm.Manager.Wake instead compiles, because the
// signatures match. This test is what fails when someone does.
func TestTheWakeTrapCannotResurrectAStoppedVirtualMachine(t *testing.T) {
	machines := &fakeMechanics{status: model.StatusSleeping}
	parts := newTestParts(t, machines)
	assertDesired(t, parts, model.PowerStopped)

	if err := parts.wake(context.Background(), adoptedUuid); err != nil {
		t.Fatalf("the wake callback failed: %v", err)
	}
	waitForPass(t, machines)

	if machines.resumed() {
		t.Error("an unauthenticated SYN resurrected a virtual machine an operator had stopped")
	}
}

// The positive control for the test above: the same callback, the same trap,
// a VM Atlas still wants running — and the guest comes back. Without this, a
// callback wired to nothing at all would pass the test above.
func TestTheWakeTrapResumesAVirtualMachineThatIsStillWanted(t *testing.T) {
	machines := &fakeMechanics{status: model.StatusSleeping}
	parts := newTestParts(t, machines)
	assertDesired(t, parts, model.PowerRunning)

	if err := parts.wake(context.Background(), adoptedUuid); err != nil {
		t.Fatalf("the wake callback failed: %v", err)
	}
	waitForPass(t, machines)

	if !machines.resumed() {
		t.Error("a sleeping virtual machine Atlas still wants running was not resumed by its wake")
	}
}

// A Server built without a Reconciler serializes its own verbs and nothing
// else, so the daemon acquiring a second driver of the host is silent. This is
// what keeps it from being silent.
func TestTheApiIsBuiltWithTheReconciler(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})

	dependencies := parts.dependencies()

	if dependencies.Reconciler == nil {
		t.Error("the API was built without a reconciler, so its verbs do not exclude a reconcile pass")
	}
	if dependencies.Reconciler != parts.reconciler {
		t.Error("the API serializes against a different reconciler than the one driving the host")
	}
}

func TestTheJournalSitsBesideTheStore(t *testing.T) {
	if got := journalPath("/var/lib/boat/boat.db"); got != "/var/lib/boat/journal.db" {
		t.Errorf("got %s, want the journal beside the store", got)
	}
}

// assertDesired is Atlas asserting intent, fence and all: the reconciler boots
// nothing it holds no epoch for, so a test about waking has to say the epoch
// out loud.
func assertDesired(t *testing.T, parts *daemonParts, power model.DesiredPower) {
	t.Helper()
	if err := parts.store.SetFenceEpoch(adoptedUuid, 1); err != nil {
		t.Fatalf("could not assert the fence epoch: %v", err)
	}
	record := model.DesiredVirtualMachine{UUID: adoptedUuid, BootEpoch: 1, DesiredPower: power}
	if err := parts.store.PutDesired(record); err != nil {
		t.Fatalf("could not assert the desired state: %v", err)
	}
}

// waitForPass waits for the pass the callback asked for. Wake never runs the
// pass on the caller's goroutine — an HTTP handler must not wait for a guest to
// boot — so the test waits for the host read every pass begins with, and then
// for the pass to settle.
func waitForPass(t *testing.T, machines *fakeMechanics) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for machines.seen() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the wake callback never reached the reconciler")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

func closeListeners(active []listening) {
	for _, each := range active {
		each.listener.Close()
	}
}
