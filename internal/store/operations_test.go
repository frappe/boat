package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
)

func TestClaimOperationRecordsAFreshRunningOperation(t *testing.T) {
	store := newTestStore(t)
	operation, claimed, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	if operation.Status != model.OperationRunning {
		t.Fatalf("status = %q, want Running", operation.Status)
	}
	if operation.StartedAt.IsZero() {
		t.Fatalf("a claimed operation must be stamped with when it started")
	}

	// The claim is durable the moment ClaimOperation returns: a crash between
	// here and the verb finishing is what leaves the Running record a reconciler
	// later has to resume.
	recorded, found, err := store.GetOperation("task-1")
	if err != nil || !found {
		t.Fatalf("get after claim: found=%v err=%v", found, err)
	}
	if recorded.Status != model.OperationRunning || recorded.Verb != "start" {
		t.Fatalf("recorded %+v, want a Running start", recorded)
	}
}

func TestClaimOperationTwiceIsAReplay(t *testing.T) {
	store := newTestStore(t)
	first, _, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	replay, claimed, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if claimed {
		t.Fatalf("a retried Task claimed the operation a second time — it would run the verb twice")
	}
	if !replay.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("replay started at %v, want the original %v", replay.StartedAt, first.StartedAt)
	}
}

func TestClaimOperationReplaysTheRecordedOutcome(t *testing.T) {
	store := newTestStore(t)
	claim, _, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.CompleteOperation(succeeded(claim, "restored from snapshot")); err != nil {
		t.Fatalf("complete: %v", err)
	}

	replay, claimed, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil || claimed {
		t.Fatalf("replay of a finished operation: claimed=%v err=%v", claimed, err)
	}
	if replay.Status != model.OperationSuccess || replay.Output != "restored from snapshot" {
		t.Fatalf("replay returned %+v, want the recorded outcome", replay)
	}
}

func TestClaimOperationRefusesAnIdentifierReusedForDifferentWork(t *testing.T) {
	reuses := map[string]struct{ verb, uuid string }{
		"different verb": {"stop", "vm-a"},
		"different vm":   {"start", "vm-b"},
		"both":           {"stop", "vm-b"},
	}
	for name, reuse := range reuses {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			if _, _, err := store.ClaimOperation("task-1", "start", "vm-a"); err != nil {
				t.Fatalf("first claim: %v", err)
			}
			_, claimed, err := store.ClaimOperation("task-1", reuse.verb, reuse.uuid)
			if !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("err = %v, want ErrOperationConflict", err)
			}
			if claimed {
				t.Fatalf("claimed = true on a conflicting identifier")
			}
		})
	}
}

// The single-claim guarantee is the whole reason this package exists, so it is
// proved against a real file with real goroutines rather than argued about.
func TestConcurrentClaimsProduceExactlyOneWinner(t *testing.T) {
	store := newTestStore(t)
	const callers = 64

	operations := make([]model.Operation, callers)
	claims := make([]bool, callers)
	failures := make([]error, callers)
	release := make(chan struct{})
	var callersDone sync.WaitGroup
	for caller := range callers {
		callersDone.Add(1)
		go func() {
			defer callersDone.Done()
			<-release // Line them all up so they contend on the same transaction.
			operations[caller], claims[caller], failures[caller] = store.ClaimOperation("task-1", "start", "vm-a")
		}()
	}
	close(release)
	callersDone.Wait()

	winners := 0
	for caller := range callers {
		if failures[caller] != nil {
			t.Fatalf("caller %d: %v", caller, failures[caller])
		}
		if claims[caller] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d callers were told to run the verb, want exactly 1", winners)
	}
	assertOneRecordWasReturnedToEveryone(t, operations)
}

// Losing a claim is only useful if the loser is handed the winner's record. They
// all share one StartedAt precisely when nobody invented a second claim.
func assertOneRecordWasReturnedToEveryone(t *testing.T, operations []model.Operation) {
	t.Helper()
	for caller, operation := range operations {
		if operation.Identifier != "task-1" || operation.Verb != "start" {
			t.Fatalf("caller %d got %+v, want the claimed operation", caller, operation)
		}
		if !operation.StartedAt.Equal(operations[0].StartedAt) {
			t.Fatalf("caller %d started at %v, caller 0 at %v — two claims were made",
				caller, operation.StartedAt, operations[0].StartedAt)
		}
	}
}

func TestCompleteOperationWritesTheTerminalRecord(t *testing.T) {
	store := newTestStore(t)
	claim, _, err := store.ClaimOperation("task-1", "stop", "vm-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.CompleteOperation(succeeded(claim, "+ systemctl stop")); err != nil {
		t.Fatalf("complete: %v", err)
	}

	recorded, found, err := store.GetOperation("task-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !recorded.Finished() || recorded.Status != model.OperationSuccess {
		t.Fatalf("recorded %+v, want a finished Success", recorded)
	}
	if !recorded.StartedAt.Equal(claim.StartedAt) {
		t.Fatalf("completion rewrote StartedAt to %v; the claim owns it", recorded.StartedAt)
	}
	if recorded.EndedAt.IsZero() {
		t.Fatalf("a terminal record must carry when it ended")
	}
}

func TestCompleteOperationKeepsTheFirstOutcome(t *testing.T) {
	store := newTestStore(t)
	claim, _, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.CompleteOperation(succeeded(claim, "first")); err != nil {
		t.Fatalf("first completion: %v", err)
	}

	late := succeeded(claim, "late")
	late.Status = model.OperationFailure
	late.Error = "boom"
	if err := store.CompleteOperation(late); err != nil {
		t.Fatalf("a late completion is dropped, not refused: %v", err)
	}

	recorded, _, err := store.GetOperation("task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if recorded.Status != model.OperationSuccess || recorded.Output != "first" || recorded.Error != "" {
		t.Fatalf("recorded %+v, want the first outcome untouched", recorded)
	}
}

func TestCompleteOperationRejectsANonTerminalStatus(t *testing.T) {
	store := newTestStore(t)
	claim, _, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.CompleteOperation(claim); err == nil {
		t.Fatalf("completing with status Running was accepted")
	}
}

func TestCompleteOperationRejectsAnUnclaimedIdentifier(t *testing.T) {
	store := newTestStore(t)
	orphan := model.Operation{
		Identifier:         "task-1",
		Verb:               "start",
		VirtualMachineUUID: "vm-a",
		Status:             model.OperationSuccess,
	}
	err := store.CompleteOperation(orphan)
	if !errors.Is(err, errUnclaimedOperation) {
		t.Fatalf("err = %v, want errUnclaimedOperation", err)
	}
}

func TestCompleteOperationRefusesDifferentWork(t *testing.T) {
	store := newTestStore(t)
	claim, _, err := store.ClaimOperation("task-1", "start", "vm-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	impostor := succeeded(claim, "")
	impostor.Verb = "stop"
	if err := store.CompleteOperation(impostor); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
}

func TestGetOperationAbsentIsNotAnError(t *testing.T) {
	store := newTestStore(t)
	operation, found, err := store.GetOperation("never-posted")
	if err != nil {
		t.Fatalf("absence must not be an error, got %v", err)
	}
	if found {
		t.Fatalf("found = true for an identifier the journal never saw")
	}
	if operation != (model.Operation{}) {
		t.Fatalf("operation = %+v, want the zero value", operation)
	}
}

func succeeded(claim model.Operation, output string) model.Operation {
	claim.Status = model.OperationSuccess
	claim.EndedAt = time.Now().UTC()
	claim.Output = output
	return claim
}
