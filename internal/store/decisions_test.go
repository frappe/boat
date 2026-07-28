package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/frappe/boat/internal/model"
)

// reopenable is a store whose path outlives it, so a test can close it and open
// it again — which is the only way to reach a second incarnation, and the only
// way to prove a record is on disk rather than in a struct.
type reopenable struct {
	path  string
	store *Store
}

func newReopenableStore(t *testing.T) *reopenable {
	t.Helper()
	host := &reopenable{path: filepath.Join(t.TempDir(), "boat.db")}
	host.reopen(t)
	return host
}

func (host *reopenable) reopen(t *testing.T) *Store {
	t.Helper()
	if host.store != nil {
		if err := host.store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
	store, err := Open(host.path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	host.store = store
	t.Cleanup(func() { store.Close() })
	return store
}

func claim(t *testing.T, store *Store, identifier string) model.Operation {
	t.Helper()
	operation, claimed, err := store.ClaimOperation(identifier, "start-vm", "vm-a")
	if err != nil || !claimed {
		t.Fatalf("claim %s: claimed=%v err=%v", identifier, claimed, err)
	}
	return operation
}

// The stamp is written by the claim's own transaction, so there is no window in
// which an operation exists without one. That is what makes a crash between the
// claim and the terminal record visible afterwards.
func TestClaimStampsThisRunsIncarnation(t *testing.T) {
	host := newReopenableStore(t)
	first := claim(t, host.store, "task-1")
	if first.Incarnation != host.store.Incarnation() {
		t.Fatalf("claimed under incarnation %d, want this run's %d", first.Incarnation, host.store.Incarnation())
	}

	second := host.reopen(t)
	if second.Incarnation() <= first.Incarnation {
		t.Fatalf("the second run holds incarnation %d, want one later than %d", second.Incarnation(), first.Incarnation)
	}
	recorded, _, err := second.GetOperation("task-1")
	if err != nil {
		t.Fatalf("read the operation the first run claimed: %v", err)
	}
	if recorded.Incarnation != first.Incarnation {
		t.Fatalf("the record now says incarnation %d, want the run that claimed it (%d)",
			recorded.Incarnation, first.Incarnation)
	}
}

// A replay belongs to the run that started the work. Re-stamping it would let a
// restarted daemon adopt an operation it never began, which is exactly what the
// stamp exists to stop it from doing.
func TestReplayingAClaimKeepsTheIncarnationThatTookIt(t *testing.T) {
	host := newReopenableStore(t)
	first := claim(t, host.store, "task-1")

	second := host.reopen(t)
	replay, claimed, err := second.ClaimOperation("task-1", "start-vm", "vm-a")
	if err != nil || claimed {
		t.Fatalf("replay: claimed=%v err=%v", claimed, err)
	}
	if replay.Incarnation != first.Incarnation {
		t.Fatalf("replay carries incarnation %d, want the original %d", replay.Incarnation, first.Incarnation)
	}
}

// Listing is what makes a crash findable: without it an operation left Running
// is in the file and reachable only by a name nobody has.
func TestListOperationsReturnsEveryRecordInKeyOrder(t *testing.T) {
	store := newTestStore(t)
	for _, identifier := range []string{"task-3", "task-1", "task-2"} {
		claim(t, store, identifier)
	}
	operations, err := store.ListOperations()
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	got := []string{}
	for _, operation := range operations {
		got = append(got, operation.Identifier)
	}
	want := []string{"task-1", "task-2", "task-3"}
	if len(got) != len(want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("listed %v, want %v in key order", got, want)
		}
	}
}

func TestListOperationsOnAHostThatHasClaimedNothing(t *testing.T) {
	operations, err := newTestStore(t).ListOperations()
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("listed %d operations on a fresh host, want none", len(operations))
	}
}

// The authorization and the record commit together, so a decision can never
// name work this host was not doing.
func TestRecordDecisionRefusesAnOperationTheStoreNeverClaimed(t *testing.T) {
	store := newTestStore(t)
	err := store.RecordDecision(model.Decision{OperationID: "task-1", Step: "allocate-address"})
	if !errors.Is(err, errUnclaimedOperation) {
		t.Fatalf("recording against an unclaimed operation returned %v, want a refusal", err)
	}
	decisions, err := store.Decisions("task-1")
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("the refused decision was written anyway: %v", decisions)
	}
}

func TestRecordDecisionRefusesAnOperationThatAlreadyFinished(t *testing.T) {
	store := newTestStore(t)
	operation := claim(t, store, "task-1")
	operation.Status = model.OperationSuccess
	if err := store.CompleteOperation(operation); err != nil {
		t.Fatalf("complete: %v", err)
	}

	err := store.RecordDecision(model.Decision{OperationID: "task-1", Step: "allocate-address"})
	if !errors.Is(err, errUndecidableOperation) {
		t.Fatalf("recording against a finished operation returned %v, want a refusal", err)
	}
}

// Durability is the whole contract: the decision is on the platter when Record
// returns, so the run that reads it back is a run the first one did not survive.
func TestDecisionsSurviveReopeningTheFile(t *testing.T) {
	host := newReopenableStore(t)
	claim(t, host.store, "task-1")
	decision := model.Decision{
		OperationID: "task-1",
		Step:        "allocate-address",
		Values:      map[string]string{"ipv6": "2001:db8::5"},
	}
	if err := host.store.RecordDecision(decision); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	decisions, err := host.reopen(t).Decisions("task-1")
	if err != nil {
		t.Fatalf("read decisions after the reopen: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Values["ipv6"] != "2001:db8::5" {
		t.Fatalf("decisions after the reopen = %v, want the address decided before it", decisions)
	}
}
