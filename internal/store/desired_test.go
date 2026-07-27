package store

import (
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
)

func desiredVirtualMachine(uuid string, power model.DesiredPower) model.DesiredVirtualMachine {
	return model.DesiredVirtualMachine{
		UUID:            uuid,
		BootEpoch:       7,
		DesiredPower:    power,
		VCPUs:           2,
		MemoryMegabytes: 2048,
		DiskGigabytes:   40,
		SleepOnIdle:     true,
		PrivateAddress:  "10.0.0.2",
		AssertedAt:      time.Now().UTC().Truncate(time.Second),
	}
}

func TestDesiredRoundTrip(t *testing.T) {
	store := newTestStore(t)
	written := desiredVirtualMachine("vm-a", model.PowerRunning)
	if err := store.PutDesired(written); err != nil {
		t.Fatalf("put desired: %v", err)
	}

	read, found, err := store.GetDesired("vm-a")
	if err != nil || !found {
		t.Fatalf("get desired: found=%v err=%v", found, err)
	}
	if !read.AssertedAt.Equal(written.AssertedAt) {
		t.Fatalf("asserted at = %v, want %v", read.AssertedAt, written.AssertedAt)
	}
	read.AssertedAt = written.AssertedAt // time.Time carries a monotonic reading JSON cannot.
	if read != written {
		t.Fatalf("read back %+v, want %+v", read, written)
	}
}

func TestPutDesiredReplacesTheEarlierAssertion(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutDesired(desiredVirtualMachine("vm-a", model.PowerRunning)); err != nil {
		t.Fatalf("first assert: %v", err)
	}
	if err := store.PutDesired(desiredVirtualMachine("vm-a", model.PowerStopped)); err != nil {
		t.Fatalf("second assert: %v", err)
	}

	record, _, err := store.GetDesired("vm-a")
	if err != nil {
		t.Fatalf("get desired: %v", err)
	}
	if record.DesiredPower != model.PowerStopped {
		t.Fatalf("desired power = %q, want the latest assertion Stopped", record.DesiredPower)
	}
	records, err := store.ListDesired()
	if err != nil || len(records) != 1 {
		t.Fatalf("list = %d records (err %v), want 1 — desired state is latest-wins", len(records), err)
	}
}

func TestGetDesiredAbsentIsNotAnError(t *testing.T) {
	store := newTestStore(t)
	record, found, err := store.GetDesired("never-asserted")
	if err != nil {
		t.Fatalf("absence must not be an error, got %v", err)
	}
	if found {
		t.Fatalf("found = true for a VM Atlas never asserted to this host")
	}
	if record.UUID != "" || record.BootEpoch != 0 {
		t.Fatalf("record = %+v, want the zero value", record)
	}
}

func TestListDesiredIsEmptyThenOrderedByUUID(t *testing.T) {
	store := newTestStore(t)
	records, err := store.ListDesired()
	if err != nil {
		t.Fatalf("list on a fresh store: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("fresh store listed %d desired records", len(records))
	}

	for _, uuid := range []string{"vm-c", "vm-a", "vm-b"} {
		if err := store.PutDesired(desiredVirtualMachine(uuid, model.PowerRunning)); err != nil {
			t.Fatalf("put desired %s: %v", uuid, err)
		}
	}
	records, err = store.ListDesired()
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

func TestDesiredSurvivesReopen(t *testing.T) {
	path := reopenablePath(t)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.PutDesired(desiredVirtualMachine("vm-a", model.PowerRunning)); err != nil {
		t.Fatalf("put desired: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	record, found, err := reopened.GetDesired("vm-a")
	if err != nil || !found {
		t.Fatalf("get desired after reopen: found=%v err=%v", found, err)
	}
	if record.DesiredPower != model.PowerRunning || record.BootEpoch != 7 {
		t.Fatalf("read back %+v, want the asserted record", record)
	}
}

// Desired state is Atlas's input, not this host's observation, and none of it
// reaches an export. A bump here would fail a caller's CAS over a write that
// changed nothing the caller can see.
func TestPutDesiredLeavesTheObservedEpochAlone(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutVirtualMachine(observedVirtualMachine("vm-a", model.StatusRunning)); err != nil {
		t.Fatalf("put: %v", err)
	}
	before := observedEpochOrFail(t, store)

	for range 3 {
		if err := store.PutDesired(desiredVirtualMachine("vm-a", model.PowerRunning)); err != nil {
			t.Fatalf("put desired: %v", err)
		}
	}

	if after := observedEpochOrFail(t, store); after != before {
		t.Fatalf("observed epoch moved %d -> %d on a desired-state write", before, after)
	}
}
