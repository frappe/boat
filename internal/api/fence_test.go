package api

import (
	"net/http"
	"testing"

	"github.com/frappe/boat/internal/wire"
)

// The acceptance criterion of the whole fence: an empty fence store boots
// nothing. A Boat that lost its bbolt file must wait for Atlas to re-assert,
// because the VM whose artifacts are still on this disk may already be running
// on the host it was migrated to.
func TestStartIsRefusedWhenThisHostHoldsNoFence(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(state, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-20"})

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the refusal carried no sentence")
	}
	if machines.starts != 0 {
		t.Errorf("an unfenced virtual machine was started %d times", machines.starts)
	}
	// The refusal must stay retryable under the same Atlas Task name: the retry
	// that matters is the one right after Atlas asserts the epoch.
	if _, claimed := state.operations["Task-20"]; claimed {
		t.Error("the refusal burnt the operation identifier the retry needs")
	}
}

func TestStartIsPermittedOnceAFenceIsHeld(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(state, machines).SocketHandler()
	putJSON(t, handler, "/vms/"+testUuid, desiredBody(2, wire.DesiredPowerRunning))

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-21"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if machines.starts != 1 {
		t.Errorf("the verb ran %d times, want 1", machines.starts)
	}
}

// A stop is never fenced. Refusing to stop a VM this host is running is how two
// live copies of one VM stay alive.
func TestStopIsNotFenced(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(state, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-22"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if machines.stops != 1 {
		t.Errorf("the verb ran %d times, want 1", machines.stops)
	}
}
