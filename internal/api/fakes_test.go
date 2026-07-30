package api

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/units"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/watch"
)

// fakeStore is the store without bbolt. The rules the handlers lean on are kept
// rather than stubbed: a second claim of one identifier is a replay, a claim
// against different work is a conflict, a fence epoch never moves backwards, and
// every observed write bumps the epoch. Those rules are what these tests are
// about, so a fake that let them slide would prove nothing.
//
// Every method takes the mutex, because *store.Store is safe for concurrent use
// and the tests that matter most here are the ones with two requests in flight
// at once: a fake that raced where the real store does not would make the
// serialization tests untestable rather than passing.
type fakeStore struct {
	mutex           sync.Mutex
	operations      map[string]model.Operation
	virtualMachines map[string]model.VirtualMachine
	desired         map[string]model.DesiredVirtualMachine
	fences          map[string]int64
	decisions       []model.Decision
	epoch           int64
	logicalVolumes  []model.LogicalVolume
	// movedAt is the observed epoch the CAS pretends each VM's record was last
	// written at. The real store stamps it on every observed write; here it is set
	// by hand, so a test can state "this VM moved at epoch 12" without driving
	// twelve observations to get there.
	movedAt        map[string]int64
	desiredWrites  int
	desiredDeletes int
	fenceWrites    int
	claimError     error
	completeError  error
	decisionError  error
	readError      error
	writeError     error
	snapshotError  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		operations:      map[string]model.Operation{},
		virtualMachines: map[string]model.VirtualMachine{},
		desired:         map[string]model.DesiredVirtualMachine{},
		fences:          map[string]int64{},
		movedAt:         map[string]int64{},
	}
}

// fence records an epoch the way a PUT would. A start is refused without one, so
// every test that expects a VM to boot has to say Atlas asserted it — which is
// the fence doing its job, not test ceremony.
// fence records the epoch AND the desired record, because a PUT writes both
// from one document and a fence with no desired state is therefore not a state
// Atlas can produce. It is reachable only by a retraction, which the gate now
// refuses — so a test that wants that state says so with fenceWithoutDesire.
func (fake *fakeStore) fence(uuid string, epoch int64) {
	fake.fenceWithoutDesire(uuid, epoch)
	fake.desire(model.DesiredVirtualMachine{
		UUID: uuid, BootEpoch: epoch, DesiredPower: model.PowerRunning,
	})
}

func (fake *fakeStore) fenceWithoutDesire(uuid string, epoch int64) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.fences[uuid] = epoch
}

// desire records what Atlas asserted, the way a PUT would. The verbs that read
// desired state instead of taking numbers on the wire are unreadable without
// it, and so is the precedence rule that outranks a wake.
func (fake *fakeStore) desire(record model.DesiredVirtualMachine) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.desired[record.UUID] = record
}

func (fake *fakeStore) ClaimOperation(identifier, verb, uuid string) (model.Operation, bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.claimError != nil {
		return model.Operation{}, false, fake.claimError
	}
	if existing, found := fake.operations[identifier]; found {
		if !existing.Matches(verb, uuid) {
			return model.Operation{}, false, store.ErrOperationConflict
		}
		return existing, false, nil
	}
	claimed := model.Operation{
		Identifier:         identifier,
		Verb:               verb,
		VirtualMachineUUID: uuid,
		Status:             model.OperationRunning,
		StartedAt:          time.Now().UTC(),
	}
	fake.operations[identifier] = claimed
	return claimed, true, nil
}

func (fake *fakeStore) CompleteOperation(operation model.Operation) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.completeError != nil {
		return fake.completeError
	}
	fake.operations[operation.Identifier] = operation
	return nil
}

func (fake *fakeStore) GetOperation(identifier string) (model.Operation, bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return model.Operation{}, false, fake.readError
	}
	operation, found := fake.operations[identifier]
	return operation, found, nil
}

// Record keeps the decisions in the order they were taken, which is the property
// the real store's key layout buys and the one a test asserting a write-ahead
// ordering depends on.
func (fake *fakeStore) Record(decision model.Decision) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.decisionError != nil {
		return fake.decisionError
	}
	fake.decisions = append(fake.decisions, decision)
	return nil
}

func (fake *fakeStore) decided() []model.Decision {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]model.Decision{}, fake.decisions...)
}

// PutVirtualMachine bumps the observed epoch with the write, as the real store
// does in one transaction: a snapshot and its epoch may never disagree.
func (fake *fakeStore) PutVirtualMachine(record model.VirtualMachine) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.writeError != nil {
		return fake.writeError
	}
	fake.virtualMachines[record.UUID] = record
	fake.epoch++
	return nil
}

func (fake *fakeStore) GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return model.VirtualMachine{}, false, fake.readError
	}
	record, found := fake.virtualMachines[uuid]
	return record, found, nil
}

func (fake *fakeStore) ListVirtualMachines() ([]model.VirtualMachine, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return nil, fake.readError
	}
	return fake.observed(), nil
}

// observed is the listing in a fixed order, so a document assembled from it can
// be compared field by field instead of searched.
func (fake *fakeStore) observed() []model.VirtualMachine {
	records := make([]model.VirtualMachine, 0, len(fake.virtualMachines))
	for _, record := range fake.virtualMachines {
		records = append(records, record)
	}
	slices.SortFunc(records, func(first, second model.VirtualMachine) int {
		return strings.Compare(first.UUID, second.UUID)
	})
	return records
}

func (fake *fakeStore) PutDesired(record model.DesiredVirtualMachine) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.writeError != nil {
		return fake.writeError
	}
	fake.desired[record.UUID] = record
	fake.desiredWrites++
	return nil
}

// DeleteDesired keeps the store's asymmetry: the assertion goes and the fence
// epoch stays, which is what the retraction tests are about.
func (fake *fakeStore) DeleteDesired(uuid string) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.writeError != nil {
		return fake.writeError
	}
	delete(fake.desired, uuid)
	fake.desiredDeletes++
	return nil
}

func (fake *fakeStore) GetDesired(uuid string) (model.DesiredVirtualMachine, bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return model.DesiredVirtualMachine{}, false, fake.readError
	}
	record, found := fake.desired[uuid]
	return record, found, nil
}

// SetFenceEpoch keeps the store's rule: an epoch that can go backwards is not a
// fence.
func (fake *fakeStore) SetFenceEpoch(uuid string, epoch int64) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.writeError != nil {
		return fake.writeError
	}
	if held, found := fake.fences[uuid]; found && epoch < held {
		return fmt.Errorf("%w: %s holds epoch %d, refusing %d", store.ErrFenceRegression, uuid, held, epoch)
	}
	fake.fences[uuid] = epoch
	fake.fenceWrites++
	return nil
}

func (fake *fakeStore) FenceEpoch(uuid string) (int64, bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return 0, false, fake.readError
	}
	epoch, found := fake.fences[uuid]
	return epoch, found, nil
}

func (fake *fakeStore) ObservedEpoch() (int64, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return 0, fake.readError
	}
	return fake.epoch, nil
}

// CheckVirtualMachineUnmoved keeps the real rule rather than stubbing it: an
// epoch newer than the host has reached is a token from another store, and a
// record written after the epoch offered has moved. Stubbing either would make
// every CAS test a test of the fake.
func (fake *fakeStore) CheckVirtualMachineUnmoved(uuid string, offered int64) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.readError != nil {
		return fake.readError
	}
	if offered > fake.epoch || fake.movedAt[uuid] > offered {
		return fmt.Errorf("%w: vm %s", store.ErrObservationMoved, uuid)
	}
	return nil
}

func (fake *fakeStore) Snapshot() (model.Export, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.snapshotError != nil {
		return model.Export{}, fake.snapshotError
	}
	return model.Export{
		ObservedEpoch:   fake.epoch,
		TakenAt:         time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
		VirtualMachines: fake.observed(),
		LogicalVolumes:  fake.logicalVolumes,
		FenceEpochs:     maps.Clone(fake.fences),
	}, nil
}

// fakeVirtualMachines counts what it was asked to do, so a test can prove a
// replay ran nothing at all.
type fakeVirtualMachines struct {
	trace        io.Writer
	traceText    string
	starts       int
	stops        int
	stopRequests []vm.StopRequest
	startError   error
	stopError    error
	missing      bool
	observed     model.VirtualMachine
	observeError error

	// One counter per WO-2 verb, and the request each was handed. The requests
	// are what the desired-state tests are about: a verb that ran is not the same
	// claim as a verb that ran with the numbers Atlas asserted.
	pauses             int
	resumes            int
	wakes              int
	terminates         int
	sleepRequests      []vm.SleepRequest
	resizeRequests     []vm.ResizeRequest
	rebuildRequests    []vm.RebuildRequest
	reservedIPRequests []vm.ReservedIPRequest
	reservedDelivery   reservedip.Delivery
	verbError          error
	sleepResult        vm.SleepResult
	firecrackerUID     int
	firecrackerUIDErr  error
	// beforeRebuild runs as the rebuild reaches the host, which is the only place
	// a test can see what was recorded BEFORE the volume would have been dropped.
	// Ordering is the whole of the write-ahead rule, and it is invisible to any
	// assertion made after the request returns.
	beforeRebuild func()
	// beforeTerminate is the same seam for the same reason: whether the desired
	// state was already retracted when the host was reached is only observable
	// from inside the verb, and a crash lands in exactly that window.
	beforeTerminate func()

	// hold is how long a verb pretends to take, and overlapped is whether a
	// second one ever ran while it did. Counters alone cannot see the failure the
	// serialization tests are about — a stop and a start that both ran, correctly
	// counted, having interleaved vm-network-down with vm-network-up — so the fake
	// watches for the overlap itself.
	concurrency sync.Mutex
	hold        time.Duration
	inFlight    int
	overlapped  bool
}

func (fake *fakeVirtualMachines) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	defer fake.enter()()
	fake.starts++
	fake.writeTrace()
	return false, fake.startError
}

func (fake *fakeVirtualMachines) Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error {
	defer fake.enter()()
	fake.stops++
	fake.stopRequests = append(fake.stopRequests, request)
	fake.writeTrace()
	return fake.stopError
}

func (fake *fakeVirtualMachines) Pause(ctx context.Context, runner *run.Runner, uuid string) error {
	defer fake.enter()()
	fake.pauses++
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) Resume(ctx context.Context, runner *run.Runner, uuid string) error {
	defer fake.enter()()
	fake.resumes++
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest,
) (vm.SleepResult, error) {
	defer fake.enter()()
	fake.sleepRequests = append(fake.sleepRequests, request)
	fake.writeTrace()
	return fake.sleepResult, fake.verbError
}

func (fake *fakeVirtualMachines) Wake(ctx context.Context, runner *run.Runner, uuid string) error {
	defer fake.enter()()
	fake.wakes++
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) Resize(
	ctx context.Context, runner *run.Runner, uuid string, request vm.ResizeRequest,
) error {
	defer fake.enter()()
	fake.resizeRequests = append(fake.resizeRequests, request)
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) Rebuild(
	ctx context.Context, runner *run.Runner, uuid string, request vm.RebuildRequest,
) error {
	defer fake.enter()()
	if fake.beforeRebuild != nil {
		fake.beforeRebuild()
	}
	fake.rebuildRequests = append(fake.rebuildRequests, request)
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) ReservedIP(
	ctx context.Context, runner *run.Runner, uuid string, request vm.ReservedIPRequest,
) (reservedip.Delivery, error) {
	defer fake.enter()()
	fake.reservedIPRequests = append(fake.reservedIPRequests, request)
	fake.writeTrace()
	return fake.reservedDelivery, fake.verbError
}

func (fake *fakeVirtualMachines) Terminate(ctx context.Context, runner *run.Runner, uuid string) error {
	defer fake.enter()()
	if fake.beforeTerminate != nil {
		fake.beforeTerminate()
	}
	fake.terminates++
	fake.writeTrace()
	return fake.verbError
}

func (fake *fakeVirtualMachines) FirecrackerUID(
	ctx context.Context, runner *run.Runner, uuid string,
) (int, error) {
	return fake.firecrackerUID, fake.firecrackerUIDErr
}

// enter records that a verb is running on the host and returns the function
// that records it leaving. It sleeps for hold while it is in, so two verbs that
// were not serialized are certain to be caught overlapping rather than merely
// likely to be.
func (fake *fakeVirtualMachines) enter() func() {
	fake.concurrency.Lock()
	fake.inFlight++
	if fake.inFlight > 1 {
		fake.overlapped = true
	}
	hold := fake.hold
	fake.concurrency.Unlock()
	time.Sleep(hold)
	return func() {
		fake.concurrency.Lock()
		defer fake.concurrency.Unlock()
		fake.inFlight--
	}
}

// everOverlapped reports whether two verbs were ever inside the host at once.
func (fake *fakeVirtualMachines) everOverlapped() bool {
	fake.concurrency.Lock()
	defer fake.concurrency.Unlock()
	return fake.overlapped
}

func (fake *fakeVirtualMachines) Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error) {
	if fake.observeError != nil {
		return model.VirtualMachine{}, fake.observeError
	}
	observed := fake.observed
	observed.UUID = uuid
	return observed, nil
}

func (fake *fakeVirtualMachines) Exists(ctx context.Context, runner *run.Runner, uuid string) bool {
	return !fake.missing
}

// writeTrace stands in for the `+ command` lines a real verb's runner emits.
// run.Runner keeps its writer to itself, so the fake is handed the same writer
// the server gave the runner — see newTestServer.
func (fake *fakeVirtualMachines) writeTrace() {
	if fake.trace != nil && fake.traceText != "" {
		fmt.Fprint(fake.trace, fake.traceText)
	}
}

// newTestServer wires the fakes together and lets the fake verb write the
// operation's trace, which is what proves the trace reaches Operation.Output.
//
// One fake serves both store interfaces because one bbolt file serves both in
// the daemon: the journal, the observed records, the fences and the epoch are
// the same store, and a test that split them could not catch a handler reading
// an epoch that belongs to another snapshot.
func newTestServer(operations *fakeStore, machines *fakeVirtualMachines) *Server {
	return newTestServerWithUnits(operations, machines, newFakeUnits())
}

func newTestServerWithUnits(operations *fakeStore, machines *fakeVirtualMachines, supervisor *fakeUnits) *Server {
	server := NewServer(Dependencies{
		Operations:      operations,
		State:           operations,
		VirtualMachines: machines,
		Decisions:       operations,
		Watch:           watch.NewHub(),
		Units:           supervisor,
		StartedAt:       time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
	})
	server.newRunner = func(trace io.Writer) *run.Runner {
		machines.trace = trace
		return run.NewRunner(trace)
	}
	// The real reader runs lsblk, free and firecracker --version. A handler test
	// has no host, so it is told what the host is.
	server.hostFacts = func(ctx context.Context, runner *run.Runner) (model.HostFacts, error) {
		return model.HostFacts{Hostname: "boat-test-host", KernelVersion: "6.19.0"}, nil
	}
	return server
}

// fakeUnits is the sibling-unit supervisor without systemd. It keeps the one
// rule the handlers lean on — a name outside the supervised set is refused —
// because that rule is what stops this endpoint reaching a per-VM unit, and a
// fake that let it slide would make every test about it pass for free.
type fakeUnits struct {
	liveness []model.UnitLiveness
	acted    []string
	readErr  error
	actErr   error
}

func newFakeUnits() *fakeUnits { return &fakeUnits{liveness: []model.UnitLiveness{}} }

func (fake *fakeUnits) Liveness(ctx context.Context, runner *run.Runner) ([]model.UnitLiveness, error) {
	if fake.readErr != nil {
		return nil, fake.readErr
	}
	return fake.liveness, nil
}

func (fake *fakeUnits) LivenessOf(
	ctx context.Context, runner *run.Runner, name string,
) (model.UnitLiveness, bool, error) {
	if fake.readErr != nil {
		return model.UnitLiveness{}, false, fake.readErr
	}
	if !units.IsSupervised(name) {
		return model.UnitLiveness{}, false, fmt.Errorf("this host supervises no unit named %q", name)
	}
	for _, unit := range fake.liveness {
		if unit.Name == name {
			return unit, true, nil
		}
	}
	return model.UnitLiveness{}, false, nil
}

func (fake *fakeUnits) Act(ctx context.Context, runner *run.Runner, name string, action units.Action) error {
	fake.acted = append(fake.acted, string(action)+" "+name)
	return fake.actErr
}
