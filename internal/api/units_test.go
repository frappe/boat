package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

func poolUp() model.UnitLiveness {
	return model.UnitLiveness{Name: "atlas-pool.service", ActiveState: "active", SubState: "exited"}
}

func TestGetUnitReportsWhatSystemdSays(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{poolUp()}
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	recorder := get(t, server.SocketHandler(), "/units/atlas-pool.service")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var unit wire.UnitLiveness
	decode(t, recorder, &unit)
	if unit.Name != "atlas-pool.service" || unit.ActiveState != "active" || unit.SubState != "exited" {
		t.Errorf("got %+v, want the pool's liveness verbatim", unit)
	}
}

// The property the whole endpoint turns on. A per-VM unit has its own verbs, its
// own fence and its own journal, and reaching one through unit supervision would
// be a way around all three — so the name is refused before anything is read.
func TestThePerVirtualMachineUnitsAreNotReachableThroughUnitSupervision(t *testing.T) {
	supervisor := newFakeUnits()
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)
	refused := []string{
		"firecracker-vm@" + testUuid + ".service",
		"sshd.service",
		"boat.service",
	}

	for _, name := range refused {
		if code := get(t, server.SocketHandler(), "/units/"+name).Code; code != http.StatusNotFound {
			t.Errorf("GET /units/%s got %d, want 404", name, code)
		}
		body := wire.UnitActionRequest{Action: wire.UnitActionRestart}
		if code := postJSON(t, server.SocketHandler(), "/units/"+name, body).Code; code != http.StatusNotFound {
			t.Errorf("POST /units/%s got %d, want 404", name, code)
		}
	}
	if len(supervisor.acted) != 0 {
		t.Errorf("the supervisor was asked to act anyway: %v", supervisor.acted)
	}
}

// Two different facts, and a caller that conflated them would read a host
// missing its network daemon as a Boat that had never heard of one.
func TestAnUninstalledUnitIsADifferentAnswerFromAnUnsupervisedOne(t *testing.T) {
	supervisor := newFakeUnits()
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	installed := get(t, server.SocketHandler(), "/units/atlas-networkd.service")
	supervised := get(t, server.SocketHandler(), "/units/nginx.service")

	if installed.Code != http.StatusNotFound || supervised.Code != http.StatusNotFound {
		t.Fatalf("got %d and %d, want 404 for both", installed.Code, supervised.Code)
	}
	if !strings.Contains(installed.Body.String(), "does not have") {
		t.Errorf("got %q, want it to say the unit is not installed here", installed.Body)
	}
	if !strings.Contains(supervised.Body.String(), "supervises no unit") {
		t.Errorf("got %q, want it to say the unit is not supervised", supervised.Body)
	}
}

func TestActOnUnitRestartsAndReportsWhatTheUnitBecame(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{poolUp()}
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	recorder := postJSON(t, server.SocketHandler(), "/units/atlas-pool.service",
		wire.UnitActionRequest{Action: wire.UnitActionRestart})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if len(supervisor.acted) != 1 || supervisor.acted[0] != "restart atlas-pool.service" {
		t.Fatalf("got %v, want one restart of the pool", supervisor.acted)
	}
	var unit wire.UnitLiveness
	decode(t, recorder, &unit)
	if unit.ActiveState != "active" {
		t.Errorf("got %+v, want the liveness read back after the action", unit)
	}
}

// The verb set converges upward and there is nothing in it that takes a unit
// down. `stop` is refused at the boundary rather than passed to systemd.
func TestTheOnlyActionsAreStartAndRestart(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{poolUp()}
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	for _, refused := range []string{"stop", "kill", "disable", "reset-failed", ""} {
		body := wire.UnitActionRequest{Action: wire.UnitAction(refused)}
		if code := postJSON(t, server.SocketHandler(), "/units/atlas-pool.service", body).Code; code != http.StatusBadRequest {
			t.Errorf("action %q got %d, want 400", refused, code)
		}
	}
	for _, accepted := range []wire.UnitAction{wire.UnitActionStart, wire.UnitActionRestart} {
		body := wire.UnitActionRequest{Action: accepted}
		if code := postJSON(t, server.SocketHandler(), "/units/atlas-pool.service", body).Code; code != http.StatusOK {
			t.Errorf("action %q got %d, want 200", accepted, code)
		}
	}
	if len(supervisor.acted) != 2 {
		t.Errorf("got %v, want exactly the two accepted actions to have reached the host", supervisor.acted)
	}
}

func TestAFailedUnitActionIsReportedRatherThanClaimedDone(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{poolUp()}
	supervisor.actErr = errors.New("Start request repeated too quickly")
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	recorder := postJSON(t, server.SocketHandler(), "/units/atlas-pool.service",
		wire.UnitActionRequest{Action: wire.UnitActionRestart})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
}

// A host is not only its VMs. A machine whose thin pool never rebound after a
// reboot has every VM on it about to fail to start, and until GET /host carried
// unit liveness that fact reached the control plane through nothing at all.
func TestGetHostCarriesUnitLiveness(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{poolUp()}
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	recorder := get(t, server.SocketHandler(), "/host")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var host wire.Host
	decode(t, recorder, &host)
	if len(host.Units) != 1 || host.Units[0].Name != "atlas-pool.service" {
		t.Errorf("got units %+v, want the one the supervisor reported", host.Units)
	}
}

func TestGetHostFailsWhenTheUnitsCannotBeRead(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.readErr = errors.New("systemd is not answering")
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	if code := get(t, server.SocketHandler(), "/host").Code; code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", code)
	}
}
