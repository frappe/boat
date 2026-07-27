package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/wire"
)

const testUuid = "1f8e0a2c-0000-4000-8000-000000000001"

func TestStartRecordsSuccessAndObservesTheHost(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{
		traceText: "+ systemctl start firecracker-vm@" + testUuid + ".service\n",
		observed:  model.VirtualMachine{ObservedStatus: model.StatusRunning, UnitActiveState: "active"},
	}
	handler := newTestServer(operations, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-1"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	operation := decodeOperation(t, recorder)
	if operation.Status != wire.OperationStatusSuccess {
		t.Errorf("got status %q, want Success", operation.Status)
	}
	if operation.Verb != verbStartVirtualMachine || operation.Uuid != testUuid {
		t.Errorf("operation names the wrong work: %+v", operation)
	}
	if operation.ExitCode == nil || *operation.ExitCode != 0 {
		t.Errorf("got exit code %v, want 0", operation.ExitCode)
	}
	if operation.Output == nil || !strings.Contains(*operation.Output, "systemctl start") {
		t.Errorf("the trace did not reach Output: %v", operation.Output)
	}
	if operations.virtualMachines[testUuid].ObservedStatus != model.StatusRunning {
		t.Errorf("the observed record was not persisted: %+v", operations.virtualMachines)
	}
}

// A retried Atlas Task carries the same name, and must not boot the VM twice.
func TestStartReplayReturnsTheFirstResultAndRunsNothing(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{traceText: "+ systemctl start\n"}
	handler := newTestServer(operations, machines).SocketHandler()
	body := wire.StartRequest{OperationId: "Task-2"}

	first := decodeOperation(t, postJSON(t, handler, "/vms/"+testUuid+"/start", body))
	second := postJSON(t, handler, "/vms/"+testUuid+"/start", body)

	if second.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", second.Code, second.Body)
	}
	if machines.starts != 1 {
		t.Errorf("the verb ran %d times, want 1", machines.starts)
	}
	replayed := decodeOperation(t, second)
	if replayed.Status != first.Status || *replayed.Output != *first.Output {
		t.Errorf("the replay differs from the first result: %+v vs %+v", replayed, first)
	}
}

func TestStartRefusesAnIdentifierAlreadyUsedForOtherWork(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(operations, machines).SocketHandler()
	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-3"})

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-3"})

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the conflict carried no sentence")
	}
	if machines.starts != 0 {
		t.Errorf("a conflicting claim ran the verb %d times", machines.starts)
	}
}

func TestFailingVerbIsRecordedWithItsTrace(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{
		traceText:  "+ systemctl start firecracker-vm@x.service\n",
		startError: errors.New("the unit did not become active"),
	}
	handler := newTestServer(operations, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-4"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	operation := decodeOperation(t, recorder)
	if operation.Status != wire.OperationStatusFailure {
		t.Errorf("got status %q, want Failure", operation.Status)
	}
	if operation.ExitCode == nil || *operation.ExitCode != 1 {
		t.Errorf("got exit code %v, want 1", operation.ExitCode)
	}
	if operation.Output == nil || !strings.Contains(*operation.Output, "systemctl start") {
		t.Errorf("the trace did not reach Output: %v", operation.Output)
	}
	if operation.Error == nil || *operation.Error == "" {
		t.Error("a failure carried no error")
	}
	if operations.operations["Task-4"].Status != model.OperationFailure {
		t.Error("the journal was not told the operation failed")
	}
	if machines.observeError == nil && operations.virtualMachines[testUuid].UUID != "" {
		t.Error("a failed verb must not write an observation it never made")
	}
}

// The exit code a Task carried is the command's, so a non-zero exit has to
// survive the trip through the journal rather than flatten to 1.
func TestExitCodeComesFromTheCommandThatFailed(t *testing.T) {
	commandError := &run.CommandError{Argv: []string{"systemctl", "start"}, ExitCode: 5, Output: "job failed"}
	if got := exitCodeOf(commandError); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
	if got := exitCodeOf(errors.New("never reached a command")); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

// A failed verb must still leave a terminal record: the response is never
// written ahead of the journal.
func TestUnknownVirtualMachineIsRefusedAndStillJournalled(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{missing: true}
	handler := newTestServer(operations, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-5"})

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", recorder.Code, recorder.Body)
	}
	if machines.starts != 0 {
		t.Error("a missing VM was started anyway")
	}
	recorded := operations.operations["Task-5"]
	if recorded.Status != model.OperationFailure || recorded.Error == "" {
		t.Errorf("the refusal left no terminal record: %+v", recorded)
	}
}

func TestStopAppliesTheContractsDefaults(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(operations, machines).SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-6"})

	if len(machines.stopRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.stopRequests))
	}
	if machines.stopRequests[0].Forced {
		t.Error("an unset graceful must mean a cooperative stop")
	}
	if machines.stopRequests[0].TimeoutSeconds != 0 {
		t.Error("an unset timeout must leave systemd's drain alone")
	}
}

func TestStopCarriesGracefulAndTimeoutThrough(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(operations, machines).SocketHandler()
	graceful := false
	timeout := 45

	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{
		OperationId:        "Task-7",
		Graceful:           &graceful,
		StopTimeoutSeconds: &timeout,
	})

	if len(machines.stopRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.stopRequests))
	}
	if !machines.stopRequests[0].Forced || machines.stopRequests[0].TimeoutSeconds != 45 {
		t.Errorf("the request was not carried through: %+v", machines.stopRequests[0])
	}
}

func TestStartWithoutAnOperationIdentifierIsRefused(t *testing.T) {
	operations := newFakeOperationStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(operations, machines).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if machines.starts != 0 {
		t.Error("an unreplayable request ran the verb")
	}
}

func TestMalformedBodyIsRefusedInTheContractsShape(t *testing.T) {
	handler := newTestServer(newFakeOperationStore(), &fakeVirtualMachines{}).SocketHandler()

	recorder := postBody(handler, "/vms/"+testUuid+"/start", "{not json")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the refusal carried no sentence")
	}
}

// An unjournalled outcome is not an outcome: if the record cannot be written,
// the caller must not be told the operation succeeded.
func TestAnUnwritableJournalIsAnInternalFault(t *testing.T) {
	operations := newFakeOperationStore()
	operations.completeError = errors.New("/var/lib/boat/boat.db: no space left on device")
	handler := newTestServer(operations, &fakeVirtualMachines{}).SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-8"})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(decodeError(t, recorder).Error, "boat.db") {
		t.Error("the boundary leaked a host path to the caller")
	}
}
