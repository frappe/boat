package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/frappe/boat/internal/model"
)

// runInBackground starts the loop and returns what it exits with, so a test can
// assert on the shutdown as well as on the work.
func runInBackground(t *testing.T, harness *harness) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan error, 1)
	go func() { exited <- harness.reconciler.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, exited
}

// The ordering rule: a VM whose operation a crash left unfinished is driven
// before the steady-state sweep gets an opinion about anything. The crashed
// operation is on the second UUID precisely because the sweep walks the store in
// key order and would otherwise reach the first one first — so "the crashed VM
// was touched first" cannot be an accident of ordering.
func TestRunResumesUnfinishedOperationsBeforeItSweeps(t *testing.T) {
	harness := newHarness(t)
	harness.crash(t, "task-aaaa1111", secondVirtualMachine, "allocate-address")
	harness.start(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.desire(t, secondVirtualMachine, model.PowerStopped)
	harness.machines.setStatus(secondVirtualMachine, model.StatusRunning)

	runInBackground(t, harness)
	waitFor(t, "both virtual machines to be reconciled", func() bool {
		return harness.machines.counted("start") > 0 && harness.machines.counted("stop") > 0
	})

	first := harness.machines.recorded()[0]
	if first.uuid != secondVirtualMachine {
		t.Fatalf("the first thing the reconciler touched was %s, want the virtual machine whose operation was unfinished", first.uuid)
	}
}

// Seven of the nine verbs choose nothing, so the only thing a crash in the
// middle of one leaves behind is the claim. That claim is what a replay reads
// and what GET /ops answers with, so it has to be recovered exactly as a
// rebuild's would be — this is the case that was unreachable while only
// decisions were stamped.
func TestRunResumesAnOperationThatRecordedNoDecision(t *testing.T) {
	harness := newHarness(t)
	harness.crash(t, "task-aaaa1111", firstVirtualMachine)
	harness.start(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)

	runInBackground(t, harness)
	waitFor(t, "the interrupted operation's virtual machine to be driven", func() bool {
		return harness.machines.counted("start") > 0
	})
}

// An operation left Running is one whose Task can never be answered: GET /ops
// reports a verb as in flight forever, and a replayed claim reads the same
// non-terminal record. The resume closes it, and closes it as a Failure —
// nobody observed the verb finish, and asserting a Success would invent an
// outcome.
func TestRunClosesTheRecordOfAnInterruptedOperation(t *testing.T) {
	harness := newHarness(t)
	harness.crash(t, "task-aaaa1111", firstVirtualMachine)
	harness.start(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)

	runInBackground(t, harness)
	waitFor(t, "the interrupted operation to be closed", func() bool {
		return harness.operation(t, "task-aaaa1111").Finished()
	})

	closed := harness.operation(t, "task-aaaa1111")
	if closed.Status != model.OperationFailure {
		t.Fatalf("the interrupted operation was closed as %q, want a Failure nobody has to guess about", closed.Status)
	}
	if closed.Error == "" || closed.EndedAt.IsZero() {
		t.Fatalf("closed as %+v, want a record that says why and when", closed)
	}
}

// And it is closed for good. An operation handed back to every restart after
// this one would make the resume a queue that never empties, which is the leak
// that turns crash recovery into a permanent tax.
func TestRunResumesAnInterruptedOperationOnlyOnce(t *testing.T) {
	harness := newHarness(t)
	harness.crash(t, "task-aaaa1111", firstVirtualMachine)
	harness.start(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	runInBackground(t, harness)
	waitFor(t, "the interrupted operation to be closed", func() bool {
		return harness.operation(t, "task-aaaa1111").Finished()
	})

	harness.open(t)
	harness.start(t)
	unfinished, err := harness.journal.Unfinished()
	if err != nil {
		t.Fatalf("read unfinished operations after the restart: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("the next restart was handed %d operations to resume again", len(unfinished))
	}
}

// A journal that cannot be read means this daemon does not know what it was
// doing when it died. Reconciling anyway is how a fresh pass fights a
// half-finished operation, so the loop refuses to start at all.
func TestRunRefusesToStartWhenTheJournalCannotBeRead(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	if err := harness.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if err := harness.reconciler.Run(context.Background()); err == nil {
		t.Fatal("the reconciler started its loop without knowing what was unfinished")
	}
	if got := harness.machines.recorded(); len(got) != 0 {
		t.Fatalf("the host was driven anyway: %v", got)
	}
}

// The sweep is the safety net: every VM this host holds desired state for is
// converged without anybody asking.
func TestRunSweepsEveryVirtualMachineItHoldsDesiredStateFor(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.desire(t, secondVirtualMachine, model.PowerStopped)
	harness.machines.setStatus(secondVirtualMachine, model.StatusRunning)

	runInBackground(t, harness)
	waitFor(t, "the sweep to converge both virtual machines", func() bool {
		return harness.machines.counted("start") > 0 && harness.machines.counted("stop") > 0
	})
}

// Returning from Run stops the reconciler for good. A daemon being replaced by a
// new binary must not leave passes driving units behind it.
func TestRunStopsEveryPassWhenItReturns(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	cancel, exited := runInBackground(t, harness)
	waitFor(t, "the first sweep", func() bool { return harness.machines.counted("observe") > 0 })

	cancel()
	if err := <-exited; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want the cancellation that ended it", err)
	}
	harness.settled(t)

	before := len(harness.machines.recorded())
	harness.reconciler.Wake(firstVirtualMachine)
	harness.settled(t)
	if after := len(harness.machines.recorded()); after != before {
		t.Fatalf("the host was driven %d more times after the reconciler stopped", after-before)
	}
}
