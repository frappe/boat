package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

func desiredDocument() wire.DesiredVirtualMachine {
	return wire.DesiredVirtualMachine{Uuid: testUuid, BootEpoch: 1, DesiredPower: wire.DesiredPowerRunning}
}

// Atlas re-asserts intent before every verb and on every reconnect, and none of
// those are decisions taken from the mirror. A precondition there would make
// resynchronisation fail exactly when the mirror is furthest behind.
func TestAPutWithoutAPreconditionIsNotGated(t *testing.T) {
	state := newFakeStore()
	state.epoch = 11
	state.movedAt[testUuid] = 11
	server := newTestServer(state, &fakeVirtualMachines{})

	recorder := putJSON(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument())

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

func TestAPutMatchingTheEpochItDecidedFromIsAccepted(t *testing.T) {
	state := newFakeStore()
	state.epoch = 11
	state.movedAt[testUuid] = 7
	server := newTestServer(state, &fakeVirtualMachines{})

	recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), "11")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

// The refusal §11.2 exists for. Without it, "the mirror is disposable because no
// contended decision is ever taken from it" is true only because nothing has yet
// been taught to take one.
func TestAPutWhoseVirtualMachineMovedIsRefusedWithItsReason(t *testing.T) {
	state := newFakeStore()
	state.epoch = 11
	state.movedAt[testUuid] = 9
	server := newTestServer(state, &fakeVirtualMachines{})

	recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), "8")

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	failure := decodeError(t, recorder)
	if failure.Reason == nil || *failure.Reason != wire.ErrorReasonStaleObservation {
		t.Errorf("got reason %v, want stale-observation", failure.Reason)
	}
	if state.desiredWrites != 0 {
		t.Errorf("the desired state was written anyway (%d writes)", state.desiredWrites)
	}
}

// Both 409s live on this one operation now, and they call for opposite
// behaviour: a fence regression must never be retried, and a stale observation
// must be retried against a fresh export. A caller told only "409" would get one
// of the two wrong.
func TestTheTwoConflictsOnAPutAreToldApartByTheirReason(t *testing.T) {
	state := newFakeStore()
	state.fence(testUuid, 4)
	server := newTestServer(state, &fakeVirtualMachines{})
	regressed := desiredDocument()
	regressed.BootEpoch = 2

	recorder := putJSON(t, server.SocketHandler(), "/vms/"+testUuid, regressed)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	failure := decodeError(t, recorder)
	if failure.Reason == nil || *failure.Reason != wire.ErrorReasonFenceRegression {
		t.Errorf("got reason %v, want fence-regression", failure.Reason)
	}
}

// A caller quoting an epoch this host never issued read it from a different
// store — a Boat whose bbolt file was lost and whose epoch restarted from zero.
// That is the state in which a mirror is most confidently wrong.
func TestAnEpochFromAnotherStoreIsRefused(t *testing.T) {
	state := newFakeStore()
	state.epoch = 3
	server := newTestServer(state, &fakeVirtualMachines{})

	recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), "900")

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
}

// The precondition is a number, optionally in the quotes an HTTP entity tag
// carries. Anything else is refused rather than read as "no precondition" —
// including `*`, which in HTTP means "any current representation" and would be a
// way to look like a CAS without being one.
func TestOnlyAnObservedEpochIsAPrecondition(t *testing.T) {
	state := newFakeStore()
	state.epoch = 11
	server := newTestServer(state, &fakeVirtualMachines{})

	for _, accepted := range []string{"11", `"11"`, `W/"11"`, " 11 "} {
		recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), accepted)
		if recorder.Code != http.StatusOK {
			t.Errorf("If-Match %q got %d, want 200: %s", accepted, recorder.Code, recorder.Body)
		}
	}
	for _, refused := range []string{"*", "latest", "11.5", "-1", `"11", "12"`} {
		recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), refused)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("If-Match %q got %d, want 400: %s", refused, recorder.Code, recorder.Body)
		}
	}
}

// The CAS is scoped to the VM the request names, and this is the property that
// makes it usable at all: the reconciler writes an observation per VM per sweep,
// so a whole-host comparison against a five-minute-old mirror would refuse every
// write on a busy host — and a precondition that always fails gets removed.
func TestAnotherVirtualMachineMovingDoesNotRefuseThisOne(t *testing.T) {
	state := newFakeStore()
	state.epoch = 400
	state.movedAt["another-vm"] = 400
	state.movedAt[testUuid] = 6
	server := newTestServer(state, &fakeVirtualMachines{})

	recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(), "7")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

// The store's own record is the token's source, so a CAS against the epoch an
// export carried is matched against the same scale the export was taken on.
func TestTheEpochAnExportCarriesIsThePreconditionThatHolds(t *testing.T) {
	state := newFakeStore()
	state.epoch = 11
	state.virtualMachines[testUuid] = model.VirtualMachine{UUID: testUuid, ObservedEpoch: 11}
	state.movedAt[testUuid] = 11
	server := newTestServer(state, &fakeVirtualMachines{})

	var export wire.Export
	decode(t, get(t, server.SocketHandler(), "/export"), &export)
	recorder := putJSONMatching(t, server.SocketHandler(), "/vms/"+testUuid, desiredDocument(),
		strconv.FormatInt(export.ObservedEpoch, 10))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}
