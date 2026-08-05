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
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{
		traceText: "+ systemctl start firecracker-vm@" + testUuid + ".service\n",
		observed:  model.VirtualMachine{ObservedStatus: model.StatusRunning, UnitActiveState: "active"},
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-1"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	// The POST answers with the claim; the outcome is the journal record, read
	// here as the client reads it.
	operation := recordOf(t, server, handler, "Task-1")
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

// The {uuid} path parameter is a bare string in the IDL and the generated binder
// checks no pattern, so the boundary does. A name that is not the 8-4-4-4-12 hex
// shape is refused before any host command, because downstream it becomes a path
// segment spliced into sudo'd commands and an nft identifier — the first line of
// defence the sudoers allow-list is the second half of (sudoers.d/boat).
func TestAMalformedUUIDIsRefusedAtTheBoundary(t *testing.T) {
	machines := &fakeVirtualMachines{}
	server := newTestServer(newFakeStore(), machines)
	handler := server.SocketHandler()
	const bad = "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"

	// A verb reaches perform's check before it claims an operation or touches the
	// host: stop goes straight to perform, so the 400 is the boundary's own.
	stop := postJSON(t, handler, "/vms/"+bad+"/stop", wire.StopRequest{OperationId: "Task-1"})
	if stop.Code != http.StatusBadRequest {
		t.Errorf("stop with a malformed uuid: got %d, want 400: %s", stop.Code, stop.Body)
	}
	if machines.stops != 0 {
		t.Errorf("a malformed uuid reached the host: %d stops", machines.stops)
	}

	// A read is refused the same way, so no endpoint takes a name it would render.
	if got := get(t, handler, "/vms/"+bad); got.Code != http.StatusBadRequest {
		t.Errorf("get with a malformed uuid: got %d, want 400: %s", got.Code, got.Body)
	}
}

// A retried Atlas Task carries the same name, and must not boot the VM twice.
func TestStartReplayReturnsTheFirstResultAndRunsNothing(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{traceText: "+ systemctl start\n"}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	body := wire.StartRequest{OperationId: "Task-2"}

	postJSON(t, handler, "/vms/"+testUuid+"/start", body)
	first := recordOf(t, server, handler, "Task-2")
	second := postJSON(t, handler, "/vms/"+testUuid+"/start", body)

	if second.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", second.Code, second.Body)
	}
	if machines.starts != 1 {
		t.Errorf("the verb ran %d times, want 1", machines.starts)
	}
	// A replay is answered from the record rather than re-claimed, so the second
	// POST carries the finished operation where a first one carries the claim.
	replayed := decodeOperation(t, second)
	if replayed.Status != first.Status || *replayed.Output != *first.Output {
		t.Errorf("the replay differs from the first result: %+v vs %+v", replayed, first)
	}
}

func TestStartRefusesAnIdentifierAlreadyUsedForOtherWork(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
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
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{
		traceText:  "+ systemctl start firecracker-vm@x.service\n",
		startError: errors.New("the unit did not become active"),
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-4"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	operation := recordOf(t, server, handler, "Task-4")
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
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{missing: true}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-5"})
	awaitOperation(t, server)

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
	operations := newFakeStore()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-6"})
	awaitOperation(t, server)

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
	operations := newFakeStore()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	graceful := false
	timeout := 45

	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{
		OperationId:        "Task-7",
		Graceful:           &graceful,
		StopTimeoutSeconds: &timeout,
	})
	awaitOperation(t, server)

	if len(machines.stopRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.stopRequests))
	}
	if !machines.stopRequests[0].Forced || machines.stopRequests[0].TimeoutSeconds != 45 {
		t.Errorf("the request was not carried through: %+v", machines.stopRequests[0])
	}
}

func TestStartWithoutAnOperationIdentifierIsRefused(t *testing.T) {
	operations := newFakeStore()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if machines.starts != 0 {
		t.Error("an unreplayable request ran the verb")
	}
}

func TestMalformedBodyIsRefusedInTheContractsShape(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	handler := server.SocketHandler()

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
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	operations.completeError = errors.New("/var/lib/boat/boat.db: no space left on device")
	server := newTestServer(operations, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-8"})
	awaitOperation(t, server)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(decodeError(t, recorder).Error, "boat.db") {
		t.Error("the boundary leaked a host path to the caller")
	}
}

// TestAPollingCallerIsAnsweredWithTheClaimAndReadsTheOutcomeFromTheRecord is the
// shape Atlas uses: a verb that takes half an hour must not hold a connection
// for half an hour, and a connection dropped mid-verb must not lose the outcome.
func TestAPollingCallerIsAnsweredWithTheClaimAndReadsTheOutcomeFromTheRecord(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	machines := &fakeVirtualMachines{traceText: "+ systemctl start\n", beforeStart: func() {
		close(started)
		<-release
	}}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postAsync(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-async"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	// Answered while the verb is still running — that is the whole point.
	<-started
	claimed := decodeOperation(t, recorder)
	if claimed.Status != wire.OperationStatusRunning {
		t.Errorf("the claim came back %q, want Running", claimed.Status)
	}
	if polled := decodeOperation(t, get(t, handler, "/ops/Task-async")); polled.Status != wire.OperationStatusRunning {
		t.Errorf("a poll mid-verb reported %q, want Running", polled.Status)
	}

	close(release)
	finished := recordOf(t, server, handler, "Task-async")

	if finished.Status != wire.OperationStatusSuccess {
		t.Errorf("the finished record says %q, want Success", finished.Status)
	}
	if finished.Output == nil || !strings.Contains(*finished.Output, "systemctl start") {
		t.Errorf("the trace did not reach the record: %v", finished.Output)
	}
}

// A waiting caller gets the outcome in the response, because the operator's
// break-glass verb has nothing to poll with.
func TestAWaitingCallerIsAnsweredWithTheOutcome(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	server := newTestServer(operations, &fakeVirtualMachines{traceText: "+ systemctl start\n"})
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-wait"})

	if operation := decodeOperation(t, recorder); operation.Status != wire.OperationStatusSuccess {
		t.Errorf("got %q, want Success in the response itself", operation.Status)
	}
}
