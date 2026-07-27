package journal

import (
	"testing"

	"github.com/frappe/boat/internal/model"
)

func unfinishedOf(t *testing.T, journal *Journal) []model.Operation {
	t.Helper()
	operations, err := journal.Unfinished()
	if err != nil {
		t.Fatalf("read unfinished operations: %v", err)
	}
	return operations
}

func identifiers(operations []model.Operation) []string {
	names := []string{}
	for _, operation := range operations {
		names = append(names, operation.Identifier)
	}
	return names
}

func TestUnfinishedIsEmptyOnAHostThatHasDecidedNothing(t *testing.T) {
	if operations := unfinishedOf(t, newHost(t).restart(t)); len(operations) != 0 {
		t.Fatalf("unfinished = %v on a fresh host, want none", identifiers(operations))
	}
}

// Slow is not unfinished. An operation this daemon is still running is
// non-terminal in the store and has decisions in the journal, and resuming it
// would double-run the verb the journal exists to make replayable.
func TestUnfinishedIgnoresAnOperationThisIncarnationIsStillRunning(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", nil))

	if operations := unfinishedOf(t, journal); len(operations) != 0 {
		t.Fatalf("unfinished = %v while this incarnation owns it, want none", identifiers(operations))
	}
}

// The converse: the same store record, the same decisions, read by the next
// incarnation. The process that owned it is gone, so this is the work a crash
// left behind.
func TestUnfinishedListsWhatAnEarlierIncarnationLeftRunning(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	resumed := host.restart(t)
	operations := unfinishedOf(t, resumed)
	if got := identifiers(operations); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished = %v, want the operation the crash left behind", got)
	}
	if operations[0].Status != model.OperationRunning {
		t.Fatalf("status = %q, want the store's own record", operations[0].Status)
	}
	// And its checkpoint is readable, which is the reason resuming it is not the
	// same as starting it again.
	if steps := steps(decisionsOf(t, resumed, firstOperation)); !equal(steps, []string{"allocate-address"}) {
		t.Fatalf("decisions after the restart = %v", steps)
	}
}

// The store owns status. An operation that finished before the crash is
// finished, however many decisions it recorded on the way.
func TestUnfinishedSkipsAnOperationThatReachedATerminalState(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	host.claim(t, secondOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", nil))
	record(t, journal, decisionOf(secondOperation, "allocate-address", nil))
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	complete(t, host, firstOperation, model.OperationSuccess)

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{secondOperation}) {
		t.Fatalf("unfinished = %v, want only the one that never completed", got)
	}
}

// A resume that crashes is resumed again. The in-flight stamp moves to the
// incarnation that took the latest decision, so an operation abandoned twice is
// not lost the second time.
func TestUnfinishedListsAnOperationAResumeAbandonedAgain(t *testing.T) {
	host := newHost(t)
	first := host.restart(t)
	host.claim(t, firstOperation)
	record(t, first, decisionOf(firstOperation, "allocate-address", nil))
	if err := first.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	second := host.restart(t)
	if got := identifiers(unfinishedOf(t, second)); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished after the first crash = %v", got)
	}
	record(t, second, decisionOf(firstOperation, "create-volume", nil))
	if operations := unfinishedOf(t, second); len(operations) != 0 {
		t.Fatalf("unfinished = %v while this incarnation is resuming it", identifiers(operations))
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished after the second crash = %v, want it back", got)
	}
}

// A decision filed against an operation the store never claimed is a caller bug.
// It is skipped rather than resumed — there is no record to drive forward — and
// it must not take the resume of every other operation down with it.
func TestUnfinishedSkipsADecisionTheStoreNeverClaimed(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, secondOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", nil))
	record(t, journal, decisionOf(secondOperation, "allocate-address", nil))
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{secondOperation}) {
		t.Fatalf("unfinished = %v, want only the operation the store knows", got)
	}
}

func complete(t *testing.T, host host, identifier string, status model.OperationStatus) {
	t.Helper()
	operation := model.Operation{
		Identifier:         identifier,
		Verb:               "start-vm",
		VirtualMachineUUID: virtualMachine,
		Status:             status,
	}
	if err := host.store.CompleteOperation(operation); err != nil {
		t.Fatalf("complete %s: %v", identifier, err)
	}
}
