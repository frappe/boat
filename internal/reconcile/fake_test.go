// The harness every scenario in this package runs on: a fake host that records
// what was asked of it, a real bbolt store and a real journal under t.TempDir,
// and an occupancy check that fails the test the moment two goroutines are
// inside one VM at once.

package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/journal"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
)

const (
	firstVirtualMachine  = "11111111-2222-3333-4444-555555555555"
	secondVirtualMachine = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// testDeadline bounds every wait in this package's tests. Nothing here should
// take a millisecond; the deadline exists so that a reconciler which serialized
// two VMs against each other fails with a message instead of hanging the suite.
const testDeadline = 5 * time.Second

var errHostRefused = errors.New("the host refused")

// errStartSkippedWhileSleeping is what a real Start reports for a sleeping VM,
// and modelling it is the point: the unit's ConditionPathNotExists sees the
// sleeping marker, `systemctl start` skips the unit and exits 0, and the
// trailing `is-active` then fails the whole start. A fake whose Start set
// Running unconditionally made a reconciler that could not wake anything look
// like one that could.
var errStartSkippedWhileSleeping = errors.New("the unit was skipped: the virtual machine is asleep")

// occupancy is how "one actor per VM" is asserted rather than assumed.
//
// The fake host marks a VM on the way into every call and unmarks it on the way
// out, so any overlap — two passes, a verb inside a pass, a pass inside a
// verb — is counted here. It is a count rather than a t.Fatal because the
// overlap happens on a goroutine the test does not own, and a failure reported
// from there would race the test's own completion.
type occupancy struct {
	mutex      sync.Mutex
	inside     map[string]int
	violations int
}

func newOccupancy() *occupancy { return &occupancy{inside: map[string]int{}} }

// enter yields the processor before returning, so that two goroutines racing for
// one VM actually interleave rather than being serialized by luck. Without it a
// broken reconciler would pass this test on a machine fast enough to finish each
// pass inside its scheduling quantum.
func (occupancy *occupancy) enter(uuid string) {
	occupancy.mutex.Lock()
	occupancy.inside[uuid]++
	if occupancy.inside[uuid] > 1 {
		occupancy.violations++
	}
	occupancy.mutex.Unlock()
	runtime.Gosched()
}

func (occupancy *occupancy) exit(uuid string) {
	occupancy.mutex.Lock()
	defer occupancy.mutex.Unlock()
	occupancy.inside[uuid]--
}

func (occupancy *occupancy) overlaps() int {
	occupancy.mutex.Lock()
	defer occupancy.mutex.Unlock()
	return occupancy.violations
}

// call is one thing the reconciler asked of the host.
type call struct {
	verb string
	uuid string
}

// machines is the fake host. It holds each VM's status the way a real one does —
// Start and Stop change what a later Observe reports — so a test can assert that
// a pass converged rather than that it called something.
type machines struct {
	mutex     sync.Mutex
	status    map[string]model.VirtualMachineStatus
	calls     []call
	refusals  map[string]error
	occupancy *occupancy
	// hook runs inside every call while the VM is still marked occupied, which is
	// how a test blocks one VM's actor or learns that a pass reached the host.
	hook func(verb string, uuid string)
}

func newMachines(occupancy *occupancy) *machines {
	return &machines{
		status:    map[string]model.VirtualMachineStatus{},
		refusals:  map[string]error{},
		occupancy: occupancy,
	}
}

var _ VirtualMachines = (*machines)(nil)

func (machines *machines) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	if err := machines.called("start", uuid); err != nil {
		return false, err
	}
	// A start does not clear the sleeping marker, so a VM that was asleep is
	// still asleep afterwards and the start reports the failure it really
	// reports. This is the one behaviour of the real host that a fake here must
	// not simplify away: it is the difference between a wake that works and a
	// pass that fails every five minutes forever.
	if machines.statusOf(uuid) == model.StatusSleeping {
		return false, errStartSkippedWhileSleeping
	}
	machines.setStatus(uuid, model.StatusRunning)
	return false, nil
}

func (machines *machines) Stop(
	ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest,
) error {
	if err := machines.called("stop", uuid); err != nil {
		return err
	}
	machines.setStatus(uuid, model.StatusStopped)
	return nil
}

// Sleep parks the VM. Stopped-and-marked-sleeping is one status here because
// the marker is what a real Observe reports Sleeping from.
func (machines *machines) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest,
) (vm.SleepResult, error) {
	if err := machines.called("sleep", uuid); err != nil {
		return vm.SleepResult{}, err
	}
	machines.setStatus(uuid, model.StatusSleeping)
	return vm.SleepResult{MemorySnapshot: true}, nil
}

// Wake is Start with the marker taken off first, which is the whole of why it
// exists: the same start that is skipped above succeeds once the marker is gone.
func (machines *machines) Wake(ctx context.Context, runner *run.Runner, uuid string) error {
	if err := machines.called("wake", uuid); err != nil {
		return err
	}
	machines.setStatus(uuid, model.StatusRunning)
	return nil
}

func (machines *machines) Observe(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	if err := machines.called("observe", uuid); err != nil {
		return model.VirtualMachine{UUID: uuid, ObservedStatus: model.StatusUnknown}, err
	}
	return model.VirtualMachine{
		UUID:           uuid,
		ObservedStatus: machines.statusOf(uuid),
		ObservedAt:     time.Now().UTC(),
	}, nil
}

// called records the call, holds the VM occupied across the test's hook, and
// returns whatever refusal the test staged for this verb.
func (machines *machines) called(verb string, uuid string) error {
	machines.occupancy.enter(uuid)
	defer machines.occupancy.exit(uuid)
	machines.mutex.Lock()
	machines.calls = append(machines.calls, call{verb: verb, uuid: uuid})
	hook, refusal := machines.hook, machines.refusals[verb]
	machines.mutex.Unlock()
	if hook != nil {
		hook(verb, uuid)
	}
	return refusal
}

func (machines *machines) setStatus(uuid string, status model.VirtualMachineStatus) {
	machines.mutex.Lock()
	defer machines.mutex.Unlock()
	machines.status[uuid] = status
}

func (machines *machines) statusOf(uuid string) model.VirtualMachineStatus {
	machines.mutex.Lock()
	defer machines.mutex.Unlock()
	status, found := machines.status[uuid]
	if !found {
		return model.StatusStopped
	}
	return status
}

func (machines *machines) refuse(verb string, err error) {
	machines.mutex.Lock()
	defer machines.mutex.Unlock()
	machines.refusals[verb] = err
}

func (machines *machines) onCall(hook func(verb string, uuid string)) {
	machines.mutex.Lock()
	defer machines.mutex.Unlock()
	machines.hook = hook
}

func (machines *machines) recorded() []call {
	machines.mutex.Lock()
	defer machines.mutex.Unlock()
	return append([]call{}, machines.calls...)
}

// counted is how many times the host was asked to do verb, for any VM.
func (machines *machines) counted(verb string) int {
	count := 0
	for _, recorded := range machines.recorded() {
		if recorded.verb == verb {
			count++
		}
	}
	return count
}

// harness is one host: the store and journal a reconciler persists through, the
// fake mechanics it drives, and the delays it asked to wait.
type harness struct {
	store       *store.Store
	journal     *journal.Journal
	machines    *machines
	occupancy   *occupancy
	reconciler  *Reconciler
	journalPath string
	delays      chan time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "boat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	occupancy := newOccupancy()
	return &harness{
		store:       database,
		machines:    newMachines(occupancy),
		occupancy:   occupancy,
		journalPath: filepath.Join(directory, "journal.db"),
		delays:      make(chan time.Duration, 64),
	}
}

// start opens the journal and builds the reconciler. It is separate from
// newHarness so that a test can leave a crashed operation in the journal first,
// which is the only way to reach the state a restart recovers from.
//
// The intervals are shortened and the wait is replaced with a recorder: the
// backoff is asserted from the delays the reconciler asked for, so no test ever
// lives through one.
func (harness *harness) start(t *testing.T) *Reconciler {
	t.Helper()
	record, err := journal.New(harness.store, harness.journalPath)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { record.Close() })
	harness.journal = record
	harness.reconciler = New(harness.store, harness.machines, record)
	harness.reconciler.sweepInterval = time.Millisecond
	harness.reconciler.backoff = backoff{base: 10 * time.Millisecond, max: 40 * time.Millisecond}
	harness.reconciler.wait = harness.recordDelay
	t.Cleanup(func() {
		harness.reconciler.stop()
		if overlaps := harness.occupancy.overlaps(); overlaps != 0 {
			t.Errorf("%d goroutines ran inside one virtual machine at the same time", overlaps)
		}
	})
	return harness.reconciler
}

// recordDelay stands in for sleeping. It never blocks the actor: a full channel
// means a test that is not reading delays, and stalling its serve goroutine
// would turn an assertion about backoff into a deadlock.
func (harness *harness) recordDelay(ctx context.Context, delay time.Duration) {
	select {
	case harness.delays <- delay:
	default:
	}
}

func newReconciler(t *testing.T) *harness {
	t.Helper()
	harness := newHarness(t)
	harness.start(t)
	return harness
}

// desire records what Atlas wants of a VM, with the fence epoch that permits
// this host to boot it. Both, always: a desired record without an epoch is a VM
// this host refuses to start, which is a different test.
func (harness *harness) desire(t *testing.T, uuid string, power model.DesiredPower) {
	t.Helper()
	harness.desireUnfenced(t, uuid, power)
	if err := harness.store.SetFenceEpoch(uuid, 1); err != nil {
		t.Fatalf("set fence epoch: %v", err)
	}
}

func (harness *harness) desireUnfenced(t *testing.T, uuid string, power model.DesiredPower) {
	t.Helper()
	record := model.DesiredVirtualMachine{UUID: uuid, BootEpoch: 1, DesiredPower: power}
	if err := harness.store.PutDesired(record); err != nil {
		t.Fatalf("put desired: %v", err)
	}
}

// crash leaves behind exactly what a daemon that died mid-operation does: a
// claimed operation the store still calls Running, and a decision recorded
// against it by an incarnation that is gone.
func (harness *harness) crash(t *testing.T, identifier string, uuid string) {
	t.Helper()
	if _, claimed, err := harness.store.ClaimOperation(identifier, "stop-vm", uuid); err != nil || !claimed {
		t.Fatalf("claim %s: claimed=%v err=%v", identifier, claimed, err)
	}
	record, err := journal.New(harness.store, harness.journalPath)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	decision := journal.Decision{OperationID: identifier, Step: "allocate-address"}
	if err := record.Record(decision); err != nil {
		t.Fatalf("record decision: %v", err)
	}
	if err := record.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func (harness *harness) observedStatus(t *testing.T, uuid string) model.VirtualMachineStatus {
	t.Helper()
	record, found, err := harness.store.GetVirtualMachine(uuid)
	if err != nil || !found {
		t.Fatalf("read the observed record of %s: found=%v err=%v", uuid, found, err)
	}
	return record.ObservedStatus
}

// waitFor polls until the condition holds, and fails with what the test was
// waiting for rather than hanging when it never does.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settled waits until the host has been left alone for a moment, which is how a
// test asserts that nothing more is coming without asserting on a clock.
func (harness *harness) settled(t *testing.T) {
	t.Helper()
	previous := -1
	for attempt := 0; attempt < 100; attempt++ {
		current := len(harness.machines.recorded())
		if current == previous {
			return
		}
		previous = current
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the host never stopped being driven")
}
