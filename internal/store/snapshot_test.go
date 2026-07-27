package store

import (
	"fmt"
	"testing"

	"github.com/frappe/boat/internal/model"
)

func TestSnapshotReportsEverythingTheStoreKnows(t *testing.T) {
	store := newTestStore(t)
	for _, uuid := range []string{"vm-b", "vm-a"} {
		if err := store.PutVirtualMachine(observedVirtualMachine(uuid, model.StatusRunning)); err != nil {
			t.Fatalf("put %s: %v", uuid, err)
		}
		if err := store.SetFenceEpoch(uuid, 7); err != nil {
			t.Fatalf("assert %s: %v", uuid, err)
		}
	}

	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(export.VirtualMachines) != 2 || export.VirtualMachines[0].UUID != "vm-a" {
		t.Fatalf("virtual machines = %+v, want both in UUID order", export.VirtualMachines)
	}
	if export.FenceEpochs["vm-a"] != 7 || export.FenceEpochs["vm-b"] != 7 {
		t.Fatalf("fence epochs = %v, want both held at 7", export.FenceEpochs)
	}
	if export.TakenAt.IsZero() {
		t.Fatalf("an export with no timestamp cannot be ordered against another")
	}
}

// The store has no way to observe host facts, units or logical volumes, and
// reporting empty ones would state "this host has no logical volumes" — a claim
// it is not entitled to make. The caller enriches them.
func TestSnapshotLeavesWhatTheStoreCannotObserveToTheCaller(t *testing.T) {
	store := newTestStore(t)
	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if export.Host != (model.HostFacts{}) {
		t.Fatalf("host = %+v, want the zero value for the caller to fill", export.Host)
	}
	if export.Units != nil || export.LogicalVolumes != nil {
		t.Fatalf("units = %v and volumes = %v, want both left to the caller",
			export.Units, export.LogicalVolumes)
	}
	if export.VirtualMachines == nil || export.FenceEpochs == nil {
		t.Fatalf("the store does know it holds no VMs and no epochs, and must say so")
	}
}

// The epoch is only a CAS token if it names the state it arrived with.
func TestSnapshotEpochMatchesTheStandaloneRead(t *testing.T) {
	store := newTestStore(t)
	for step := range 4 {
		if step%2 == 0 {
			if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
				t.Fatalf("put: %v", err)
			}
		} else if err := store.SetFenceEpoch("vm-a", int64(step)); err != nil {
			t.Fatalf("assert: %v", err)
		}

		export, err := store.Snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if epoch := observedEpochOrFail(t, store); export.ObservedEpoch != epoch {
			t.Fatalf("snapshot reports epoch %d, the store reports %d", export.ObservedEpoch, epoch)
		}
	}
}

// Snapshot returns a value, not a view: whatever happens to the store next, the
// export the caller is about to stream is the one it read.
func TestSnapshotIsDetachedFromTheStore(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.SetFenceEpoch("vm-a", 7); err != nil {
		t.Fatalf("assert: %v", err)
	}
	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if err := store.PutVirtualMachine(observedVirtualMachine("vm-b", model.StatusRunning)); err != nil {
		t.Fatalf("put during the stream: %v", err)
	}
	if err := store.SetFenceEpoch("vm-a", 9); err != nil {
		t.Fatalf("assert during the stream: %v", err)
	}

	if len(export.VirtualMachines) != 1 || export.FenceEpochs["vm-a"] != 7 {
		t.Fatalf("the export changed under its holder: %+v", export)
	}
	export.FenceEpochs["vm-a"] = 1 // The caller owns the map it was handed.
	if epoch, _, _ := store.FenceEpoch("vm-a"); epoch != 9 {
		t.Fatalf("mutating the returned export reached back into the store (epoch %d)", epoch)
	}
}

// The read transaction has to be closed before the caller can start streaming,
// which is the difference between a slow client costing latency and a slow
// client making a healthy host look partitioned.
func TestSnapshotHoldsNoTransactionOnceItReturns(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := store.Snapshot(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if open := store.database.Stats().OpenTxN; open != 0 {
		t.Fatalf("%d read transactions still open after Snapshot returned", open)
	}
}

// One writer, one snapshotter, one arithmetic invariant: every write in the loop
// adds exactly one key and bumps the epoch exactly once, so an export whose key
// count and epoch disagree is an export assembled from two different instants of
// the file — precisely what reading the records and the epoch in separate
// transactions would produce.
//
// The writer waits at its halfway point for the reader's first snapshot, so the
// race is a fact of the test rather than a hope about scheduling: that snapshot
// is taken with writes still to come, and it is checked like all the others.
func TestSnapshotIsInternallyConsistentUnderConcurrentWrites(t *testing.T) {
	store := newTestStore(t)
	const writes = 40

	failed := make(chan error, 1)
	done := make(chan struct{})
	racing := make(chan struct{})
	go func() {
		defer close(done)
		for write := range writes {
			if write == writes/2 {
				<-racing
			}
			if err := writePair(store, fmt.Sprintf("vm-%03d", write)); err != nil {
				failed <- err
				return
			}
		}
	}()

	first := 0
	for taken := 0; ; taken++ {
		keys := assertSnapshotIsConsistent(t, store)
		if taken == 0 {
			first = keys
			close(racing)
		}
		select {
		case <-done:
			assertSnapshotIsConsistent(t, store) // Once more, now the writes are all in.
			assertWriterSucceeded(t, failed, store, first, writes)
			return
		default:
		}
	}
}

func writePair(store *Store, uuid string) error {
	if err := store.PutVirtualMachine(observedVirtualMachine(uuid, model.StatusRunning)); err != nil {
		return err
	}
	return store.SetFenceEpoch(uuid, 1)
}

func assertSnapshotIsConsistent(t *testing.T, store *Store) int {
	t.Helper()
	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	keys := len(export.VirtualMachines) + len(export.FenceEpochs)
	if int64(keys) != export.ObservedEpoch {
		t.Fatalf("export holds %d records at epoch %d: the records and the epoch were read at different instants",
			keys, export.ObservedEpoch)
	}
	return keys
}

func assertWriterSucceeded(t *testing.T, failed chan error, store *Store, first, writes int) {
	t.Helper()
	select {
	case err := <-failed:
		t.Fatalf("writer: %v", err)
	default:
	}
	if first >= 2*writes {
		t.Fatalf("the first snapshot already held everything (%d records); nothing was raced", first)
	}
	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(export.VirtualMachines) != writes {
		t.Fatalf("final export holds %d VMs, want %d", len(export.VirtualMachines), writes)
	}
}
