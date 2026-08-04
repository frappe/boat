package snapshot

import (
	"context"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

const testFirecrackerUID = 247312

// vmPaths is the shared path set the stop/warm tests address the test VM through.
func vmPaths() paths.VirtualMachine { return paths.ForVirtualMachine(testUUID) }

// The happy path: launcher is snapshot-aware, socket is up, room is free — so
// pause, write the pair, mark it complete, and stop the unit. The pause comes
// BETWEEN the dir chown and the PUT, exactly as scripts/snapshot-stop-vm.py.
func TestSnapshotStopVMCapturesMemory(t *testing.T) {
	vm := vmPaths()
	fake := newFakeCommands().
		exists("sudo grep -q snapshot/READY "+vm.JailerLaunch()).
		exists("sudo test -S "+vm.APISocket()).
		output("sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(), "1024").
		output("df --output=avail -B1 "+paths.AtlasRoot, "Avail\n999999999999\n").
		exists("sudo test -s "+vm.MemorySnapshotVMState()).
		exists("sudo test -s "+vm.MemorySnapshotMemory()).
		output("sudo stat -c %s "+vm.MemorySnapshotMemory(), "1073741824")

	result, err := SnapshotStopVM(context.Background(), fake, SnapshotStopParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
	})
	if err != nil {
		t.Fatalf("SnapshotStopVM: %v", err)
	}
	if !result.MemorySnapshot || result.MemorySnapshotBytes != 1073741824 || result.Reason != "" {
		t.Errorf("result = %+v", result)
	}
	assertTrace(t, fake,
		"? sudo grep -q snapshot/READY "+vm.JailerLaunch(),
		"? sudo test -S "+vm.APISocket(),
		"sudo rm -rf "+vm.MemorySnapshotDirectory(),
		"sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(),
		"df --output=avail -B1 "+paths.AtlasRoot,
		"install-dir 0700 "+vm.MemorySnapshotDirectory(),
		"sudo chown 247312:247312 "+vm.MemorySnapshotDirectory(),
		"fcapi "+vm.APISocketDirectory()+" "+vm.APISocketName()+" PATCH /vm "+pausedStateBody,
		"fcapi "+vm.APISocketDirectory()+" "+vm.APISocketName()+" PUT /snapshot/create "+memorySnapshotBody,
		"? sudo test -s "+vm.MemorySnapshotVMState(),
		"? sudo test -s "+vm.MemorySnapshotMemory(),
		"sudo touch "+vm.MemorySnapshotMarker(),
		"sudo systemctl stop "+vm.SystemdUnit(),
		"sudo stat -c %s "+vm.MemorySnapshotMemory(),
	)
}

// A launcher that predates memory snapshots falls straight back to a plain stop:
// the stale marker is cleared and the unit stopped, and the VM ends Stopped with
// the reason recorded. No pause, no PUT.
func TestSnapshotStopVMFallsBackWhenLauncherIsOld(t *testing.T) {
	vm := vmPaths()
	fake := newFakeCommands() // grep gate absent → old launcher

	result, err := SnapshotStopVM(context.Background(), fake, SnapshotStopParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
	})
	if err != nil {
		t.Fatalf("SnapshotStopVM: %v", err)
	}
	if result.MemorySnapshot || result.Reason == "" {
		t.Errorf("expected a plain stop with a reason, got %+v", result)
	}
	assertTrace(t, fake,
		"? sudo grep -q snapshot/READY "+vm.JailerLaunch(),
		"sudo rm -f "+vm.MemorySnapshotMarker(),
		"sudo systemctl stop "+vm.SystemdUnit(),
	)
	assertNotIssued(t, fake, "fcapi")
	assertNotIssued(t, fake, "PUT /snapshot/create")
}

// Not enough free space for the RAM-sized memory file also falls back — after the
// space read, before any pause.
func TestSnapshotStopVMFallsBackWhenNoSpace(t *testing.T) {
	vm := vmPaths()
	fake := newFakeCommands().
		exists("sudo grep -q snapshot/READY "+vm.JailerLaunch()).
		exists("sudo test -S "+vm.APISocket()).
		output("sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(), "8192").
		output("df --output=avail -B1 "+paths.AtlasRoot, "Avail\n1024\n")

	result, err := SnapshotStopVM(context.Background(), fake, SnapshotStopParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
	})
	if err != nil {
		t.Fatalf("SnapshotStopVM: %v", err)
	}
	if result.MemorySnapshot {
		t.Errorf("captured a snapshot with no room: %+v", result)
	}
	assertNotIssued(t, fake, "fcapi")
	assertIssued(t, fake, "sudo systemctl stop "+vm.SystemdUnit())
}
