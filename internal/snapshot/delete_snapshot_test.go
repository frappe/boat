package snapshot

import (
	"context"
	"testing"
)

// A cold snapshot: remove the root snapshot LV. Present → lvremove.
func TestDeleteSnapshotVMRootOnly(t *testing.T) {
	fake := newFakeCommands().exists("sudo lvs --noheadings atlas/" + rootSnap)

	if err := DeleteSnapshotVM(context.Background(), fake, DeleteSnapshotVMParams{
		SnapshotRootfsPath: rootSnapPath,
	}); err != nil {
		t.Fatalf("DeleteSnapshotVM: %v", err)
	}
	assertTrace(t, fake,
		"? sudo lvs --noheadings atlas/"+rootSnap,
		"sudo lvremove -f atlas/"+rootSnap,
	)
}

// A missing LV is a clean no-op, not an error.
func TestDeleteSnapshotVMIsIdempotent(t *testing.T) {
	fake := newFakeCommands() // snapshot LV absent

	if err := DeleteSnapshotVM(context.Background(), fake, DeleteSnapshotVMParams{
		SnapshotRootfsPath: rootSnapPath,
	}); err != nil {
		t.Fatalf("DeleteSnapshotVM: %v", err)
	}
	assertNotIssued(t, fake, "lvremove")
}

// A warm snapshot: remove both disk halves and the durable memory directory.
func TestDeleteSnapshotVMWarm(t *testing.T) {
	memoryDirectory := "/var/lib/atlas/snapshots/snap-golden"
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/" + rootSnap).
		exists("sudo lvs --noheadings atlas/" + dataSnap)

	if err := DeleteSnapshotVM(context.Background(), fake, DeleteSnapshotVMParams{
		SnapshotRootfsPath:     rootSnapPath,
		DataSnapshotRootfsPath: dataSnapPath,
		MemoryDirectory:        memoryDirectory,
	}); err != nil {
		t.Fatalf("DeleteSnapshotVM: %v", err)
	}
	assertTrace(t, fake,
		"? sudo lvs --noheadings atlas/"+rootSnap,
		"sudo lvremove -f atlas/"+rootSnap,
		"? sudo lvs --noheadings atlas/"+dataSnap,
		"sudo lvremove -f atlas/"+dataSnap,
		"sudo rm -rf "+memoryDirectory,
	)
}

// A memory directory outside the snapshots tree is refused before any rm -rf — the
// guard that keeps a malformed row from sweeping the host.
func TestDeleteSnapshotVMRefusesMemoryDirectoryOutsideTree(t *testing.T) {
	fake := newFakeCommands().exists("sudo lvs --noheadings atlas/" + rootSnap)

	err := DeleteSnapshotVM(context.Background(), fake, DeleteSnapshotVMParams{
		SnapshotRootfsPath: rootSnapPath,
		MemoryDirectory:    "/etc",
	})
	if err == nil {
		t.Fatal("DeleteSnapshotVM accepted a memory directory outside the snapshots tree")
	}
	assertNotIssued(t, fake, "rm -rf")
}
