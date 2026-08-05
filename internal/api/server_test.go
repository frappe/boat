package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/version"
	"github.com/frappe/boat/internal/wire"
)

func TestHealthSaysOnlyThatBoatIsUp(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	handler := server.SocketHandler()

	recorder := get(t, handler, "/health")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var health wire.Health
	decode(t, recorder, &health)
	if health.Status != wire.HealthStatusOk || health.BoatVersion != version.Version {
		t.Errorf("got %+v, want ok and %q", health, version.Version)
	}
}

func TestHostReportsTheDaemonsStartAndItsVirtualMachineCount(t *testing.T) {
	operations := newFakeStore()
	operations.virtualMachines["one"] = model.VirtualMachine{UUID: "one"}
	operations.virtualMachines["two"] = model.VirtualMachine{UUID: "two"}
	server := newTestServer(operations, &fakeVirtualMachines{})

	recorder := get(t, server.SocketHandler(), "/host")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var host wire.Host
	decode(t, recorder, &host)
	if host.VirtualMachineCount != 2 {
		t.Errorf("got %d virtual machines, want 2", host.VirtualMachineCount)
	}
	if !host.StartedAt.Equal(server.startedAt) {
		t.Errorf("got started_at %s, want %s", host.StartedAt, server.startedAt)
	}
	if host.Hostname == "" || host.BoatVersion != version.Version {
		t.Errorf("got %+v, want a hostname and version %q", host, version.Version)
	}
}

func TestVirtualMachineListAndDocumentComeFromTheStore(t *testing.T) {
	operations := newFakeStore()
	operations.virtualMachines[testUuid] = model.VirtualMachine{
		UUID:            testUuid,
		ObservedStatus:  model.StatusSleeping,
		ObservedAt:      time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
		UnitActiveState: "inactive",
		Sleeping:        true,
	}
	server := newTestServer(operations, &fakeVirtualMachines{})
	handler := server.SocketHandler()

	var listed []wire.VirtualMachine
	decode(t, get(t, handler, "/vms"), &listed)
	if len(listed) != 1 || listed[0].Uuid != testUuid {
		t.Fatalf("got %+v, want the one record the store holds", listed)
	}

	var document wire.VirtualMachine
	decode(t, get(t, handler, "/vms/"+testUuid), &document)
	if document.ObservedStatus != wire.VirtualMachineStatusSleeping {
		t.Errorf("got status %q, want Sleeping", document.ObservedStatus)
	}
	if document.Sleeping == nil || !*document.Sleeping {
		t.Error("the sleeping marker did not survive the translation")
	}
}

func TestUnknownVirtualMachineAndOperationAreNotFound(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	handler := server.SocketHandler()

	for _, path := range []string{"/vms/" + testUuid, "/ops/Task-404"} {
		recorder := get(t, handler, path)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404: %s", path, recorder.Code, recorder.Body)
		}
		if decodeError(t, recorder).Error == "" {
			t.Errorf("%s: the refusal carried no sentence", path)
		}
	}
}

// The journal outlives the request that wrote it: that is what makes it
// crash-recovery truth rather than a response cache.
func TestOperationRecordIsReadableAfterTheRun(t *testing.T) {
	operations := newFakeStore()
	server := newTestServer(operations, &fakeVirtualMachines{traceText: "+ systemctl stop\n"})
	handler := server.SocketHandler()
	postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-9"})

	recorder := get(t, handler, "/ops/Task-9")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	operation := recordOf(t, server, handler, "Task-9")
	if operation.Verb != verbStopVirtualMachine || operation.Status != wire.OperationStatusSuccess {
		t.Errorf("got %+v, want a successful stop-vm", operation)
	}
	if operation.EndedAt == nil {
		t.Error("a terminal record must carry an end time")
	}
}

func TestARunningOperationCarriesNoEndTimeOrExitCode(t *testing.T) {
	running := model.Operation{
		Identifier: "Task-10",
		Verb:       verbStartVirtualMachine,
		Status:     model.OperationRunning,
		StartedAt:  time.Now().UTC(),
	}

	document := operationToWire(running)

	if document.EndedAt != nil || document.ExitCode != nil {
		t.Errorf("got %+v, want no end time and no exit code", document)
	}
}
