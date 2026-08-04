package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/migration"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/wire"
)

// recordedPhase is one call the migration seam saw, so a test can prove a phase
// reached the host exactly once and with the parameters Atlas sent.
type recordedPhase struct {
	uuid  string
	phase string
	body  wire.MigrateRequest
}

// migrationSeam substitutes runMigrationPhase for a handler test: it records each
// call and returns a canned result, so the tests are about the endpoint's
// claim/turn/record behaviour rather than about qemu-nbd or dmsetup. The phase
// logic itself is unit-tested in internal/migration and the result mappers below.
type migrationSeam struct {
	mutex      sync.Mutex
	calls      []recordedPhase
	result     model.OperationResult
	err        error
	hold       time.Duration
	inFlight   int
	overlapped bool
}

func (seam *migrationSeam) run(_ context.Context, _ *run.Runner, uuid, phase string, body wire.MigrateRequest) (model.OperationResult, error) {
	seam.mutex.Lock()
	seam.calls = append(seam.calls, recordedPhase{uuid: uuid, phase: phase, body: body})
	seam.inFlight++
	if seam.inFlight > 1 {
		seam.overlapped = true
	}
	hold := seam.hold
	seam.mutex.Unlock()

	time.Sleep(hold)

	seam.mutex.Lock()
	defer seam.mutex.Unlock()
	seam.inFlight--
	return seam.result, seam.err
}

func (seam *migrationSeam) count() int {
	seam.mutex.Lock()
	defer seam.mutex.Unlock()
	return len(seam.calls)
}

// migrationServer wires a server whose migration phase runs the seam rather than
// the host.
func migrationServer(t *testing.T, store *fakeStore, machines *fakeVirtualMachines, seam *migrationSeam) *Server {
	t.Helper()
	server := newTestServer(store, machines)
	server.runMigrationPhase = seam.run
	return server
}

// A mutating phase runs once, records the verb migrate-<phase> against the VM,
// and carries the phase's typed result on the operation record.
func TestMigratePhaseRunsAndRecordsTheResult(t *testing.T) {
	seam := &migrationSeam{result: model.OperationResult{"nbd_port": 10001, "nbd_pid": 4242}}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-1", BindAddress: ptr("203.0.113.7")}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/export-source", body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 1 {
		t.Fatalf("the phase ran %d times, want 1", seam.count())
	}
	call := seam.calls[0]
	if call.uuid != testUuid || call.phase != "export-source" || optional(call.body.BindAddress) != "203.0.113.7" {
		t.Fatalf("the phase saw the wrong request: %+v", call)
	}
	operation := decodeOperation(t, recorder)
	if operation.Verb != "migrate-export-source" || operation.Uuid != testUuid {
		t.Errorf("operation names the wrong work: %+v", operation)
	}
	if operation.Status != wire.OperationStatusSuccess {
		t.Errorf("operation status is %s, want Success", operation.Status)
	}
	if operation.Result == nil || (*operation.Result)["nbd_port"] != float64(10001) {
		t.Errorf("the phase result did not reach the record: %+v", operation.Result)
	}
}

// Re-posting a completed operation identifier returns the first result and does
// not run the phase again — the idempotent-replay contract every verb shares.
func TestMigratePhaseReplayDoesNotRerun(t *testing.T) {
	seam := &migrationSeam{result: model.OperationResult{"forwarding": true}}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-2", VirtualMachineIpv6: ptr("2001:db8::1")}
	first := postJSON(t, handler, "/vms/"+testUuid+"/migrate/source-forward", body)
	second := postJSON(t, handler, "/vms/"+testUuid+"/migrate/source-forward", body)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("got %d then %d, want 200 twice", first.Code, second.Code)
	}
	if seam.count() != 1 {
		t.Fatalf("a replay re-ran the phase: it ran %d times", seam.count())
	}
}

// The same identifier reused for a different phase is a caller bug, refused 409
// rather than answered with the first phase's result.
func TestMigratePhaseConflictOnReusedIdentifier(t *testing.T) {
	seam := &migrationSeam{}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-3"}
	postJSON(t, handler, "/vms/"+testUuid+"/migrate/export-source", body)
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/clone-target", body)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
}

// A phase this host does not run is refused at the boundary, before an operation
// is claimed, so a typo cannot burn an identifier.
func TestMigrateRefusesUnknownPhase(t *testing.T) {
	seam := &migrationSeam{}
	store := newFakeStore()
	handler := migrationServer(t, store, &fakeVirtualMachines{}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-4"}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/teleport", body)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 0 {
		t.Errorf("an unknown phase reached the host: %+v", seam.calls)
	}
	if _, found, _ := store.GetOperation("Task-mig-4"); found {
		t.Error("an unknown phase claimed an operation identifier")
	}
}

// A POST to the hydration PATH is an unknown phase: hydration is a GET, and the
// GET/POST split is what keeps the poll off the journal.
func TestMigratePostToHydrationPathIsUnknownPhase(t *testing.T) {
	seam := &migrationSeam{}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/hydration",
		wire.MigrateRequest{OperationId: "Task-mig-5"})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 0 {
		t.Errorf("a POST to the hydration path ran a phase: %+v", seam.calls)
	}
}

func TestMigrateRefusesMissingOperationIdentifier(t *testing.T) {
	seam := &migrationSeam{}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/export-source", wire.MigrateRequest{})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 0 {
		t.Errorf("a request with no identifier reached the host: %+v", seam.calls)
	}
}

func TestMigrateRefusesMalformedUUID(t *testing.T) {
	seam := &migrationSeam{}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	recorder := postJSON(t, handler, "/vms/not-a-uuid/migrate/export-source",
		wire.MigrateRequest{OperationId: "Task-mig-6"})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 0 {
		t.Errorf("a malformed uuid reached the host: %+v", seam.calls)
	}
}

// A migration phase runs even when this host holds no VM directory — the target
// side builds the VM, so unlike a lifecycle verb it must NOT 404 on an absent VM.
func TestMigratePhaseRunsWhenVMIsAbsent(t *testing.T) {
	seam := &migrationSeam{result: model.OperationResult{"root_clone_device": "/dev/mapper/atlas-vm-x-clone"}}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{missing: true}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-7", ImageName: ptr("bench-v16"), DiskGb: ptr(20)}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/clone-target", body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 on an absent VM: %s", recorder.Code, recorder.Body)
	}
	if seam.count() != 1 {
		t.Fatalf("the phase did not run on an absent VM: it ran %d times", seam.count())
	}
}

// A phase that fails on the host is recorded as a failure with its error, and
// carries no result — a failed operation must not hand a caller a value to act on.
func TestMigratePhaseFailureIsRecorded(t *testing.T) {
	seam := &migrationSeam{err: errTestPhase}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	body := wire.MigrateRequest{OperationId: "Task-mig-8"}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/migrate/collapse-clone", body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (the record is the answer): %s", recorder.Code, recorder.Body)
	}
	operation := decodeOperation(t, recorder)
	if operation.Status != wire.OperationStatusFailure {
		t.Errorf("operation status is %s, want Failure", operation.Status)
	}
	if operation.Error == nil || *operation.Error == "" {
		t.Errorf("a failed phase carried no error: %+v", operation)
	}
	if operation.Result != nil {
		t.Errorf("a failed phase carried a result: %+v", operation.Result)
	}
}

// Two phases for one VM never overlap: the per-UUID turn serializes them exactly
// as it does every other verb.
func TestMigratePhasesForOneVMAreSerialized(t *testing.T) {
	seam := &migrationSeam{hold: 20 * time.Millisecond}
	handler := migrationServer(t, newFakeStore(), &fakeVirtualMachines{}, seam).SocketHandler()

	var waiting sync.WaitGroup
	for _, identifier := range []string{"Task-mig-9a", "Task-mig-9b"} {
		waiting.Add(1)
		go func(id string) {
			defer waiting.Done()
			postJSON(t, handler, "/vms/"+testUuid+"/migrate/forward-up",
				wire.MigrateRequest{OperationId: id, Role: ptr(wire.MigrateRequestRoleSource)})
		}(identifier)
	}
	waiting.Wait()

	seam.mutex.Lock()
	defer seam.mutex.Unlock()
	if seam.overlapped {
		t.Error("two phases for one VM ran at once")
	}
	if len(seam.calls) != 2 {
		t.Fatalf("both phases should have run: %d did", len(seam.calls))
	}
}

// The Hydrating poll returns the reading and writes NOTHING to the journal: it is
// a plain read, so no operation record exists after it.
func TestHydrationReturnsReadingWithoutJournaling(t *testing.T) {
	store := newFakeStore()
	server := newTestServer(store, &fakeVirtualMachines{})
	server.pollHydration = func(_ context.Context, _ *run.Runner, uuid, cloneDevice string) (migration.PollHydrationResult, error) {
		if uuid != testUuid || cloneDevice != "" {
			t.Errorf("the poll saw uuid=%q clone=%q", uuid, cloneDevice)
		}
		return migration.PollHydrationResult{HydrationPercent: 73, SourceHealthy: true}, nil
	}
	handler := server.SocketHandler()

	recorder := get(t, handler, "/vms/"+testUuid+"/migrate/hydration")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var reading wire.MigrationHydration
	decode(t, recorder, &reading)
	if reading.HydrationPercent != 73 || !reading.SourceHealthy {
		t.Errorf("the reading did not reach the caller: %+v", reading)
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if len(store.operations) != 0 {
		t.Errorf("the poll journalled %d operations, want 0", len(store.operations))
	}
}

// The clone_device query parameter reaches the poll, so a local-base ship can
// reuse this percent keyed on its own clone.
func TestHydrationPassesCloneDeviceOverride(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	var seen string
	server.pollHydration = func(_ context.Context, _ *run.Runner, _ string, cloneDevice string) (migration.PollHydrationResult, error) {
		seen = cloneDevice
		return migration.PollHydrationResult{HydrationPercent: 100, SourceHealthy: true}, nil
	}
	handler := server.SocketHandler()

	recorder := get(t, handler, "/vms/"+testUuid+"/migrate/hydration?clone_device=atlas-base-bench-v16-clone")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if seen != "atlas-base-bench-v16-clone" {
		t.Errorf("the clone_device override did not reach the poll: %q", seen)
	}
}

func TestHydrationRefusesMalformedUUID(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	called := false
	server.pollHydration = func(context.Context, *run.Runner, string, string) (migration.PollHydrationResult, error) {
		called = true
		return migration.PollHydrationResult{}, nil
	}
	handler := server.SocketHandler()

	recorder := get(t, handler, "/vms/not-a-uuid/migrate/hydration")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if called {
		t.Error("a malformed uuid reached the poll")
	}
}

// migrationVerb names every phase migrate-<phase> and rejects anything else.
func TestMigrationVerbNamesEveryPhase(t *testing.T) {
	for _, phase := range []string{
		phaseExportSource, phaseExportBase, phaseCloneTarget, phaseReceiveBase,
		phaseInjectIdentity, phaseCollapseClone, phaseForwardUp, phaseSourceForward,
		phaseTargetReceive, phaseForwardDown, phaseWithdrawPrivate, phaseCleanupSource,
	} {
		verb, known := migrationVerb(phase)
		if !known || verb != "migrate-"+phase {
			t.Errorf("migrationVerb(%q) = %q, %v", phase, verb, known)
		}
	}
	if _, known := migrationVerb("hydration"); known {
		t.Error("hydration is not a mutating phase")
	}
	if _, known := migrationVerb(""); known {
		t.Error("the empty phase is not a phase")
	}
}

// The result mappers carry every field a caller acts on, and omit a zero that
// would read as a false measurement — a data disk that does not exist.
func TestExportSourceResultOmitsAbsentDataSize(t *testing.T) {
	withData := exportSourceResult(migration.ExportSourceResult{NBDPort: 10000, NBDPID: 9, RootSizeBytes: 42, DataSizeBytes: 7})
	if withData["data_size_bytes"] != int64(7) {
		t.Errorf("a real data disk was dropped: %+v", withData)
	}
	withoutData := exportSourceResult(migration.ExportSourceResult{NBDPort: 10000, NBDPID: 9, RootSizeBytes: 42})
	if _, present := withoutData["data_size_bytes"]; present {
		t.Errorf("an absent data disk was sent as zero: %+v", withoutData)
	}
	if withoutData["nbd_port"] != 10000 || withoutData["root_size_bytes"] != int64(42) {
		t.Errorf("a core field was dropped: %+v", withoutData)
	}
}

func TestExportBaseResultCarriesBothExports(t *testing.T) {
	values := exportBaseResult(migration.ExportBaseResult{
		NBDPort: 10002, NBDPID: 11, BaseSizeBytes: 100,
		MetaPort: 10003, MetaPID: 12, MetaSizeBytes: 200,
	})
	for key, want := range map[string]any{
		"nbd_port": 10002, "meta_port": 10003, "base_size_bytes": int64(100), "meta_size_bytes": int64(200),
	} {
		if values[key] != want {
			t.Errorf("%s = %v, want %v", key, values[key], want)
		}
	}
}

func TestCloneTargetResultOmitsAbsentDataClone(t *testing.T) {
	withData := cloneTargetResult(migration.CloneTargetResult{RootCloneDevice: "/dev/mapper/root", DataCloneDevice: "/dev/mapper/data"})
	if withData["data_clone_device"] != "/dev/mapper/data" {
		t.Errorf("a real data clone was dropped: %+v", withData)
	}
	withoutData := cloneTargetResult(migration.CloneTargetResult{RootCloneDevice: "/dev/mapper/root"})
	if _, present := withoutData["data_clone_device"]; present {
		t.Errorf("an absent data clone was sent: %+v", withoutData)
	}
}

var errTestPhase = &phaseError{}

type phaseError struct{}

func (*phaseError) Error() string { return "the phase failed on the host" }

// ptr is the generic pointer helper the optional wire fields need. The existing
// pointerTo in translate_test.go is int-only, so this covers the strings and
// enums a MigrateRequest carries.
func ptr[Value any](value Value) *Value { return &value }
