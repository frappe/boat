package api

import (
	"net/http"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

func desiredBody(epoch int64, power wire.DesiredPower) wire.DesiredVirtualMachine {
	vcpus := 2
	memory := 4096
	return wire.DesiredVirtualMachine{
		Uuid:            testUuid,
		BootEpoch:       epoch,
		DesiredPower:    power,
		Vcpus:           &vcpus,
		MemoryMegabytes: &memory,
	}
}

func TestPutStoresTheDesiredStateAndTheFenceEpoch(t *testing.T) {
	state := newFakeStore()
	machines := &fakeVirtualMachines{}
	handler := newTestServer(state, machines).SocketHandler()

	recorder := putJSON(t, handler, "/vms/"+testUuid, desiredBody(3, wire.DesiredPowerRunning))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var stored wire.DesiredVirtualMachine
	decode(t, recorder, &stored)
	if stored.Uuid != testUuid || stored.BootEpoch != 3 || stored.DesiredPower != wire.DesiredPowerRunning {
		t.Errorf("got %+v, want the assertion as stored", stored)
	}
	if state.desired[testUuid].MemoryMegabytes != 4096 || state.desired[testUuid].VCPUs != 2 {
		t.Errorf("the numbers did not reach the store: %+v", state.desired[testUuid])
	}
	if state.fences[testUuid] != 3 {
		t.Errorf("got fence epoch %d, want 3", state.fences[testUuid])
	}
	// WO-1 records intent and nothing else. The reconciler that acts on it is
	// WO-2's, and a PUT that started a VM would run work no journal records.
	if machines.starts != 0 || machines.stops != 0 {
		t.Errorf("a PUT ran %d starts and %d stops, want none", machines.starts, machines.stops)
	}
}

// An epoch never goes backwards. Answering 200 to one that did would tell the
// loser of a migration that its claim on the VM is current.
func TestPutRefusesAnEpochThatWentBackwards(t *testing.T) {
	state := newFakeStore()
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()
	putJSON(t, handler, "/vms/"+testUuid, desiredBody(7, wire.DesiredPowerRunning))

	recorder := putJSON(t, handler, "/vms/"+testUuid, desiredBody(6, wire.DesiredPowerStopped))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the refusal carried no sentence")
	}
	if state.fences[testUuid] != 7 {
		t.Errorf("got fence epoch %d, want the 7 it already held", state.fences[testUuid])
	}
	if state.desired[testUuid].DesiredPower != model.PowerRunning {
		t.Errorf("a refused assertion still changed the desired state: %+v", state.desired[testUuid])
	}
}

// A PUT is how intent is re-asserted after a partition, so the same document
// arriving twice has to be free: no write, no epoch move, nothing for a
// reconciler to notice.
func TestARepeatedPutChangesNothing(t *testing.T) {
	state := newFakeStore()
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()
	body := desiredBody(5, wire.DesiredPowerRunning)
	first := putJSON(t, handler, "/vms/"+testUuid, body)
	writes, fences := state.desiredWrites, state.fenceWrites

	second := putJSON(t, handler, "/vms/"+testUuid, body)

	if second.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", second.Code, second.Body)
	}
	if state.desiredWrites != writes || state.fenceWrites != fences {
		t.Errorf("a re-assertion wrote %d desired and %d fences, want %d and %d",
			state.desiredWrites, state.fenceWrites, writes, fences)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("a re-assertion answered %s, want the first answer %s", second.Body, first.Body)
	}
}

// A host that kept the desired record but lost its fence has something to do:
// treating that as a no-op would leave a VM Atlas believes is fenced unable to
// boot until someone noticed.
func TestARepeatedPutRestoresAFenceTheHostLost(t *testing.T) {
	state := newFakeStore()
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()
	body := desiredBody(5, wire.DesiredPowerRunning)
	putJSON(t, handler, "/vms/"+testUuid, body)
	delete(state.fences, testUuid)

	putJSON(t, handler, "/vms/"+testUuid, body)

	if state.fences[testUuid] != 5 {
		t.Errorf("got fence epoch %d, want the 5 that was re-asserted", state.fences[testUuid])
	}
}

func TestPutRefusesABodyThatNamesAnotherVirtualMachine(t *testing.T) {
	state := newFakeStore()
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()
	body := desiredBody(1, wire.DesiredPowerRunning)
	body.Uuid = "1f8e0a2c-0000-4000-8000-00000000ffff"

	recorder := putJSON(t, handler, "/vms/"+testUuid, body)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if len(state.desired) != 0 || len(state.fences) != 0 {
		t.Error("a contradictory assertion was stored anyway")
	}
}

func TestPutRefusesAPowerTheReconcilerCannotRead(t *testing.T) {
	state := newFakeStore()
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()

	recorder := putJSON(t, handler, "/vms/"+testUuid, desiredBody(1, wire.DesiredPower("")))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if len(state.fences) != 0 {
		t.Error("an unreadable assertion still moved the fence")
	}
}
