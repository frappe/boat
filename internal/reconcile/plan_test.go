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
		"a sleeping VM asked for by name is resumed": {
			model.PowerRunning, model.StatusSleeping, triggerRequest, stepStart,
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
