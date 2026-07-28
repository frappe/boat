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

func TestUnfinishedIsEmptyOnAHostThatHasClaimedNothing(t *testing.T) {
	if operations := unfinishedOf(t, newHost(t).restart(t)); len(operations) != 0 {
		t.Fatalf("unfinished = %v on a fresh host, want none", identifiers(operations))
	}
}

// The headline case, and the one that was unreachable while only decisions were
// stamped: start, stop, pause, resume, sleep, wake, resize and terminate choose
// nothing, so a crash in the middle of one leaves a claim and no decision at
// all. That claim is still work no process is doing.
func TestUnfinishedListsAnOperationThatDecidedNothing(t *testing.T) {
	host := newHost(t)
	host.restart(t)
	host.claim(t, firstOperation)

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished = %v, want the claim the crash left behind", got)
	}
}

// Slow is not unfinished. An operation this daemon is still running is
// non-terminal too, and resuming it would double-run the verb the journal exists
// to make replayable.
func TestUnfinishedIgnoresAnOperationThisIncarnationIsStillRunning(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", nil))

	if operations := unfinishedOf(t, journal); len(operations) != 0 {
		t.Fatalf("unfinished = %v while this incarnation owns it, want none", identifiers(operations))
	}
}

// The converse: the same record, the same decisions, read by the next
// incarnation. The process that owned it is gone, so this is the work a crash
// left behind — and its checkpoint is readable, which is the reason resuming it
// is not the same as starting it again.
func TestUnfinishedListsWhatAnEarlierIncarnationLeftRunning(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))

	resumed := host.restart(t)
	operations := unfinishedOf(t, resumed)
	if got := identifiers(operations); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished = %v, want the operation the crash left behind", got)
	}
	if operations[0].Status != model.OperationRunning {
		t.Fatalf("status = %q, want the store's own record", operations[0].Status)
	}
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
	complete(t, host, firstOperation, model.OperationSuccess)

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{secondOperation}) {
		t.Fatalf("unfinished = %v, want only the one that never completed", got)
	}
}

// A resume that crashes is resumed again. The stamp is the incarnation that
// CLAIMED the operation and no later run rewrites it, so an operation abandoned
// twice is still stranded the second time — and it stops being listed only when
// something records a terminal outcome for it.
func TestUnfinishedListsAnOperationTwoRestartsFailedToClose(t *testing.T) {
	host := newHost(t)
	host.restart(t)
	host.claim(t, firstOperation)

	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished after the first crash = %v", got)
	}
	if got := identifiers(unfinishedOf(t, host.restart(t))); !equal(got, []string{firstOperation}) {
		t.Fatalf("unfinished after the second crash = %v, want it back", got)
	}

	complete(t, host, firstOperation, model.OperationFailure)
	if operations := unfinishedOf(t, host.restart(t)); len(operations) != 0 {
		t.Fatalf("unfinished = %v after it was closed, want none", identifiers(operations))
	}
}

func complete(t *testing.T, host *host, identifier string, status model.OperationStatus) {
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
