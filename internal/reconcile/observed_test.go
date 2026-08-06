package reconcile

import (
	"testing"

	"github.com/frappe/boat/internal/model"
)

// A change the reconciler notices on its own has to reach the watch stream, or
// the stream announces only what a verb caused — i.e. only what Atlas already
// knows, never a guest that died, a unit that failed, or a VM the wake trap
// resumed. So every observation a pass writes is handed to the publisher.
func TestEachObservationAPassWritesIsPublished(t *testing.T) {
	harness := newReconciler(t)

	var published []model.VirtualMachine
	harness.reconciler.OnObserved(func(record model.VirtualMachine) {
		published = append(published, record)
	})

	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.setStatus(firstVirtualMachine, model.StatusStopped)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("pass: %v", err)
	}

	// The pass observed before it acted and again after it acted, so the watchers
	// hear twice: the Stopped it found and the Running it produced. The last word
	// is what the host became, which is what an export would later confirm.
	if len(published) != 2 {
		t.Fatalf("published %d observations, want 2 (before and after the start)", len(published))
	}
	if last := published[len(published)-1]; last.UUID != firstVirtualMachine || last.ObservedStatus != model.StatusRunning {
		t.Fatalf("last published = %+v, want %s Running", last, firstVirtualMachine)
	}
}

// A pass that changes nothing still publishes: a watcher exists for freshness,
// and "still Running" an interval later is what tells it this daemon is watching
// at all. The single observation is the one the pass made.
func TestASteadyPassStillPublishesItsObservation(t *testing.T) {
	harness := newReconciler(t)

	published := 0
	harness.reconciler.OnObserved(func(model.VirtualMachine) { published++ })

	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.setStatus(firstVirtualMachine, model.StatusRunning)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := verbs(harness); len(got) != 0 {
		t.Fatalf("the host was asked for %v, want nothing", got)
	}
	if published != 1 {
		t.Fatalf("published %d observations, want 1 (the steady observation)", published)
	}
}

// A reconciler wired to no publisher announces nothing and drives the host all
// the same. The daemon points OnObserved at the API's hub only after that server
// exists, so the unwired reconciler is a real state, not a test artefact — and
// every other test in this package runs in it.
func TestObservationsWithoutAPublisherAreHarmless(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.setStatus(firstVirtualMachine, model.StatusStopped)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if status := harness.observedStatus(t, firstVirtualMachine); status != model.StatusRunning {
		t.Fatalf("recorded status = %q, want Running", status)
	}
}
