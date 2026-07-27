package store

import (
	"fmt"
	"sync"
	"testing"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

func TestObservedEpochStartsAtZero(t *testing.T) {
	store := newTestStore(t)
	if epoch := observedEpochOrFail(t, store); epoch != 0 {
		t.Fatalf("fresh store reports epoch %d, want 0", epoch)
	}
}

// Every write that changes something an export carries moves the epoch exactly
// once, and every write that does not, does not.
func TestObservedEpochAdvancesOncePerObservedWrite(t *testing.T) {
	store := newTestStore(t)
	writes := []struct {
		name  string
		write func() error
		bumps bool
	}{
		{"observe a VM", func() error {
			return store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning))
		}, true},
		{"observe it again", func() error {
			return store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusStopped))
		}, true},
		{"assert a fence epoch", func() error { return store.SetFenceEpoch("vm-a", 7) }, true},
		{"re-assert the same epoch", func() error { return store.SetFenceEpoch("vm-a", 7) }, false},
		{"move the fence forward", func() error { return store.SetFenceEpoch("vm-a", 8) }, true},
		{"assert desired state", func() error {
			return store.PutDesired(desiredVirtualMachine("vm-a", model.PowerRunning))
		}, false},
		{"claim an operation", func() error {
			_, _, err := store.ClaimOperation("task-1", "start", "vm-a")
			return err
		}, false},
	}

	want := int64(0)
	for _, write := range writes {
		before := observedEpochOrFail(t, store)
		if err := write.write(); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		if write.bumps {
			want++
		}
		after := observedEpochOrFail(t, store)
		if after != want {
			t.Fatalf("%s moved the epoch %d -> %d, want %d", write.name, before, after, want)
		}
	}
}

func TestObservedEpochSurvivesReopen(t *testing.T) {
	path := reopenablePath(t)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, uuid := range []string{"vm-a", "vm-b"} {
		if err := store.PutVirtualMachine(observedVirtualMachine(uuid, model.StatusRunning)); err != nil {
			t.Fatalf("put %s: %v", uuid, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if epoch := observedEpochOrFail(t, reopened); epoch != 2 {
		t.Fatalf("epoch after reopen = %d, want 2 — a restart must not rewind it", epoch)
	}
	if err := reopened.PutVirtualMachine(observedVirtualMachine("vm-c", model.StatusRunning)); err != nil {
		t.Fatalf("put after reopen: %v", err)
	}
	if epoch := observedEpochOrFail(t, reopened); epoch != 3 {
		t.Fatalf("epoch after a post-restart write = %d, want 3", epoch)
	}
}

// Mixed writers against one file: if any two of them read the same current epoch
// and wrote the same successor, one bump would be lost and the total would come
// up short. Exactly one epoch per bumping write is the whole invariant.
func TestObservedEpochIsMonotonicUnderConcurrentWriters(t *testing.T) {
	store := newTestStore(t)
	const writers = 32

	failures := make([]error, writers)
	release := make(chan struct{})
	var writersDone sync.WaitGroup
	for writer := range writers {
		writersDone.Add(1)
		go func() {
			defer writersDone.Done()
			<-release // Line them up so they contend for the write transaction.
			uuid := fmt.Sprintf("vm-%03d", writer)
			if err := store.PutVirtualMachine(observedVirtualMachine(uuid, model.StatusRunning)); err != nil {
				failures[writer] = err
				return
			}
			// A fence assert and a desired assert on the same VM: three writes
			// per writer, of which two are observed and one is not.
			if err := store.SetFenceEpoch(uuid, 1); err != nil {
				failures[writer] = err
				return
			}
			failures[writer] = store.PutDesired(desiredVirtualMachine(uuid, model.PowerRunning))
		}()
	}
	close(release)
	writersDone.Wait()

	for writer, err := range failures {
		if err != nil {
			t.Fatalf("writer %d: %v", writer, err)
		}
	}
	if epoch := observedEpochOrFail(t, store); epoch != 2*writers {
		t.Fatalf("epoch = %d after %d observed writes, want %d — bumps were lost to a race",
			epoch, 2*writers, 2*writers)
	}
}

// The stronger statement, made directly against the counter: every concurrent
// bump gets its own number. A repeated epoch is a CAS token that matches a state
// its holder never read, so it is not enough that the total comes out right.
func TestConcurrentBumpsNeverRepeatAnEpoch(t *testing.T) {
	store := newTestStore(t)
	const bumps = 64

	epochs := make([]int64, bumps)
	failures := make([]error, bumps)
	release := make(chan struct{})
	var bumpsDone sync.WaitGroup
	for bump := range bumps {
		bumpsDone.Add(1)
		go func() {
			defer bumpsDone.Done()
			<-release
			failures[bump] = store.database.Update(func(transaction *bbolt.Tx) error {
				var err error
				epochs[bump], err = bumpObservedEpoch(transaction)
				return err
			})
		}()
	}
	close(release)
	bumpsDone.Wait()

	seen := map[int64]int{}
	for bump, err := range failures {
		if err != nil {
			t.Fatalf("bump %d: %v", bump, err)
		}
		if first, repeated := seen[epochs[bump]]; repeated {
			t.Fatalf("bumps %d and %d were both handed epoch %d", first, bump, epochs[bump])
		}
		seen[epochs[bump]] = bump
	}
	for epoch := int64(1); epoch <= bumps; epoch++ {
		if _, handed := seen[epoch]; !handed {
			t.Fatalf("epoch %d was never handed out; the counter skipped it", epoch)
		}
	}
}
