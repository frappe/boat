package store

import (
	"errors"
	"testing"

	"github.com/frappe/boat/internal/model"
)

// The whole point of the per-resource comparison: an unrelated VM's observation
// must not invalidate a decision about this one. A host running forty VMs bumps
// the whole-host epoch about forty times a sweep, so a comparison that failed
// here would fail every real write and be turned off within a week.
func TestObservationOfAnotherVirtualMachineDoesNotMoveThisOne(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put vm-a: %v", err)
	}
	decidedAt, err := store.ObservedEpoch()
	if err != nil {
		t.Fatalf("read the epoch: %v", err)
	}
	for range 40 {
		if err := store.PutVirtualMachine(observedVirtualMachine("vm-b", model.StatusRunning)); err != nil {
			t.Fatalf("put vm-b: %v", err)
		}
	}

	if err := store.CheckVirtualMachineUnmoved("vm-a", decidedAt); err != nil {
		t.Errorf("vm-a read as moved after forty observations of vm-b: %v", err)
	}
}

func TestAVirtualMachineWrittenAfterTheEpochOfferedHasMoved(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	decidedAt, err := store.ObservedEpoch()
	if err != nil {
		t.Fatalf("read the epoch: %v", err)
	}
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusStopped)); err != nil {
		t.Fatalf("second put: %v", err)
	}

	if err := store.CheckVirtualMachineUnmoved("vm-a", decidedAt); !errors.Is(err, ErrObservationMoved) {
		t.Errorf("got %v, want ErrObservationMoved", err)
	}
}

// A caller quoting an epoch this host has never reached read it from a different
// store — a Boat whose bbolt file was lost and whose epoch restarted from zero,
// which is the state in which a mirror is most confidently wrong.
func TestAnEpochThisHostNeverIssuedIsRefused(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := store.CheckVirtualMachineUnmoved("vm-a", 900); !errors.Is(err, ErrObservationMoved) {
		t.Errorf("got %v, want ErrObservationMoved for an epoch from another store", err)
	}
}

// Nothing about a VM this host has never seen can have moved, because there is
// nothing. Refusing here would block the first assertion Atlas ever makes about
// a newly placed VM.
func TestAVirtualMachineThisHostHasNeverObservedHasNotMoved(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	reached, err := store.ObservedEpoch()
	if err != nil {
		t.Fatalf("read the epoch: %v", err)
	}

	if err := store.CheckVirtualMachineUnmoved("vm-never-seen", reached); err != nil {
		t.Errorf("got %v, want an unobserved VM to pass", err)
	}
}

// Epoch 0 is what a caller offers after reading a fresh host that has observed
// nothing. It is a real precondition and it holds until the first write.
func TestEpochZeroHoldsUntilTheFirstObservation(t *testing.T) {
	store := newTestStore(t)

	if err := store.CheckVirtualMachineUnmoved("vm-a", 0); err != nil {
		t.Fatalf("got %v, want epoch 0 to hold on a store that has observed nothing", err)
	}
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.CheckVirtualMachineUnmoved("vm-a", 0); !errors.Is(err, ErrObservationMoved) {
		t.Error("epoch 0 still held after the VM was observed")
	}
}

// The stamp on the record and the epoch in the export have to be the same
// number, or a caller CASing against the export's epoch would be matched against
// a scale it never saw.
func TestTheRecordCarriesTheEpochTheSnapshotReports(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}

	export, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(export.VirtualMachines) != 1 {
		t.Fatalf("got %d records, want one", len(export.VirtualMachines))
	}
	if export.VirtualMachines[0].ObservedEpoch != export.ObservedEpoch {
		t.Errorf("record stamped at %d, snapshot taken at %d",
			export.VirtualMachines[0].ObservedEpoch, export.ObservedEpoch)
	}
}
