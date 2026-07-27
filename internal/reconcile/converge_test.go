package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/frappe/boat/internal/fence"
	"github.com/frappe/boat/internal/model"
)

// verbs is what the host was asked to do, in order, ignoring the observations
// every pass makes on its way in and out.
func verbs(harness *harness) []string {
	asked := []string{}
	for _, recorded := range harness.machines.recorded() {
		if recorded.verb != "observe" {
			asked = append(asked, recorded.verb)
		}
	}
	return asked
}

func passOver(t *testing.T, harness *harness, uuid string, why trigger) error {
	t.Helper()
	return harness.reconciler.pass(context.Background(), uuid, why)
}

func TestAStoppedVirtualMachineThatShouldBeRunningIsStarted(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.setStatus(firstVirtualMachine, model.StatusStopped)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := verbs(harness); len(got) != 1 || got[0] != "start" {
		t.Fatalf("the host was asked for %v, want one start", got)
	}
	// And the store holds what the host became, not what the pass intended.
	if status := harness.observedStatus(t, firstVirtualMachine); status != model.StatusRunning {
		t.Fatalf("recorded status = %q, want Running", status)
	}
}

func TestARunningVirtualMachineThatShouldBeStoppedIsStopped(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerStopped)
	harness.machines.setStatus(firstVirtualMachine, model.StatusRunning)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := verbs(harness); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("the host was asked for %v, want one stop", got)
	}
	if status := harness.observedStatus(t, firstVirtualMachine); status != model.StatusStopped {
		t.Fatalf("recorded status = %q, want Stopped", status)
	}
}

// A VM already where it should be is observed and nothing else. A reconciler
// that "made sure" by starting a running VM every interval would restart the
// host's whole fleet on the day systemctl start stops being a no-op.
func TestAVirtualMachineInItsDesiredStateIsLeftAlone(t *testing.T) {
	cases := map[string]struct {
		power  model.DesiredPower
		status model.VirtualMachineStatus
	}{
		"running and wanted running": {model.PowerRunning, model.StatusRunning},
		"stopped and wanted stopped": {model.PowerStopped, model.StatusStopped},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			harness := newReconciler(t)
			harness.desire(t, firstVirtualMachine, want.power)
			harness.machines.setStatus(firstVirtualMachine, want.status)

			if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
				t.Fatalf("pass: %v", err)
			}
			if got := verbs(harness); len(got) != 0 {
				t.Fatalf("the host was asked for %v, want nothing", got)
			}
			if harness.machines.counted("observe") != 1 {
				t.Fatalf("observed %d times, want once", harness.machines.counted("observe"))
			}
		})
	}
}

// Re-entering a pass is the ordinary case, not the exceptional one: the sweep
// runs every interval and a wake may land at any moment. The second pass must
// find nothing to do.
func TestAPassIsSafeToRunAgain(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)

	for attempt := 0; attempt < 3; attempt++ {
		if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
			t.Fatalf("pass %d: %v", attempt, err)
		}
	}
	if got := verbs(harness); len(got) != 1 || got[0] != "start" {
		t.Fatalf("the host was asked for %v, want exactly one start across three passes", got)
	}
}

// No assertion from Atlas is no authority to act — not even to look. The VM
// whose artifacts are on this disk may already be running on the host it was
// migrated to.
func TestAVirtualMachineWithNoDesiredStateIsNotTouched(t *testing.T) {
	harness := newReconciler(t)
	if err := passOver(t, harness, firstVirtualMachine, triggerRequest); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := harness.machines.recorded(); len(got) != 0 {
		t.Fatalf("the host was asked for %v, want nothing at all", got)
	}
}

// The fence is the reconciler's gate too, and for a stronger reason than the
// API's: nobody has to ask for a background loop.
func TestAVirtualMachineWithNoFenceIsNotStarted(t *testing.T) {
	harness := newReconciler(t)
	harness.desireUnfenced(t, firstVirtualMachine, model.PowerRunning)

	err := passOver(t, harness, firstVirtualMachine, triggerSweep)
	if !errors.Is(err, fence.ErrNoFence) {
		t.Fatalf("pass error = %v, want a refusal for want of a fence", err)
	}
	if count := harness.machines.counted("start"); count != 0 {
		t.Fatalf("started %d times without a fence", count)
	}
}

// A host that could not be read is not a host to act on. The pass fails, which
// is what puts the VM into backoff rather than into a guess.
func TestAHostThatCannotBeReadIsNotDriven(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.refuse("observe", errHostRefused)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); !errors.Is(err, errHostRefused) {
		t.Fatalf("pass error = %v, want the host's refusal", err)
	}
	if count := harness.machines.counted("start"); count != 0 {
		t.Fatalf("started %d times on a host that could not be read", count)
	}
}

// A sleeping VM is left asleep by the sweep and resumed when someone asks for it
// by name — the two halves of sleep-on-idle staying useful.
func TestASleepingVirtualMachineIsResumedOnlyWhenAskedFor(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.setStatus(firstVirtualMachine, model.StatusSleeping)

	if err := passOver(t, harness, firstVirtualMachine, triggerSweep); err != nil {
		t.Fatalf("sweep pass: %v", err)
	}
	if count := harness.machines.counted("start"); count != 0 {
		t.Fatalf("the sweep woke a sleeping virtual machine %d times", count)
	}
	if err := passOver(t, harness, firstVirtualMachine, triggerRequest); err != nil {
		t.Fatalf("requested pass: %v", err)
	}
	if count := harness.machines.counted("start"); count != 1 {
		t.Fatalf("started %d times, want the requested pass to resume it once", count)
	}
}

// The precedence rule, driven end to end: a VM an operator stopped is not
// resurrected by a stranger's SYN, whether it is stopped or parked.
func TestAStoppedVirtualMachineIsNotStartedByAWake(t *testing.T) {
	for name, status := range map[string]model.VirtualMachineStatus{
		"stopped":  model.StatusStopped,
		"sleeping": model.StatusSleeping,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newReconciler(t)
			harness.desire(t, firstVirtualMachine, model.PowerStopped)
			harness.machines.setStatus(firstVirtualMachine, status)

			harness.reconciler.Wake(firstVirtualMachine)
			harness.settled(t)
			if count := harness.machines.counted("start"); count != 0 {
				t.Fatalf("a wake started a virtual machine that was told to stay down (%d times)", count)
			}
		})
	}
}
