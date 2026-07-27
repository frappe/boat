package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// Every test here runs against a real bbolt file under t.TempDir. This package's
// entire job is durability, so a mock would test nothing that matters.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "boat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func observedVirtualMachine(uuid string, status model.VirtualMachineStatus) model.VirtualMachine {
	return model.VirtualMachine{
		UUID:            uuid,
		ObservedStatus:  status,
		ObservedAt:      time.Now().UTC().Truncate(time.Second),
		UnitActiveState: "active",
		UnitSubState:    "running",
	}
}

// reopenablePath is for the tests that close the store and open it again, which
// is the only way to prove a field is durable rather than merely in a struct.
func reopenablePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "boat.db")
}

func observedEpochOrFail(t *testing.T, store *Store) int64 {
	t.Helper()
	epoch, err := store.ObservedEpoch()
	if err != nil {
		t.Fatalf("read observed epoch: %v", err)
	}
	return epoch
}

func TestOpenCreatesParentDirectoryAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "lib", "boat", "boat.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open into a directory that does not exist yet: %v", err)
	}
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A Boat restart re-opens the same file under running VMs; the buckets are
	// already there and the records have to still be.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen existing database: %v", err)
	}
	defer reopened.Close()
	record, found, err := reopened.GetVirtualMachine("vm-a")
	if err != nil || !found {
		t.Fatalf("get after reopen: found=%v err=%v", found, err)
	}
	if record.ObservedStatus != model.StatusRunning {
		t.Fatalf("status after reopen = %q, want Running", record.ObservedStatus)
	}
}

func TestVirtualMachineRoundTrip(t *testing.T) {
	store := newTestStore(t)
	written := observedVirtualMachine("vm-a", model.StatusSleeping)
	written.Sleeping = true
	written.HasMemorySnapshot = true
	if err := store.PutVirtualMachine(written); err != nil {
		t.Fatalf("put: %v", err)
	}

	read, found, err := store.GetVirtualMachine("vm-a")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !read.ObservedAt.Equal(written.ObservedAt) {
		t.Fatalf("observed at = %v, want %v", read.ObservedAt, written.ObservedAt)
	}
	read.ObservedAt = written.ObservedAt // time.Time carries a monotonic reading JSON cannot.
	if read != written {
		t.Fatalf("read back %+v, want %+v", read, written)
	}
}

func TestPutVirtualMachineReplacesTheEarlierObservation(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusStopped)); err != nil {
		t.Fatalf("second put: %v", err)
	}

	record, _, err := store.GetVirtualMachine("vm-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if record.ObservedStatus != model.StatusStopped {
		t.Fatalf("status = %q, want the latest observation Stopped", record.ObservedStatus)
	}
	records, err := store.ListVirtualMachines()
	if err != nil || len(records) != 1 {
		t.Fatalf("list = %d records (err %v), want 1 — observations are latest-wins", len(records), err)
	}
}

func TestGetVirtualMachineAbsentIsNotAnError(t *testing.T) {
	store := newTestStore(t)
	record, found, err := store.GetVirtualMachine("never-seen")
	if err != nil {
		t.Fatalf("absence must not be an error, got %v", err)
	}
	if found {
		t.Fatalf("found = true for a VM this host never observed")
	}
	if record != (model.VirtualMachine{}) {
		t.Fatalf("record = %+v, want the zero value", record)
	}
}

func TestListVirtualMachinesIsEmptyThenOrderedByUUID(t *testing.T) {
	store := newTestStore(t)
	records, err := store.ListVirtualMachines()
	if err != nil {
		t.Fatalf("list on a fresh store: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("fresh store listed %d records", len(records))
	}

	for _, uuid := range []string{"vm-c", "vm-a", "vm-b"} {
		if err := store.PutVirtualMachine(observedVirtualMachine(uuid, model.StatusRunning)); err != nil {
			t.Fatalf("put %s: %v", uuid, err)
		}
	}
	records, err = store.ListVirtualMachines()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := []string{}
	for _, record := range records {
		listed = append(listed, record.UUID)
	}
	if strings.Join(listed, ",") != "vm-a,vm-b,vm-c" {
		t.Fatalf("list order = %v, want UUID order", listed)
	}
}

// The stored bytes are part of this package's contract: an operator with a hex
// dump and nothing else has to be able to read them.
func TestStoredValuesAreIndentedJSON(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}

	var stored []byte
	err := store.database.View(func(transaction *bbolt.Tx) error {
		stored = append(stored, transaction.Bucket(virtualMachinesBucket).Get([]byte("vm-a"))...)
		return nil
	})
	if err != nil {
		t.Fatalf("read raw value: %v", err)
	}
	if !json.Valid(stored) {
		t.Fatalf("stored value is not JSON: %q", stored)
	}
	if !strings.Contains(string(stored), "\n  \"uuid\": \"vm-a\"") {
		t.Fatalf("stored value is not indented JSON:\n%s", stored)
	}
}
