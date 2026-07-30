package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/version"
	"github.com/frappe/boat/internal/wire"
)

func TestExportCarriesTheSnapshotTheHostFactsAndTheEpoch(t *testing.T) {
	state := newFakeStore()
	state.virtualMachines[testUuid] = model.VirtualMachine{
		UUID:           testUuid,
		ObservedStatus: model.StatusSleeping,
		ObservedAt:     time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
		Sleeping:       true,
	}
	state.fence(testUuid, 4)
	state.logicalVolumes = []model.LogicalVolume{{Name: "vm-" + testUuid, SizeBytes: 42, Pool: "atlas"}}
	state.epoch = 11
	supervisor := newFakeUnits()
	supervisor.liveness = []model.UnitLiveness{{Name: "atlas-pool.service", ActiveState: "active", SubState: "exited"}}
	server := newTestServerWithUnits(state, &fakeVirtualMachines{}, supervisor)
	server.hostFacts = func(ctx context.Context, runner *run.Runner) (model.HostFacts, error) {
		return model.HostFacts{Hostname: "atlas-host-1", VCPUsTotal: 8, MemoryMegabytesFree: 2048}, nil
	}

	recorder := get(t, server.SocketHandler(), "/export")

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var export wire.Export
	decode(t, recorder, &export)
	if export.ObservedEpoch != 11 {
		t.Errorf("got observed epoch %d, want the 11 the snapshot was taken at", export.ObservedEpoch)
	}
	if export.TakenAt.IsZero() {
		t.Error("the export did not say when it was taken")
	}
	if len(export.VirtualMachines) != 1 || export.VirtualMachines[0].Uuid != testUuid {
		t.Fatalf("got %+v, want the one record the store holds", export.VirtualMachines)
	}
	if export.VirtualMachines[0].ObservedStatus != wire.VirtualMachineStatusSleeping {
		t.Errorf("got status %q, want Sleeping", export.VirtualMachines[0].ObservedStatus)
	}
	if export.FenceEpochs == nil || (*export.FenceEpochs)[testUuid] != 4 {
		t.Errorf("got fences %v, want epoch 4 for the one VM", export.FenceEpochs)
	}
	// Unit liveness is read from the host at export time, not carried in the
	// store's snapshot: a cached ActiveState is a claim about a service that may
	// have died since.
	if export.Units == nil || len(*export.Units) != 1 || (*export.Units)[0].Name != "atlas-pool.service" {
		t.Errorf("got units %v, want the one the supervisor reported", export.Units)
	}
	if export.LogicalVolumes == nil || len(*export.LogicalVolumes) != 1 {
		t.Errorf("got volumes %v, want the one the snapshot held", export.LogicalVolumes)
	}
	if export.Host.Hostname != "atlas-host-1" || export.Host.VcpusTotal == nil || *export.Host.VcpusTotal != 8 {
		t.Errorf("the live host facts did not reach the document: %+v", export.Host)
	}
	// /health, /host and the export answer from one place, because three
	// endpoints disagreeing about which Boat is running is how a stuck upgrade
	// hides.
	if export.Host.BoatVersion != version.Version {
		t.Errorf("got version %q, want the running binary's %q", export.Host.BoatVersion, version.Version)
	}
}

// The store refuses to claim a host has no logical volumes when it has not
// looked, and the document must not make that claim on its behalf.
func TestExportOmitsWhatTheSnapshotDidNotObserve(t *testing.T) {
	state := newFakeStore()
	server := newTestServer(state, &fakeVirtualMachines{})

	var export wire.Export
	decode(t, get(t, server.SocketHandler(), "/export"), &export)

	if export.LogicalVolumes != nil {
		t.Errorf("got volumes %v, want them absent", export.LogicalVolumes)
	}
	if export.VirtualMachines == nil {
		t.Error("the observed set is a fact the store does hold, so it must be present and empty")
	}
}

// Units are the opposite case to the volumes above, and the contrast is the
// point: the supervisor asks systemd about every supervised name on every
// export, so an empty answer means "this host runs none of them" and is a fact.
// An absent array would mean "not looked at", which is what it meant before this
// endpoint had a supervisor behind it and what Atlas's mirror still reads it as.
func TestExportReportsAnEmptyUnitSetRatherThanOmittingIt(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})

	var export wire.Export
	decode(t, get(t, server.SocketHandler(), "/export"), &export)

	if export.Units == nil {
		t.Fatal("got no units array at all, which says this host was never asked")
	}
	if len(*export.Units) != 0 {
		t.Errorf("got units %v, want an empty array", *export.Units)
	}
}

// The export is how a partitioned Atlas rebuilds its mirror, so a host that
// cannot answer for its own services must fail the whole document rather than
// send one that simply does not mention them.
func TestExportFailsWhenTheUnitsCannotBeRead(t *testing.T) {
	supervisor := newFakeUnits()
	supervisor.readErr = errors.New("systemd is not answering")
	server := newTestServerWithUnits(newFakeStore(), &fakeVirtualMachines{}, supervisor)

	recorder := get(t, server.SocketHandler(), "/export")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
}

func TestExportFailsLoudlyWhenTheHostCannotBeRead(t *testing.T) {
	state := newFakeStore()
	server := newTestServer(state, &fakeVirtualMachines{})
	server.hostFacts = func(ctx context.Context, runner *run.Runner) (model.HostFacts, error) {
		return model.HostFacts{}, errors.New("lsblk: command not found")
	}

	recorder := get(t, server.SocketHandler(), "/export")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the fault carried no sentence")
	}
}

func TestExportFailsLoudlyWhenTheSnapshotCannotBeTaken(t *testing.T) {
	state := newFakeStore()
	state.snapshotError = errors.New("/var/lib/boat/boat.db: input/output error")
	handler := newTestServer(state, &fakeVirtualMachines{}).SocketHandler()

	recorder := get(t, handler, "/export")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(decodeError(t, recorder).Error, "boat.db") {
		t.Error("the boundary leaked a host path to the caller")
	}
}
