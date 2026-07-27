package reconcile

import (
	"testing"

	"github.com/frappe/boat/internal/model"
)

// plan is a pure function, so the rules it encodes are a table. Each row is a
// sentence about power, and the two that carry the most weight are the sleeping
// ones: a stopped VM that a wake must not resurrect, and a sleeping VM that a
// sweep must not disturb.
func TestPlan(t *testing.T) {
	cases := map[string]struct {
		power    model.DesiredPower
		observed model.VirtualMachineStatus
		why      trigger
		want     step
	}{
		"a stopped VM that should run is started": {
			model.PowerRunning, model.StatusStopped, triggerSweep, stepStart,
		},
		"a failed VM that should run is started": {
			model.PowerRunning, model.StatusFailed, triggerSweep, stepStart,
		},
		"a running VM that should run is left alone": {
			model.PowerRunning, model.StatusRunning, triggerSweep, stepNone,
		},
		"a running VM that should be stopped is stopped": {
			model.PowerStopped, model.StatusRunning, triggerSweep, stepStop,
		},
		"a stopped VM that should be stopped is left alone": {
			model.PowerStopped, model.StatusStopped, triggerSweep, stepNone,
		},
		"a paused VM that should run is left to the resume verb": {
			model.PowerRunning, model.StatusPaused, triggerSweep, stepNone,
		},
		"a paused VM that should be stopped is stopped": {
			model.PowerStopped, model.StatusPaused, triggerSweep, stepStop,
		},
		"a sleeping VM is not woken by the sweep": {
			model.PowerRunning, model.StatusSleeping, triggerSweep, stepNone,
		},
		// Wake and not start: a start on a VM whose sleeping marker is still there
		// is skipped by the unit and converges nothing.
		"a sleeping VM asked for by name is woken": {
			model.PowerRunning, model.StatusSleeping, triggerRequest, stepWake,
		},
		"a stopped VM is not started by a wake": {
			model.PowerStopped, model.StatusStopped, triggerRequest, stepNone,
		},
		"a sleeping VM the operator stopped is not resurrected by a wake": {
			model.PowerStopped, model.StatusSleeping, triggerRequest, stepNone,
		},
		"a host that could not be read is not acted on": {
			model.PowerRunning, model.StatusUnknown, triggerRequest, stepNone,
		},
		"a desired record that states no power is not guessed at": {
			"", model.StatusStopped, triggerRequest, stepNone,
		},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			desired := model.DesiredVirtualMachine{UUID: firstVirtualMachine, DesiredPower: want.power}
			observed := model.VirtualMachine{UUID: firstVirtualMachine, ObservedStatus: want.observed}
			if got := plan(desired, observed, want.why); got != want.want {
				t.Fatalf("plan = %v, want %v", got, want.want)
			}
		})
	}
}

// everyObservedStatus is exhaustive on purpose: the rule below is about all of
// them, so a status added later is covered by adding it here rather than by
// hoping somebody remembers to write another row.
var everyObservedStatus = []model.VirtualMachineStatus{
	model.StatusRunning,
	model.StatusStopped,
	model.StatusSleeping,
	model.StatusPaused,
	model.StatusFailed,
	model.StatusUnknown,
}

// The precedence rule, pinned where it cannot quietly move: under
// desired_power = Stopped the trigger changes NOTHING, whatever the host was
// observed to be.
//
// This is not a restatement of the table above, it is a test about the SHAPE of
// plan. The obvious way to add a wake step is to notice the sleeping VM and the
// request first — `if why == triggerRequest && observed is Sleeping` near the
// top — and that version passes every ordinary case and fails the sleeping row
// here, which is precisely the resurrection this rule exists to refuse: the wake
// trap turns an unauthenticated SYN into a requested pass, so a trigger read
// ahead of the desire is a stranger's packet outranking an operator's stop.
func TestAStoppedDesireIsDecidedBeforeTheTriggerIsRead(t *testing.T) {
	desired := model.DesiredVirtualMachine{UUID: firstVirtualMachine, DesiredPower: model.PowerStopped}
	for _, status := range everyObservedStatus {
		observed := model.VirtualMachine{UUID: firstVirtualMachine, ObservedStatus: status}
		swept := plan(desired, observed, triggerSweep)
		requested := plan(desired, observed, triggerRequest)
		if swept != requested {
			t.Errorf(
				"a %s virtual machine that should be stopped plans %v when swept and %v when asked for:"+
					" the trigger reached a stopped desire", status, swept, requested,
			)
		}
		if requested == stepStart || requested == stepWake {
			t.Errorf("a wake on a %s virtual machine that should be stopped planned %v", status, requested)
		}
	}
}
