// The verb result: the few values a CALLER acts on, as against the trace an
// operator reads.
//
// Without it, Atlas's sleep raised AFTER the verb had already succeeded — it
// parses a typed result out of the Task's stdout, and a Boat operation carried
// no such line — so the VM parked, the Task committed Success, the row stayed
// Running, and the idle sweeper re-slept the same VM once a minute forever while
// nothing ever booked the freed RAM.

package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// resultOf reads the result off the operation a verb answered with, and fails
// the test when there is none — the field is optional on the wire, so a nil here
// is the defect these tests are about rather than a missing assertion.
func resultOf(t *testing.T, operation wire.Operation) map[string]any {
	t.Helper()
	if operation.Result == nil {
		t.Fatalf("the operation carried no result: %+v", operation)
	}
	return *operation.Result
}

func TestSleepReportsWhetherTheGuestsMemoryWasCaptured(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{
		sleepResult: vm.SleepResult{MemorySnapshot: true, MemorySnapshotBytes: 536870912},
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/sleep", wire.OperationRequest{OperationId: "Task-sleep-1"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	result := resultOf(t, recordOf(t, server, handler, "Task-sleep-1"))
	if result["memory_snapshot"] != true {
		t.Errorf("memory_snapshot = %v, want true", result["memory_snapshot"])
	}
	// JSON has one number type, so the size comes back as a float. What matters is
	// that the value survived, not which Go type carried it.
	if size, ok := result["memory_snapshot_bytes"].(float64); !ok || int64(size) != 536870912 {
		t.Errorf("memory_snapshot_bytes = %v, want the 512 MiB the host freed", result["memory_snapshot_bytes"])
	}
	// An empty reason is not a reason. A key that is present and empty invites a
	// reader to display it.
	if _, present := result["reason"]; present {
		t.Errorf("a snapshot that succeeded carried a reason: %v", result)
	}
}

// The fallback is a successful sleep with a slower wake, and the reason is the
// only record of WHY — a host fault an operator can fix. It travels with the
// result rather than staying in the daemon's log, because the log is on the host
// and the operator is in Atlas, reading the Task.
func TestSleepReportsWhyTheNextWakeWillBeAColdBoot(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{
		sleepResult: vm.SleepResult{Reason: "not enough free space for a 1024 MiB memory file"},
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/sleep", wire.OperationRequest{OperationId: "Task-sleep-2"})

	result := resultOf(t, recordOf(t, server, handler, "Task-sleep-2"))
	if result["memory_snapshot"] != false {
		t.Errorf("memory_snapshot = %v, want false", result["memory_snapshot"])
	}
	if reason, _ := result["reason"].(string); reason == "" {
		t.Errorf("the plain sleep did not say why: %v", result)
	}
	// Nothing was freed by a snapshot, so there is no size to state.
	if _, present := result["memory_snapshot_bytes"]; present {
		t.Errorf("a sleep with no snapshot reported a size: %v", result)
	}
}

// Absent is not false. Eight of the nine verbs report nothing but their trace,
// and a caller that finds no result must not read it as a result saying no.
func TestAVerbWithNothingToReportCarriesNoResult(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	server := newTestServer(operations, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-start-1"})

	if operation := decodeOperation(t, recorder); operation.Result != nil {
		t.Errorf("a start reported %v, want no result at all", *operation.Result)
	}
}

// A verb that failed may still have computed half a result. Recording it would
// hand a caller a value to act on out of an operation that did not finish.
func TestAFailedVerbCarriesNoResult(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{
		sleepResult: vm.SleepResult{MemorySnapshot: true},
		verbError:   errors.New("the virtual machine is stopped but could not be parked for wake"),
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/sleep", wire.OperationRequest{OperationId: "Task-sleep-3"})

	operation := recordOf(t, server, handler, "Task-sleep-3")
	if operation.Status != wire.OperationStatusFailure {
		t.Fatalf("got status %q, want Failure", operation.Status)
	}
	if operation.Result != nil {
		t.Errorf("a failed sleep reported %v, want no result", *operation.Result)
	}
}

// The result is journalled with the rest of the record, so the answer a replayed
// Task reads is the same one the first attempt returned — that is the whole
// point of the journal, and a result that lived only in the response would be
// lost to the retry that most needs it.
func TestAReplayedOperationReturnsTheResultTheFirstAttemptRecorded(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{
		sleepResult: vm.SleepResult{MemorySnapshot: true, MemorySnapshotBytes: 4096},
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	body := wire.OperationRequest{OperationId: "Task-sleep-4"}

	postJSON(t, handler, "/vms/"+testUuid+"/sleep", body)
	awaitOperation(t, server)
	replay := postJSON(t, handler, "/vms/"+testUuid+"/sleep", body)

	if len(machines.sleepRequests) != 1 {
		t.Fatalf("the verb ran %d times, want once", len(machines.sleepRequests))
	}
	result := resultOf(t, decodeOperation(t, replay))
	if result["memory_snapshot"] != true {
		t.Errorf("the replay answered %v, want the first attempt's result", result)
	}
}
