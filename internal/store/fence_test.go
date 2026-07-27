package store

import (
	"errors"
	"testing"
)

func TestSetFenceEpochRoundTripsAndMovesForward(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetFenceEpoch("vm-a", 7); err != nil {
		t.Fatalf("first assert: %v", err)
	}
	epoch, found, err := store.FenceEpoch("vm-a")
	if err != nil || !found {
		t.Fatalf("read fence epoch: found=%v err=%v", found, err)
	}
	if epoch != 7 {
		t.Fatalf("held epoch = %d, want 7", epoch)
	}

	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("forward move: %v", err)
	}
	if epoch, _, _ = store.FenceEpoch("vm-a"); epoch != 8 {
		t.Fatalf("held epoch = %d after a forward move, want 8", epoch)
	}
}

// A fence that can be walked backwards is not a fence: the lower epoch is
// exactly what a start left over from before a migration carries.
func TestSetFenceEpochRefusesARegressionAndKeepsWhatItHeld(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("assert: %v", err)
	}

	err := store.SetFenceEpoch("vm-a", 7)
	if !errors.Is(err, ErrFenceRegression) {
		t.Fatalf("err = %v, want ErrFenceRegression", err)
	}
	epoch, found, err := store.FenceEpoch("vm-a")
	if err != nil || !found || epoch != 8 {
		t.Fatalf("held epoch = %d (found %v, err %v) after a refused regression, want 8 untouched",
			epoch, found, err)
	}
}

// Atlas retries: a PUT whose reply was lost comes back verbatim. Refusing the
// repeat would leave a VM whose first assert landed permanently unbootable.
func TestSetFenceEpochAcceptsAnEqualReassertAsANoOp(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("assert: %v", err)
	}
	before := observedEpochOrFail(t, store)

	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("re-assert of the epoch already held: %v", err)
	}

	epoch, _, err := store.FenceEpoch("vm-a")
	if err != nil || epoch != 8 {
		t.Fatalf("held epoch = %d (err %v), want 8", epoch, err)
	}
	if after := observedEpochOrFail(t, store); after != before {
		t.Fatalf("observed epoch moved %d -> %d on a re-assert that changed nothing", before, after)
	}
}

func TestFenceEpochAbsentIsNotAnError(t *testing.T) {
	store := newTestStore(t)
	epoch, found, err := store.FenceEpoch("never-fenced")
	if err != nil {
		t.Fatalf("absence must not be an error, got %v", err)
	}
	if found {
		t.Fatalf("found = true for a VM this host holds no epoch for")
	}
	if epoch != 0 {
		t.Fatalf("epoch = %d, want the zero value", epoch)
	}
}

func TestFenceEpochsIsEmptyThenListsEveryHeldEpoch(t *testing.T) {
	store := newTestStore(t)
	epochs, err := store.FenceEpochs()
	if err != nil {
		t.Fatalf("list on a fresh store: %v", err)
	}
	if epochs == nil {
		t.Fatalf("a host that fences nothing must say so with an empty map, not nil")
	}
	if len(epochs) != 0 {
		t.Fatalf("fresh store holds %d epochs", len(epochs))
	}

	held := map[string]int64{"vm-a": 3, "vm-b": 9}
	for uuid, epoch := range held {
		if err := store.SetFenceEpoch(uuid, epoch); err != nil {
			t.Fatalf("assert %s: %v", uuid, err)
		}
	}
	epochs, err = store.FenceEpochs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(epochs) != len(held) {
		t.Fatalf("listed %v, want %v", epochs, held)
	}
	for uuid, epoch := range held {
		if epochs[uuid] != epoch {
			t.Fatalf("epoch for %s = %d, want %d", uuid, epochs[uuid], epoch)
		}
	}
}

// The fence is the one record that must outlive the process holding it: it is
// read on startup, before anything is allowed to boot.
func TestFenceEpochSurvivesReopen(t *testing.T) {
	path := reopenablePath(t)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	epoch, found, err := reopened.FenceEpoch("vm-a")
	if err != nil || !found || epoch != 8 {
		t.Fatalf("held epoch = %d (found %v, err %v) after reopen, want 8", epoch, found, err)
	}
	if err := reopened.SetFenceEpoch("vm-a", 7); !errors.Is(err, ErrFenceRegression) {
		t.Fatalf("a regression was accepted across a restart: %v", err)
	}
}

// A fence epoch is part of the export, so a change to one has to be visible as a
// new observed epoch. Otherwise two exports that differ share a CAS token.
func TestSetFenceEpochBumpsTheObservedEpoch(t *testing.T) {
	store := newTestStore(t)
	before := observedEpochOrFail(t, store)
	if err := store.SetFenceEpoch("vm-a", 7); err != nil {
		t.Fatalf("assert: %v", err)
	}
	afterAssert := observedEpochOrFail(t, store)
	if afterAssert != before+1 {
		t.Fatalf("observed epoch = %d after a fence assert, want %d", afterAssert, before+1)
	}

	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("forward move: %v", err)
	}
	if after := observedEpochOrFail(t, store); after != afterAssert+1 {
		t.Fatalf("observed epoch = %d after a forward move, want %d", after, afterAssert+1)
	}
}

// A refused write must not leave a trace. An epoch that moved on a rejected
// regression would tell a watcher something changed when nothing did.
func TestRefusedRegressionDoesNotBumpTheObservedEpoch(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetFenceEpoch("vm-a", 8); err != nil {
		t.Fatalf("assert: %v", err)
	}
	before := observedEpochOrFail(t, store)

	if err := store.SetFenceEpoch("vm-a", 1); !errors.Is(err, ErrFenceRegression) {
		t.Fatalf("err = %v, want ErrFenceRegression", err)
	}
	if after := observedEpochOrFail(t, store); after != before {
		t.Fatalf("observed epoch moved %d -> %d on a refused write", before, after)
	}
}
