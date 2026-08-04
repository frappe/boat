package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

const restoreUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// No marker is the common cold boot: the launcher already booted the guest from
// --config-file, so restore is a pure no-op that never touches Firecracker.
func TestRestoreColdBootsWithoutAMarker(t *testing.T) {
	fake := newFakeCommands()
	result, err := RestoreVM(context.Background(), fake, restoreUUID)
	if err != nil {
		t.Fatalf("cold boot errored: %v", err)
	}
	if !result.ColdBoot || result.Restored {
		t.Errorf("no marker should be a pure cold boot, got %+v", result)
	}
	if traceContains(fake, "fcapi") {
		t.Error("a cold boot talked to Firecracker")
	}
}

// The load is PUT before the marker is consumed, and the resume is strictly last —
// the ordering that makes a crash anywhere either "marker present, disk untouched,
// retry restores" or "marker gone, next start cold-boots", never a double-restore.
func TestRestoreLoadsPausedThenConsumesMarkerThenResumes(t *testing.T) {
	vm := paths.ForVirtualMachine(restoreUUID)
	fake := newFakeCommands()
	fake.exists("sudo test -e " + vm.MemorySnapshotMarker()) // marker present
	fake.exists("sudo test -S " + vm.APISocket())            // socket up

	result, err := RestoreVM(context.Background(), fake, restoreUUID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !result.Restored {
		t.Errorf("a marker with a healthy load should restore, got %+v", result)
	}
	load := traceIndex(t, fake, "PUT /snapshot/load")
	consume := traceIndex(t, fake, "sudo rm -f "+vm.MemorySnapshotMarker())
	resume := traceIndex(t, fake, "PATCH /vm")
	if !(load < consume && consume < resume) {
		t.Errorf("order was load=%d consume=%d resume=%d, want load<consume<resume", load, consume, resume)
	}
}

// A staged signature that will not validate is a mismatch by construction — never
// load a pair we cannot validate — so the marker is consumed and nothing resumes.
func TestRestoreConsumesMarkerAndRefusesToResumeOnSignatureMismatch(t *testing.T) {
	vm := paths.ForVirtualMachine(restoreUUID)
	fake := newFakeCommands()
	fake.exists("sudo test -e " + vm.MemorySnapshotMarker())
	fake.exists("sudo test -e " + vm.MemorySnapshotSignature())
	fake.output("sudo cat "+vm.MemorySnapshotSignature(), "not-json{{{")

	if _, err := RestoreVM(context.Background(), fake, restoreUUID); err == nil {
		t.Fatal("a signature mismatch should fail the restore")
	}
	if !traceContains(fake, "sudo rm -f "+vm.MemorySnapshotMarker()) {
		t.Error("the marker was not consumed on a mismatch")
	}
	if traceContains(fake, "PUT /snapshot/load") || traceContains(fake, "Resumed") {
		t.Error("a mismatched pair was loaded or resumed")
	}
}

func traceContains(fake *fakeCommands, fragment string) bool {
	for _, line := range fake.trace {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func traceIndex(t *testing.T, fake *fakeCommands, fragment string) int {
	t.Helper()
	for index, line := range fake.trace {
		if strings.Contains(line, fragment) {
			return index
		}
	}
	t.Fatalf("%q was never issued in %v", fragment, fake.trace)
	return -1
}
