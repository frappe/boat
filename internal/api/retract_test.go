// Retraction: the one thing that takes an assertion back, and the verb that
// does it for a VM it has just destroyed.
//
// The defect these cover is not hypothetical and needs no mistake by Atlas: a
// terminate used to leave `{epoch, Running}` behind, so the next sweep saw
// Stopped against Running, passed the fence — still held — and ran `systemctl
// start` on a VM whose root volume had just been lvremoved. Every sweep
// interval, forever.

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

// errStoreUnavailable is the store failing to write, which is the one way a
// retraction can fail at all.
var errStoreUnavailable = errors.New("/var/lib/boat/boat.db: input/output error")

func deleteVirtualMachine(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, nil))
	return recorder
}

// asserted is Atlas stating what it wants, fence and all: the two writes a PUT
// makes, which is the state every scenario below starts from.
func asserted(state *fakeStore, power model.DesiredPower, epoch int64) {
	state.fence(testUuid, epoch)
	state.desire(model.DesiredVirtualMachine{UUID: testUuid, BootEpoch: epoch, DesiredPower: power})
}

func TestDeleteRetractsTheDesiredStateAndTouchesNothingOnTheHost(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	asserted(state, model.PowerRunning, 4)
	server := newTestServer(state, machines)
	handler := server.SocketHandler()

	recorder := deleteVirtualMachine(handler, "/vms/"+testUuid)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", recorder.Code, recorder.Body)
	}
	if _, found := state.desired[testUuid]; found {
		t.Error("the assertion survived its own retraction")
	}
	// A retraction is not a terminate. The VM running here goes on running; what
	// ends is this host's authority to drive it.
	if machines.stops != 0 || machines.terminates != 0 || machines.starts != 0 {
		t.Errorf("a retraction reached the host: %d stops, %d terminates, %d starts",
			machines.stops, machines.terminates, machines.starts)
	}
}

// The half that looks like an oversight and is not. Clearing the epoch with the
// record would leave the host holding NO epoch, which is the state a fresh PUT
// of any epoch satisfies — including one a partitioned Atlas has already seen
// superseded. A source host that retracted after a migration would then accept
// the old claim and boot a VM that lives somewhere else now.
func TestDeleteKeepsTheFenceEpoch(t *testing.T) {
	state := newFakeStore()
	asserted(state, model.PowerRunning, 9)
	server := newTestServer(state, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	deleteVirtualMachine(handler, "/vms/"+testUuid)

	if epoch, held := state.fences[testUuid]; !held || epoch != 9 {
		t.Errorf("got fence epoch %d held=%v, want the 9 it already held", epoch, held)
	}
}

// A retraction whose reply was lost is repeated, and answering 404 to the repeat
// would tell Atlas this host still holds intent it does not hold.
func TestDeleteIsIdempotent(t *testing.T) {
	state := newFakeStore()
	server := newTestServer(state, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	first := deleteVirtualMachine(handler, "/vms/"+testUuid)
	second := deleteVirtualMachine(handler, "/vms/"+testUuid)

	if first.Code != http.StatusNoContent || second.Code != http.StatusNoContent {
		t.Fatalf("got %d then %d, want 204 both times", first.Code, second.Code)
	}
}

func TestDeleteReportsAStoreItCouldNotWrite(t *testing.T) {
	state := newFakeStore()
	asserted(state, model.PowerRunning, 1)
	state.writeError = errStoreUnavailable
	server := newTestServer(state, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	recorder := deleteVirtualMachine(handler, "/vms/"+testUuid)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
	if _, found := state.desired[testUuid]; !found {
		t.Error("the assertion was dropped by a write that failed")
	}
}

// The verb has not finished until the assertion that would resurrect the VM is
// gone. Nothing else in the system retracts it: Atlas is not asked to make a
// second call, because a crash between the two would leave exactly the state
// this closes.
func TestTerminateRetractsTheDesiredStateThatWouldRestartTheVirtualMachine(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	asserted(state, model.PowerRunning, 2)
	server := newTestServer(state, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/terminate",
		wire.OperationRequest{OperationId: "Task-terminate-1"})
	awaitOperation(t, server)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if machines.terminates != 1 {
		t.Errorf("the host was terminated %d times, want once", machines.terminates)
	}
	if _, found := state.desired[testUuid]; found {
		t.Error("a terminated VM is still desired Running, so the next sweep starts it")
	}
	// The epoch stays, for the reason TestDeleteKeepsTheFenceEpoch gives: a
	// terminate is not permission to boot this UUID under an older claim.
	if epoch := state.fences[testUuid]; epoch != 2 {
		t.Errorf("got fence epoch %d, want the 2 it already held", epoch)
	}
}

// BEFORE the mechanics, not after. Desired state is stale from the instant Atlas
// asks for a terminate, and a crash between the lvremove and the retraction is
// the worst moment for a sweep to find Stopped against Running: the VM it would
// start no longer has a root volume.
func TestTerminateRetractsBeforeItReachesTheHost(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	asserted(state, model.PowerRunning, 1)
	stillDesired := true
	machines.beforeTerminate = func() {
		_, stillDesired, _ = state.GetDesired(testUuid)
	}
	server := newTestServer(state, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/terminate", wire.OperationRequest{OperationId: "Task-terminate-2"})
	awaitOperation(t, server)

	if stillDesired {
		t.Error("the host was torn down while the assertion that resurrects it was still stored")
	}
}

// A retraction that could not be written fails the verb before anything is
// destroyed. The alternative is a VM whose disks are gone and whose desired
// state says Running, which is the state that starts a volume-less guest every
// thirty seconds.
func TestTerminateThatCannotRetractDestroysNothing(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	asserted(state, model.PowerRunning, 1)
	state.writeError = errStoreUnavailable
	server := newTestServer(state, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/terminate",
		wire.OperationRequest{OperationId: "Task-terminate-3"})
	awaitOperation(t, server)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want the operation record: %s", recorder.Code, recorder.Body)
	}
	var operation wire.Operation
	decode(t, recorder, &operation)
	if operation.Status != wire.OperationStatusFailure {
		t.Errorf("got %s, want a failure the operator can retry", operation.Status)
	}
	if machines.terminates != 0 {
		t.Error("the VM was destroyed although its desired state could not be retracted")
	}
}
